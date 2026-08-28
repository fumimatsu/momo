#ifndef IMU_FUSION_TELEMETRY_DECODER_H_
#define IMU_FUSION_TELEMETRY_DECODER_H_

#include <cstdint>
#include <string>
#include <vector>

namespace momo::serial_data_channel {

bool DecodeImuFusionMotionPayload(const std::vector<uint8_t>& payload,
                                  std::string* line);

}  // namespace momo::serial_data_channel

#endif
