#include "metrics_session.h"

// Boost
#include <boost/beast/core/error.hpp>
#include <boost/beast/http/empty_body.hpp>
#include <boost/beast/http/error.hpp>
#include <boost/beast/http/read.hpp>
#include <boost/beast/http/string_body.hpp>
#include <boost/beast/version.hpp>
#include <boost/json.hpp>

#include <chrono>

#ifdef _WIN32
#include <codecvt>
#endif

#include "momo_version.h"
#include "util.h"

MetricsSession::MetricsSession(boost::asio::io_context& ioc,
                               boost::asio::ip::tcp::socket socket,
                               RTCManager* rtc_manager,
                               std::shared_ptr<StatsCollector> stats_collector,
                               MetricsSessionConfig config)
    : ioc_(ioc),
      socket_(std::move(socket)),
      strand_(socket_.get_executor()),
      stats_timer_(ioc),
      rtc_manager_(rtc_manager),
      stats_collector_(stats_collector),
      config_(std::move(config)) {}

// Start the asynchronous operation
void MetricsSession::Run() {
  DoRead();
}

void MetricsSession::DoRead() {
  // Make the request empty before reading,
  // otherwise the operation behavior is undefined.
  req_ = {};

  // Read a request
  boost::beast::http::async_read(
      socket_, buffer_, req_,
      boost::asio::bind_executor(
          strand_, std::bind(&MetricsSession::OnRead, shared_from_this(),
                             std::placeholders::_1, std::placeholders::_2)));
}

void MetricsSession::OnRead(boost::system::error_code ec,
                            std::size_t bytes_transferred) {
  boost::ignore_unused(bytes_transferred);

  // 接続が切られた
  if (ec == boost::beast::http::error::end_of_stream)
    return DoClose();

  if (ec) {
    MOMO_BOOST_ERROR(ec, "read");
    return DoClose();
  }

  if (req_.method() == boost::beast::http::verb::get) {
    if (req_.target() == "/metrics") {
      std::shared_ptr<MetricsSession> self(shared_from_this());
      response_sent_ = false;
      stats_timer_.expires_after(std::chrono::seconds(2));
      stats_timer_.async_wait(boost::asio::bind_executor(
          strand_, [self](const boost::system::error_code& ec) {
            if (ec) {
              return;
            }
            self->SendMetricsTimeout();
          }));
      stats_collector_->GetStats([self](
                                     const webrtc::scoped_refptr<
                                         const webrtc::RTCStatsReport>& report) {
        boost::asio::post(self->strand_, [self, report]() {
          self->SendMetricsResponse(report);
        });
      });
    } else {
      SendResponse(Util::NotFound(req_, req_.target()));
    }
  } else {
    SendResponse(Util::BadRequest(req_, "Invalid Method"));
  }
}

void MetricsSession::OnWrite(boost::system::error_code ec,
                             std::size_t bytes_transferred,
                             bool close) {
  boost::ignore_unused(bytes_transferred);

  if (ec) {
    MOMO_BOOST_ERROR(ec, "write");
    return DoClose();
  }

  if (close)
    return DoClose();

  res_ = nullptr;

  DoRead();
}

void MetricsSession::DoClose() {
  boost::system::error_code ec;
  stats_timer_.cancel();
  socket_.shutdown(boost::asio::ip::tcp::socket::shutdown_send, ec);
  socket_.close(ec);
}

void MetricsSession::SendMetricsResponse(
    const webrtc::scoped_refptr<const webrtc::RTCStatsReport>& report) {
  if (response_sent_) {
    return;
  }
  response_sent_ = true;
  stats_timer_.cancel();

  std::string stats = report ? report->ToJson() : "[]";
  boost::json::value json_message = {
      {"version", MomoVersion::GetClientName()},
      {"libwebrtc", MomoVersion::GetLibwebrtcName()},
      {"environment", MomoVersion::GetEnvironmentName()},
      {"stats", boost::json::parse(stats)}};

  SendResponse(CreateOKWithJSON(req_, std::move(json_message)));
}

void MetricsSession::SendMetricsTimeout() {
  if (response_sent_) {
    return;
  }
  response_sent_ = true;
  SendResponse(Util::ServerError(req_, "GetStats timed out"));
}

boost::beast::http::response<boost::beast::http::string_body>
MetricsSession::CreateOKWithJSON(
    const boost::beast::http::request<boost::beast::http::string_body>& req,
    boost::json::value json_message) {
  boost::beast::http::response<boost::beast::http::string_body> res{
      boost::beast::http::status::ok, 11};
  res.set(boost::beast::http::field::server, BOOST_BEAST_VERSION_STRING);
  res.set(boost::beast::http::field::content_type, "application/json");
  res.keep_alive(false);
  res.body() = boost::json::serialize(json_message);
  res.prepare_payload();

  return res;
}
