#include "p2p/observer_audio_receiver.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cstring>
#include <string_view>
#include <utility>

#include <rtc_base/logging.h>

#include "rtc/rtc_connection.h"

namespace {

constexpr const char* kTelemetryLabel = "momo-telemetry";
constexpr size_t kPacketBytes = 84;
constexpr size_t kFrameSamples = 160;
constexpr size_t kStartupFrames = 4;
constexpr size_t kMaxGapFrames = 12;
constexpr int kMaxQueuedBytes = 8000;  // 0.5 秒。停止中の遅延蓄積を防ぐ。

constexpr std::array<int, 16> kImaIndexTable = {
    -1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8,
};
constexpr std::array<int, 89> kImaStepTable = {
    7,    8,    9,    10,   11,   12,   13,   14,   16,   17,   19,
    21,   23,   25,   28,   31,   34,   37,   41,   45,   50,   55,
    60,   66,   73,   80,   88,   97,   107,  118,  130,  143,  157,
    173,  190,  209,  230,  253,  279,  307,  337,  371,  408,  449,
    494,  544,  598,  658,  724,  796,  876,  963,  1060, 1166, 1282,
    1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327, 3660,
    4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
    11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623,
    27086, 29794, 32767,
};

int Clamp(int value, int min, int max) {
  return std::max(min, std::min(max, value));
}

int DecodeBase64Char(char value) {
  if (value >= 'A' && value <= 'Z') return value - 'A';
  if (value >= 'a' && value <= 'z') return value - 'a' + 26;
  if (value >= '0' && value <= '9') return value - '0' + 52;
  if (value == '+') return 62;
  if (value == '/') return 63;
  return -1;
}

bool DecodeBase64(std::string_view input, std::vector<uint8_t>* output) {
  if (input.empty() || input.size() % 4 != 0 || output == nullptr) {
    return false;
  }
  output->clear();
  output->reserve(input.size() / 4 * 3);
  for (size_t i = 0; i < input.size(); i += 4) {
    const int a = DecodeBase64Char(input[i]);
    const int b = DecodeBase64Char(input[i + 1]);
    const int c = input[i + 2] == '=' ? -2 : DecodeBase64Char(input[i + 2]);
    const int d = input[i + 3] == '=' ? -2 : DecodeBase64Char(input[i + 3]);
    if (a < 0 || b < 0 || c < -2 || d < -2 ||
        (c == -2 && d != -2) ||
        ((c == -2 || d == -2) && i + 4 != input.size())) {
      return false;
    }
    output->push_back(static_cast<uint8_t>((a << 2) | (b >> 4)));
    if (c != -2) {
      output->push_back(static_cast<uint8_t>((b << 4) | (c >> 2)));
      if (d != -2) {
        output->push_back(static_cast<uint8_t>((c << 6) | d));
      }
    }
  }
  return true;
}

bool DecodeImaFrame(const std::vector<uint8_t>& packet,
                    std::vector<int16_t>* samples) {
  if (packet.size() != kPacketBytes || packet[2] > 88 || samples == nullptr) {
    return false;
  }
  int predictor = static_cast<int16_t>(
      static_cast<uint16_t>(packet[0]) | (static_cast<uint16_t>(packet[1]) << 8));
  int step_index = packet[2];
  samples->assign(kFrameSamples, 0);
  (*samples)[0] = static_cast<int16_t>(predictor);
  size_t sample_index = 1;
  const auto decode_nibble = [&predictor, &step_index](int nibble) {
    const int step = kImaStepTable[step_index];
    int difference = step >> 3;
    if ((nibble & 4) != 0) difference += step;
    if ((nibble & 2) != 0) difference += step >> 1;
    if ((nibble & 1) != 0) difference += step >> 2;
    predictor = Clamp(predictor + ((nibble & 8) != 0 ? -difference : difference),
                      -32768, 32767);
    step_index = Clamp(step_index + kImaIndexTable[nibble & 0x0f], 0, 88);
    return static_cast<int16_t>(predictor);
  };
  for (size_t packet_index = 4;
       packet_index < packet.size() && sample_index < kFrameSamples;
       ++packet_index) {
    const uint8_t packed = packet[packet_index];
    (*samples)[sample_index++] = decode_nibble(packed & 0x0f);
    if (sample_index < kFrameSamples) {
      (*samples)[sample_index++] = decode_nibble((packed >> 4) & 0x0f);
    }
  }
  return sample_index == kFrameSamples;
}

}  // namespace

ObserverAudioReceiver::ObserverAudioReceiver(std::string source_name)
    : source_name_(std::move(source_name)) {
  if (!SDL_InitSubSystem(SDL_INIT_AUDIO)) {
    initialization_error_ = SDL_GetError();
    RTC_LOG(LS_ERROR) << "Observer audio initialization failed: "
                      << initialization_error_;
    return;
  }
  SDL_AudioSpec spec{};
  spec.format = SDL_AUDIO_S16;
  spec.channels = 1;
  spec.freq = 8000;
  stream_ = SDL_OpenAudioDeviceStream(SDL_AUDIO_DEVICE_DEFAULT_PLAYBACK,
                                      &spec, nullptr, nullptr);
  if (stream_ == nullptr) {
    initialization_error_ = SDL_GetError();
    RTC_LOG(LS_ERROR) << "Observer audio output could not open: "
                      << initialization_error_;
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
    return;
  }
  if (!SDL_ResumeAudioStreamDevice(stream_)) {
    initialization_error_ = SDL_GetError();
    RTC_LOG(LS_ERROR) << "Observer audio output could not start: "
                      << initialization_error_;
    SDL_DestroyAudioStream(stream_);
    stream_ = nullptr;
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
    return;
  }
  audio_initialized_ = true;
  RTC_LOG(LS_INFO) << "Observer audio enabled for source " << source_name_;
}

ObserverAudioReceiver::~ObserverAudioReceiver() {
  DetachChannel();
  webrtc::MutexLock lock(&mutex_);
  if (stream_ != nullptr) {
    SDL_DestroyAudioStream(stream_);
    stream_ = nullptr;
  }
  if (audio_initialized_) {
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
    audio_initialized_ = false;
  }
}

void ObserverAudioReceiver::AttachDataChannel(
    const std::shared_ptr<RTCConnection>& connection) {
  if (!connection) return;
  webrtc::DataChannelInit config;
  config.ordered = false;
  config.maxRetransmits = 0;
  auto result = connection->GetConnection()->CreateDataChannelOrError(
      kTelemetryLabel, &config);
  if (!result.ok()) {
    RTC_LOG(LS_ERROR) << "Observer audio DataChannel creation failed for "
                      << source_name_ << ": " << result.error().message();
    return;
  }
  auto channel = result.MoveValue();
  channel->RegisterObserver(this);
  webrtc::MutexLock lock(&mutex_);
  channel_ = std::move(channel);
}

void ObserverAudioReceiver::OnStateChange() {
  webrtc::MutexLock lock(&mutex_);
  channel_open_ = channel_ &&
                  channel_->state() == webrtc::DataChannelInterface::kOpen;
  if (channel_open_) {
    RTC_LOG(LS_INFO) << "Observer audio DataChannel opened for "
                     << source_name_;
  }
}

void ObserverAudioReceiver::OnMessage(const webrtc::DataBuffer& buffer) {
  // Relay may retain the upstream DataChannel payload type.  Momo's UART
  // bridge can therefore deliver the ASCII AUD: frame as either a text or a
  // binary DataChannel message.  The audio frame format itself is textual,
  // so handle both forms identically and let DecodeAndQueue reject unrelated
  // telemetry.
  DecodeAndQueue(std::string(buffer.data.data<char>(), buffer.data.size()));
}

void ObserverAudioReceiver::OnBufferedAmountChange(uint64_t previous_amount) {
  static_cast<void>(previous_amount);
}

bool ObserverAudioReceiver::DecodeAndQueue(const std::string& message) {
  if (message.rfind("AUD:1,", 0) != 0) return false;
  const std::array<size_t, 5> separators = {
      message.find(','), message.find(',', 6), std::string::npos,
      std::string::npos, std::string::npos};
  if (separators[0] == std::string::npos || separators[1] == std::string::npos) {
    return InvalidAudioFrame();
  }
  std::array<size_t, 5> positions{};
  positions[0] = separators[0];
  positions[1] = separators[1];
  for (size_t i = 2; i < positions.size(); ++i) {
    positions[i] = message.find(',', positions[i - 1] + 1);
    if (positions[i] == std::string::npos) return InvalidAudioFrame();
  }
  const std::string boot_id = message.substr(positions[0] + 1,
                                             positions[1] - positions[0] - 1);
  const std::string sequence_text = message.substr(
      positions[1] + 1, positions[2] - positions[1] - 1);
  if (message.substr(positions[2] + 1, positions[3] - positions[2] - 1) != "8" ||
      message.substr(positions[3] + 1, positions[4] - positions[3] - 1) != "ima") {
    return InvalidAudioFrame();
  }
  uint64_t sequence = 0;
  const auto sequence_parse = std::from_chars(
      sequence_text.data(), sequence_text.data() + sequence_text.size(), sequence);
  if (sequence_parse.ec != std::errc() || sequence_parse.ptr != sequence_text.data() + sequence_text.size()) {
    return InvalidAudioFrame();
  }
  std::vector<uint8_t> packet;
  if (!DecodeBase64(std::string_view(message).substr(positions[4] + 1), &packet)) {
    return InvalidAudioFrame();
  }
  std::vector<int16_t> samples;
  if (!DecodeImaFrame(packet, &samples)) return InvalidAudioFrame();

  webrtc::MutexLock lock(&mutex_);
  if (stream_ == nullptr) return true;
  if (boot_id_ != boot_id || (has_sequence_ && sequence <= last_sequence_)) {
    boot_id_ = boot_id;
    has_sequence_ = false;
    ResetStream();
  }
  if (has_sequence_ && sequence > last_sequence_ + 1) {
    const size_t missing = static_cast<size_t>(
        std::min<uint64_t>(kMaxGapFrames, sequence - last_sequence_ - 1));
    std::vector<int16_t> silence(missing * kFrameSamples, 0);
    QueueSamples(silence);
  }
  last_sequence_ = sequence;
  has_sequence_ = true;
  ++received_frames_;
  if (!QueueSamples(samples)) {
    ++invalid_frames_;
    return false;
  }
  if (received_frames_ == 1 || received_frames_ % 250 == 0) {
    RTC_LOG(LS_INFO) << "Observer audio " << source_name_ << " rx="
                     << received_frames_ << " invalid=" << invalid_frames_;
  }
  return true;
}

bool ObserverAudioReceiver::InvalidAudioFrame() {
  webrtc::MutexLock lock(&mutex_);
  ++invalid_frames_;
  return false;
}

ObserverAudioReceiver::Diagnostics ObserverAudioReceiver::GetDiagnostics() const {
  webrtc::MutexLock lock(&mutex_);
  Diagnostics diagnostics;
  diagnostics.initialized = audio_initialized_;
  diagnostics.channel_open = channel_open_;
  diagnostics.playback_started = playback_started_;
  diagnostics.received_frames = received_frames_;
  diagnostics.invalid_frames = invalid_frames_;
  diagnostics.initialization_error = initialization_error_;
  diagnostics.queued_samples = startup_samples_.size();
  if (stream_ != nullptr) {
    const int queued_bytes = SDL_GetAudioStreamQueued(stream_);
    if (queued_bytes > 0) {
      diagnostics.queued_samples +=
          static_cast<size_t>(queued_bytes) / sizeof(int16_t);
    }
  }
  return diagnostics;
}

bool ObserverAudioReceiver::QueueSamples(const std::vector<int16_t>& samples) {
  if (stream_ == nullptr || samples.empty()) return false;
  if (!playback_started_) {
    startup_samples_.insert(startup_samples_.end(), samples.begin(), samples.end());
    if (startup_samples_.size() < kStartupFrames * kFrameSamples) return true;
    if (!SDL_PutAudioStreamData(stream_, startup_samples_.data(),
                                static_cast<int>(startup_samples_.size() * sizeof(int16_t)))) {
      RTC_LOG(LS_WARNING) << "Observer audio enqueue failed: " << SDL_GetError();
      return false;
    }
    startup_samples_.clear();
    playback_started_ = true;
    return true;
  }
  if (SDL_GetAudioStreamQueued(stream_) > kMaxQueuedBytes) {
    SDL_ClearAudioStream(stream_);
    playback_started_ = false;
    startup_samples_.clear();
    startup_samples_.insert(startup_samples_.end(), samples.begin(), samples.end());
    return true;
  }
  if (!SDL_PutAudioStreamData(stream_, samples.data(),
                              static_cast<int>(samples.size() * sizeof(int16_t)))) {
    RTC_LOG(LS_WARNING) << "Observer audio enqueue failed: " << SDL_GetError();
    return false;
  }
  return true;
}

void ObserverAudioReceiver::ResetStream() {
  if (stream_ != nullptr) SDL_ClearAudioStream(stream_);
  startup_samples_.clear();
  playback_started_ = false;
}

void ObserverAudioReceiver::DetachChannel() {
  webrtc::scoped_refptr<webrtc::DataChannelInterface> channel;
  {
    webrtc::MutexLock lock(&mutex_);
    channel = std::move(channel_);
  }
  if (channel) channel->UnregisterObserver();
}
