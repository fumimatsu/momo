// こちらを参考にさせていただきました
// https://github.com/fedetft/serial-port/blob/master/4_callback/AsyncSerial.cpp

#include "serial_data_manager.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <functional>
#include <iostream>
#include <string>

// Boost
#include <boost/asio/post.hpp>

// WebRTC
#include <rtc_base/log_sinks.h>

// Boost
#include <boost/asio/post.hpp>

#define SERIAL_TX_BUFFER_SIZE 16
#define SERIAL_RX_BUFFER_SIZE 256

namespace {

constexpr uint8_t kTelemetryBinaryVersion = 3;
constexpr uint8_t kTelemetryBinaryState = 1;
constexpr uint8_t kTelemetryBinaryEvent = 2;
constexpr uint8_t kTelemetryBinaryEscState = 3;
constexpr size_t kTelemetryBinaryMaxEncodedBytes = 64;
constexpr uint16_t kEscMotorRpm = 1u << 0;
constexpr uint16_t kEscMaximumMotorRpm = 1u << 1;
constexpr uint16_t kEscVoltage = 1u << 2;
constexpr uint16_t kEscTemperature = 1u << 3;
constexpr uint16_t kEscDriveOutput = 1u << 4;
constexpr uint16_t kEscFresh = 1u << 0;

uint16_t Crc16Ccitt(const uint8_t* data, size_t length) {
  uint16_t crc = 0xffff;
  for (size_t index = 0; index < length; ++index) {
    crc ^= static_cast<uint16_t>(data[index]) << 8;
    for (uint8_t bit = 0; bit < 8; ++bit) {
      crc = (crc & 0x8000) != 0 ? static_cast<uint16_t>((crc << 1) ^ 0x1021)
                                : static_cast<uint16_t>(crc << 1);
    }
  }
  return crc;
}

bool CobsDecode(const uint8_t* input, size_t input_length, std::vector<uint8_t>* output) {
  output->clear();
  size_t index = 0;
  while (index < input_length) {
    const uint8_t code = input[index++];
    if (code == 0 || index + code - 1 > input_length) return false;
    for (uint8_t count = 1; count < code; ++count) output->push_back(input[index++]);
    if (code != 0xff && index < input_length) output->push_back(0);
  }
  return true;
}

uint16_t ReadU16(const std::vector<uint8_t>& data, size_t offset) {
  return static_cast<uint16_t>(data[offset]) |
         (static_cast<uint16_t>(data[offset + 1]) << 8);
}

uint32_t ReadU32(const std::vector<uint8_t>& data, size_t offset) {
  uint32_t value = 0;
  for (uint8_t shift = 0; shift < 32; shift += 8) value |= static_cast<uint32_t>(data[offset++]) << shift;
  return value;
}

uint64_t ReadU64(const std::vector<uint8_t>& data, size_t offset) {
  uint64_t value = 0;
  for (uint8_t shift = 0; shift < 64; shift += 8) value |= static_cast<uint64_t>(data[offset++]) << shift;
  return value;
}

int16_t ReadI16(const std::vector<uint8_t>& data, size_t offset) {
  return static_cast<int16_t>(ReadU16(data, offset));
}

bool DecodeBinaryTelemetry(const uint8_t* encoded, size_t encoded_length, std::string* line) {
  if (encoded_length == 0 || encoded_length > kTelemetryBinaryMaxEncodedBytes) return false;
  std::vector<uint8_t> payload;
  if (!CobsDecode(encoded, encoded_length, &payload) || payload.size() < 22) return false;
  const uint16_t expected_crc = ReadU16(payload, payload.size() - 2);
  if (Crc16Ccitt(payload.data(), payload.size() - 2) != expected_crc ||
      payload[0] != kTelemetryBinaryVersion) return false;

  const uint8_t type = payload[1];
  const uint32_t boot = ReadU32(payload, 4);
  const uint32_t sequence = ReadU32(payload, 8);
  const uint64_t timestamp_us = ReadU64(payload, 12);
  char text[384];
  int written = 0;
  if (type == kTelemetryBinaryState && payload.size() == 34 && payload[2] == 1) {
    const float forward = ReadI16(payload, 20) * 0.01f;
    const float lateral = ReadI16(payload, 22) * 0.01f;
    const float vertical = ReadI16(payload, 24) * 0.01f;
    const float yaw = ReadI16(payload, 26) * 0.01f;
    const uint32_t period_us = ReadU32(payload, 28);
    written = std::snprintf(text, sizeof(text),
        "TEL:{\"v\":2,\"k\":\"s\",\"src\":\"imu0\",\"boot\":\"%08x\",\"seq\":%u,\"t_us\":%llu,\"m\":{\"a\":[%.2f,%.2f,%.2f],\"y\":%.2f},\"q\":{\"p\":%u,\"f\":[\"flu_axes\"]}}",
        boot, sequence, static_cast<unsigned long long>(timestamp_us), forward, lateral,
        vertical, yaw, period_us);
  } else if (type == kTelemetryBinaryEvent && payload.size() == 32 && payload[2] == 1) {
    const float magnitude = ReadU16(payload, 20) * 0.1f;
    const float forward = ReadI16(payload, 22) * 0.001f;
    const float lateral = ReadI16(payload, 24) * 0.001f;
    const float vertical = ReadI16(payload, 26) * 0.001f;
    const float jerk = ReadU16(payload, 28);
    written = std::snprintf(text, sizeof(text),
        "TEL:{\"v\":2,\"k\":\"e\",\"src\":\"imu0\",\"boot\":\"%08x\",\"seq\":%u,\"t_us\":%llu,\"e\":{\"n\":\"impact_candidate\",\"m\":%.1f,\"a\":[%.3f,%.3f,%.3f],\"j\":%.0f}}",
        boot, sequence, static_cast<unsigned long long>(timestamp_us), magnitude, forward,
        lateral, vertical, jerk);
  } else if (type == kTelemetryBinaryEscState && payload.size() == 48 &&
             payload[2] == 0 && payload[3] == 1) {
    const uint16_t valid = ReadU16(payload, 20);
    const uint16_t status = ReadU16(payload, 22);
    std::string esc = "{";
    bool first = true;
    const auto append_u32 = [&esc, &first](const char* name, uint32_t value) {
      if (!first) esc += ',';
      first = false;
      esc += '\"';
      esc += name;
      esc += "\":";
      esc += std::to_string(value);
    };
    const auto append_decimal = [&esc, &first](const char* name, double value,
                                                int precision) {
      char field[64];
      const int field_length = std::snprintf(
          field, sizeof(field), "%s\"%s\":%.*f", first ? "" : ",", name,
          precision, value);
      if (field_length > 0 && static_cast<size_t>(field_length) < sizeof(field)) {
        esc.append(field, static_cast<size_t>(field_length));
        first = false;
      }
    };
    if ((valid & kEscMotorRpm) != 0) append_u32("rpm", ReadU32(payload, 24));
    if ((valid & kEscMaximumMotorRpm) != 0) append_u32("max", ReadU32(payload, 28));
    if ((valid & kEscVoltage) != 0) {
      append_decimal("v", ReadU16(payload, 32) / 1000.0, 3);
    }
    if ((valid & kEscTemperature) != 0) {
      append_decimal("tc", ReadI16(payload, 36) / 10.0, 1);
    }
    if ((valid & kEscDriveOutput) != 0) append_u32("out", ReadU16(payload, 40));
    esc += '}';
    const uint32_t poll_period_us = static_cast<uint32_t>(ReadU16(payload, 44)) * 1000u;
    const uint16_t age_ms = ReadU16(payload, 42);
    written = std::snprintf(
        text, sizeof(text),
        "TEL:{\"v\":2,\"k\":\"s\",\"src\":\"esc0\",\"boot\":\"%08x\",\"seq\":%u,\"t_us\":%llu,\"esc\":%s,\"q\":{\"p\":%u,\"ok\":%s,\"age\":%u,\"f\":[\"blrs4_prg\"]}}",
        boot, sequence, static_cast<unsigned long long>(timestamp_us), esc.c_str(),
        poll_period_us, (status & kEscFresh) != 0 ? "true" : "false", age_ms);
  }
  if (written <= 0 || static_cast<size_t>(written) >= sizeof(text)) return false;
  *line = text;
  return true;
}

}  // namespace

SerialDataManager::SerialDataManager(boost::asio::io_context& ioc)
    : serial_port_(ioc),
      telemetry_test_timer_(ioc),
      telemetry_test_enabled_(std::getenv("MOMO_FPV_TELEMETRY_TEST") !=
                              nullptr),
      telemetry_test_seq_(0),
      read_buffer_size_(SERIAL_RX_BUFFER_SIZE) {
  post_ = [&ioc](std::function<void()> f) {
    if (ioc.stopped())
      return;
    boost::asio::post(ioc, f);
  };
}

SerialDataManager::~SerialDataManager() {
  telemetry_test_timer_.cancel();

  {
    webrtc::MutexLock lock(&channels_lock_);
    for (SerialDataChannel* serial_data_channel : serial_data_channels_) {
      delete serial_data_channel;
    }
  }

  DoCloseSerial();
}

void SerialDataManager::OnDataChannel(
    webrtc::scoped_refptr<webrtc::DataChannelInterface> data_channel) {
  webrtc::MutexLock lock(&channels_lock_);
  serial_data_channels_.push_back(new SerialDataChannel(this, data_channel));
}

void SerialDataManager::OnClosed(SerialDataChannel* serial_data_channel) {
  webrtc::MutexLock lock(&channels_lock_);
  serial_data_channels_.erase(
      std::remove(serial_data_channels_.begin(), serial_data_channels_.end(),
                  serial_data_channel),
      serial_data_channels_.end());
  delete serial_data_channel;
}

bool SerialDataManager::Connect(std::string device, unsigned int rate) {
  boost::system::error_code error;
  serial_port_.open(device, error);
  if (error) {
    std::cerr << "failed to connect serial port device : " << device
              << std::endl;
    return false;
  }

  serial_port_.set_option(boost::asio::serial_port_base::baud_rate(rate),
                          error);
  if (error) {
    std::cerr << "failed to set serial port baudrate : " << rate << std::endl;
    return false;
  }
  serial_port_.set_option(boost::asio::serial_port_base::character_size(8),
                          error);
  if (error) {
    std::cerr << "failed to set serial port character size : 8" << std::endl;
    return false;
  }
  serial_port_.set_option(
      boost::asio::serial_port_base::flow_control(
          boost::asio::serial_port_base::flow_control::none),
      error);
  if (error) {
    std::cerr << "failed to set serial port flow control : none" << std::endl;
    return false;
  }
  serial_port_.set_option(boost::asio::serial_port_base::parity(
                              boost::asio::serial_port_base::parity::none),
                          error);
  if (error) {
    std::cerr << "failed to set serial port parity : none" << std::endl;
    return false;
  }
  serial_port_.set_option(boost::asio::serial_port_base::stop_bits(
                              boost::asio::serial_port_base::stop_bits::one),
                          error);
  if (error) {
    std::cerr << "failed to set serial port stop bit : one" << std::endl;
    return false;
  }

  read_buffer_.reset(new uint8_t[read_buffer_size_]);
  post_(std::bind(&SerialDataManager::DoRead, this));
  MaybeStartTelemetryTest();
  return true;
}

void SerialDataManager::Send(const uint8_t* data, size_t length) {
  std::vector<uint8_t> v(data, data + length);
  post_(std::bind(&SerialDataManager::StartWrite, this, std::move(v)));
}

void SerialDataManager::DoCloseSerial() {
  if (!serial_port_.is_open()) {
    return;
  }
  boost::system::error_code error;
  serial_port_.cancel(error);
  serial_port_.close(error);
}

void SerialDataManager::DoRead() {
  if (!serial_port_.is_open()) {
    return;
  }
  serial_port_.async_read_some(
      boost::asio::buffer(read_buffer_.get(), read_buffer_size_),
      std::bind(&SerialDataManager::OnRead, this, std::placeholders::_1,
                std::placeholders::_2));
}

void SerialDataManager::OnRead(const boost::system::error_code& error,
                               size_t bytes_transferred) {
  if (error) {
    RTC_LOG(LS_ERROR) << __FUNCTION__
                      << " async_read_some failed  error :" << error;
    DoCloseSerial();
    return;
  }
  read_line_buffer_.insert(read_line_buffer_.end(), read_buffer_.get(),
                           read_buffer_.get() + bytes_transferred);
  {
    webrtc::MutexLock lock(&channels_lock_);
    SendLineFromSerial();
  }
  DoRead();
}

void SerialDataManager::SendLineFromSerial() {
  while (!read_line_buffer_.empty()) {
    if (read_line_buffer_.front() == 0) {
      const auto end = std::find(read_line_buffer_.begin() + 1,
                                 read_line_buffer_.end(), 0);
      if (end == read_line_buffer_.end()) {
        if (read_line_buffer_.size() > kTelemetryBinaryMaxEncodedBytes + 2) {
          read_line_buffer_.erase(read_line_buffer_.begin());
        }
        return;
      }
      std::string telemetry;
      const size_t encoded_length = std::distance(read_line_buffer_.begin() + 1, end);
      if (DecodeBinaryTelemetry(read_line_buffer_.data() + 1, encoded_length, &telemetry)) {
        for (SerialDataChannel* serial_data_channel : serial_data_channels_) {
          serial_data_channel->SendText(telemetry);
        }
      } else {
        RTC_LOG(LS_WARNING) << "Dropped invalid binary telemetry frame";
      }
      read_line_buffer_.erase(read_line_buffer_.begin(), end + 1);
      continue;
    }

    const auto newline = std::find(read_line_buffer_.begin(), read_line_buffer_.end(), '\n');
    const auto marker = std::find(read_line_buffer_.begin(), read_line_buffer_.end(), 0);
    if (newline == read_line_buffer_.end() && marker == read_line_buffer_.end()) return;
    if (marker != read_line_buffer_.end() &&
        (newline == read_line_buffer_.end() || marker < newline)) {
      // binary frame 開始 marker 前の破損データを捨て、次 loop で frame を読む。
      read_line_buffer_.erase(read_line_buffer_.begin(), marker);
      continue;
    }

    const size_t line_length = std::distance(read_line_buffer_.begin(), newline);
    const std::string line(reinterpret_cast<const char*>(read_line_buffer_.data()), line_length);
    for (SerialDataChannel* serial_data_channel : serial_data_channels_) {
      if (line.rfind("TEL:", 0) == 0) serial_data_channel->SendText(line);
      else serial_data_channel->Send(read_line_buffer_.data(), line_length);
    }
    read_line_buffer_.erase(read_line_buffer_.begin(), newline + 1);
  }
}

void SerialDataManager::MaybeStartTelemetryTest() {
  if (!telemetry_test_enabled_) {
    return;
  }
  post_(std::bind(&SerialDataManager::ScheduleTelemetryTest, this));
}

void SerialDataManager::ScheduleTelemetryTest() {
  if (!telemetry_test_enabled_) {
    return;
  }
  telemetry_test_timer_.expires_after(std::chrono::seconds(1));
  telemetry_test_timer_.async_wait([this](const boost::system::error_code& error) {
    if (error) {
      return;
    }
    SendTelemetryTest();
    ScheduleTelemetryTest();
  });
}

void SerialDataManager::SendTelemetryTest() {
  const auto now_us = std::chrono::duration_cast<std::chrono::microseconds>(
                          std::chrono::steady_clock::now().time_since_epoch())
                          .count();
  const std::string line =
      "TEL:{\"v\":1,\"k\":\"s\",\"src\":\"test0\",\"boot\":\"00000001\",\"seq\":" +
      std::to_string(++telemetry_test_seq_) +
      ",\"t_us\":" + std::to_string(now_us) +
      ",\"imu\":{\"a\":[0,0,9.8],\"g\":[0,0,0]},\"att\":{\"q\":[1,0,0,0],\"rpy\":[0,0,0]},\"qual\":{\"period_us\":1000000,\"cal\":0,\"flags\":[\"test\"]}}";

  webrtc::MutexLock lock(&channels_lock_);
  for (SerialDataChannel* serial_data_channel : serial_data_channels_) {
    serial_data_channel->SendText(line);
  }
}

void SerialDataManager::StartWrite(std::vector<uint8_t> v) {
  if (!serial_port_.is_open()) {
    return;
  }
  bool empty = write_buffer_.empty();
  write_buffer_.insert(write_buffer_.end(), v.begin(), v.end());
  if (empty && !write_buffer_.empty()) {
    DoWrite();
  }
}

void SerialDataManager::DoWrite() {
  if (write_buffer_.size() < SERIAL_TX_BUFFER_SIZE) {
    write_length_ = write_buffer_.size();
  } else {
    write_length_ = SERIAL_TX_BUFFER_SIZE;
  }
  async_write(
      serial_port_, boost::asio::buffer(write_buffer_.data(), write_length_),
      std::bind(&SerialDataManager::OnWrite, this, std::placeholders::_1));
}

void SerialDataManager::OnWrite(const boost::system::error_code& error) {
  if (error) {
    RTC_LOG(LS_ERROR) << __FUNCTION__
                      << " async_write failed  error :" << error;
    DoCloseSerial();
    return;
  }
  write_buffer_.erase(write_buffer_.begin(),
                      write_buffer_.begin() + write_length_);
  if (write_buffer_.empty()) {
    return;
  }
  DoWrite();
}
