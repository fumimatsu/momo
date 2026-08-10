#ifndef OBSERVER_AUDIO_MIXER_H_
#define OBSERVER_AUDIO_MIXER_H_

#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <unordered_map>
#include <vector>

#include <SDL3/SDL.h>

class ObserverAudioMixer {
 public:
  struct SourceDiagnostics {
    bool initialized = false;
    bool enabled = false;
    bool playback_started = false;
    uint64_t queue_resets = 0;
    uint64_t underflows = 0;
    size_t queued_samples = 0;
    std::string initialization_error;
  };

  ObserverAudioMixer() = default;
  ~ObserverAudioMixer();

  ObserverAudioMixer(const ObserverAudioMixer&) = delete;
  ObserverAudioMixer& operator=(const ObserverAudioMixer&) = delete;

  bool SetSourceEnabled(const std::string& source_name, bool enabled);
  bool IsSourceEnabled(const std::string& source_name) const;
  bool QueueSamples(const std::string& source_name,
                    const std::vector<int16_t>& samples);
  void ResetSource(const std::string& source_name);
  SourceDiagnostics GetSourceDiagnostics(const std::string& source_name) const;

 private:
  struct SourceState {
    bool enabled = false;
    bool playback_started = false;
    uint64_t queue_resets = 0;
    uint64_t underflows = 0;
    std::deque<int16_t> samples;
  };

  bool Initialize();
  static void SDLCALL AudioCallback(void* userdata,
                                    SDL_AudioStream* stream,
                                    int additional_amount,
                                    int total_amount);
  void ProvideAudio(SDL_AudioStream* stream, int additional_amount);

  mutable std::mutex mutex_;
  std::unordered_map<std::string, SourceState> sources_;
  SDL_AudioStream* stream_ = nullptr;
  bool audio_subsystem_initialized_ = false;
  bool initialized_ = false;
  bool initializing_ = false;
  bool shutting_down_ = false;
  std::string initialization_error_;
};

#endif  // OBSERVER_AUDIO_MIXER_H_
