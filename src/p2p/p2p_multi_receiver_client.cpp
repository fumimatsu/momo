#include "p2p/p2p_multi_receiver_client.h"

#include <chrono>
#include <iomanip>
#include <sstream>
#include <utility>

#include <boost/asio/post.hpp>

#include <rtc_base/logging.h>

#include "p2p/source_video_track_receiver.h"
#include "p2p/observer_audio_receiver.h"
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
  audio_overlay_timer_ = std::make_unique<boost::asio::steady_timer>(ioc_);
  for (const P2PMultiReceiverSource& source : config_.sources) {
    Source entry;
    entry.config = source;
    entry.receiver =
        std::make_unique<SourceVideoTrackReceiver>(renderer_, source.name);
    entry.audio_receiver = std::make_shared<ObserverAudioReceiver>(
        source.name, source.name == config_.audio_source);
    entry.reconnect_timer = std::make_unique<boost::asio::steady_timer>(ioc_);
    sources_.push_back(std::move(entry));
  }
  for (size_t index = 0; index < sources_.size(); ++index) {
    if (sources_[index].config.name == config_.audio_source) {
      selected_audio_source_index_ = index;
      break;
    }
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
  std::weak_ptr<P2PMultiReceiverClient> weak_self = shared_from_this();
  renderer_->SetKeyUpHandler([weak_self](SDL_Keycode key) {
    const std::shared_ptr<P2PMultiReceiverClient> self = weak_self.lock();
    return self && self->HandleAudioKey(key);
  });
  UpdateAudioOverlay();

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
  if (audio_overlay_timer_) {
    audio_overlay_timer_->cancel();
  }
  if (renderer_ != nullptr) {
    renderer_->SetKeyUpHandler(nullptr);
    for (const Source& source : sources_) {
      renderer_->SetSourceOverlayText(source.config.name, "");
    }
  }
  if (on_shutdown) {
    on_shutdown();
  }
}

void P2PMultiReceiverClient::UpdateAudioOverlay() {
  if (shutting_down_ || renderer_ == nullptr || !audio_overlay_timer_) {
    return;
  }

  for (const Source& source : sources_) {
    if (!source.audio_receiver) {
      continue;
    }
    const ObserverAudioReceiver::Diagnostics diagnostics =
        source.audio_receiver->GetDiagnostics();
    renderer_->SetSourceRawTelemetryGraph(
        source.config.name, diagnostics.raw_acceleration_samples,
        diagnostics.raw_telemetry_active, diagnostics.impact_candidates,
        diagnostics.last_impact_mps2);
    if (source.config.name != config_.audio_source) {
      renderer_->SetSourceOverlayText(source.config.name, "");
      continue;
    }
    const double queued_ms =
        static_cast<double>(diagnostics.queued_samples) * 1000.0 / 8000.0;
    std::ostringstream text;
    text << "AUD " << source.config.name
         << (diagnostics.initialized ? " READY" : " INIT ERR")
         << (diagnostics.channel_open ? " DC OPEN" : " DC WAIT") << "\n"
         << "RX " << diagnostics.received_frames
         << "  GAP " << diagnostics.gap_frames << "\n"
         << "INVALID " << diagnostics.invalid_frames
         << "  RESET " << diagnostics.queue_resets << "\n"
         << "QUEUED " << std::fixed << std::setprecision(0) << queued_ms
         << "ms  " << (diagnostics.playback_started ? "PLAY" : "BUFFER")
         << "\nKEY 1-4 / [ ] SELECT";
    if (!diagnostics.initialization_error.empty()) {
      text << "\nERR " << diagnostics.initialization_error;
    }
    renderer_->SetSourceOverlayText(source.config.name, text.str());
  }

  audio_overlay_timer_->expires_after(std::chrono::milliseconds(250));
  std::weak_ptr<P2PMultiReceiverClient> weak_self = shared_from_this();
  audio_overlay_timer_->async_wait(
      [weak_self](const boost::system::error_code& ec) {
        if (ec) {
          return;
        }
        if (const auto self = weak_self.lock()) {
          self->UpdateAudioOverlay();
        }
      });
}

bool P2PMultiReceiverClient::HandleAudioKey(int key) {
  if (sources_.empty()) {
    return false;
  }

  size_t selected_index = static_cast<size_t>(-1);
  if (key >= SDLK_1 && key <= SDLK_4) {
    selected_index = static_cast<size_t>(key - SDLK_1);
    if (selected_index >= sources_.size()) {
      return true;
    }
  } else if (key == SDLK_LEFTBRACKET || key == SDLK_RIGHTBRACKET) {
    if (selected_audio_source_index_ == static_cast<size_t>(-1)) {
      selected_index = 0;
    } else if (key == SDLK_LEFTBRACKET) {
      selected_index = (selected_audio_source_index_ + sources_.size() - 1) %
                       sources_.size();
    } else {
      selected_index = (selected_audio_source_index_ + 1) % sources_.size();
    }
  } else {
    return false;
  }

  SelectAudioSource(selected_index);
  return true;
}

void P2PMultiReceiverClient::SelectAudioSource(size_t index) {
  if (index >= sources_.size() || shutting_down_) {
    return;
  }
  for (size_t source_index = 0; source_index < sources_.size(); ++source_index) {
    sources_[source_index].audio_receiver->SetAudioPlaybackEnabled(
        source_index == index);
  }
  selected_audio_source_index_ = index;
  config_.audio_source = sources_[index].config.name;
  RTC_LOG(LS_INFO) << "Observer audio source selected: "
                   << config_.audio_source;
  UpdateAudioOverlay();
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
  const std::shared_ptr<ObserverAudioReceiver> audio_receiver =
      source.audio_receiver;
  client_config.configure_connection = [audio_receiver](
                                          std::shared_ptr<RTCConnection> connection) {
    audio_receiver->AttachDataChannel(connection);
  };
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
