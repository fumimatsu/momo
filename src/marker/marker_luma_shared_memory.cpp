#include "marker/marker_luma_shared_memory.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cstring>
#include <mutex>
#include <shared_mutex>
#include <utility>

#if defined(_WIN32)
#include <windows.h>
#endif

#include <rtc_base/logging.h>
#include <third_party/libyuv/include/libyuv/planar_functions.h>
#include <third_party/libyuv/include/libyuv/scale.h>

namespace {

constexpr uint32_t kMagic = 0x32594c4d;  // "MLY2"
constexpr uint16_t kVersion = 2;
constexpr size_t kHeaderSize = 128;
constexpr size_t kSourceIdSize = 32;
constexpr size_t kMetadataSize = 64;
constexpr size_t kPlaneSize =
    static_cast<size_t>(MarkerLumaSharedMemoryWriter::kStride) *
    MarkerLumaSharedMemoryWriter::kHeight;
constexpr size_t kSourceTableSize =
    MarkerLumaSharedMemoryWriter::kMaximumSources * kSourceIdSize;
constexpr size_t kAllMetadataSize =
    MarkerLumaSharedMemoryWriter::kMaximumSources * kMetadataSize;
constexpr size_t kPlaneOffset =
    kHeaderSize + kSourceTableSize + kAllMetadataSize;
constexpr size_t kMappingSize =
    kPlaneOffset + MarkerLumaSharedMemoryWriter::kMaximumSources * kPlaneSize;

constexpr uint32_t kFlagConnected = 1 << 0;
constexpr uint32_t kFlagVideoValid = 1 << 1;
constexpr uint32_t kFlagScaled = 1 << 2;
constexpr uint32_t kFlagFlipVertical = 1 << 3;
constexpr uint32_t kFlagFlipHorizontal = 1 << 4;

uint32_t EncodeRacePhase(const std::string& phase) {
  if (phase == "ready" || phase == "prepared") {
    return 1;
  }
  if (phase == "countdown") {
    return 2;
  }
  if (phase == "green") {
    return 3;
  }
  if (phase == "finished") {
    return 4;
  }
  if (phase == "aborted") {
    return 5;
  }
  return 0;
}

#if defined(_WIN32)

struct MarkerLumaHeader {
  uint32_t magic;
  uint16_t version;
  uint16_t header_size;
  uint32_t mapping_size;
  uint32_t max_sources;
  uint32_t active_sources;
  uint32_t width;
  uint32_t height;
  uint32_t stride;
  uint32_t pixel_format;
  uint32_t source_id_size;
  uint32_t metadata_size;
  uint32_t plane_size;
  uint32_t flags;
  uint32_t reserved0;
  volatile LONG64 receiver_generation;
  volatile LONG64 qpc_frequency;
  volatile LONG64 topology_guard;
  uint64_t manifest_revision_hash;
  int64_t created_unix_ns;
  uint8_t reserved_tail[32];
};
static_assert(sizeof(MarkerLumaHeader) == kHeaderSize);

struct MarkerLumaSourceMetadata {
  volatile LONG64 write_guard;
  uint64_t source_sequence;
  int64_t received_qpc;
  int64_t received_unix_ns;
  uint64_t frame_count;
  uint64_t replaced_frame_count;
  uint32_t flags;
  uint32_t width;
  uint32_t height;
  uint32_t stride;
};
static_assert(sizeof(MarkerLumaSourceMetadata) == kMetadataSize);

std::wstring Utf8ToWide(const std::string& value) {
  if (value.empty()) {
    return std::wstring();
  }
  const int required = MultiByteToWideChar(
      CP_UTF8, 0, value.data(), static_cast<int>(value.size()), nullptr, 0);
  if (required <= 0) {
    return std::wstring();
  }
  std::wstring result(required, L'\0');
  MultiByteToWideChar(CP_UTF8, 0, value.data(), static_cast<int>(value.size()),
                      result.data(), required);
  return result;
}

uint64_t HashManifestRevision(const std::string& value) {
  // FNV-1a is diagnostic only; source IDs and generation define topology.
  uint64_t hash = 1469598103934665603ULL;
  for (const unsigned char character : value) {
    hash ^= character;
    hash *= 1099511628211ULL;
  }
  return hash;
}

#endif

int64_t UnixNowNs() {
  return std::chrono::duration_cast<std::chrono::nanoseconds>(
             std::chrono::system_clock::now().time_since_epoch())
      .count();
}

}  // namespace

class MarkerLumaSharedMemoryWriter::Impl {
 public:
  explicit Impl(std::string mapping_name)
      : mapping_name_(std::move(mapping_name)) {
#if defined(_WIN32)
    const std::wstring wide_name = Utf8ToWide(mapping_name_);
    if (wide_name.empty()) {
      RTC_LOG(LS_ERROR) << "MLY2 mapping name is invalid";
      return;
    }
    const std::wstring mutex_name = wide_name + L"-Producer";
    producer_mutex_ = CreateMutexW(nullptr, FALSE, mutex_name.c_str());
    if (producer_mutex_ == nullptr) {
      RTC_LOG(LS_ERROR) << "CreateMutexW for MLY2 failed: " << GetLastError();
      return;
    }
    if (GetLastError() == ERROR_ALREADY_EXISTS) {
      RTC_LOG(LS_ERROR) << "MLY2 already has another writer: " << mapping_name_;
      CloseHandle(producer_mutex_);
      producer_mutex_ = nullptr;
      return;
    }

    mapping_ = CreateFileMappingW(INVALID_HANDLE_VALUE, nullptr, PAGE_READWRITE,
                                  static_cast<DWORD>(kMappingSize >> 32),
                                  static_cast<DWORD>(kMappingSize & 0xffffffff),
                                  wide_name.c_str());
    if (mapping_ == nullptr) {
      RTC_LOG(LS_ERROR) << "CreateFileMappingW for MLY2 failed: "
                        << GetLastError();
      CloseHandle(producer_mutex_);
      producer_mutex_ = nullptr;
      return;
    }
    view_ = static_cast<uint8_t*>(
        MapViewOfFile(mapping_, FILE_MAP_ALL_ACCESS, 0, 0, kMappingSize));
    if (view_ == nullptr) {
      RTC_LOG(LS_ERROR) << "MapViewOfFile for MLY2 failed: " << GetLastError();
      CloseHandle(mapping_);
      mapping_ = nullptr;
      CloseHandle(producer_mutex_);
      producer_mutex_ = nullptr;
      return;
    }
    const std::wstring frame_ready_name = wide_name + L"-FrameReady";
    frame_ready_event_ =
        CreateEventW(nullptr, FALSE, FALSE, frame_ready_name.c_str());
    if (frame_ready_event_ == nullptr) {
      RTC_LOG(LS_WARNING) << "CreateEventW for MLY2 frame notification failed: "
                          << GetLastError();
    }

    std::memset(view_, 0, kMappingSize);
    auto* header = Header();
    header->magic = kMagic;
    header->version = kVersion;
    header->header_size = static_cast<uint16_t>(kHeaderSize);
    header->mapping_size = static_cast<uint32_t>(kMappingSize);
    header->max_sources = kMaximumSources;
    header->width = kWidth;
    header->height = kHeight;
    header->stride = kStride;
    header->pixel_format = kPixelFormatY800;
    header->source_id_size = static_cast<uint32_t>(kSourceIdSize);
    header->metadata_size = static_cast<uint32_t>(kMetadataSize);
    header->plane_size = static_cast<uint32_t>(kPlaneSize);
    LARGE_INTEGER frequency{};
    QueryPerformanceFrequency(&frequency);
    header->qpc_frequency = frequency.QuadPart;
    header->created_unix_ns = UnixNowNs();
    RTC_LOG(LS_INFO) << "MLY2 writer opened: " << mapping_name_ << " " << kWidth
                     << "x" << kHeight << " x " << kMaximumSources;
#else
    RTC_LOG(LS_WARNING) << "MLY2 is only supported on Windows";
#endif
  }

  ~Impl() {
#if defined(_WIN32)
    if (view_ != nullptr) {
      UnmapViewOfFile(view_);
    }
    if (mapping_ != nullptr) {
      CloseHandle(mapping_);
    }
    if (producer_mutex_ != nullptr) {
      CloseHandle(producer_mutex_);
    }
    if (frame_ready_event_ != nullptr) {
      CloseHandle(frame_ready_event_);
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

  uint64_t ConfigureSources(const std::vector<MarkerLumaSourceConfig>& sources,
                            const std::string& manifest_revision) {
#if defined(_WIN32)
    if (view_ == nullptr || sources.size() > kMaximumSources) {
      return 0;
    }
    std::unique_lock<std::shared_mutex> topology_lock(topology_mutex_);
    auto* header = Header();
    InterlockedIncrement64(&header->topology_guard);
    MemoryBarrier();

    std::memset(SourceTable(), 0, kSourceTableSize);
    std::memset(Metadata(0), 0, kAllMetadataSize);
    std::memset(Plane(0), 0, kMaximumSources * kPlaneSize);
    for (size_t index = 0; index < sources.size(); ++index) {
      const size_t copy_size =
          std::min(sources[index].source_id.size(), kSourceIdSize - 1);
      std::memcpy(SourceTable() + index * kSourceIdSize,
                  sources[index].source_id.data(), copy_size);
    }
    sources_ = sources;
    active_sources_.store(static_cast<uint32_t>(sources.size()),
                          std::memory_order_release);
    const uint64_t generation = generation_.fetch_add(1) + 1;
    header->active_sources = static_cast<uint32_t>(sources.size());
    header->manifest_revision_hash = HashManifestRevision(manifest_revision);
    InterlockedExchange64(&header->receiver_generation,
                          static_cast<LONG64>(generation));
    MemoryBarrier();
    InterlockedIncrement64(&header->topology_guard);
    RTC_LOG(LS_INFO) << "MLY2 topology generation " << generation << ": "
                     << sources.size() << " sources, revision "
                     << manifest_revision;
    return generation;
#else
    static_cast<void>(sources);
    static_cast<void>(manifest_revision);
    return 0;
#endif
  }

  void SetConnected(size_t slot, uint64_t generation, bool connected) {
#if defined(_WIN32)
    UpdateMetadata(slot, generation, [connected](auto* metadata) {
      if (connected) {
        metadata->flags |= kFlagConnected;
      } else {
        metadata->flags &= ~(kFlagConnected | kFlagVideoValid);
      }
    });
#else
    static_cast<void>(slot);
    static_cast<void>(generation);
    static_cast<void>(connected);
#endif
  }

  void SetRacePhase(const std::string& phase) {
#if defined(_WIN32)
    if (view_ != nullptr) {
      InterlockedExchange(reinterpret_cast<volatile LONG*>(&Header()->flags),
                          static_cast<LONG>(EncodeRacePhase(phase)));
    }
#else
    static_cast<void>(phase);
#endif
  }

  void InvalidateVideo(size_t slot, uint64_t generation) {
#if defined(_WIN32)
    UpdateMetadata(slot, generation,
                   [](auto* metadata) { metadata->flags &= ~kFlagVideoValid; });
#else
    static_cast<void>(slot);
    static_cast<void>(generation);
#endif
  }

  void WriteFrame(size_t slot,
                  uint64_t generation,
                  const uint8_t* source_y,
                  int source_stride,
                  int source_width,
                  int source_height,
                  int64_t received_unix_ns) {
#if defined(_WIN32)
    if (view_ == nullptr || source_y == nullptr || source_width <= 0 ||
        source_height <= 0 || source_stride == 0) {
      return;
    }
    std::shared_lock<std::shared_mutex> topology_lock(topology_mutex_);
    if (!IsCurrentSlot(slot, generation)) {
      return;
    }
    std::lock_guard<std::mutex> slot_lock(slot_mutexes_[slot]);
    if (!IsCurrentSlot(slot, generation)) {
      return;
    }

    auto* metadata = Metadata(slot);
    InterlockedIncrement64(&metadata->write_guard);
    MemoryBarrier();

    const MarkerLumaSourceConfig& source_config = sources_[slot];
    uint8_t* destination = Plane(slot);
    uint8_t* scale_destination = destination;
    int scale_stride = kStride;
    if (source_config.flip_horizontal) {
      auto& scratch = flip_scratch_[slot];
      if (scratch.size() != kPlaneSize) {
        scratch.resize(kPlaneSize);
      }
      scale_destination = scratch.data();
    }
    if (source_config.flip_vertical) {
      scale_destination += (kHeight - 1) * kStride;
      scale_stride = -kStride;
    }
    if (source_width == kWidth && source_height == kHeight &&
        !source_config.flip_horizontal && !source_config.flip_vertical) {
      libyuv::CopyPlane(source_y, source_stride, destination, kStride, kWidth,
                        kHeight);
    } else {
      libyuv::ScalePlane(source_y, source_stride, source_width, source_height,
                         scale_destination, scale_stride, kWidth, kHeight,
                         libyuv::kFilterNone);
    }
    if (source_config.flip_horizontal) {
      const auto& scratch = flip_scratch_[slot];
      libyuv::MirrorPlane(scratch.data(), kStride, destination, kStride, kWidth,
                          kHeight);
    }

    LARGE_INTEGER received_qpc{};
    QueryPerformanceCounter(&received_qpc);
    const uint64_t next_sequence = metadata->source_sequence + 1;
    metadata->source_sequence = next_sequence;
    metadata->received_qpc = received_qpc.QuadPart;
    metadata->received_unix_ns = received_unix_ns;
    metadata->frame_count += 1;
    if (next_sequence > 1) {
      metadata->replaced_frame_count += 1;
    }
    metadata->flags = kFlagConnected | kFlagVideoValid | kFlagScaled |
                      (source_config.flip_vertical ? kFlagFlipVertical : 0) |
                      (source_config.flip_horizontal ? kFlagFlipHorizontal : 0);
    metadata->width = kWidth;
    metadata->height = kHeight;
    metadata->stride = kStride;
    MemoryBarrier();
    InterlockedIncrement64(&metadata->write_guard);
    if (frame_ready_event_ != nullptr) {
      SetEvent(frame_ready_event_);
    }
#else
    static_cast<void>(slot);
    static_cast<void>(generation);
    static_cast<void>(source_y);
    static_cast<void>(source_stride);
    static_cast<void>(source_width);
    static_cast<void>(source_height);
    static_cast<void>(received_unix_ns);
#endif
  }

 private:
#if defined(_WIN32)
  template <typename F>
  void UpdateMetadata(size_t slot, uint64_t generation, F update) {
    std::shared_lock<std::shared_mutex> topology_lock(topology_mutex_);
    if (!IsCurrentSlot(slot, generation)) {
      return;
    }
    std::lock_guard<std::mutex> slot_lock(slot_mutexes_[slot]);
    if (!IsCurrentSlot(slot, generation)) {
      return;
    }
    auto* metadata = Metadata(slot);
    InterlockedIncrement64(&metadata->write_guard);
    MemoryBarrier();
    update(metadata);
    MemoryBarrier();
    InterlockedIncrement64(&metadata->write_guard);
  }

  bool IsCurrentSlot(size_t slot, uint64_t generation) const {
    return slot < active_sources_.load(std::memory_order_acquire) &&
           generation != 0 &&
           generation == generation_.load(std::memory_order_acquire);
  }

  MarkerLumaHeader* Header() const {
    return reinterpret_cast<MarkerLumaHeader*>(view_);
  }
  uint8_t* SourceTable() const { return view_ + kHeaderSize; }
  MarkerLumaSourceMetadata* Metadata(size_t slot) const {
    return reinterpret_cast<MarkerLumaSourceMetadata*>(
        view_ + kHeaderSize + kSourceTableSize + slot * kMetadataSize);
  }
  uint8_t* Plane(size_t slot) const {
    return view_ + kPlaneOffset + slot * kPlaneSize;
  }

  HANDLE mapping_ = nullptr;
  HANDLE producer_mutex_ = nullptr;
  HANDLE frame_ready_event_ = nullptr;
  uint8_t* view_ = nullptr;
  std::shared_mutex topology_mutex_;
  std::array<std::mutex, kMaximumSources> slot_mutexes_;
  std::array<std::vector<uint8_t>, kMaximumSources> flip_scratch_;
  std::atomic<uint64_t> generation_{0};
  std::atomic<uint32_t> active_sources_{0};
  std::vector<MarkerLumaSourceConfig> sources_;
#endif
  std::string mapping_name_;
};

MarkerLumaSharedMemoryWriter::MarkerLumaSharedMemoryWriter(
    std::string mapping_name)
    : impl_(std::make_unique<Impl>(std::move(mapping_name))) {}

MarkerLumaSharedMemoryWriter::~MarkerLumaSharedMemoryWriter() = default;

bool MarkerLumaSharedMemoryWriter::IsOpen() const {
  return impl_->IsOpen();
}

uint64_t MarkerLumaSharedMemoryWriter::ConfigureSources(
    const std::vector<MarkerLumaSourceConfig>& sources,
    const std::string& manifest_revision) {
  return impl_->ConfigureSources(sources, manifest_revision);
}

void MarkerLumaSharedMemoryWriter::SetRacePhase(const std::string& phase) {
  impl_->SetRacePhase(phase);
}

void MarkerLumaSharedMemoryWriter::SetConnected(size_t slot,
                                                uint64_t generation,
                                                bool connected) {
  impl_->SetConnected(slot, generation, connected);
}

void MarkerLumaSharedMemoryWriter::InvalidateVideo(size_t slot,
                                                   uint64_t generation) {
  impl_->InvalidateVideo(slot, generation);
}

void MarkerLumaSharedMemoryWriter::WriteFrame(size_t slot,
                                              uint64_t generation,
                                              const uint8_t* source_y,
                                              int source_stride,
                                              int source_width,
                                              int source_height,
                                              int64_t received_unix_ns) {
  impl_->WriteFrame(slot, generation, source_y, source_stride, source_width,
                    source_height, received_unix_ns);
}
