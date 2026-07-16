#ifndef P2P_MULTI_RECEIVER_CLIENT_H_
#define P2P_MULTI_RECEIVER_CLIENT_H_

#include <atomic>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include <boost/asio/io_context.hpp>
#include <boost/asio/steady_timer.hpp>

#include "p2p/p2p_receiver_client.h"

class RTCManager;
class SDLRenderer;
class SourceVideoTrackReceiver;

struct P2PMultiReceiverSource {
  std::string name;
  std::string endpoint;
};

struct P2PMultiReceiverClientConfig {
  std::vector<P2PMultiReceiverSource> sources;
  bool no_google_stun = false;
};

class P2PMultiReceiverClient
    : public std::enable_shared_from_this<P2PMultiReceiverClient> {
 public:
  static std::shared_ptr<P2PMultiReceiverClient> Create(
      boost::asio::io_context& ioc,
      RTCManager* manager,
      SDLRenderer* renderer,
      P2PMultiReceiverClientConfig config) {
    return std::shared_ptr<P2PMultiReceiverClient>(new P2PMultiReceiverClient(
        ioc, manager, renderer, std::move(config)));
  }

  ~P2PMultiReceiverClient();

  void Connect();
  void Shutdown(std::function<void()> on_shutdown = nullptr);

 private:
  struct Source {
    P2PMultiReceiverSource config;
    std::unique_ptr<SourceVideoTrackReceiver> receiver;
    std::shared_ptr<P2PReceiverClient> client;
    std::unique_ptr<boost::asio::steady_timer> reconnect_timer;
  };

  P2PMultiReceiverClient(boost::asio::io_context& ioc,
                         RTCManager* manager,
                         SDLRenderer* renderer,
                         P2PMultiReceiverClientConfig config);
  void ConnectSource(size_t index);
  void ScheduleReconnect(size_t index);

  boost::asio::io_context& ioc_;
  RTCManager* manager_;
  SDLRenderer* renderer_;
  P2PMultiReceiverClientConfig config_;
  std::vector<Source> sources_;
  std::atomic_bool shutting_down_ = false;
};

#endif  // P2P_MULTI_RECEIVER_CLIENT_H_
