#include "p2p_receiver_client.h"

#include <algorithm>
#include <string>
#include <vector>
#include <utility>

// Boost
#include <boost/asio/post.hpp>
#include <boost/beast/websocket/error.hpp>
#include <boost/json.hpp>

// WebRTC
#include <api/rtp_transceiver_direction.h>
#include <api/rtp_transceiver_interface.h>
#include <rtc_base/logging.h>

#include "rtc/rtc_connection.h"
#include "util.h"

P2PReceiverClient::P2PReceiverClient(boost::asio::io_context& ioc,
                                     RTCManager* manager,
                                     P2PReceiverClientConfig config)
    : ioc_(ioc), manager_(manager), config_(std::move(config)) {
  URLParts parts;
  if (!URLParts::Parse(config_.endpoint, parts)) {
    RTC_LOG(LS_ERROR) << "Invalid P2P receiver endpoint: "
                      << config_.endpoint;
    return;
  }

  if (parts.scheme == "wss") {
    ws_ = std::make_unique<Websocket>(Websocket::ssl_tag(), ioc_, false, "",
                                      "");
  } else if (parts.scheme == "ws") {
    ws_ = std::make_unique<Websocket>(ioc_);
  } else {
    RTC_LOG(LS_ERROR) << "P2P receiver endpoint must use ws or wss: "
                      << config_.endpoint;
  }
}

P2PReceiverClient::~P2PReceiverClient() {
  shutting_down_ = true;
  CloseConnection();
  if (ws_) {
    ws_->ForceClose();
  }
}

void P2PReceiverClient::Connect() {
  if (shutting_down_) {
    return;
  }
  if (!ws_) {
    RTC_LOG(LS_ERROR) << "P2P receiver signaling WebSocket is unavailable";
    NotifyDisconnected();
    Shutdown();
    return;
  }

  RTC_LOG(LS_INFO) << "Connecting to P2P receiver endpoint: "
                   << config_.endpoint;
  ws_->Connect(config_.endpoint,
               std::bind(&P2PReceiverClient::OnConnect, shared_from_this(),
                         std::placeholders::_1));
}

void P2PReceiverClient::Shutdown(std::function<void()> on_shutdown) {
  if (shutting_down_.exchange(true)) {
    if (on_shutdown) {
      on_shutdown();
    }
    return;
  }

  CloseConnection();
  if (ws_) {
    ws_->ForceClose();
  }
  if (on_shutdown) {
    on_shutdown();
  }
}

void P2PReceiverClient::DoRead() {
  if (shutting_down_ || !ws_) {
    return;
  }

  ws_->Read(std::bind(&P2PReceiverClient::OnRead, shared_from_this(),
                      std::placeholders::_1, std::placeholders::_2,
                      std::placeholders::_3));
}

void P2PReceiverClient::OnConnect(boost::system::error_code ec) {
  if (shutting_down_) {
    return;
  }
  if (ec) {
    RTC_LOG(LS_ERROR) << "Failed to connect to P2P receiver endpoint: "
                      << ec.message();
    NotifyDisconnected();
    Shutdown();
    return;
  }

  RTC_LOG(LS_INFO) << "Connected to P2P receiver endpoint";
  if (config_.on_connected) {
    config_.on_connected();
  }
  DoRead();
  CreatePeerConnection();
}

void P2PReceiverClient::OnRead(boost::system::error_code ec,
                               std::size_t bytes_transferred,
                               std::string text) {
  boost::ignore_unused(bytes_transferred);

  if (shutting_down_) {
    return;
  }

  if (ec == boost::beast::websocket::error::closed) {
    RTC_LOG(LS_WARNING) << "P2P receiver signaling WebSocket closed";
    NotifyDisconnected();
    Shutdown();
    return;
  }
  if (ec) {
    RTC_LOG(LS_ERROR) << "P2P receiver signaling read failed: "
                      << ec.message();
    NotifyDisconnected();
    Shutdown();
    return;
  }

  struct ReadGuard {
    P2PReceiverClient* client;
    ~ReadGuard() { client->DoRead(); }
  } read_guard{this};

  boost::system::error_code json_ec;
  boost::json::value message = boost::json::parse(text, json_ec);
  if (json_ec || !message.is_object()) {
    RTC_LOG(LS_WARNING) << "Ignoring invalid P2P signaling message";
    return;
  }

  const auto& object = message.as_object();
  auto type_it = object.find("type");
  if (type_it == object.end() || !type_it->value().is_string()) {
    RTC_LOG(LS_WARNING) << "P2P signaling message has no type";
    return;
  }

  const std::string type = type_it->value().as_string().c_str();
  if (type == "answer") {
    auto sdp_it = object.find("sdp");
    if (sdp_it != object.end() && sdp_it->value().is_string()) {
      OnAnswer(sdp_it->value().as_string().c_str());
    }
  } else if (type == "candidate") {
    auto ice_it = object.find("ice");
    if (ice_it == object.end() || !ice_it->value().is_object()) {
      return;
    }

    const auto& ice = ice_it->value().as_object();
    auto mid_it = ice.find("sdpMid");
    auto index_it = ice.find("sdpMLineIndex");
    auto candidate_it = ice.find("candidate");
    if (mid_it == ice.end() || index_it == ice.end() ||
        candidate_it == ice.end() || !mid_it->value().is_string() ||
        !candidate_it->value().is_string()) {
      return;
    }

    IceCandidate candidate{
        mid_it->value().as_string().c_str(),
        index_it->value().to_number<int>(),
        candidate_it->value().as_string().c_str(),
    };
    if (has_remote_description_) {
      AddIceCandidate(candidate);
    } else {
      pending_candidates_.push_back(std::move(candidate));
    }
  } else if (type == "close" || type == "bye") {
    RTC_LOG(LS_WARNING) << "P2P receiver peer closed the signaling session";
    NotifyDisconnected();
    Shutdown();
  } else {
    RTC_LOG(LS_INFO) << "Ignoring P2P signaling message: " << type;
  }
}

void P2PReceiverClient::CreatePeerConnection() {
  if (shutting_down_ || !manager_) {
    return;
  }

  webrtc::PeerConnectionInterface::RTCConfiguration rtc_config;
  if (!config_.no_google_stun) {
    webrtc::PeerConnectionInterface::IceServer ice_server;
    ice_server.uri = "stun:stun.l.google.com:19302";
    rtc_config.servers.push_back(ice_server);
  }

  connection_ = manager_->CreateConnection(rtc_config, this, config_.receiver);
  if (!connection_) {
    RTC_LOG(LS_ERROR) << "Failed to create P2P receiver PeerConnection";
    NotifyDisconnected();
    Shutdown();
    return;
  }

  if (config_.configure_connection) {
    config_.configure_connection(connection_);
  }

  webrtc::RtpTransceiverInit init;
  init.direction = webrtc::RtpTransceiverDirection::kRecvOnly;
  auto result = connection_->GetConnection()->AddTransceiver(
      webrtc::MediaType::VIDEO, init);
  if (!result.ok()) {
    RTC_LOG(LS_ERROR) << "Failed to add recvonly video transceiver: "
                      << result.error().message();
    NotifyDisconnected();
    Shutdown();
    return;
  }

  // WEB Viewer は H264 を優先して映像を提示する。Native 側でも同じ順序に
  // しないと、送信側によっては VP9 が選択され、映像フレームが出ないことが
  // あるため、H264 を先頭にする。
  auto factory = manager_->GetFactory();
  if (factory) {
    const auto sender_capabilities =
        factory->GetRtpSenderCapabilities(webrtc::MediaType::VIDEO);
    const auto receiver_capabilities =
        factory->GetRtpReceiverCapabilities(webrtc::MediaType::VIDEO);

    std::vector<webrtc::RtpCodecCapability> common_codecs;
    for (const auto& sender_codec : sender_capabilities.codecs) {
      const auto found = std::find_if(
          receiver_capabilities.codecs.begin(), receiver_capabilities.codecs.end(),
          [&sender_codec](const auto& receiver_codec) {
            return sender_codec.mime_type() == receiver_codec.mime_type();
          });
      if (found != receiver_capabilities.codecs.end()) {
        common_codecs.push_back(sender_codec);
      }
    }

    const auto is_auxiliary_codec = [](const std::string& name) {
      return name == "RTX" || name == "RED" || name == "ULPFEC" ||
             name == "FLEXFEC-03";
    };

    std::vector<webrtc::RtpCodecCapability> preferred_codecs;
    for (const auto& codec : common_codecs) {
      if (codec.name == "H264") {
        preferred_codecs.push_back(codec);
      }
    }
    for (const auto& codec : common_codecs) {
      if (codec.name != "H264" && !is_auxiliary_codec(codec.name)) {
        preferred_codecs.push_back(codec);
      }
    }

    if (!preferred_codecs.empty()) {
      auto codec_error =
          result.value()->SetCodecPreferences(preferred_codecs);
      if (!codec_error.ok()) {
        RTC_LOG(LS_WARNING)
            << "Failed to set P2P receiver video codec preferences: "
            << codec_error.message();
      } else {
        RTC_LOG(LS_INFO)
            << "P2P receiver video codec preference set: H264 first";
      }
    }
  }

  connection_->CreateOffer(
      [self = shared_from_this()](webrtc::SessionDescriptionInterface* desc) {
        self->SendOffer(desc);
      },
      [self = shared_from_this()](webrtc::RTCError error) {
        RTC_LOG(LS_ERROR) << "Failed to create P2P receiver offer: "
                          << error.message();
        self->NotifyDisconnected();
        self->Shutdown();
      },
      false,
      false);
}

void P2PReceiverClient::SendOffer(webrtc::SessionDescriptionInterface* desc) {
  if (shutting_down_ || !ws_ || !desc) {
    return;
  }

  std::string sdp;
  desc->ToString(&sdp);
  boost::json::value message = {
      {"type", "offer"},
      {"sdp", sdp},
  };
  ws_->WriteText(boost::json::serialize(message));
  RTC_LOG(LS_INFO) << "Sent P2P receiver offer";
}

void P2PReceiverClient::OnAnswer(const std::string& sdp) {
  if (shutting_down_ || !connection_) {
    return;
  }

  connection_->SetAnswer(
      sdp,
      [self = shared_from_this()]() {
        boost::asio::post(self->ioc_, [self]() {
          if (self->shutting_down_ || !self->connection_) {
            return;
          }
          self->has_remote_description_ = true;
          for (const auto& candidate : self->pending_candidates_) {
            self->AddIceCandidate(candidate);
          }
          self->pending_candidates_.clear();
          RTC_LOG(LS_INFO) << "Applied P2P receiver answer";
        });
      },
      [self = shared_from_this()](webrtc::RTCError error) {
        RTC_LOG(LS_ERROR) << "Failed to apply P2P receiver answer: "
                          << error.message();
        self->NotifyDisconnected();
        self->Shutdown();
      });
}

void P2PReceiverClient::AddIceCandidate(const IceCandidate& candidate) {
  if (shutting_down_ || !connection_) {
    return;
  }
  connection_->AddIceCandidate(candidate.sdp_mid, candidate.sdp_mline_index,
                               candidate.candidate);
}

void P2PReceiverClient::CloseConnection() {
  if (!connection_) {
    return;
  }
  connection_->CloseDetached();
  connection_ = nullptr;
}

void P2PReceiverClient::NotifyDisconnected() {
  if (disconnected_notified_.exchange(true)) {
    return;
  }
  if (config_.on_disconnected) {
    config_.on_disconnected();
  }
}

void P2PReceiverClient::OnIceConnectionStateChange(
    webrtc::PeerConnectionInterface::IceConnectionState new_state) {
  RTC_LOG(LS_INFO) << "P2P receiver ICE state: "
                   << Util::IceConnectionStateToString(new_state);
  if (new_state ==
          webrtc::PeerConnectionInterface::kIceConnectionConnected ||
      new_state ==
          webrtc::PeerConnectionInterface::kIceConnectionCompleted) {
    RTC_LOG(LS_INFO) << "P2P receiver media connection established";
  }
  if (new_state ==
          webrtc::PeerConnectionInterface::kIceConnectionFailed ||
      new_state ==
          webrtc::PeerConnectionInterface::kIceConnectionClosed) {
    NotifyDisconnected();
    Shutdown();
  }
}

void P2PReceiverClient::OnIceCandidate(const std::string sdp_mid,
                                       const int sdp_mlineindex,
                                       const std::string sdp) {
  if (shutting_down_ || !ws_) {
    return;
  }

  boost::json::value message = {
      {"type", "candidate"},
      {"ice", {{"candidate", sdp},
                {"sdpMLineIndex", sdp_mlineindex},
                {"sdpMid", sdp_mid}}},
  };
  ws_->WriteText(boost::json::serialize(message));
}
