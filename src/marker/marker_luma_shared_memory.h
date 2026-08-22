#ifndef MARKER_LUMA_SHARED_MEMORY_H_
#define MARKER_LUMA_SHARED_MEMORY_H_

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

struct MarkerLumaSourceConfig {
  std::string source_id;
  bool flip_vertical = false;
  bool flip_horizontal = false;
};

class MarkerLumaSharedMemoryWriter {
 public:
  static constexpr uint32_t kMaximumSources = 32;
  static constexpr uint32_t kWidth = 960;
  static constexpr uint32_t kHeight = 528;
  static constexpr uint32_t kStride = kWidth;
  static constexpr uint32_t kPixelFormatY800 = 0x30303859;

  explicit MarkerLumaSharedMemoryWriter(std::string mapping_name);
  ~MarkerLumaSharedMemoryWriter();

  MarkerLumaSharedMemoryWriter(const MarkerLumaSharedMemoryWriter&) = delete;
  MarkerLumaSharedMemoryWriter& operator=(const MarkerLumaSharedMemoryWriter&) =
      delete;

  bool IsOpen() const;
  uint64_t ConfigureSources(const std::vector<MarkerLumaSourceConfig>& sources,
                            const std::string& manifest_revision);
  void SetRacePhase(const std::string& phase);
  void SetConnected(size_t slot, uint64_t generation, bool connected);
  void InvalidateVideo(size_t slot, uint64_t generation);
  void WriteFrame(size_t slot,
                  uint64_t generation,
                  const uint8_t* source_y,
                  int source_stride,
                  int source_width,
                  int source_height,
                  int64_t received_unix_ns);

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

#endif  // MARKER_LUMA_SHARED_MEMORY_H_
