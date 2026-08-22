#include "p2p/marker_video_track_receiver.h"

#include <chrono>
#include <utility>

#include <api/media_stream_interface.h>
#include <api/video/i420_buffer.h>
#include <api/video/video_frame.h>
#include <api/video/video_sink_interface.h>

#include "marker/marker_luma_shared_memory.h"

class MarkerVideoTrackReceiver::Sink
    : public webrtc::VideoSinkInterface<webrtc::VideoFrame> {
 public:
  Sink(std::shared_ptr<MarkerLumaSharedMemoryWriter> writer,
       webrtc::VideoTrackInterface* track,
       size_t slot,
       uint64_t generation,
       int maximum_framerate)
      : writer_(std::move(writer)),
        track_(track),
        slot_(slot),
        generation_(generation) {
    webrtc::VideoSinkWants wants;
    wants.max_framerate_fps = maximum_framerate;
    track_->AddOrUpdateSink(this, wants);
  }

  ~Sink() override { track_->RemoveSink(this); }

  void OnFrame(const webrtc::VideoFrame& frame) override {
    auto source = frame.video_frame_buffer()->ToI420();
    if (frame.rotation() != webrtc::kVideoRotation_0) {
      source = webrtc::I420Buffer::Rotate(*source, frame.rotation());
    }
    const int64_t received_unix_ns =
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count();
    writer_->WriteFrame(slot_, generation_, source->DataY(), source->StrideY(),
                        source->width(), source->height(), received_unix_ns);
  }

 private:
  std::shared_ptr<MarkerLumaSharedMemoryWriter> writer_;
  webrtc::scoped_refptr<webrtc::VideoTrackInterface> track_;
  size_t slot_;
  uint64_t generation_;
};

MarkerVideoTrackReceiver::MarkerVideoTrackReceiver(
    std::shared_ptr<MarkerLumaSharedMemoryWriter> writer,
    size_t slot,
    uint64_t generation,
    int maximum_framerate)
    : writer_(std::move(writer)),
      slot_(slot),
      generation_(generation),
      maximum_framerate_(maximum_framerate) {}

MarkerVideoTrackReceiver::~MarkerVideoTrackReceiver() = default;

void MarkerVideoTrackReceiver::AddTrack(webrtc::VideoTrackInterface* track) {
  std::lock_guard<std::mutex> lock(sink_mutex_);
  sink_ = std::make_unique<Sink>(writer_, track, slot_, generation_,
                                 maximum_framerate_);
  writer_->SetConnected(slot_, generation_, true);
}

void MarkerVideoTrackReceiver::RemoveTrack(webrtc::VideoTrackInterface* track) {
  static_cast<void>(track);
  std::lock_guard<std::mutex> lock(sink_mutex_);
  sink_.reset();
  writer_->SetConnected(slot_, generation_, false);
}
