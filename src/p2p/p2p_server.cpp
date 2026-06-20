#include "p2p_server.h"

#include <algorithm>

#include "util.h"

void P2PServer::GetStats(
    std::function<void(
        const webrtc::scoped_refptr<const webrtc::RTCStatsReport>&)> callback) {
  for (auto it = p2p_sessions_.rbegin(); it != p2p_sessions_.rend(); ++it) {
    if (!*it || (*it)->IsClosed()) {
      continue;
    }
    auto connection = (*it)->GetRTCConnection();
    if (connection) {
      connection->GetStats(std::move(callback));
      return;
    }
  }
  callback(nullptr);
}

P2PServer::P2PServer(boost::asio::io_context& ioc,
                     boost::asio::ip::tcp::endpoint endpoint,
                     RTCManager* rtc_manager,
                     P2PServerConfig config)
    : ioc_(ioc),
      acceptor_(ioc),
      socket_(ioc),
      rtc_manager_(rtc_manager),
      config_(std::move(config)) {
  boost::system::error_code ec;

  // Open the acceptor
  acceptor_.open(endpoint.protocol(), ec);
  if (ec) {
    MOMO_BOOST_ERROR(ec, "open");
    return;
  }

  // Allow address reuse
  acceptor_.set_option(boost::asio::socket_base::reuse_address(true), ec);
  if (ec) {
    MOMO_BOOST_ERROR(ec, "set_option");
    return;
  }

  // Bind to the server address
  acceptor_.bind(endpoint, ec);
  if (ec) {
    MOMO_BOOST_ERROR(ec, "bind");
    return;
  }

  // Start listening for connections
  acceptor_.listen(boost::asio::socket_base::max_listen_connections, ec);
  if (ec) {
    MOMO_BOOST_ERROR(ec, "listen");
    return;
  }
}

void P2PServer::Run() {
  if (!acceptor_.is_open())
    return;
  DoAccept();
}

void P2PServer::DoAccept() {
  acceptor_.async_accept(socket_,
                         std::bind(&P2PServer::OnAccept, shared_from_this(),
                                   std::placeholders::_1));
}

void P2PServer::OnAccept(boost::system::error_code ec) {
  if (ec) {
    MOMO_BOOST_ERROR(ec, "accept");
    return;
  }

  p2p_sessions_.erase(
      std::remove_if(p2p_sessions_.begin(), p2p_sessions_.end(),
                     [](const std::shared_ptr<P2PSession>& session) {
                       return !session || session->IsClosed();
                     }),
      p2p_sessions_.end());

  P2PSessionConfig config = config_;
  auto p2p_session =
      P2PSession::Create(ioc_, std::move(socket_), rtc_manager_, config);
  p2p_sessions_.push_back(p2p_session);
  p2p_session->Run();

  DoAccept();
}
