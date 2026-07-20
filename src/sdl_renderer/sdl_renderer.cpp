#include "sdl_renderer.h"

#include <algorithm>
#include <cmath>
#include <csignal>
#include <cstring>
#include <iomanip>
#include <sstream>

#if defined(_WIN32)
#include <windows.h>
#endif

// WebRTC
#include <api/video/i420_buffer.h>
#include <rtc_base/logging.h>
#include <third_party/libyuv/include/libyuv/convert_from.h>
#include <third_party/libyuv/include/libyuv/video_common.h>

#if defined(USE_OPENCV_ARUCO)
#include <opencv2/imgproc.hpp>
#endif

#define STD_ASPECT 1.33
#define WIDE_ASPECT 1.78
#define FRAME_INTERVAL (1000 / 50)

namespace {

constexpr int kSharedFrameWidth = 1920;
constexpr int kSharedFrameHeight = 1080;
constexpr int kSharedFrameStride = kSharedFrameWidth * 4;
constexpr int kSharedFrameBufferCount = 3;
constexpr int kSharedSlotWidth = 960;
constexpr int kSharedSlotHeight = 540;
constexpr int kSharedSourceHeight = 528;
constexpr int kSharedSourceVerticalOffset =
    (kSharedSlotHeight - kSharedSourceHeight) / 2;
constexpr uint32_t kSharedFrameMagic = 0x3146504d;  // "MFP1"
constexpr uint32_t kSharedFramePixelFormat = 0x41524742;  // "BGRA"

#if defined(_WIN32)
struct SharedFrameHeader {
  uint32_t magic;
  uint16_t version;
  uint16_t header_size;
  uint32_t width;
  uint32_t height;
  uint32_t stride;
  uint32_t pixel_format;
  uint32_t buffer_count;
  volatile LONG active_buffer;
  volatile LONG reserved;
  volatile LONG64 sequence;
  volatile LONG64 timestamp_ns;
  uint8_t reserved_tail[8];
};
static_assert(sizeof(SharedFrameHeader) == 64);

std::wstring Utf8ToWide(const std::string& value) {
  if (value.empty()) {
    return std::wstring();
  }
  const int required = MultiByteToWideChar(CP_UTF8, 0, value.data(),
                                            value.size(), nullptr, 0);
  if (required <= 0) {
    return std::wstring();
  }
  std::wstring result(required, L'\0');
  MultiByteToWideChar(CP_UTF8, 0, value.data(), value.size(), result.data(),
                      required);
  return result;
}
#endif

}  // namespace

class SharedFrameWriter {
 public:
  explicit SharedFrameWriter(const std::string& name) {
#if defined(_WIN32)
    const std::wstring wide_name = Utf8ToWide(name);
    if (wide_name.empty()) {
      RTC_LOG(LS_ERROR) << "Shared frame mapping name is invalid";
      return;
    }

    const size_t mapping_size =
        sizeof(SharedFrameHeader) +
        static_cast<size_t>(kSharedFrameStride) * kSharedFrameHeight *
            kSharedFrameBufferCount;
    mapping_ = CreateFileMappingW(INVALID_HANDLE_VALUE, nullptr,
                                  PAGE_READWRITE,
                                  static_cast<DWORD>(mapping_size >> 32),
                                  static_cast<DWORD>(mapping_size),
                                  wide_name.c_str());
    if (mapping_ == nullptr) {
      RTC_LOG(LS_ERROR) << "CreateFileMappingW failed: " << GetLastError();
      return;
    }
    if (GetLastError() == ERROR_ALREADY_EXISTS) {
      RTC_LOG(LS_ERROR) << "Shared frame mapping already has another writer: "
                        << name;
      CloseHandle(mapping_);
      mapping_ = nullptr;
      return;
    }
    view_ = static_cast<uint8_t*>(
        MapViewOfFile(mapping_, FILE_MAP_ALL_ACCESS, 0, 0, mapping_size));
    if (view_ == nullptr) {
      RTC_LOG(LS_ERROR) << "MapViewOfFile failed: " << GetLastError();
      CloseHandle(mapping_);
      mapping_ = nullptr;
      return;
    }

    auto* header = reinterpret_cast<SharedFrameHeader*>(view_);
    std::memset(header, 0, sizeof(*header));
    header->magic = kSharedFrameMagic;
    header->version = 1;
    header->header_size = sizeof(*header);
    header->width = kSharedFrameWidth;
    header->height = kSharedFrameHeight;
    header->stride = kSharedFrameStride;
    header->pixel_format = kSharedFramePixelFormat;
    header->buffer_count = kSharedFrameBufferCount;
    RTC_LOG(LS_INFO) << "Shared frame writer opened: " << name << " "
                     << kSharedFrameWidth << "x" << kSharedFrameHeight;
#else
    RTC_LOG(LS_WARNING) << "Shared frame output is only supported on Windows";
    static_cast<void>(name);
#endif
  }

  ~SharedFrameWriter() {
#if defined(_WIN32)
    if (view_ != nullptr) {
      UnmapViewOfFile(view_);
    }
    if (mapping_ != nullptr) {
      CloseHandle(mapping_);
    }
#endif
  }

  bool IsOpen() const {
#if defined(_WIN32)
    return view_ != nullptr;
#else
    return false;
#endif
  }

  void Write(const uint8_t* frame, size_t size, int64_t timestamp_ns) {
#if defined(_WIN32)
    const size_t frame_size =
        static_cast<size_t>(kSharedFrameStride) * kSharedFrameHeight;
    if (view_ == nullptr || size != frame_size) {
      return;
    }

    auto* header = reinterpret_cast<SharedFrameHeader*>(view_);
    InterlockedIncrement64(&header->sequence);  // odd: write in progress
    uint8_t* destination = view_ + sizeof(SharedFrameHeader) +
                           static_cast<size_t>(next_buffer_) * frame_size;
    std::memcpy(destination, frame, frame_size);
    header->timestamp_ns = timestamp_ns;
    MemoryBarrier();
    InterlockedExchange(&header->active_buffer, next_buffer_);
    MemoryBarrier();
    InterlockedIncrement64(&header->sequence);  // even: complete frame
    next_buffer_ = (next_buffer_ + 1) % kSharedFrameBufferCount;
#else
    static_cast<void>(frame);
    static_cast<void>(size);
    static_cast<void>(timestamp_ns);
#endif
  }

 private:
#if defined(_WIN32)
  HANDLE mapping_ = nullptr;
  uint8_t* view_ = nullptr;
  LONG next_buffer_ = 0;
#endif
};

SDLRenderer::SDLRenderer(int width, int height, bool fullscreen,
                         bool enable_aruco, bool flip_vertical,
                         bool flip_horizontal, std::string shared_frame_name)
    : running_(true),
      window_(nullptr),
      renderer_(nullptr),
      dispatch_(nullptr),
      flip_vertical_(flip_vertical),
      flip_horizontal_(flip_horizontal),
      width_(width),
      height_(height),
      rows_(1),
      cols_(1),
      enable_aruco_(enable_aruco) {
#if !defined(USE_OPENCV_ARUCO)
  if (enable_aruco_) {
    RTC_LOG(LS_ERROR) << "ArUco detection was requested, but this build does "
                         "not include OpenCV";
    enable_aruco_ = false;
  }
#endif
  if (!shared_frame_name.empty()) {
    shared_frame_writer_ =
        std::make_unique<SharedFrameWriter>(shared_frame_name);
    if (shared_frame_writer_->IsOpen()) {
      shared_frame_buffer_.resize(
          static_cast<size_t>(kSharedFrameStride) * kSharedFrameHeight);
    } else {
      shared_frame_writer_.reset();
    }
  }
  // 映像表示はジョイスティック機能に依存させない。配布先の SDL が
  // joystick backend を含まない場合でも、Pilot の映像 Viewer 自体は
  // 起動して状態を表示できる必要がある。
  if (!SDL_Init(SDL_INIT_VIDEO)) {
    RTC_LOG(LS_ERROR) << __FUNCTION__ << ": SDL_Init failed " << SDL_GetError();
    return;
  }

  window_ = SDL_CreateWindow("Momo WebRTC Native Client", width_, height_,
                             SDL_WINDOW_OPENGL | SDL_WINDOW_RESIZABLE);
  if (window_ == nullptr) {
    RTC_LOG(LS_ERROR) << __FUNCTION__ << ": SDL_CreateWindow failed "
                      << SDL_GetError();
    return;
  }

  if (fullscreen) {
    SetFullScreen(true);
  }

#if defined(__APPLE__)
  // Apple Silicon Mac + macOS 11.0 だと、
  // SDL_CreateRenderer をメインスレッドで呼ばないとエラーになる
  renderer_ = SDL_CreateRenderer(window_, NULL);
  if (renderer_ == nullptr) {
    RTC_LOG(LS_ERROR) << __FUNCTION__ << ": SDL_CreateRenderer failed "
                      << SDL_GetError();
    return;
  }
#endif

  thread_ = SDL_CreateThread(SDLRenderer::RenderThreadExec, "Render", this);
}

SDLRenderer::~SDLRenderer() {
  running_ = false;
  int ret = 0;
  SDL_WaitThread(thread_, &ret);
  if (ret != 0) {
    RTC_LOG(LS_ERROR) << __FUNCTION__ << ": SDL Thread error:" << ret;
  }
  if (renderer_) {
    SDL_DestroyRenderer(renderer_);
  }
  if (window_) {
    SDL_DestroyWindow(window_);
  }
  SDL_Quit();
}

bool SDLRenderer::IsFullScreen() {
  return SDL_GetWindowFlags(window_) & SDL_WINDOW_FULLSCREEN;
}

void SDLRenderer::SetFullScreen(bool fullscreen) {
  SDL_SetWindowFullscreen(window_, fullscreen);
  if (fullscreen) {
    SDL_HideCursor();
  } else {
    SDL_ShowCursor();
  }
}

void SDLRenderer::PollEvent() {
  SDL_Event e;
  // 必ずメインスレッドから呼び出す
  while (SDL_PollEvent(&e)) {
    if (e.type == SDL_EVENT_WINDOW_RESIZED &&
        e.window.windowID == SDL_GetWindowID(window_)) {
      webrtc::MutexLock lock(&sinks_lock_);
      width_ = e.window.data1;
      height_ = e.window.data2;
      SetOutlines();
    }
    if (e.type == SDL_EVENT_KEY_UP) {
      switch (e.key.key) {
        case SDLK_F:
          SetFullScreen(!IsFullScreen());
          break;
        case SDLK_V:
          flip_vertical_.store(!flip_vertical_.load());
          RTC_LOG(LS_INFO) << "SDLRenderer: vertical flip="
                           << flip_vertical_.load();
          break;
        case SDLK_H:
          flip_horizontal_.store(!flip_horizontal_.load());
          RTC_LOG(LS_INFO) << "SDLRenderer: horizontal flip="
                           << flip_horizontal_.load();
          break;
        case SDLK_Q:
          std::raise(SIGTERM);
          break;
      }
    }
    if (e.type == SDL_EVENT_QUIT) {
      std::raise(SIGTERM);
    }
  }
}

void SDLRenderer::SetDispatchFunction(
    std::function<void(std::function<void()>)> dispatch) {
  webrtc::MutexLock lock(&sinks_lock_);
  dispatch_ = std::move(dispatch);
}

int SDLRenderer::RenderThreadExec(void* data) {
  return ((SDLRenderer*)data)->RenderThread();
}

int SDLRenderer::RenderThread() {
#if !defined(__APPLE__)
  renderer_ = SDL_CreateRenderer(window_, NULL);
  if (renderer_ == nullptr) {
    RTC_LOG(LS_ERROR) << __FUNCTION__ << ": SDL_CreateRenderer failed "
                      << SDL_GetError();
    return 1;
  }
#endif

  SDL_SetRenderDrawColor(renderer_, 0, 0, 0, 255);

  uint32_t start_time, duration;
  while (running_) {
    start_time = SDL_GetTicks();
    {
      webrtc::MutexLock lock(&sinks_lock_);
      SDL_RenderClear(renderer_);
      for (const VideoTrackSinkVector::value_type& sinks : sinks_) {
        Sink* sink = sinks.second.get();

        webrtc::MutexLock frame_lock(sink->GetMutex());

        if (!sink->GetOutlineChanged())
          continue;

        int width = sink->GetFrameWidth();
        int height = sink->GetFrameHeight();

        if (width == 0 || height == 0)
          continue;

        SDL_Surface* surface =
            SDL_CreateSurfaceFrom(width, height, SDL_PIXELFORMAT_ARGB8888,
                                  sink->GetImage(), width * 4);
        SDL_Texture* texture = SDL_CreateTextureFromSurface(renderer_, surface);
        SDL_DestroySurface(surface);

        SDL_FRect image_rect = {0, 0, (float)width, (float)height};
        SDL_FRect draw_rect = {
            (float)sink->GetOffsetX(), (float)sink->GetOffsetY(),
            (float)sink->GetWidth(), (float)sink->GetHeight()};

        // ArUco モードでは Sink 側で先に映像を反転してから文字やマーカーを
        // 描画しているため、ここでは再反転しない。
        bool flip_vertical = flip_vertical_.load();
        bool flip_horizontal = flip_horizontal_.load();
        GetEffectiveFlip(sink->GetSourceName(), &flip_vertical,
                         &flip_horizontal);
        const SDL_FlipMode flip = enable_aruco_
                                      ? SDL_FLIP_NONE
                                      : static_cast<SDL_FlipMode>(
                                            (flip_vertical
                                                 ? SDL_FLIP_VERTICAL
                                                 : SDL_FLIP_NONE) |
                                            (flip_horizontal
                                                 ? SDL_FLIP_HORIZONTAL
                                                 : SDL_FLIP_NONE));
        SDL_RenderTextureRotated(renderer_, texture, &image_rect, &draw_rect,
                                 0, nullptr, flip);

        SDL_DestroyTexture(texture);
      }
      WriteSharedFrame();
      RenderSourceOverlay();
      SDL_RenderPresent(renderer_);

      if (dispatch_) {
        dispatch_(std::bind(&SDLRenderer::PollEvent, this));
      }
    }
    duration = SDL_GetTicks() - start_time;
    if (duration < FRAME_INTERVAL) {
      SDL_Delay(FRAME_INTERVAL - duration);
    }
  }

  SDL_DestroyRenderer(renderer_);
  renderer_ = nullptr;

  return 0;
}

SDLRenderer::Sink::Sink(SDLRenderer* renderer,
                        webrtc::VideoTrackInterface* track,
                        bool enable_aruco,
                        std::string source_name,
                        int slot_index)
    : renderer_(renderer),
      track_(track),
      outline_offset_x_(0),
      outline_offset_y_(0),
      outline_width_(0),
      outline_height_(0),
      outline_changed_(false),
      input_width_(0),
      input_height_(0),
      scaled_(false),
      width_(0),
      height_(0),
      source_width_(0),
      source_height_(0),
      fps_window_start_(std::chrono::steady_clock::now()),
      last_frame_time_(std::chrono::steady_clock::time_point::min()),
      fps_window_frame_count_(0),
      fps_(0.0),
      source_name_(std::move(source_name)),
      slot_index_(slot_index) {
#if defined(USE_OPENCV_ARUCO)
  if (enable_aruco) {
    aruco_detector_ = std::make_unique<ArucoDetector>();
  }
#else
  static_cast<void>(enable_aruco);
#endif
  track_->AddOrUpdateSink(this, webrtc::VideoSinkWants());
}

SDLRenderer::Sink::~Sink() {
  track_->RemoveSink(this);
}

void SDLRenderer::Sink::OnFrame(const webrtc::VideoFrame& frame) {
  const uint64_t frame_count = ++frame_count_;
  if (frame_count == 1 || frame_count % 30 == 0) {
    RTC_LOG(LS_INFO) << "SDLRenderer::Sink::OnFrame count=" << frame_count
                     << " size=" << frame.width() << "x" << frame.height();
  }
  if (outline_width_ == 0 || outline_height_ == 0)
    return;
  if (frame.width() == 0 || frame.height() == 0)
    return;

  const auto now = std::chrono::steady_clock::now();
  ++fps_window_frame_count_;
  const std::chrono::duration<double> elapsed = now - fps_window_start_;
  if (elapsed.count() >= 0.5) {
    fps_ = static_cast<double>(fps_window_frame_count_) / elapsed.count();
    fps_window_start_ = now;
    fps_window_frame_count_ = 0;
  }

  webrtc::MutexLock lock(GetMutex());
  last_frame_time_ = now;
  webrtc::scoped_refptr<webrtc::I420BufferInterface> source_buffer =
      frame.video_frame_buffer()->ToI420();
  if (frame.rotation() != webrtc::kVideoRotation_0) {
    source_buffer =
        webrtc::I420Buffer::Rotate(*source_buffer, frame.rotation());
  }
  if (source_buffer->width() != source_width_ ||
      source_buffer->height() != source_height_) {
    source_width_ = source_buffer->width();
    source_height_ = source_buffer->height();
    source_image_.reset(new uint8_t[source_width_ * source_height_ * 4]);
  }
  libyuv::ConvertFromI420(
      source_buffer->DataY(), source_buffer->StrideY(), source_buffer->DataU(),
      source_buffer->StrideU(), source_buffer->DataV(), source_buffer->StrideV(),
      source_image_.get(), source_width_ * 4, source_width_, source_height_,
      libyuv::FOURCC_ARGB);
  if (outline_changed_ || frame.width() != input_width_ ||
      frame.height() != input_height_) {
    int width, height;
    float frame_aspect = (float)frame.width() / (float)frame.height();
    if (frame_aspect > outline_aspect_) {
      width = outline_width_;
      height = width / frame_aspect;
      offset_x_ = 0;
      offset_y_ = (outline_height_ - height) / 2;
    } else {
      height = outline_height_;
      width = height * frame_aspect;
      offset_x_ = (outline_width_ - width) / 2;
      offset_y_ = 0;
    }
    if (width_ != width || height_ != height) {
      width_ = width;
      height_ = height;
    }
    input_width_ = frame.width();
    input_height_ = frame.height();
    scaled_ = width_ < input_width_;
    if (scaled_) {
      image_.reset(new uint8_t[width_ * height_ * 4]);
    } else {
      image_.reset(new uint8_t[input_width_ * input_height_ * 4]);
    }
    RTC_LOG(LS_VERBOSE) << __FUNCTION__ << ": scaled_=" << scaled_;
    outline_changed_ = false;
  }
  webrtc::scoped_refptr<webrtc::I420BufferInterface> buffer_if;
  if (scaled_) {
    webrtc::scoped_refptr<webrtc::I420Buffer> buffer =
        webrtc::I420Buffer::Create(width_, height_);
    buffer->ScaleFrom(*frame.video_frame_buffer()->ToI420());
    if (frame.rotation() != webrtc::kVideoRotation_0) {
      buffer = webrtc::I420Buffer::Rotate(*buffer, frame.rotation());
    }
    buffer_if = buffer;
  } else {
    buffer_if = frame.video_frame_buffer()->ToI420();
  }
#if defined(USE_OPENCV_ARUCO)
  if (aruco_detector_) {
    cv::Mat bgr;
    aruco_detector_->DetectAndDraw(*buffer_if, bgr,
                                   renderer_->IsFlipVertical(),
                                   renderer_->IsFlipHorizontal());
    std::ostringstream fps_text;
    fps_text << "FPS: " << std::fixed << std::setprecision(1) << fps_;
    cv::putText(bgr, fps_text.str(), cv::Point(20, 36),
                cv::FONT_HERSHEY_SIMPLEX, 0.9, cv::Scalar(0, 255, 0), 2,
                cv::LINE_AA);
    cv::Mat bgra(buffer_if->height(), buffer_if->width(), CV_8UC4,
                 image_.get(), (scaled_ ? width_ : input_width_) * 4);
    cv::cvtColor(bgr, bgra, cv::COLOR_BGR2BGRA);
  } else {
#endif
    libyuv::ConvertFromI420(
        buffer_if->DataY(), buffer_if->StrideY(), buffer_if->DataU(),
        buffer_if->StrideU(), buffer_if->DataV(), buffer_if->StrideV(),
        image_.get(), (scaled_ ? width_ : input_width_) * 4,
        buffer_if->width(), buffer_if->height(), libyuv::FOURCC_ARGB);
#if defined(USE_OPENCV_ARUCO)
  }
#endif
}

void SDLRenderer::Sink::SetOutlineRect(int x, int y, int width, int height) {
  outline_offset_x_ = x;
  outline_offset_y_ = y;
  if (outline_width_ == width && outline_height_ == height) {
    return;
  }
  webrtc::MutexLock lock(GetMutex());
  offset_y_ = 0;
  offset_x_ = 0;
  outline_width_ = width;
  outline_height_ = height;
  outline_aspect_ = (float)outline_width_ / (float)outline_height_;
  outline_changed_ = true;
}

webrtc::Mutex* SDLRenderer::Sink::GetMutex() {
  return &frame_params_lock_;
}

bool SDLRenderer::Sink::GetOutlineChanged() {
  return !outline_changed_;
}

int SDLRenderer::Sink::GetOffsetX() {
  return outline_offset_x_ + offset_x_;
}

int SDLRenderer::Sink::GetOffsetY() {
  return outline_offset_y_ + offset_y_;
}

int SDLRenderer::Sink::GetFrameWidth() {
  return scaled_ ? width_ : input_width_;
}

int SDLRenderer::Sink::GetFrameHeight() {
  return scaled_ ? height_ : input_height_;
}

int SDLRenderer::Sink::GetWidth() {
  return width_;
}

int SDLRenderer::Sink::GetHeight() {
  return height_;
}

uint8_t* SDLRenderer::Sink::GetImage() {
  return image_.get();
}

const std::string& SDLRenderer::Sink::GetSourceName() const {
  return source_name_;
}

int SDLRenderer::Sink::GetSlotIndex() const {
  return slot_index_;
}

double SDLRenderer::Sink::GetFps() const {
  return fps_;
}

bool SDLRenderer::Sink::IsReceiving(
    std::chrono::steady_clock::time_point now) const {
  return last_frame_time_ != std::chrono::steady_clock::time_point::min() &&
         now - last_frame_time_ < std::chrono::seconds(1);
}

bool SDLRenderer::Sink::CopySourceTo(uint8_t* destination,
                                     int destination_stride,
                                     int destination_width,
                                     int destination_height,
                                     bool flip_vertical,
                                     bool flip_horizontal) const {
  if (source_image_ == nullptr || source_width_ == 0 || source_height_ == 0) {
    return false;
  }

  const bool is_same_size = source_width_ == destination_width &&
                            source_height_ == destination_height;
  for (int y = 0; y < destination_height; ++y) {
    int source_y = y * source_height_ / destination_height;
    if (flip_vertical) {
      source_y = source_height_ - 1 - source_y;
    }
    auto* destination_row =
        reinterpret_cast<uint32_t*>(destination + y * destination_stride);
    const auto* source_row = reinterpret_cast<const uint32_t*>(
        source_image_.get() + source_y * source_width_ * 4);
    if (is_same_size && !flip_horizontal) {
      std::memcpy(destination_row, source_row,
                  static_cast<size_t>(destination_width) * 4);
      continue;
    }
    for (int x = 0; x < destination_width; ++x) {
      int source_x = x * source_width_ / destination_width;
      if (flip_horizontal) {
        source_x = source_width_ - 1 - source_x;
      }
      destination_row[x] = source_row[source_x];
    }
  }
  return true;
}

void SDLRenderer::SetOutlines() {
  if (!fixed_slots_.empty()) {
    const int slot_count = fixed_slots_.size();
    const int cols = slot_count == 1 ? 1 : 2;
    const int rows = slot_count <= 2 ? 1 : 2;
    for (const VideoTrackSinkVector::value_type& sink_entry : sinks_) {
      Sink* sink = sink_entry.second.get();
      if (sink->GetSlotIndex() < 0 || sink->GetSlotIndex() >= slot_count) {
        continue;
      }
      const OutlineRect outline = GetSlotOutline(sink->GetSlotIndex());
      sink->SetOutlineRect(outline.x, outline.y, outline.width,
                           outline.height);
    }
    rows_ = rows;
    cols_ = cols;
    return;
  }

  float window_aspect = (float)width_ / (float)height_;
  bool window_is_wide = window_aspect > ((STD_ASPECT + WIDE_ASPECT) / 2.0);
  float frame_aspect = window_is_wide ? WIDE_ASPECT : STD_ASPECT;
  int rows = 1;
  int cols = 1;
  if (window_aspect >= frame_aspect) {
    int times = std::floor(window_aspect / frame_aspect);
    if (times < 1)
      times = 1;
    while (rows * cols < sinks_.size()) {
      if (times < (cols / rows)) {
        rows++;
      } else {
        cols++;
      }
    }
  } else {
    int times = std::floor(frame_aspect / window_aspect);
    if (times < 1)
      times = 1;
    while (rows * cols < sinks_.size()) {
      if (times < (rows / cols)) {
        cols++;
      } else {
        rows++;
      }
    }
  }
  RTC_LOG(LS_VERBOSE) << __FUNCTION__ << " rows:" << rows << " cols:" << cols;
  int outline_width = std::floor(width_ / cols);
  int outline_height = std::floor(height_ / rows);
  int sinks_count = sinks_.size();
  for (int i = 0; i < sinks_count; i++) {
    Sink* sink = sinks_[i].second.get();
    int offset_x = outline_width * (i % cols);
    int offset_y = outline_height * std::floor(i / cols);
    sink->SetOutlineRect(offset_x, offset_y, outline_width, outline_height);
    RTC_LOG(LS_VERBOSE) << __FUNCTION__ << " offset_x:" << offset_x
                        << " offset_y:" << offset_y
                        << " outline_width:" << outline_width
                        << " outline_height:" << outline_height;
  }
  rows_ = rows;
  cols_ = cols;
}

void SDLRenderer::AddTrack(webrtc::VideoTrackInterface* track) {
  RTC_LOG(LS_INFO) << "SDLRenderer::AddTrack: received video track";
  std::unique_ptr<Sink> sink(new Sink(this, track, enable_aruco_));
  webrtc::MutexLock lock(&sinks_lock_);
  sinks_.push_back(std::make_pair(track, std::move(sink)));
  SetOutlines();
}

void SDLRenderer::RemoveTrack(webrtc::VideoTrackInterface* track) {
  webrtc::MutexLock lock(&sinks_lock_);
  sinks_.erase(
      std::remove_if(sinks_.begin(), sinks_.end(),
                     [track](const VideoTrackSinkVector::value_type& sink) {
                       return sink.first == track;
                     }),
      sinks_.end());
  SetOutlines();
}

void SDLRenderer::ConfigureFixedSlots(
    const std::vector<std::string>& source_names) {
  webrtc::MutexLock lock(&sinks_lock_);
  fixed_slots_.clear();
  for (const std::string& source_name : source_names) {
    fixed_slots_.push_back({source_name, SourceState::kConnecting});
  }
  SetOutlines();
}

void SDLRenderer::AddTrackForSource(webrtc::VideoTrackInterface* track,
                                    const std::string& source_name) {
  webrtc::MutexLock lock(&sinks_lock_);
  const int slot_index = FindSourceSlot(source_name);
  if (slot_index < 0) {
    RTC_LOG(LS_ERROR) << "SDLRenderer: source is not configured: "
                      << source_name;
    return;
  }

  sinks_.erase(
      std::remove_if(sinks_.begin(), sinks_.end(),
                     [&source_name](const VideoTrackSinkVector::value_type&
                                        sink) {
                       return sink.second->GetSourceName() == source_name;
                     }),
      sinks_.end());
  std::unique_ptr<Sink> sink(
      new Sink(this, track, false, source_name, slot_index));
  sinks_.push_back(std::make_pair(track, std::move(sink)));
  fixed_slots_[slot_index].state = SourceState::kLive;
  SetOutlines();
}

void SDLRenderer::SetSourceState(const std::string& source_name,
                                 SourceState state) {
  webrtc::MutexLock lock(&sinks_lock_);
  const int slot_index = FindSourceSlot(source_name);
  if (slot_index >= 0) {
    fixed_slots_[slot_index].state = state;
  }
}

void SDLRenderer::SetOverlayText(std::string text) {
  webrtc::MutexLock lock(&overlay_lock_);
  overlay_text_ = std::move(text);
}

double SDLRenderer::GetPrimaryFps() {
  webrtc::MutexLock lock(&sinks_lock_);
  if (sinks_.empty()) {
    return 0.0;
  }
  Sink* sink = sinks_.front().second.get();
  webrtc::MutexLock frame_lock(sink->GetMutex());
  return sink->GetFps();
}

bool SDLRenderer::IsFlipVertical() const {
  return flip_vertical_.load();
}

bool SDLRenderer::IsFlipHorizontal() const {
  return flip_horizontal_.load();
}

void SDLRenderer::ConfigureSourceFlips(
    const std::vector<std::string>& source_flips) {
  webrtc::MutexLock lock(&source_flips_lock_);
  source_flips_.clear();
  for (const std::string& source_flip : source_flips) {
    const size_t separator = source_flip.find('=');
    const std::string name = source_flip.substr(0, separator);
    const std::string mode = source_flip.substr(separator + 1);
    source_flips_[name] = {mode.find('V') != std::string::npos,
                           mode.find('H') != std::string::npos};
  }
}

void SDLRenderer::GetEffectiveFlip(const std::string& source_name,
                                   bool* flip_vertical,
                                   bool* flip_horizontal) const {
  *flip_vertical = flip_vertical_.load();
  *flip_horizontal = flip_horizontal_.load();
  webrtc::MutexLock lock(&source_flips_lock_);
  const auto source_flip = source_flips_.find(source_name);
  if (source_flip == source_flips_.end()) {
    return;
  }
  *flip_vertical = source_flip->second.first;
  *flip_horizontal = source_flip->second.second;
}

SDLRenderer::OutlineRect SDLRenderer::GetSlotOutline(int slot_index) const {
  const int slot_count = fixed_slots_.size();
  const int cols = slot_count == 1 ? 1 : 2;
  const int rows = slot_count <= 2 ? 1 : 2;
  const int outline_width = width_ / cols;
  const int outline_height = height_ / rows;
  return {
      outline_width * (slot_index % cols),
      outline_height * (slot_index / cols),
      outline_width,
      outline_height,
  };
}

int SDLRenderer::FindSourceSlot(const std::string& source_name) const {
  for (int i = 0; i < fixed_slots_.size(); ++i) {
    if (fixed_slots_[i].name == source_name) {
      return i;
    }
  }
  return -1;
}

SDLRenderer::Sink* SDLRenderer::FindSinkForSource(
    const std::string& source_name) {
  for (const VideoTrackSinkVector::value_type& sink_entry : sinks_) {
    if (sink_entry.second->GetSourceName() == source_name) {
      return sink_entry.second.get();
    }
  }
  return nullptr;
}

void SDLRenderer::WriteSharedFrame() {
  if (shared_frame_writer_ == nullptr || shared_frame_buffer_.empty()) {
    return;
  }

  // BGRA: 緑の余白は Unity 側で 960x528 の有効領域を切り出す目印にする。
  auto* pixels = reinterpret_cast<uint32_t*>(shared_frame_buffer_.data());
  std::fill(pixels, pixels + kSharedFrameWidth * kSharedFrameHeight,
            0xff00ff00U);

  const int slot_count = std::min(static_cast<int>(fixed_slots_.size()), 4);
  for (int slot_index = 0; slot_index < slot_count; ++slot_index) {
    Sink* sink = FindSinkForSource(fixed_slots_[slot_index].name);
    if (sink == nullptr) {
      continue;
    }
    webrtc::MutexLock frame_lock(sink->GetMutex());
    const int slot_x = (slot_index % 2) * kSharedSlotWidth;
    const int slot_y = (slot_index / 2) * kSharedSlotHeight +
                       kSharedSourceVerticalOffset;
    bool flip_vertical = flip_vertical_.load();
    bool flip_horizontal = flip_horizontal_.load();
    GetEffectiveFlip(fixed_slots_[slot_index].name, &flip_vertical,
                     &flip_horizontal);
    sink->CopySourceTo(
        shared_frame_buffer_.data() + slot_y * kSharedFrameStride + slot_x * 4,
        kSharedFrameStride, kSharedSlotWidth, kSharedSourceHeight,
        flip_vertical, flip_horizontal);
  }

  const auto now = std::chrono::system_clock::now().time_since_epoch();
  const int64_t timestamp_ns =
      std::chrono::duration_cast<std::chrono::nanoseconds>(now).count();
  shared_frame_writer_->Write(shared_frame_buffer_.data(),
                              shared_frame_buffer_.size(), timestamp_ns);
}

void SDLRenderer::RenderSourceOverlay() {
  const auto now = std::chrono::steady_clock::now();
  const bool blink_on =
      (std::chrono::duration_cast<std::chrono::milliseconds>(
           now.time_since_epoch())
           .count() /
       500) %
          2 ==
      0;
  for (int i = 0; i < fixed_slots_.size(); ++i) {
    const SourceSlot& slot = fixed_slots_[i];
    const OutlineRect outline = GetSlotOutline(i);
    Sink* sink = FindSinkForSource(slot.name);
    double fps = 0.0;
    bool receiving = false;
    if (sink != nullptr) {
      webrtc::MutexLock frame_lock(sink->GetMutex());
      fps = sink->GetFps();
      receiving = sink->IsReceiving(now);
    }

    const char* state = "OFFLINE";
    Uint8 red = 220;
    Uint8 green = 64;
    if (slot.state == SourceState::kConnecting) {
      state = "CONNECTING";
      red = 240;
      green = 180;
    } else if (slot.state == SourceState::kLive) {
      state = "LIVE";
      red = 64;
      green = 220;
    }

    SDL_FRect background = {
        static_cast<float>(outline.x), static_cast<float>(outline.y),
        224.0f, 20.0f};
    SDL_SetRenderDrawColor(renderer_, 0, 0, 0, 220);
    SDL_RenderFillRect(renderer_, &background);
    const bool indicator_on = receiving && blink_on;
    SDL_SetRenderDrawColor(renderer_, indicator_on ? 64 : 72,
                           indicator_on ? 255 : 72,
                           indicator_on ? 96 : 72, 255);
    SDL_FRect indicator = {static_cast<float>(outline.x + 5),
                           static_cast<float>(outline.y + 6), 8.0f, 8.0f};
    SDL_RenderFillRect(renderer_, &indicator);
    SDL_SetRenderDrawColor(renderer_, red, green, 64, 255);
    SDL_RenderDebugTextFormat(renderer_, static_cast<float>(outline.x + 18),
                              static_cast<float>(outline.y + 6),
                              "%s %s FPS %.1f", slot.name.c_str(), state,
                              fps);
  }

  std::string overlay;
  {
    webrtc::MutexLock lock(&overlay_lock_);
    overlay = overlay_text_;
  }
  if (!overlay.empty()) {
    std::vector<std::string> lines;
    size_t begin = 0;
    while (begin <= overlay.size()) {
      const size_t end = overlay.find('\n', begin);
      lines.push_back(overlay.substr(begin, end - begin));
      if (end == std::string::npos) {
        break;
      }
      begin = end + 1;
    }
    size_t longest = 0;
    for (const std::string& line : lines) {
      longest = std::max(longest, line.size());
    }
    SDL_FRect background = {0.0f, 0.0f,
                            static_cast<float>(longest * 8 + 12),
                            static_cast<float>(lines.size() * 13 + 10)};
    SDL_SetRenderDrawColor(renderer_, 0, 0, 0, 220);
    SDL_RenderFillRect(renderer_, &background);
    SDL_SetRenderDrawColor(renderer_, 255, 220, 64, 255);
    for (int i = 0; i < lines.size(); ++i) {
      SDL_RenderDebugText(renderer_, 6.0f, static_cast<float>(7 + i * 13),
                          lines[i].c_str());
    }
  }
}
