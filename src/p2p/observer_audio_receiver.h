#ifndef OBSERVER_AUDIO_RECEIVER_H_
#define OBSERVER_AUDIO_RECEIVER_H_

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include <SDL3/SDL.h>

#include <api/data_channel_interface.h>
#include <rtc_base/synchronization/mutex.h>

class RTCConnection;

// Relay が送る M5 の AUD: フレームを受け、既定の Windows 音声出力へ流す。
// 1 インスタンスは 1 機体だけに対応し、複数車両を混ぜない。
class ObserverAudioReceiver : public webrtc::DataChannelObserver {
 public:
  struct Diagnostics {
    bool initialized = false;
    bool channel_open = false;
    bool playback_started = false;
    uint64_t received_frames = 0;
    uint64_t invalid_frames = 0;
    size_t queued_samples = 0;
    std::string initialization_error;
  };

  explicit ObserverAudioReceiver(std::string source_name);
  ~ObserverAudioReceiver() override;

  void AttachDataChannel(const std::shared_ptr<RTCConnection>& connection);
  Diagnostics GetDiagnostics() const;

  void OnStateChange() override;
  void OnMessage(const webrtc::DataBuffer& buffer) override;
  void OnBufferedAmountChange(uint64_t previous_amount) override;

 private:
  bool DecodeAndQueue(const std::string& message);
  bool InvalidAudioFrame();
  bool QueueSamples(const std::vector<int16_t>& samples);
  void ResetStream();
  void DetachChannel();

  std::string source_name_;
  mutable webrtc::Mutex mutex_;
  webrtc::scoped_refptr<webrtc::DataChannelInterface> channel_
      RTC_GUARDED_BY(mutex_);
  SDL_AudioStream* stream_ RTC_GUARDED_BY(mutex_) = nullptr;
  bool audio_initialized_ RTC_GUARDED_BY(mutex_) = false;
  bool channel_open_ RTC_GUARDED_BY(mutex_) = false;
  std::string initialization_error_ RTC_GUARDED_BY(mutex_);
  bool playback_started_ RTC_GUARDED_BY(mutex_) = false;
  std::string boot_id_ RTC_GUARDED_BY(mutex_);
  uint64_t last_sequence_ RTC_GUARDED_BY(mutex_) = 0;
  bool has_sequence_ RTC_GUARDED_BY(mutex_) = false;
  std::vector<int16_t> startup_samples_ RTC_GUARDED_BY(mutex_);
  uint64_t received_frames_ RTC_GUARDED_BY(mutex_) = 0;
  uint64_t invalid_frames_ RTC_GUARDED_BY(mutex_) = 0;
};

#endif  // OBSERVER_AUDIO_RECEIVER_H_
