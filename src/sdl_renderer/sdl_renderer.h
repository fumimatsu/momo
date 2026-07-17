#ifndef SDL_RENDERER_H_
#define SDL_RENDERER_H_

#include <atomic>
#include <chrono>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

// SDL
#include <SDL3/SDL.h>

// Boost
#include <boost/asio.hpp>

// WebRTC
#include <api/media_stream_interface.h>
#include <api/scoped_refptr.h>
#include <api/video/video_frame.h>
#include <api/video/video_sink_interface.h>
#include <rtc/video_track_receiver.h>
#include <rtc_base/synchronization/mutex.h>

#if defined(USE_OPENCV_ARUCO)
#include "aruco/aruco_detector.h"
#endif

class SharedFrameWriter;

class SDLRenderer : public VideoTrackReceiver {
 public:
  enum class SourceState {
    kConnecting,
    kLive,
    kOffline,
  };

  SDLRenderer(int width, int height, bool fullscreen,
              bool enable_aruco = false, bool flip_vertical = false,
              bool flip_horizontal = false,
              std::string shared_frame_name = "");
  ~SDLRenderer();

  void SetDispatchFunction(std::function<void(std::function<void()>)> dispatch);

  static int RenderThreadExec(void* data);
  int RenderThread();

  void SetOutlines();
  void AddTrack(webrtc::VideoTrackInterface* track) override;
  void RemoveTrack(webrtc::VideoTrackInterface* track) override;
  void ConfigureFixedSlots(const std::vector<std::string>& source_names);
  void AddTrackForSource(webrtc::VideoTrackInterface* track,
                         const std::string& source_name);
  void SetSourceState(const std::string& source_name, SourceState state);
  void SetOverlayText(std::string text);
  double GetPrimaryFps();
  bool IsFlipVertical() const;
  bool IsFlipHorizontal() const;

 protected:
  class Sink : public webrtc::VideoSinkInterface<webrtc::VideoFrame> {
   public:
    Sink(SDLRenderer* renderer,
         webrtc::VideoTrackInterface* track,
         bool enable_aruco,
         std::string source_name = "",
         int slot_index = -1);
    ~Sink();

    void OnFrame(const webrtc::VideoFrame& frame) override;

    void SetOutlineRect(int x, int y, int width, int height);

    webrtc::Mutex* GetMutex();
    bool GetOutlineChanged();
    int GetOffsetX();
    int GetOffsetY();
    int GetFrameWidth();
    int GetFrameHeight();
    int GetWidth();
    int GetHeight();
    uint8_t* GetImage();
    const std::string& GetSourceName() const;
    int GetSlotIndex() const;
    double GetFps() const;
    bool IsReceiving(std::chrono::steady_clock::time_point now) const;
    bool CopySourceTo(uint8_t* destination, int destination_stride,
                      int destination_width, int destination_height,
                      bool flip_vertical, bool flip_horizontal) const;

   private:
    SDLRenderer* renderer_;
    webrtc::scoped_refptr<webrtc::VideoTrackInterface> track_;
    webrtc::Mutex frame_params_lock_;
    int outline_offset_x_;
    int outline_offset_y_;
    int outline_width_;
    int outline_height_;
    bool outline_changed_;
    float outline_aspect_;
    int input_width_;
    int input_height_;
    bool scaled_;
    std::unique_ptr<uint8_t[]> image_;
    std::unique_ptr<uint8_t[]> source_image_;
    int source_width_;
    int source_height_;
    int offset_x_;
    int offset_y_;
    int width_;
    int height_;
    std::atomic<uint64_t> frame_count_{0};
    std::chrono::steady_clock::time_point fps_window_start_;
    std::chrono::steady_clock::time_point last_frame_time_;
    uint64_t fps_window_frame_count_;
    double fps_;
    std::string source_name_;
    int slot_index_;
#if defined(USE_OPENCV_ARUCO)
    std::unique_ptr<ArucoDetector> aruco_detector_;
#endif
  };

 private:
  bool IsFullScreen();
  void SetFullScreen(bool fullscreen);
  void PollEvent();
  void WriteSharedFrame();
  struct SourceSlot {
    std::string name;
    SourceState state;
  };
  struct OutlineRect {
    int x;
    int y;
    int width;
    int height;
  };
  OutlineRect GetSlotOutline(int slot_index) const;
  int FindSourceSlot(const std::string& source_name) const;
  Sink* FindSinkForSource(const std::string& source_name);
  void RenderSourceOverlay();

  webrtc::Mutex sinks_lock_;
  typedef std::vector<
      std::pair<webrtc::VideoTrackInterface*, std::unique_ptr<Sink> > >
      VideoTrackSinkVector;
  VideoTrackSinkVector sinks_;
  std::atomic<bool> running_;
  SDL_Thread* thread_;
  SDL_Window* window_;
  SDL_Renderer* renderer_;
  std::function<void(std::function<void()>)> dispatch_;
  std::atomic<bool> flip_vertical_;
  std::atomic<bool> flip_horizontal_;
  int width_;
  int height_;
  int rows_;
  int cols_;
  bool enable_aruco_;
  std::unique_ptr<SharedFrameWriter> shared_frame_writer_;
  std::vector<uint8_t> shared_frame_buffer_;
  std::vector<SourceSlot> fixed_slots_;
  webrtc::Mutex overlay_lock_;
  std::string overlay_text_ RTC_GUARDED_BY(overlay_lock_);
};

#endif
