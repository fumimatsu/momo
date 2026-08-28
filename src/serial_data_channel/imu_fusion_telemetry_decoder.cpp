#include "imu_fusion_telemetry_decoder.h"

#include <cstdio>

namespace momo::serial_data_channel {
namespace {

constexpr uint8_t kTelemetryBinaryVersion = 3;
constexpr uint8_t kTelemetryBinaryImuFusionState = 5;
constexpr uint8_t kMotionStatusFluAxes = 1u << 0;
constexpr uint8_t kMotionStatusAttitudeValid = 1u << 1;
constexpr uint8_t kMotionStatusLinearAccelerationValid = 1u << 2;
constexpr uint8_t kMotionStatusMask = kMotionStatusFluAxes |
                                      kMotionStatusAttitudeValid |
                                      kMotionStatusLinearAccelerationValid;
constexpr uint8_t kCapabilityFuelCommandV1 = 1u << 0;
constexpr uint8_t kCapabilityVehicleColorCommandV1 = 1u << 1;
constexpr uint8_t kCapabilityImuFusionV1 = 1u << 2;
constexpr uint8_t kCapabilityMask = kCapabilityFuelCommandV1 |
                                    kCapabilityVehicleColorCommandV1 |
                                    kCapabilityImuFusionV1;
constexpr uint8_t kCalibrationIdle = 0;
constexpr uint8_t kCalibrationReady = 2;
constexpr uint8_t kCalibrationFailed = 3;
constexpr uint8_t kCalibrationFailureNone = 0;
constexpr uint8_t kCalibrationFailureMaximum = 4;

uint16_t ReadU16(const std::vector<uint8_t>& data, size_t offset) {
  return static_cast<uint16_t>(data[offset]) |
         (static_cast<uint16_t>(data[offset + 1]) << 8);
}

uint32_t ReadU32(const std::vector<uint8_t>& data, size_t offset) {
  uint32_t value = 0;
  for (uint8_t shift = 0; shift < 32; shift += 8) {
    value |= static_cast<uint32_t>(data[offset++]) << shift;
  }
  return value;
}

uint64_t ReadU64(const std::vector<uint8_t>& data, size_t offset) {
  uint64_t value = 0;
  for (uint8_t shift = 0; shift < 64; shift += 8) {
    value |= static_cast<uint64_t>(data[offset++]) << shift;
  }
  return value;
}

int16_t ReadI16(const std::vector<uint8_t>& data, size_t offset) {
  return static_cast<int16_t>(ReadU16(data, offset));
}

}  // namespace

bool DecodeImuFusionMotionPayload(const std::vector<uint8_t>& payload,
                                  std::string* line) {
  if (line == nullptr || payload.size() != 48 ||
      payload[0] != kTelemetryBinaryVersion ||
      payload[1] != kTelemetryBinaryImuFusionState) {
    return false;
  }

  const uint8_t status = payload[2];
  const uint8_t capabilities = payload[3];
  const uint8_t calibration_state = payload[38];
  const uint8_t calibration_failure = payload[39];
  const bool attitude_valid =
      (status & kMotionStatusAttitudeValid) != 0;
  const bool linear_valid =
      (status & kMotionStatusLinearAccelerationValid) != 0;
  if ((status & ~kMotionStatusMask) != 0 ||
      (status & kMotionStatusFluAxes) == 0 ||
      (capabilities & ~kCapabilityMask) != 0 ||
      (capabilities & kCapabilityImuFusionV1) == 0 ||
      calibration_state > kCalibrationFailed ||
      calibration_failure > kCalibrationFailureMaximum ||
      (calibration_state == kCalibrationIdle) !=
          (ReadU32(payload, 40) == 0) ||
      (calibration_state == kCalibrationFailed) ==
          (calibration_failure == kCalibrationFailureNone) ||
      (linear_valid && (!attitude_valid ||
                        calibration_state != kCalibrationReady)) ||
      ReadU16(payload, 44) != 0) {
    return false;
  }
  if (!linear_valid &&
      (ReadI16(payload, 26) != 0 || ReadI16(payload, 28) != 0 ||
       ReadI16(payload, 30) != 0)) {
    return false;
  }

  const uint32_t boot = ReadU32(payload, 4);
  const uint32_t sequence = ReadU32(payload, 8);
  const uint64_t timestamp_us = ReadU64(payload, 12);
  const float forward = ReadI16(payload, 20) * 0.01f;
  const float lateral = ReadI16(payload, 22) * 0.01f;
  const float vertical = ReadI16(payload, 24) * 0.01f;
  const float fused_forward = ReadI16(payload, 26) * 0.01f;
  const float fused_lateral = ReadI16(payload, 28) * 0.01f;
  const float fused_vertical = ReadI16(payload, 30) * 0.01f;
  const float yaw = ReadI16(payload, 32) * 0.01f;
  const uint32_t period_us = ReadU32(payload, 34);
  const uint32_t calibration_id = ReadU32(payload, 40);

  std::string capability_flags = "\"flu_axes\"";
  if ((capabilities & kCapabilityFuelCommandV1) != 0) {
    capability_flags += ",\"fuel_command_v1\"";
  }
  if ((capabilities & kCapabilityVehicleColorCommandV1) != 0) {
    capability_flags += ",\"vehicle_color_command_v1\"";
  }
  capability_flags += ",\"imu_fusion_v1\"";

  char text[384];
  const int written = linear_valid
      ? std::snprintf(
            text, sizeof(text),
            "TEL:{\"v\":2,\"k\":\"s\",\"src\":\"imu0\",\"boot\":\"%08x\",\"seq\":%u,\"t_us\":%llu,\"m\":{\"a\":[%.2f,%.2f,%.2f],\"l\":[%.2f,%.2f,%.2f],\"y\":%.2f},\"q\":{\"p\":%u,\"c\":[%u,%u,%u],\"f\":[%s]}}",
            boot, sequence, static_cast<unsigned long long>(timestamp_us),
            forward, lateral, vertical, fused_forward, fused_lateral,
            fused_vertical, yaw, period_us, calibration_state,
            calibration_id, calibration_failure, capability_flags.c_str())
      : std::snprintf(
            text, sizeof(text),
            "TEL:{\"v\":2,\"k\":\"s\",\"src\":\"imu0\",\"boot\":\"%08x\",\"seq\":%u,\"t_us\":%llu,\"m\":{\"a\":[%.2f,%.2f,%.2f],\"y\":%.2f},\"q\":{\"p\":%u,\"c\":[%u,%u,%u],\"f\":[%s]}}",
            boot, sequence, static_cast<unsigned long long>(timestamp_us),
            forward, lateral, vertical, yaw, period_us, calibration_state,
            calibration_id, calibration_failure, capability_flags.c_str());
  if (written <= 0 || static_cast<size_t>(written) >= sizeof(text)) {
    return false;
  }
  *line = text;
  return true;
}

}  // namespace momo::serial_data_channel
