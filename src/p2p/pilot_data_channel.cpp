#include "p2p/pilot_data_channel.h"

#include <utility>

#include <rtc_base/logging.h>

PilotDataChannel::~PilotDataChannel() {
  DetachChannels();
}

void PilotDataChannel::AttachDataChannels(
    const std::shared_ptr<RTCConnection>& connection) {
  if (!connection) {
    return;
  }

  auto peer = connection->GetConnection();
  webrtc::DataChannelInit config;
  config.ordered = false;
  config.maxRetransmits = 0;

  for (const char* label : {kCommandLabel, kTelemetryLabel}) {
    auto result = peer->CreateDataChannelOrError(label, &config);
    if (!result.ok()) {
      RTC_LOG(LS_ERROR) << "Failed to create relay DataChannel " << label
                        << ": " << result.error().message();
      continue;
    }
    OnDataChannel(result.MoveValue());
  }
}

bool PilotDataChannel::SendCommand(const std::string& command) {
  webrtc::scoped_refptr<webrtc::DataChannelInterface> channel;
  {
    webrtc::MutexLock lock(&mutex_);
    channel = command_channel_;
  }
  if (!channel || channel->state() != webrtc::DataChannelInterface::kOpen) {
    return false;
  }

  return channel->Send(webrtc::DataBuffer(command));
}

bool PilotDataChannel::IsCommandOpen() const {
  webrtc::MutexLock lock(&mutex_);
  return command_channel_ &&
         command_channel_->state() == webrtc::DataChannelInterface::kOpen;
}

std::string PilotDataChannel::GetLastTelemetry() const {
  webrtc::MutexLock lock(&mutex_);
  return last_telemetry_;
}

void PilotDataChannel::OnDataChannel(
    webrtc::scoped_refptr<webrtc::DataChannelInterface> data_channel) {
  if (!data_channel) {
    return;
  }

  const std::string label = data_channel->label();
  if (label != kCommandLabel && label != kTelemetryLabel) {
    return;
  }
  data_channel->RegisterObserver(this);
  webrtc::MutexLock lock(&mutex_);
  if (label == kCommandLabel) {
    command_channel_ = std::move(data_channel);
  } else {
    telemetry_channel_ = std::move(data_channel);
  }
}

void PilotDataChannel::OnStateChange() {
  RTC_LOG(LS_INFO) << "Pilot DataChannel state changed";
}

void PilotDataChannel::OnMessage(const webrtc::DataBuffer& buffer) {
  const std::string message(buffer.data.data<char>(), buffer.data.size());
  webrtc::MutexLock lock(&mutex_);
  last_telemetry_ = message;
}

void PilotDataChannel::OnBufferedAmountChange(uint64_t previous_amount) {
  (void)previous_amount;
}

void PilotDataChannel::DetachChannels() {
  webrtc::scoped_refptr<webrtc::DataChannelInterface> command;
  webrtc::scoped_refptr<webrtc::DataChannelInterface> telemetry;
  {
    webrtc::MutexLock lock(&mutex_);
    command = std::move(command_channel_);
    telemetry = std::move(telemetry_channel_);
  }
  if (command) {
    command->UnregisterObserver();
  }
  if (telemetry) {
    telemetry->UnregisterObserver();
  }
}
