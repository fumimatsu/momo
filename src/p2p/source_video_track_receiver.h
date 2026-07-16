#ifndef SOURCE_VIDEO_TRACK_RECEIVER_H_
#define SOURCE_VIDEO_TRACK_RECEIVER_H_

#include <string>

#include "rtc/video_track_receiver.h"

class SDLRenderer;

class SourceVideoTrackReceiver : public VideoTrackReceiver {
 public:
  SourceVideoTrackReceiver(SDLRenderer* renderer, std::string source_name);

  void AddTrack(webrtc::VideoTrackInterface* track) override;
  void RemoveTrack(webrtc::VideoTrackInterface* track) override;

 private:
  SDLRenderer* renderer_;
  std::string source_name_;
};

#endif  // SOURCE_VIDEO_TRACK_RECEIVER_H_
