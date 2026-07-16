#ifndef P2P_RECEIVER_CLIENT_H_
#define P2P_RECEIVER_CLIENT_H_

#include <atomic>
#include <functional>
#include <memory>
#include <string>
#include <utility>
#include <vector>

// Boost
#include <boost/asio/io_context.hpp>
#include <boost/system/error_code.hpp>

// WebRTC
#include <api/peer_connection_interface.h>

#include "rtc/rtc_manager.h"
#include "rtc/rtc_message_sender.h"
#include "rtc/video_track_receiver.h"
#include "websocket.h"

struct P2PReceiverClientConfig {
  std::string endpoint = "ws://127.0.0.1:8080/ws";
  bool no_google_stun = false;
  VideoTrackReceiver* receiver = nullptr;
  // Offer を作成する前に DataChannel などを追加するためのフック。
  std::function<void(std::shared_ptr<RTCConnection>)> configure_connection;
  std::function<void()> on_connected;
  std::function<void()> on_disconnected;
};

class P2PReceiverClient : public std::enable_shared_from_this<P2PReceiverClient>,
                          public RTCMessageSender {
  P2PReceiverClient(boost::asio::io_context& ioc,
                    RTCManager* manager,
                    P2PReceiverClientConfig config);

 public:
  static std::shared_ptr<P2PReceiverClient> Create(
      boost::asio::io_context& ioc,
      RTCManager* manager,
      P2PReceiverClientConfig config) {
    return std::shared_ptr<P2PReceiverClient>(
        new P2PReceiverClient(ioc, manager, std::move(config)));
  }

  ~P2PReceiverClient();

  void Connect();
  void Shutdown(std::function<void()> on_shutdown = nullptr);

 private:
  struct IceCandidate {
    std::string sdp_mid;
    int sdp_mline_index;
    std::string candidate;
  };

  void DoRead();
  void OnConnect(boost::system::error_code ec);
  void OnRead(boost::system::error_code ec,
              std::size_t bytes_transferred,
              std::string text);
  void OnAnswer(const std::string& sdp);
  void AddIceCandidate(const IceCandidate& candidate);
  void CreatePeerConnection();
  void SendOffer(webrtc::SessionDescriptionInterface* desc);
  void CloseConnection();
  void NotifyDisconnected();

  // WebRTC からのコールバック
  void OnIceConnectionStateChange(
      webrtc::PeerConnectionInterface::IceConnectionState new_state) override;
  void OnIceCandidate(const std::string sdp_mid,
                      const int sdp_mlineindex,
                      const std::string sdp) override;

 private:
  boost::asio::io_context& ioc_;
  RTCManager* manager_;
  P2PReceiverClientConfig config_;
  std::unique_ptr<Websocket> ws_;
  std::shared_ptr<RTCConnection> connection_;
  std::vector<IceCandidate> pending_candidates_;

  std::atomic_bool shutting_down_ = false;
  std::atomic_bool disconnected_notified_ = false;
  bool has_remote_description_ = false;
};

#endif  // P2P_RECEIVER_CLIENT_H_
