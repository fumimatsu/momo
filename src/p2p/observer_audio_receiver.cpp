#include "p2p/observer_audio_receiver.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cmath>
#include <cstdlib>
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
constexpr size_t kMaxRawTelemetrySamples = 180;  // BIN 30 Hz で約 6 秒。

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

bool ParseFloatArray3(const std::string& text, size_t start,
                      std::array<float, 3>* values) {
  if (values == nullptr || start >= text.size()) return false;
  const char* cursor = text.c_str() + start;
  for (size_t index = 0; index < values->size(); ++index) {
    char* end = nullptr;
    const float value = std::strtof(cursor, &end);
    if (end == cursor || !std::isfinite(value)) return false;
    (*values)[index] = value;
    if (index + 1 == values->size()) {
      return *end == ']';
    }
    if (*end != ',') return false;
    cursor = end + 1;
  }
  return false;
}

bool ParseFloatAfterKey(const std::string& text, std::string_view key,
                        float* value) {
  if (value == nullptr) return false;
  const size_t key_position = text.find(key);
  if (key_position == std::string::npos) return false;
  const char* cursor = text.c_str() + key_position + key.size();
  char* end = nullptr;
  const float parsed = std::strtof(cursor, &end);
  if (end == cursor || !std::isfinite(parsed)) return false;
  *value = parsed;
  return true;
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

ObserverAudioReceiver::ObserverAudioReceiver(std::string source_name,
                                             bool enable_audio_playback)
    : source_name_(std::move(source_name)) {
  SetAudioPlaybackEnabled(enable_audio_playback);
}

ObserverAudioReceiver::~ObserverAudioReceiver() {
  DetachChannel();
  SetAudioPlaybackEnabled(false);
}

void ObserverAudioReceiver::SetAudioPlaybackEnabled(bool enabled) {
  webrtc::MutexLock lock(&mutex_);
  if (enabled == audio_playback_enabled_) {
    return;
  }
  if (enabled) {
    audio_playback_enabled_ = EnableAudioPlaybackLocked();
  } else {
    DisableAudioPlaybackLocked();
  }
}

bool ObserverAudioReceiver::EnableAudioPlaybackLocked() {
  initialization_error_.clear();
  if (!SDL_InitSubSystem(SDL_INIT_AUDIO)) {
    initialization_error_ = SDL_GetError();
    RTC_LOG(LS_ERROR) << "Observer audio initialization failed: "
                      << initialization_error_;
    return false;
  }
  audio_subsystem_initialized_ = true;
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
    DisableAudioPlaybackLocked();
    return false;
  }
  if (!SDL_ResumeAudioStreamDevice(stream_)) {
    initialization_error_ = SDL_GetError();
    RTC_LOG(LS_ERROR) << "Observer audio output could not start: "
                      << initialization_error_;
    SDL_DestroyAudioStream(stream_);
    stream_ = nullptr;
    DisableAudioPlaybackLocked();
    return false;
  }
  audio_initialized_ = true;
  RTC_LOG(LS_INFO) << "Observer audio enabled for source " << source_name_;
  return true;
}

void ObserverAudioReceiver::DisableAudioPlaybackLocked() {
  if (stream_ != nullptr) {
    SDL_DestroyAudioStream(stream_);
    stream_ = nullptr;
  }
  if (audio_subsystem_initialized_) {
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
    audio_subsystem_initialized_ = false;
  }
  audio_initialized_ = false;
  audio_playback_enabled_ = false;
  initialization_error_.clear();
  boot_id_.clear();
  last_sequence_ = 0;
  has_sequence_ = false;
  startup_samples_.clear();
  playback_started_ = false;
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

bool ObserverAudioReceiver::DecodeTelemetryAcceleration(const std::string& message) {
  std::string_view acceleration_key;
  if (message.rfind("TEL:{\"v\":1,\"k\":\"s\"", 0) == 0) {
    acceleration_key = "\"imu\":{\"a\":[";
  } else if (message.rfind("TEL:{\"v\":2,\"k\":\"s\"", 0) == 0) {
    // BIN V3 is restored by Momo to this compact V2 representation before it
    // reaches the Observer DataChannel.
    acceleration_key = "\"m\":{\"a\":[";
  } else {
    return false;
  }
  const size_t acceleration_position = message.find(acceleration_key);
  if (acceleration_position == std::string::npos) {
    return true;
  }
  std::array<float, 3> acceleration{};
  if (!ParseFloatArray3(message,
                        acceleration_position + acceleration_key.size(),
                        &acceleration)) {
    return true;
  }

  webrtc::MutexLock lock(&mutex_);
  raw_acceleration_samples_.push_back(acceleration);
  while (raw_acceleration_samples_.size() > kMaxRawTelemetrySamples) {
    raw_acceleration_samples_.pop_front();
  }
  raw_telemetry_received_at_ = std::chrono::steady_clock::now();
  ++raw_telemetry_frames_;
  return true;
}

bool ObserverAudioReceiver::DecodeImpactCandidate(const std::string& message) {
  if (message.rfind("TEL:{\"v\":2,\"k\":\"e\"", 0) != 0 ||
      message.find("\"n\":\"impact_candidate\"") == std::string::npos) {
    return false;
  }
  float magnitude = 0.0f;
  if (!ParseFloatAfterKey(message, "\"m\":", &magnitude)) {
    return true;
  }
  webrtc::MutexLock lock(&mutex_);
  ++impact_candidates_;
  last_impact_mps2_ = magnitude;
  return true;
}

bool ObserverAudioReceiver::DecodeVehicleHealth(const std::string& message) {
  constexpr char kPrefix[] = "VHS:1,";
  if (message.rfind(kPrefix, 0) != 0) return false;
  const char* cursor = message.c_str() + sizeof(kPrefix) - 1;
  char* end = nullptr;
  const float hp = std::strtof(cursor, &end);
  if (end == cursor || !std::isfinite(hp) || *end != ',') return true;
  cursor = end + 1;
  const float speed_cap = std::strtof(cursor, &end);
  if (end == cursor || !std::isfinite(speed_cap) || *end != ',') return true;
  const std::string mode(end + 1);
  if (mode != "healthy" && mode != "damaged" && mode != "critical" &&
      mode != "limp") {
    return true;
  }

  webrtc::MutexLock lock(&mutex_);
  vehicle_hp_ = std::clamp(hp, 0.0f, 100.0f);
  vehicle_speed_cap_ = std::clamp(speed_cap, 0.0f, 1.0f);
  vehicle_health_mode_ = mode;
  vehicle_health_received_at_ = std::chrono::steady_clock::now();
  return true;
}

void ObserverAudioReceiver::OnMessage(const webrtc::DataBuffer& buffer) {
  // Relay may retain the upstream DataChannel payload type.  Momo's UART
  // bridge can therefore deliver the ASCII AUD: frame as either a text or a
  // binary DataChannel message.  The audio frame format itself is textual,
  // so handle both forms identically and let DecodeAndQueue reject unrelated
  // telemetry.
  const std::string message(buffer.data.data<char>(), buffer.data.size());
  if (DecodeVehicleHealth(message) || DecodeTelemetryAcceleration(message) ||
      DecodeImpactCandidate(message)) {
    return;
  }
  bool audio_playback_enabled = false;
  {
    webrtc::MutexLock lock(&mutex_);
    audio_playback_enabled = audio_playback_enabled_;
  }
  if (audio_playback_enabled) {
    DecodeAndQueue(message);
  }
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
    gap_frames_ += missing;
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
  diagnostics.gap_frames = gap_frames_;
  diagnostics.invalid_frames = invalid_frames_;
  diagnostics.queue_resets = queue_resets_;
  diagnostics.initialization_error = initialization_error_;
  diagnostics.raw_telemetry_active =
      raw_telemetry_received_at_ != std::chrono::steady_clock::time_point::min() &&
      std::chrono::steady_clock::now() - raw_telemetry_received_at_ <
          std::chrono::milliseconds(750);
  diagnostics.raw_telemetry_frames = raw_telemetry_frames_;
  diagnostics.impact_candidates = impact_candidates_;
  diagnostics.last_impact_mps2 = last_impact_mps2_;
  diagnostics.vehicle_health_active =
      vehicle_health_received_at_ != std::chrono::steady_clock::time_point::min() &&
      std::chrono::steady_clock::now() - vehicle_health_received_at_ <
          std::chrono::milliseconds(750);
  diagnostics.vehicle_hp = vehicle_hp_;
  diagnostics.vehicle_speed_cap = vehicle_speed_cap_;
  diagnostics.vehicle_health_mode = vehicle_health_mode_;
  diagnostics.raw_acceleration_samples.assign(raw_acceleration_samples_.begin(),
                                               raw_acceleration_samples_.end());
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
    ++queue_resets_;
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
