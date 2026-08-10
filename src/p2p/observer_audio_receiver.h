#ifndef OBSERVER_AUDIO_RECEIVER_H_
#define OBSERVER_AUDIO_RECEIVER_H_

#include <array>
#include <chrono>
#include <cstdint>
#include <deque>
#include <memory>
#include <string>
#include <vector>

#include <SDL3/SDL.h>

#include <api/data_channel_interface.h>
#include <rtc_base/synchronization/mutex.h>

class RTCConnection;

// Relay が送る M5 の AUD: フレームを受け、既定の Windows 音声出力へ流す。
// 1 インスタンスは 1 機体に対応し、複数インスタンスの出力は SDL が合成する。
class ObserverAudioReceiver : public webrtc::DataChannelObserver {
 public:
  struct Diagnostics {
    bool initialized = false;
    bool channel_open = false;
    bool playback_started = false;
    uint64_t received_frames = 0;
    uint64_t gap_frames = 0;
    uint64_t invalid_frames = 0;
    uint64_t queue_resets = 0;
    size_t queued_samples = 0;
    std::string initialization_error;
    bool raw_telemetry_active = false;
    uint64_t raw_telemetry_frames = 0;
    uint64_t impact_candidates = 0;
    float last_impact_mps2 = 0.0f;
    bool vehicle_health_active = false;
    float vehicle_hp = 100.0f;
    float vehicle_speed_cap = 1.0f;
    std::string vehicle_health_mode;
    std::vector<std::array<float, 3>> raw_acceleration_samples;
  };

  explicit ObserverAudioReceiver(std::string source_name,
                                 bool enable_audio_playback);
  ~ObserverAudioReceiver() override;

  void AttachDataChannel(const std::shared_ptr<RTCConnection>& connection);
  void SetAudioPlaybackEnabled(bool enabled);
  Diagnostics GetDiagnostics() const;

  void OnStateChange() override;
  void OnMessage(const webrtc::DataBuffer& buffer) override;
  void OnBufferedAmountChange(uint64_t previous_amount) override;

 private:
  bool DecodeAndQueue(const std::string& message);
  bool DecodeTelemetryAcceleration(const std::string& message);
  bool DecodeImpactCandidate(const std::string& message);
  bool DecodeVehicleHealth(const std::string& message);
  bool InvalidAudioFrame();
  bool QueueSamples(const std::vector<int16_t>& samples);
  void ResetStream();
  void DetachChannel();
  bool EnableAudioPlaybackLocked();
  void DisableAudioPlaybackLocked();

  std::string source_name_;
  mutable webrtc::Mutex mutex_;
  bool audio_playback_enabled_ RTC_GUARDED_BY(mutex_) = false;
  webrtc::scoped_refptr<webrtc::DataChannelInterface> channel_
      RTC_GUARDED_BY(mutex_);
  SDL_AudioStream* stream_ RTC_GUARDED_BY(mutex_) = nullptr;
  bool audio_subsystem_initialized_ RTC_GUARDED_BY(mutex_) = false;
  bool audio_initialized_ RTC_GUARDED_BY(mutex_) = false;
  bool channel_open_ RTC_GUARDED_BY(mutex_) = false;
  std::string initialization_error_ RTC_GUARDED_BY(mutex_);
  bool playback_started_ RTC_GUARDED_BY(mutex_) = false;
  std::string boot_id_ RTC_GUARDED_BY(mutex_);
  uint64_t last_sequence_ RTC_GUARDED_BY(mutex_) = 0;
  bool has_sequence_ RTC_GUARDED_BY(mutex_) = false;
  std::vector<int16_t> startup_samples_ RTC_GUARDED_BY(mutex_);
  uint64_t received_frames_ RTC_GUARDED_BY(mutex_) = 0;
  uint64_t gap_frames_ RTC_GUARDED_BY(mutex_) = 0;
  uint64_t invalid_frames_ RTC_GUARDED_BY(mutex_) = 0;
  uint64_t queue_resets_ RTC_GUARDED_BY(mutex_) = 0;
  std::deque<std::array<float, 3>> raw_acceleration_samples_
      RTC_GUARDED_BY(mutex_);
  std::chrono::steady_clock::time_point raw_telemetry_received_at_
      RTC_GUARDED_BY(mutex_) = std::chrono::steady_clock::time_point::min();
  uint64_t raw_telemetry_frames_ RTC_GUARDED_BY(mutex_) = 0;
  uint64_t impact_candidates_ RTC_GUARDED_BY(mutex_) = 0;
  float last_impact_mps2_ RTC_GUARDED_BY(mutex_) = 0.0f;
  std::chrono::steady_clock::time_point vehicle_health_received_at_
      RTC_GUARDED_BY(mutex_) = std::chrono::steady_clock::time_point::min();
  float vehicle_hp_ RTC_GUARDED_BY(mutex_) = 100.0f;
  float vehicle_speed_cap_ RTC_GUARDED_BY(mutex_) = 1.0f;
  std::string vehicle_health_mode_ RTC_GUARDED_BY(mutex_);
};

#endif  // OBSERVER_AUDIO_RECEIVER_H_
