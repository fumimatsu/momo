#ifndef MARKER_VIDEO_TRACK_RECEIVER_H_
#define MARKER_VIDEO_TRACK_RECEIVER_H_

#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>

#include "rtc/video_track_receiver.h"

class MarkerLumaSharedMemoryWriter;

class MarkerVideoTrackReceiver : public VideoTrackReceiver {
 public:
  MarkerVideoTrackReceiver(std::shared_ptr<MarkerLumaSharedMemoryWriter> writer,
                           size_t slot,
                           uint64_t generation,
                           int maximum_framerate);
  ~MarkerVideoTrackReceiver();

  void AddTrack(webrtc::VideoTrackInterface* track) override;
  void RemoveTrack(webrtc::VideoTrackInterface* track) override;

 private:
  class Sink;
  std::shared_ptr<MarkerLumaSharedMemoryWriter> writer_;
  size_t slot_;
  uint64_t generation_;
  int maximum_framerate_;
  std::mutex sink_mutex_;
  std::unique_ptr<Sink> sink_;
};

#endif  // MARKER_VIDEO_TRACK_RECEIVER_H_
