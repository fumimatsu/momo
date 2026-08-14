#include "p2p/observer_audio_mixer.h"

#include <algorithm>
#include <cmath>
#include <limits>

#include <rtc_base/logging.h>

namespace {

constexpr int kSampleRate = 8000;
constexpr size_t kFrameSamples = 160;
constexpr size_t kStartupFrames = 6;
constexpr size_t kStartupSamples = kStartupFrames * kFrameSamples;
constexpr size_t kMaxQueuedSamples = kSampleRate / 2;
constexpr double kMinimumMasterGain = 0.5;
constexpr double kMaximumMasterGain = 3.0;

int16_t ClampSample(int64_t sample) {
  return static_cast<int16_t>(
      std::clamp<int64_t>(sample, std::numeric_limits<int16_t>::min(),
                          std::numeric_limits<int16_t>::max()));
}

}  // namespace

ObserverAudioMixer::~ObserverAudioMixer() {
  SDL_AudioStream* stream = nullptr;
  bool quit_audio = false;
  {
    std::lock_guard<std::mutex> lock(mutex_);
    shutting_down_ = true;
    stream = stream_;
    stream_ = nullptr;
    quit_audio = audio_subsystem_initialized_;
    audio_subsystem_initialized_ = false;
    initialized_ = false;
  }
  if (stream != nullptr) {
    SDL_DestroyAudioStream(stream);
  }
  if (quit_audio) {
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
  }
}

bool ObserverAudioMixer::Initialize() {
  {
    std::lock_guard<std::mutex> lock(mutex_);
    if (initialized_) {
      return true;
    }
    if (initializing_ || shutting_down_) {
      return false;
    }
    initializing_ = true;
    initialization_error_.clear();
  }

  SDL_AudioStream* stream = nullptr;
  std::string error;
  bool audio_subsystem_initialized = SDL_InitSubSystem(SDL_INIT_AUDIO);
  if (!audio_subsystem_initialized) {
    error = SDL_GetError();
  } else {
    SDL_AudioSpec spec{};
    spec.format = SDL_AUDIO_S16;
    spec.channels = 1;
    spec.freq = kSampleRate;
    stream = SDL_OpenAudioDeviceStream(SDL_AUDIO_DEVICE_DEFAULT_PLAYBACK, &spec,
                                       AudioCallback, this);
    if (stream == nullptr) {
      error = SDL_GetError();
      SDL_QuitSubSystem(SDL_INIT_AUDIO);
      audio_subsystem_initialized = false;
    }
  }

  {
    std::lock_guard<std::mutex> lock(mutex_);
    initializing_ = false;
    if (shutting_down_) {
      if (stream != nullptr) {
        SDL_DestroyAudioStream(stream);
      }
      if (audio_subsystem_initialized) {
        SDL_QuitSubSystem(SDL_INIT_AUDIO);
      }
      return false;
    }
    stream_ = stream;
    audio_subsystem_initialized_ = audio_subsystem_initialized;
    initialization_error_ = error;
    initialized_ = stream != nullptr;
  }

  if (stream == nullptr) {
    RTC_LOG(LS_ERROR) << "Observer audio mixer initialization failed: "
                      << error;
    return false;
  }
  if (!SDL_ResumeAudioStreamDevice(stream)) {
    error = SDL_GetError();
    {
      std::lock_guard<std::mutex> lock(mutex_);
      stream_ = nullptr;
      initialized_ = false;
      initialization_error_ = error;
      audio_subsystem_initialized_ = false;
    }
    SDL_DestroyAudioStream(stream);
    SDL_QuitSubSystem(SDL_INIT_AUDIO);
    RTC_LOG(LS_ERROR) << "Observer audio mixer could not start: " << error;
    return false;
  }

  RTC_LOG(LS_INFO) << "Observer audio mixer enabled";
  return true;
}

bool ObserverAudioMixer::SetSourceEnabled(const std::string& source_name,
                                          bool enabled) {
  if (enabled && !Initialize()) {
    return false;
  }
  std::lock_guard<std::mutex> lock(mutex_);
  SourceState& source = sources_[source_name];
  source.enabled = enabled;
  if (!enabled) {
    source.samples.clear();
    source.playback_started = false;
  }
  return !enabled || initialized_;
}

bool ObserverAudioMixer::IsSourceEnabled(const std::string& source_name) const {
  std::lock_guard<std::mutex> lock(mutex_);
  const auto source = sources_.find(source_name);
  return source != sources_.end() && source->second.enabled;
}

double ObserverAudioMixer::SetMasterGain(double gain) {
  std::lock_guard<std::mutex> lock(mutex_);
  if (!std::isfinite(gain)) {
    gain = 1.0;
  }
  master_gain_ = std::clamp(gain, kMinimumMasterGain, kMaximumMasterGain);
  return master_gain_;
}

double ObserverAudioMixer::AdjustMasterGain(double delta) {
  std::lock_guard<std::mutex> lock(mutex_);
  if (!std::isfinite(delta)) {
    return master_gain_;
  }
  master_gain_ = std::clamp(master_gain_ + delta, kMinimumMasterGain,
                            kMaximumMasterGain);
  return master_gain_;
}

double ObserverAudioMixer::GetMasterGain() const {
  std::lock_guard<std::mutex> lock(mutex_);
  return master_gain_;
}

bool ObserverAudioMixer::QueueSamples(const std::string& source_name,
                                      const std::vector<int16_t>& samples) {
  if (samples.empty()) {
    return false;
  }
  std::lock_guard<std::mutex> lock(mutex_);
  auto source = sources_.find(source_name);
  if (!initialized_ || source == sources_.end() || !source->second.enabled) {
    return false;
  }

  SourceState& state = source->second;
  state.samples.insert(state.samples.end(), samples.begin(), samples.end());
  if (state.samples.size() > kMaxQueuedSamples) {
    while (state.samples.size() > kStartupSamples) {
      state.samples.pop_front();
    }
    state.playback_started = false;
    ++state.queue_resets;
  }
  if (!state.playback_started && state.samples.size() >= kStartupSamples) {
    state.playback_started = true;
  }
  return true;
}

void ObserverAudioMixer::ResetSource(const std::string& source_name) {
  std::lock_guard<std::mutex> lock(mutex_);
  SourceState& source = sources_[source_name];
  source.samples.clear();
  source.playback_started = false;
}

ObserverAudioMixer::SourceDiagnostics ObserverAudioMixer::GetSourceDiagnostics(
    const std::string& source_name) const {
  std::lock_guard<std::mutex> lock(mutex_);
  SourceDiagnostics diagnostics;
  diagnostics.initialized = initialized_;
  diagnostics.initialization_error = initialization_error_;
  const auto source = sources_.find(source_name);
  if (source == sources_.end()) {
    return diagnostics;
  }
  diagnostics.enabled = source->second.enabled;
  diagnostics.playback_started = source->second.playback_started;
  diagnostics.queue_resets = source->second.queue_resets;
  diagnostics.underflows = source->second.underflows;
  diagnostics.queued_samples = source->second.samples.size();
  return diagnostics;
}

void SDLCALL ObserverAudioMixer::AudioCallback(void* userdata,
                                               SDL_AudioStream* stream,
                                               int additional_amount,
                                               int total_amount) {
  static_cast<void>(total_amount);
  if (userdata == nullptr || additional_amount <= 0) {
    return;
  }
  static_cast<ObserverAudioMixer*>(userdata)->ProvideAudio(stream,
                                                           additional_amount);
}

void ObserverAudioMixer::ProvideAudio(SDL_AudioStream* stream,
                                      int additional_amount) {
  const size_t sample_count =
      static_cast<size_t>(additional_amount) / sizeof(int16_t);
  if (sample_count == 0) {
    return;
  }

  std::vector<int16_t> mixed(sample_count, 0);
  {
    std::lock_guard<std::mutex> lock(mutex_);
    if (shutting_down_) {
      return;
    }
    for (size_t sample_index = 0; sample_index < sample_count; ++sample_index) {
      int64_t sum = 0;
      size_t contributors = 0;
      for (auto& [name, source] : sources_) {
        static_cast<void>(name);
        if (!source.enabled || !source.playback_started) {
          continue;
        }
        if (source.samples.empty()) {
          source.playback_started = false;
          ++source.underflows;
          continue;
        }
        sum += source.samples.front();
        source.samples.pop_front();
        ++contributors;
      }
      if (contributors > 0) {
        const double average = static_cast<double>(sum) /
                               static_cast<double>(contributors);
        mixed[sample_index] = ClampSample(static_cast<int64_t>(
            std::llround(average * master_gain_)));
      }
    }
  }

  if (!SDL_PutAudioStreamData(
          stream, mixed.data(),
          static_cast<int>(mixed.size() * sizeof(int16_t)))) {
    RTC_LOG(LS_WARNING) << "Observer audio mixer enqueue failed: "
                        << SDL_GetError();
  }
}
