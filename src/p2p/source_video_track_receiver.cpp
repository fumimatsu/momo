#include "p2p/source_video_track_receiver.h"

#include <utility>

#include "sdl_renderer/sdl_renderer.h"

SourceVideoTrackReceiver::SourceVideoTrackReceiver(SDLRenderer* renderer,
                                                   std::string source_name)
    : renderer_(renderer), source_name_(std::move(source_name)) {}

void SourceVideoTrackReceiver::AddTrack(webrtc::VideoTrackInterface* track) {
  if (renderer_ != nullptr) {
    renderer_->AddTrackForSource(track, source_name_);
  }
}

void SourceVideoTrackReceiver::RemoveTrack(
    webrtc::VideoTrackInterface* track) {
  if (renderer_ != nullptr) {
    renderer_->RemoveTrack(track);
    renderer_->SetSourceState(source_name_, SDLRenderer::SourceState::kOffline);
  }
}
