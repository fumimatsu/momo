#ifndef PILOT_DATA_CHANNEL_H_
#define PILOT_DATA_CHANNEL_H_

#include <memory>
#include <string>

#include <api/data_channel_interface.h>
#include <rtc_base/synchronization/mutex.h>

#include "rtc/rtc_connection.h"
#include "rtc/rtc_data_manager.h"

// relay Pilot 用の DataChannel を管理する。操縦は unreliable / unordered で送る。
class PilotDataChannel : public RTCDataManager,
                        public webrtc::DataChannelObserver {
 public:
  static constexpr const char* kCommandLabel = "momo-command";
  static constexpr const char* kTelemetryLabel = "momo-telemetry";

  ~PilotDataChannel() override;

  void AttachDataChannels(const std::shared_ptr<RTCConnection>& connection);
  bool SendCommand(const std::string& command);
  bool IsCommandOpen() const;
  std::string GetLastTelemetry() const;

  void OnDataChannel(webrtc::scoped_refptr<webrtc::DataChannelInterface>
                         data_channel) override;

  void OnStateChange() override;
  void OnMessage(const webrtc::DataBuffer& buffer) override;
  void OnBufferedAmountChange(uint64_t previous_amount) override;

 private:
  void DetachChannels();

  mutable webrtc::Mutex mutex_;
  webrtc::scoped_refptr<webrtc::DataChannelInterface> command_channel_
      RTC_GUARDED_BY(mutex_);
  webrtc::scoped_refptr<webrtc::DataChannelInterface> telemetry_channel_
      RTC_GUARDED_BY(mutex_);
  std::string last_telemetry_ RTC_GUARDED_BY(mutex_);
};

#endif  // PILOT_DATA_CHANNEL_H_
