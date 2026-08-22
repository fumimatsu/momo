#ifndef P2P_MARKER_RECEIVER_CLIENT_H_
#define P2P_MARKER_RECEIVER_CLIENT_H_

#include <atomic>
#include <chrono>
#include <deque>
#include <functional>
#include <memory>
#include <string>
#include <thread>
#include <vector>

#include <boost/asio/io_context.hpp>
#include <boost/asio/steady_timer.hpp>

#include "p2p/p2p_receiver_client.h"

class MarkerLumaSharedMemoryWriter;
class MarkerVideoTrackReceiver;
class RTCManager;

struct P2PMarkerReceiverClientConfig {
  std::string manifest_url = "http://127.0.0.1:8090/api/v1/marker-sources";
  std::string mapping_name = "Local\\MomoMarkerLumaV2";
  bool no_google_stun = false;
  bool flip_vertical = false;
  bool flip_horizontal = false;
  int maximum_framerate = 50;
  size_t maximum_concurrent_connections = 4;
  std::chrono::milliseconds connection_timeout{20000};
  std::chrono::milliseconds manifest_poll_interval{1000};
};

class P2PMarkerReceiverClient
    : public std::enable_shared_from_this<P2PMarkerReceiverClient> {
 public:
  static std::shared_ptr<P2PMarkerReceiverClient> Create(
      boost::asio::io_context& ioc,
      RTCManager* manager,
      P2PMarkerReceiverClientConfig config);

  ~P2PMarkerReceiverClient();
  bool IsReady() const;
  void Connect();
  void Shutdown(std::function<void()> on_shutdown = nullptr);

  // Exposed for the local HTTP parser; not part of the wire API.
  struct ManifestSource {
    std::string source_id;
    std::string car_id;
    std::string observer_path;
  };

  struct Manifest {
    int version = 0;
    std::string revision;
    std::string phase;
    std::string race_run_id;
    std::vector<ManifestSource> sources;
  };

 private:
  struct Source {
    ManifestSource config;
    std::string endpoint;
    size_t slot = 0;
    uint64_t generation = 0;
    uint64_t attempt_id = 0;
    bool queued = false;
    bool connecting = false;
    bool media_connected = false;
    std::unique_ptr<MarkerVideoTrackReceiver> receiver;
    std::shared_ptr<P2PReceiverClient> client;
    std::unique_ptr<boost::asio::steady_timer> reconnect_timer;
    std::unique_ptr<boost::asio::steady_timer> connection_timer;
  };

  struct PendingConnection {
    std::string source_id;
    uint64_t generation = 0;
  };

  P2PMarkerReceiverClient(boost::asio::io_context& ioc,
                          RTCManager* manager,
                          P2PMarkerReceiverClientConfig config);
  void PollManifests(std::stop_token stop_token);
  void ApplyManifest(Manifest manifest);
  void ApplyPendingManifestIfAllowed();
  void ReplaceSources(const Manifest& manifest);
  void EnqueueConnection(const std::string& source_id, uint64_t generation);
  void PumpConnections();
  void ConnectSource(Source& source);
  void HandleMediaConnected(const std::string& source_id,
                            uint64_t generation,
                            uint64_t attempt_id);
  void HandleDisconnected(const std::string& source_id,
                          uint64_t generation,
                          uint64_t attempt_id);
  void HandleConnectionTimeout(const std::string& source_id,
                               uint64_t generation,
                               uint64_t attempt_id);
  void ScheduleReconnect(const std::string& source_id, uint64_t generation);
  Source* FindSource(const std::string& source_id);
  std::string ObserverEndpoint(const std::string& observer_path) const;

  boost::asio::io_context& ioc_;
  RTCManager* manager_;
  P2PMarkerReceiverClientConfig config_;
  std::shared_ptr<MarkerLumaSharedMemoryWriter> writer_;
  std::vector<Source> sources_;
  std::deque<PendingConnection> pending_connections_;
  size_t in_flight_connections_ = 0;
  std::string applied_revision_;
  std::string current_phase_;
  std::unique_ptr<Manifest> pending_manifest_;
  std::jthread polling_thread_;
  std::atomic_bool started_{false};
  std::atomic_bool shutting_down_{false};
};

#endif  // P2P_MARKER_RECEIVER_CLIENT_H_
