#include "p2p/p2p_multi_receiver_client.h"

#include <chrono>
#include <utility>

#include <boost/asio/post.hpp>

#include <rtc_base/logging.h>

#include "p2p/source_video_track_receiver.h"
#include "sdl_renderer/sdl_renderer.h"

P2PMultiReceiverClient::P2PMultiReceiverClient(
    boost::asio::io_context& ioc,
    RTCManager* manager,
    SDLRenderer* renderer,
    P2PMultiReceiverClientConfig config)
    : ioc_(ioc),
      manager_(manager),
      renderer_(renderer),
      config_(std::move(config)) {
  for (const P2PMultiReceiverSource& source : config_.sources) {
    Source entry;
    entry.config = source;
    entry.receiver =
        std::make_unique<SourceVideoTrackReceiver>(renderer_, source.name);
    entry.reconnect_timer = std::make_unique<boost::asio::steady_timer>(ioc_);
    sources_.push_back(std::move(entry));
  }
}

P2PMultiReceiverClient::~P2PMultiReceiverClient() {
  Shutdown();
}

void P2PMultiReceiverClient::Connect() {
  if (shutting_down_ || renderer_ == nullptr) {
    return;
  }

  std::vector<std::string> source_names;
  for (const Source& source : sources_) {
    source_names.push_back(source.config.name);
  }
  renderer_->ConfigureFixedSlots(source_names);

  for (size_t i = 0; i < sources_.size(); ++i) {
    ConnectSource(i);
  }
}

void P2PMultiReceiverClient::Shutdown(std::function<void()> on_shutdown) {
  if (shutting_down_.exchange(true)) {
    if (on_shutdown) {
      on_shutdown();
    }
    return;
  }

  for (Source& source : sources_) {
    source.reconnect_timer->cancel();
    if (source.client) {
      source.client->Shutdown();
      source.client.reset();
    }
    if (renderer_ != nullptr) {
      renderer_->SetSourceState(source.config.name,
                                SDLRenderer::SourceState::kOffline);
    }
  }
  if (on_shutdown) {
    on_shutdown();
  }
}

void P2PMultiReceiverClient::ConnectSource(size_t index) {
  if (shutting_down_ || index >= sources_.size()) {
    return;
  }

  Source& source = sources_[index];
  if (renderer_ != nullptr) {
    renderer_->SetSourceState(source.config.name,
                              SDLRenderer::SourceState::kConnecting);
  }

  std::weak_ptr<P2PMultiReceiverClient> weak_self = shared_from_this();
  P2PReceiverClientConfig client_config;
  client_config.endpoint = source.config.endpoint;
  client_config.no_google_stun = config_.no_google_stun;
  client_config.receiver = source.receiver.get();
  client_config.on_disconnected = [weak_self, index]() {
    if (const auto self = weak_self.lock()) {
      boost::asio::post(self->ioc_, [weak_self, index]() {
        if (const auto posted_self = weak_self.lock()) {
          posted_self->ScheduleReconnect(index);
        }
      });
    }
  };
  source.client =
      P2PReceiverClient::Create(ioc_, manager_, std::move(client_config));
  source.client->Connect();
}

void P2PMultiReceiverClient::ScheduleReconnect(size_t index) {
  if (shutting_down_ || index >= sources_.size()) {
    return;
  }

  Source& source = sources_[index];
  if (source.client) {
    source.client->Shutdown();
    source.client.reset();
  }
  if (renderer_ != nullptr) {
    renderer_->SetSourceState(source.config.name,
                              SDLRenderer::SourceState::kOffline);
  }

  RTC_LOG(LS_WARNING) << "P2P multi receiver: reconnect "
                      << source.config.name << " in 2 seconds";
  source.reconnect_timer->expires_after(std::chrono::seconds(2));
  std::weak_ptr<P2PMultiReceiverClient> weak_self = shared_from_this();
  source.reconnect_timer->async_wait(
      [weak_self, index](const boost::system::error_code& ec) {
        if (ec) {
          return;
        }
        if (const auto self = weak_self.lock()) {
          self->ConnectSource(index);
        }
      });
}
