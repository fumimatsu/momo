#include "serial_data_channel/imu_fusion_telemetry_decoder.h"

#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

namespace {

void Fail(const std::string& message) {
  std::cerr << "FAILED: " << message << '\n';
  std::exit(1);
}

void Expect(bool condition, const std::string& message) {
  if (!condition) Fail(message);
}

void WriteU16(std::vector<uint8_t>* data, size_t offset, uint16_t value) {
  (*data)[offset] = static_cast<uint8_t>(value & 0xff);
  (*data)[offset + 1] = static_cast<uint8_t>(value >> 8);
}

void WriteU32(std::vector<uint8_t>* data, size_t offset, uint32_t value) {
  for (uint8_t shift = 0; shift < 32; shift += 8) {
    (*data)[offset++] = static_cast<uint8_t>(value >> shift);
  }
}

void WriteU64(std::vector<uint8_t>* data, size_t offset, uint64_t value) {
  for (uint8_t shift = 0; shift < 64; shift += 8) {
    (*data)[offset++] = static_cast<uint8_t>(value >> shift);
  }
}

std::vector<uint8_t> ReadyPayload() {
  std::vector<uint8_t> payload(48, 0);
  payload[0] = 3;
  payload[1] = 5;
  payload[2] = 0x07;
  payload[3] = 0x07;
  WriteU32(&payload, 4, 0x7f3a21c4);
  WriteU32(&payload, 8, 11);
  WriteU64(&payload, 12, 1050000);
  WriteU16(&payload, 20, 120);
  WriteU16(&payload, 22, static_cast<uint16_t>(-230));
  WriteU16(&payload, 24, 45);
  WriteU16(&payload, 26, 110);
  WriteU16(&payload, 28, static_cast<uint16_t>(-220));
  WriteU16(&payload, 30, 5);
  WriteU16(&payload, 32, 90);
  WriteU32(&payload, 34, 33333);
  payload[38] = 2;
  payload[39] = 0;
  WriteU32(&payload, 40, 42);
  return payload;
}

void TestReadyPayload() {
  std::string line;
  const auto payload = ReadyPayload();
  Expect(momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                  &line),
         "ready payload should decode");
  Expect(line.find("\"l\":[1.10,-2.20,0.05]") != std::string::npos,
         "ready payload should expose fused acceleration");
  Expect(line.find("\"c\":[2,42,0]") != std::string::npos,
         "ready payload should expose calibration acknowledgement");
  Expect(line.find("\"imu_fusion_v1\"") != std::string::npos,
         "ready payload should advertise fusion capability");
  Expect(line.size() <= 384, "normalized message must fit Viewer limit");
}

void TestCalibratingOmitsFusedValues() {
  auto payload = ReadyPayload();
  payload[2] = 0x03;
  payload[38] = 1;
  WriteU16(&payload, 26, 0);
  WriteU16(&payload, 28, 0);
  WriteU16(&payload, 30, 0);
  std::string line;
  Expect(momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                  &line),
         "calibrating payload should decode");
  Expect(line.find("\"l\"") == std::string::npos,
         "invalid fused acceleration must be omitted");
  Expect(line.find("\"c\":[1,42,0]") != std::string::npos,
         "calibrating state should remain observable");
}

void TestStrictRejections() {
  std::string line;
  auto payload = ReadyPayload();
  payload[44] = 1;
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "reserved bytes must be zero");

  payload = ReadyPayload();
  payload[38] = 4;
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "unknown calibration state must fail");

  payload = ReadyPayload();
  payload[38] = 3;
  payload[39] = 0;
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "failed state requires a failure reason");

  payload = ReadyPayload();
  payload[2] = 0x03;
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "hidden fused values must be zero");

  payload = ReadyPayload();
  payload[3] &= ~0x04;
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "fusion capability is required for type 5");

  payload = ReadyPayload();
  payload.pop_back();
  Expect(!momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                   &line),
         "payload length must be exact");
}

void TestLongRunningPayloadFitsNormalizedLimit() {
  auto payload = ReadyPayload();
  WriteU32(&payload, 8, 0xffffffffu);
  WriteU64(&payload, 12, 9007199254740991ull);
  for (size_t offset : {20u, 22u, 24u, 26u, 28u, 30u}) {
    WriteU16(&payload, offset, static_cast<uint16_t>(-32768));
  }
  WriteU16(&payload, 32, static_cast<uint16_t>(-10000));
  WriteU32(&payload, 34, 1000000);
  WriteU32(&payload, 40, 0xffffffffu);
  std::string line;
  Expect(momo::serial_data_channel::DecodeImuFusionMotionPayload(payload,
                                                                  &line),
         "long-running payload should decode");
  Expect(line.size() > 256,
         "long-running fusion payload should exercise the extended limit");
  Expect(line.size() <= 384,
         "long-running fusion payload must fit the normalized limit");
}

}  // namespace

int main() {
  TestReadyPayload();
  TestCalibratingOmitsFusedValues();
  TestStrictRejections();
  TestLongRunningPayloadFitsNormalizedLimit();
  std::cout << "IMU fusion telemetry decoder tests passed (9 checks)\n";
  return 0;
}
