#include "p2p/p2p_marker_receiver_client.h"

#include <algorithm>
#include <chrono>
#include <optional>
#include <string>
#include <thread>
#include <utility>

#include <rtc_base/logging.h>
#include <boost/asio/connect.hpp>
#include <boost/asio/ip/tcp.hpp>
#include <boost/asio/post.hpp>
#include <boost/beast/core.hpp>
#include <boost/beast/http.hpp>
#include <boost/json.hpp>

#include "marker/marker_luma_shared_memory.h"
#include "p2p/marker_video_track_receiver.h"
#include "url_parts.h"

namespace {

using tcp = boost::asio::ip::tcp;
namespace beast = boost::beast;
namespace http = beast::http;

bool IsTopologyLockedPhase(const std::string& phase) {
  return phase == "countdown" || phase == "green";
}

std::optional<P2PMarkerReceiverClient::Manifest> FetchManifest(
    const std::string& url,
    std::string* error) {
  URLParts parts;
  if (!URLParts::Parse(url, parts) || parts.scheme != "http" ||
      parts.host.empty()) {
    *error = "manifest URL must be a valid http URL";
    return std::nullopt;
  }

  try {
    boost::asio::io_context ioc;
    tcp::resolver resolver(ioc);
    beast::tcp_stream stream(ioc);
    stream.expires_after(std::chrono::seconds(2));
    const auto endpoints = resolver.resolve(parts.host, parts.GetPort());
    stream.connect(endpoints);

    std::string target = parts.path_query_fragment;
    if (target.empty()) {
      target = "/";
    }
    http::request<http::empty_body> request{http::verb::get, target, 11};
    request.set(http::field::host, parts.host);
    request.set(http::field::user_agent, "momo-marker-receiver/2");
    http::write(stream, request);

    beast::flat_buffer buffer;
    http::response<http::string_body> response;
    http::read(stream, buffer, response);
    beast::error_code close_error;
    stream.socket().shutdown(tcp::socket::shutdown_both, close_error);
    if (response.result() != http::status::ok) {
      *error = "manifest HTTP status " + std::to_string(response.result_int());
      return std::nullopt;
    }

    boost::system::error_code json_error;
    const boost::json::value root =
        boost::json::parse(response.body(), json_error);
    if (json_error || !root.is_object()) {
      *error = "manifest body is not a JSON object";
      return std::nullopt;
    }
    const auto& object = root.as_object();
    const auto version = object.if_contains("version");
    const auto revision = object.if_contains("revision");
    const auto sources = object.if_contains("sources");
    if (version == nullptr || !version->is_int64() || revision == nullptr ||
        !revision->is_string() || sources == nullptr || !sources->is_array()) {
      *error = "manifest requires version, revision, and sources";
      return std::nullopt;
    }

    P2PMarkerReceiverClient::Manifest manifest;
    manifest.version = static_cast<int>(version->as_int64());
    manifest.revision = revision->as_string().c_str();
    if (const auto phase = object.if_contains("phase");
        phase != nullptr && phase->is_string()) {
      manifest.phase = phase->as_string().c_str();
    }
    if (const auto run = object.if_contains("raceRunId");
        run != nullptr && run->is_string()) {
      manifest.race_run_id = run->as_string().c_str();
    }
    if (manifest.version != 1 || manifest.revision.empty()) {
      *error = "unsupported or empty manifest revision";
      return std::nullopt;
    }
    if (sources->as_array().size() >
        MarkerLumaSharedMemoryWriter::kMaximumSources) {
      *error = "manifest exceeds 32 marker sources";
      return std::nullopt;
    }

    for (const auto& source_value : sources->as_array()) {
      if (!source_value.is_object()) {
        *error = "manifest source is not an object";
        return std::nullopt;
      }
      const auto& source_object = source_value.as_object();
      const auto source_id = source_object.if_contains("sourceId");
      const auto observer_path = source_object.if_contains("observerPath");
      if (source_id == nullptr || !source_id->is_string() ||
          observer_path == nullptr || !observer_path->is_string()) {
        *error = "manifest source requires sourceId and observerPath";
        return std::nullopt;
      }
      P2PMarkerReceiverClient::ManifestSource source;
      source.source_id = source_id->as_string().c_str();
      source.observer_path = observer_path->as_string().c_str();
      if (const auto car_id = source_object.if_contains("carId");
          car_id != nullptr && car_id->is_string()) {
        source.car_id = car_id->as_string().c_str();
      }
      if (source.source_id.empty() || source.source_id.size() >= 32 ||
          source.observer_path.empty() || source.observer_path.front() != '/') {
        *error = "manifest source ID or observer path is invalid";
        return std::nullopt;
      }
      const auto duplicate =
          std::find_if(manifest.sources.begin(), manifest.sources.end(),
                       [&source](const auto& existing) {
                         return existing.source_id == source.source_id;
                       });
      if (duplicate != manifest.sources.end()) {
        *error = "manifest source ID is duplicated";
        return std::nullopt;
      }
      manifest.sources.push_back(std::move(source));
    }
    return manifest;
  } catch (const std::exception& exception) {
    *error = exception.what();
    return std::nullopt;
  }
}

}  // namespace

std::shared_ptr<P2PMarkerReceiverClient> P2PMarkerReceiverClient::Create(
    boost::asio::io_context& ioc,
    RTCManager* manager,
    P2PMarkerReceiverClientConfig config) {
  return std::shared_ptr<P2PMarkerReceiverClient>(
      new P2PMarkerReceiverClient(ioc, manager, std::move(config)));
}

P2PMarkerReceiverClient::P2PMarkerReceiverClient(
    boost::asio::io_context& ioc,
    RTCManager* manager,
    P2PMarkerReceiverClientConfig config)
    : ioc_(ioc), manager_(manager), config_(std::move(config)) {
  writer_ =
      std::make_shared<MarkerLumaSharedMemoryWriter>(config_.mapping_name);
}

P2PMarkerReceiverClient::~P2PMarkerReceiverClient() {
  Shutdown();
}

bool P2PMarkerReceiverClient::IsReady() const {
  return writer_ != nullptr && writer_->IsOpen();
}

void P2PMarkerReceiverClient::Connect() {
  if (!IsReady() || started_.exchange(true) || shutting_down_) {
    return;
  }
  std::weak_ptr<P2PMarkerReceiverClient> weak_self = shared_from_this();
  polling_thread_ = std::jthread([weak_self](std::stop_token stop_token) {
    if (const auto self = weak_self.lock()) {
      self->PollManifests(stop_token);
    }
  });
}

void P2PMarkerReceiverClient::Shutdown(std::function<void()> on_shutdown) {
  if (shutting_down_.exchange(true)) {
    if (on_shutdown) {
      on_shutdown();
    }
    return;
  }
  if (polling_thread_.joinable()) {
    polling_thread_.request_stop();
    polling_thread_.join();
  }
  for (Source& source : sources_) {
    source.reconnect_timer->cancel();
    source.connection_timer->cancel();
    if (source.client) {
      source.client->Shutdown();
      source.client.reset();
    }
    writer_->SetConnected(source.slot, source.generation, false);
  }
  pending_connections_.clear();
  in_flight_connections_ = 0;
  sources_.clear();
  if (on_shutdown) {
    on_shutdown();
  }
}

void P2PMarkerReceiverClient::PollManifests(std::stop_token stop_token) {
  size_t consecutive_errors = 0;
  while (!stop_token.stop_requested() && !shutting_down_) {
    std::string error;
    auto manifest = FetchManifest(config_.manifest_url, &error);
    if (manifest.has_value()) {
      consecutive_errors = 0;
      std::weak_ptr<P2PMarkerReceiverClient> weak_self = shared_from_this();
      boost::asio::post(ioc_,
                        [weak_self, manifest = std::move(*manifest)]() mutable {
                          if (const auto self = weak_self.lock()) {
                            self->ApplyManifest(std::move(manifest));
                          }
                        });
    } else {
      ++consecutive_errors;
      if (consecutive_errors == 1 || consecutive_errors % 30 == 0) {
        RTC_LOG(LS_WARNING) << "Marker manifest fetch failed ("
                            << consecutive_errors << "): " << error;
      }
    }

    auto remaining = config_.manifest_poll_interval;
    constexpr auto kSleepSlice = std::chrono::milliseconds(50);
    while (remaining > std::chrono::milliseconds::zero() &&
           !stop_token.stop_requested()) {
      const auto duration = std::min(remaining, kSleepSlice);
      std::this_thread::sleep_for(duration);
      remaining -= duration;
    }
  }
}

void P2PMarkerReceiverClient::ApplyManifest(Manifest manifest) {
  if (shutting_down_) {
    return;
  }
  current_phase_ = manifest.phase;
  writer_->SetRacePhase(current_phase_);
  if (manifest.revision == applied_revision_) {
    ApplyPendingManifestIfAllowed();
    return;
  }
  if (!applied_revision_.empty() && IsTopologyLockedPhase(current_phase_)) {
    if (!pending_manifest_ ||
        pending_manifest_->revision != manifest.revision) {
      RTC_LOG(LS_WARNING) << "Deferring marker topology " << manifest.revision
                          << " while phase is " << current_phase_;
    }
    pending_manifest_ = std::make_unique<Manifest>(std::move(manifest));
    return;
  }
  ReplaceSources(manifest);
  pending_manifest_.reset();
}

void P2PMarkerReceiverClient::ApplyPendingManifestIfAllowed() {
  if (!pending_manifest_ || IsTopologyLockedPhase(current_phase_)) {
    return;
  }
  Manifest manifest = std::move(*pending_manifest_);
  pending_manifest_.reset();
  ReplaceSources(manifest);
}

void P2PMarkerReceiverClient::ReplaceSources(const Manifest& manifest) {
  pending_connections_.clear();
  in_flight_connections_ = 0;
  for (Source& source : sources_) {
    source.reconnect_timer->cancel();
    source.connection_timer->cancel();
    if (source.client) {
      source.client->Shutdown();
      source.client.reset();
    }
    writer_->SetConnected(source.slot, source.generation, false);
  }
  sources_.clear();

  std::vector<MarkerLumaSourceConfig> writer_sources;
  writer_sources.reserve(manifest.sources.size());
  for (const ManifestSource& source : manifest.sources) {
    writer_sources.push_back(
        {source.source_id, config_.flip_vertical, config_.flip_horizontal});
  }
  const uint64_t generation =
      writer_->ConfigureSources(writer_sources, manifest.revision);
  if (generation == 0 && !manifest.sources.empty()) {
    RTC_LOG(LS_ERROR) << "Failed to configure MLY2 topology";
    return;
  }

  for (size_t index = 0; index < manifest.sources.size(); ++index) {
    Source source;
    source.config = manifest.sources[index];
    source.endpoint = ObserverEndpoint(source.config.observer_path);
    source.slot = index;
    source.generation = generation;
    source.receiver = std::make_unique<MarkerVideoTrackReceiver>(
        writer_, index, generation, config_.maximum_framerate);
    source.reconnect_timer = std::make_unique<boost::asio::steady_timer>(ioc_);
    source.connection_timer = std::make_unique<boost::asio::steady_timer>(ioc_);
    sources_.push_back(std::move(source));
  }
  applied_revision_ = manifest.revision;
  RTC_LOG(LS_INFO) << "Applied marker manifest " << applied_revision_ << ": "
                   << sources_.size() << " sources";
  for (Source& source : sources_) {
    EnqueueConnection(source.config.source_id, source.generation);
  }
  PumpConnections();
}

void P2PMarkerReceiverClient::EnqueueConnection(const std::string& source_id,
                                                uint64_t generation) {
  if (shutting_down_) {
    return;
  }
  Source* source = FindSource(source_id);
  if (source == nullptr || source->generation != generation || source->queued ||
      source->connecting || source->media_connected) {
    return;
  }
  source->queued = true;
  pending_connections_.push_back({source_id, generation});
}

void P2PMarkerReceiverClient::PumpConnections() {
  if (shutting_down_) {
    return;
  }
  while (in_flight_connections_ < config_.maximum_concurrent_connections &&
         !pending_connections_.empty()) {
    PendingConnection pending = std::move(pending_connections_.front());
    pending_connections_.pop_front();
    Source* source = FindSource(pending.source_id);
    if (source == nullptr || source->generation != pending.generation ||
        !source->queued) {
      continue;
    }
    source->queued = false;
    ConnectSource(*source);
  }
}

void P2PMarkerReceiverClient::ConnectSource(Source& source) {
  if (shutting_down_ || source.connecting || source.media_connected) {
    return;
  }
  source.connecting = true;
  const uint64_t attempt_id = ++source.attempt_id;
  ++in_flight_connections_;

  const std::string source_id = source.config.source_id;
  const size_t slot = source.slot;
  const uint64_t generation = source.generation;
  writer_->SetConnected(slot, generation, false);

  std::weak_ptr<P2PMarkerReceiverClient> weak_self = shared_from_this();
  source.connection_timer->expires_after(config_.connection_timeout);
  source.connection_timer->async_wait(
      [weak_self, source_id, generation,
       attempt_id](const boost::system::error_code& error) {
        if (error) {
          return;
        }
        if (const auto self = weak_self.lock()) {
          self->HandleConnectionTimeout(source_id, generation, attempt_id);
        }
      });

  P2PReceiverClientConfig client_config;
  client_config.endpoint = source.endpoint;
  client_config.no_google_stun = config_.no_google_stun;
  client_config.receiver = source.receiver.get();
  client_config.on_media_connected = [weak_self, source_id, generation,
                                      attempt_id]() {
    if (const auto self = weak_self.lock()) {
      boost::asio::post(self->ioc_, [weak_self, source_id, generation,
                                     attempt_id]() {
        if (const auto posted_self = weak_self.lock()) {
          posted_self->HandleMediaConnected(source_id, generation, attempt_id);
        }
      });
    }
  };
  client_config.on_disconnected = [weak_self, source_id, generation,
                                   attempt_id]() {
    if (const auto self = weak_self.lock()) {
      boost::asio::post(self->ioc_, [weak_self, source_id, generation,
                                     attempt_id]() {
        if (const auto posted_self = weak_self.lock()) {
          posted_self->HandleDisconnected(source_id, generation, attempt_id);
        }
      });
    }
  };
  source.client =
      P2PReceiverClient::Create(ioc_, manager_, std::move(client_config));
  source.client->Connect();
}

void P2PMarkerReceiverClient::HandleMediaConnected(const std::string& source_id,
                                                   uint64_t generation,
                                                   uint64_t attempt_id) {
  Source* source = FindSource(source_id);
  if (shutting_down_ || source == nullptr || source->generation != generation ||
      source->attempt_id != attempt_id || !source->connecting) {
    return;
  }
  source->connection_timer->cancel();
  source->connecting = false;
  source->media_connected = true;
  if (in_flight_connections_ > 0) {
    --in_flight_connections_;
  }
  RTC_LOG(LS_INFO) << "Marker media connected: " << source_id
                   << " (pending=" << pending_connections_.size()
                   << ", active_connects=" << in_flight_connections_ << ")";
  PumpConnections();
}

void P2PMarkerReceiverClient::HandleDisconnected(const std::string& source_id,
                                                 uint64_t generation,
                                                 uint64_t attempt_id) {
  if (shutting_down_) {
    return;
  }
  Source* source = FindSource(source_id);
  if (source == nullptr || source->generation != generation ||
      source->attempt_id != attempt_id) {
    return;
  }
  source->connection_timer->cancel();
  if (source->connecting && in_flight_connections_ > 0) {
    --in_flight_connections_;
  }
  source->connecting = false;
  source->media_connected = false;
  ++source->attempt_id;
  if (source->client) {
    source->client->Shutdown();
    source->client.reset();
  }
  writer_->SetConnected(source->slot, generation, false);
  ScheduleReconnect(source_id, generation);
  PumpConnections();
}

void P2PMarkerReceiverClient::HandleConnectionTimeout(
    const std::string& source_id,
    uint64_t generation,
    uint64_t attempt_id) {
  Source* source = FindSource(source_id);
  if (shutting_down_ || source == nullptr || source->generation != generation ||
      source->attempt_id != attempt_id || !source->connecting) {
    return;
  }
  RTC_LOG(LS_WARNING) << "Marker media connection timed out: " << source_id;
  if (in_flight_connections_ > 0) {
    --in_flight_connections_;
  }
  source->connecting = false;
  source->media_connected = false;
  ++source->attempt_id;
  if (source->client) {
    source->client->Shutdown();
    source->client.reset();
  }
  writer_->SetConnected(source->slot, generation, false);
  ScheduleReconnect(source_id, generation);
  PumpConnections();
}

void P2PMarkerReceiverClient::ScheduleReconnect(const std::string& source_id,
                                                uint64_t generation) {
  Source* source = FindSource(source_id);
  if (shutting_down_ || source == nullptr || source->generation != generation) {
    return;
  }
  source->reconnect_timer->expires_after(std::chrono::seconds(2));
  std::weak_ptr<P2PMarkerReceiverClient> weak_self = shared_from_this();
  source->reconnect_timer->async_wait(
      [weak_self, source_id,
       generation](const boost::system::error_code& error) {
        if (error) {
          return;
        }
        if (const auto self = weak_self.lock()) {
          self->EnqueueConnection(source_id, generation);
          self->PumpConnections();
        }
      });
}

P2PMarkerReceiverClient::Source* P2PMarkerReceiverClient::FindSource(
    const std::string& source_id) {
  const auto found = std::find_if(sources_.begin(), sources_.end(),
                                  [&source_id](const Source& source) {
                                    return source.config.source_id == source_id;
                                  });
  return found == sources_.end() ? nullptr : &*found;
}

std::string P2PMarkerReceiverClient::ObserverEndpoint(
    const std::string& observer_path) const {
  URLParts parts;
  if (!URLParts::Parse(config_.manifest_url, parts)) {
    return std::string();
  }
  return "ws://" + parts.host + ":" + parts.GetPort() + observer_path;
}
