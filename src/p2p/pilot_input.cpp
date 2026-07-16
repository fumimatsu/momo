#include "p2p/pilot_input.h"

#include <algorithm>
#include <cmath>
#include <fstream>
#include <iomanip>
#include <sstream>

#include <boost/json.hpp>

#include <rtc_base/logging.h>

#include "p2p/pilot_data_channel.h"
#include "sdl_renderer/sdl_renderer.h"

namespace {

double ReadNumber(const boost::json::object& object,
                  const char* name,
                  double fallback) {
  const auto it = object.find(name);
  if (it == object.end() || !it->value().is_number()) {
    return fallback;
  }
  return it->value().to_number<double>();
}

int ReadInt(const boost::json::object& object, const char* name, int fallback) {
  const auto it = object.find(name);
  if (it == object.end() || !it->value().is_number()) {
    return fallback;
  }
  return static_cast<int>(it->value().to_number<double>());
}

bool ReadBool(const boost::json::object& object,
              const char* name,
              bool fallback) {
  const auto it = object.find(name);
  if (it == object.end() || !it->value().is_bool()) {
    return fallback;
  }
  return it->value().as_bool();
}

}  // namespace

bool LoadPilotInputConfig(const std::string& path,
                          PilotInputConfig* config,
                          std::string* error) {
  if (path.empty() || config == nullptr) {
    return true;
  }

  std::ifstream stream(path);
  if (!stream) {
    if (error) {
      *error = "Cannot open input config: " + path;
    }
    return false;
  }
  std::stringstream content;
  content << stream.rdbuf();
  boost::system::error_code json_ec;
  const boost::json::value value = boost::json::parse(content.str(), json_ec);
  if (json_ec || !value.is_object()) {
    if (error) {
      *error = "Input config must be a JSON object: " + path;
    }
    return false;
  }

  const boost::json::object& object = value.as_object();
  config->index = ReadInt(object, "index", config->index);
  config->steering_axis = ReadInt(object, "steeringAxis", config->steering_axis);
  config->steering_invert = ReadBool(object, "steeringInvert", config->steering_invert);
  config->steering_gain = ReadNumber(object, "steeringGain", config->steering_gain);
  config->steering_deadzone = ReadNumber(object, "steeringDeadzone", config->steering_deadzone);
  config->steering_center = ReadNumber(object, "steeringCenter", config->steering_center);
  config->steering_left = ReadNumber(object, "steeringLeft", config->steering_left);
  config->steering_right = ReadNumber(object, "steeringRight", config->steering_right);
  config->throttle_axis = ReadInt(object, "throttleAxis", config->throttle_axis);
  config->throttle_invert = ReadBool(object, "throttleInvert", config->throttle_invert);
  config->throttle_idle = ReadNumber(object, "throttleIdle", config->throttle_idle);
  config->throttle_pressed = ReadNumber(object, "throttlePressed", config->throttle_pressed);
  config->brake_axis = ReadInt(object, "brakeAxis", config->brake_axis);
  config->brake_invert = ReadBool(object, "brakeInvert", config->brake_invert);
  config->brake_idle = ReadNumber(object, "brakeIdle", config->brake_idle);
  config->brake_pressed = ReadNumber(object, "brakePressed", config->brake_pressed);
  config->pedal_deadzone = ReadNumber(object, "pedalDeadzone", config->pedal_deadzone);
  config->drive_button = ReadInt(object, "driveButton", config->drive_button);
  return true;
}

PilotInput::PilotInput(boost::asio::io_context& ioc,
                       std::shared_ptr<PilotDataChannel> data_channel,
                       PilotInputConfig config,
                       SDLRenderer* renderer,
                       std::string device_label)
    : timer_(ioc),
      data_channel_(std::move(data_channel)),
      config_(config),
      renderer_(renderer),
      device_label_(std::move(device_label)) {}

PilotInput::~PilotInput() {
  Stop();
}

void PilotInput::Start() {
  stopped_ = false;
  timer_.expires_after(std::chrono::milliseconds(20));
  timer_.async_wait(std::bind(&PilotInput::Tick, this, std::placeholders::_1));
}

void PilotInput::Stop() {
  if (stopped_) {
    return;
  }
  stopped_ = true;
  timer_.cancel();
  SendNeutral();
  if (joystick_) {
    SDL_CloseJoystick(joystick_);
    joystick_ = nullptr;
  }
}

void PilotInput::Tick(const boost::system::error_code& ec) {
  if (ec || stopped_) {
    return;
  }

  if (!EnsureJoystick()) {
    if (joystick_was_available_) {
      RTC_LOG(LS_WARNING) << "Pilot input lost; forcing Drive Off";
    }
    joystick_was_available_ = false;
    drive_enabled_ = false;
    previous_drive_button_ = false;
    SendNeutral();
    UpdateOverlay("NO INPUT devices=" +
                  std::to_string(last_joystick_count_) + " index=" +
                  std::to_string(config_.index));
  } else {
    joystick_was_available_ = true;
    const bool drive_button = GetButton(config_.drive_button);
    if (drive_button && !previous_drive_button_) {
      drive_enabled_ = !drive_enabled_;
      RTC_LOG(LS_INFO) << "Pilot Drive " << (drive_enabled_ ? "ON" : "OFF");
    }
    previous_drive_button_ = drive_button;
    if (drive_enabled_) {
      SendCurrentCommand();
    } else {
      SendNeutral();
    }
    UpdateOverlay("INPUT OK");
  }

  timer_.expires_after(std::chrono::milliseconds(20));
  timer_.async_wait(std::bind(&PilotInput::Tick, this, std::placeholders::_1));
}

bool PilotInput::EnsureJoystick() {
  if (joystick_ && SDL_JoystickConnected(joystick_)) {
    return true;
  }
  if (joystick_) {
    SDL_CloseJoystick(joystick_);
    joystick_ = nullptr;
  }

  int count = 0;
  SDL_JoystickID* ids = SDL_GetJoysticks(&count);
  last_joystick_count_ = count;
  if (!ids || config_.index < 0 || config_.index >= count) {
    SDL_free(ids);
    return false;
  }
  joystick_ = SDL_OpenJoystick(ids[config_.index]);
  SDL_free(ids);
  if (!joystick_) {
    RTC_LOG(LS_WARNING) << "Cannot open configured joystick index "
                        << config_.index << ": " << SDL_GetError();
    return false;
  }
  RTC_LOG(LS_INFO) << "Opened pilot joystick " << config_.index << ": "
                    << SDL_GetJoystickName(joystick_);
  return true;
}

double PilotInput::GetAxis(int index) const {
  if (!joystick_ || index < 0 || index >= SDL_GetNumJoystickAxes(joystick_)) {
    return 0.0;
  }
  const Sint16 raw = SDL_GetJoystickAxis(joystick_, index);
  return raw >= 0 ? static_cast<double>(raw) / 32767.0
                  : static_cast<double>(raw) / 32768.0;
}

bool PilotInput::GetButton(int index) const {
  return joystick_ && index >= 0 && index < SDL_GetNumJoystickButtons(joystick_) &&
         SDL_GetJoystickButton(joystick_, index);
}

double PilotInput::NormalizeSteering(double value) const {
  const double raw = config_.steering_invert ? -value : value;
  const double center = config_.steering_invert ? -config_.steering_center
                                                 : config_.steering_center;
  const double left = config_.steering_invert ? -config_.steering_right
                                               : config_.steering_left;
  const double right = config_.steering_invert ? -config_.steering_left
                                                : config_.steering_right;
  const double left_span = std::max(0.001, std::abs(center - left));
  const double right_span = std::max(0.001, std::abs(right - center));
  const double normalized = raw < center
                                ? -std::min(1.0, std::abs(raw - center) / left_span)
                                : std::min(1.0, std::abs(raw - center) / right_span);
  return std::clamp(ApplyDeadzone(normalized, config_.steering_deadzone) *
                        config_.steering_gain,
                    -1.0, 1.0);
}

double PilotInput::NormalizePedal(double value,
                                  bool invert,
                                  double idle_value,
                                  double pressed_value) const {
  const double raw = invert ? -value : value;
  const double idle = invert ? -idle_value : idle_value;
  const double pressed = invert ? -pressed_value : pressed_value;
  const double fallback_pressed = invert ? (idle_value >= 0 ? 1.0 : -1.0)
                                         : (idle_value >= 0 ? -1.0 : 1.0);
  const double span = std::abs(pressed - idle) >= 0.001
                          ? pressed - idle
                          : fallback_pressed - idle;
  const double normalized = (raw - idle) / (std::abs(span) >= 0.001 ? span : 1.0);
  return ApplyDeadzone(std::clamp(normalized, 0.0, 1.0), config_.pedal_deadzone);
}

double PilotInput::ApplyDeadzone(double value, double deadzone) {
  return std::abs(value) < deadzone ? 0.0 : value;
}

void PilotInput::SendNeutral() {
  last_command_ = "S:1500,T:1500";
  if (data_channel_) {
    data_channel_->SendCommand(last_command_);
  }
}

void PilotInput::SendCurrentCommand() {
  const double steering = NormalizeSteering(GetAxis(config_.steering_axis));
  const double throttle = NormalizePedal(GetAxis(config_.throttle_axis),
                                         config_.throttle_invert,
                                         config_.throttle_idle,
                                         config_.throttle_pressed);
  const double brake = NormalizePedal(GetAxis(config_.brake_axis),
                                      config_.brake_invert,
                                      config_.brake_idle,
                                      config_.brake_pressed);
  const int steering_pwm = static_cast<int>(std::lround(1500.0 + steering * 400.0));
  const int throttle_pwm = brake > 0.0
                               ? static_cast<int>(std::lround(1500.0 - brake * 200.0))
                               : static_cast<int>(std::lround(1500.0 + throttle * 100.0));
  std::ostringstream command;
  command << "S:" << std::clamp(steering_pwm, 1000, 2000) << ",T:"
          << std::clamp(throttle_pwm, 1300, 2000);
  last_command_ = command.str();
  if (data_channel_) {
    data_channel_->SendCommand(last_command_);
  }
}

void PilotInput::UpdateOverlay(const std::string& input_state) const {
  if (!renderer_) {
    return;
  }
  std::ostringstream text;
  text << "ID " << (device_label_.empty() ? "n/a" : device_label_)
       << "  LINK " << (data_channel_ && data_channel_->IsCommandOpen()
                              ? "CONNECTED"
                              : "CONNECTING")
       << "  VIDEO " << std::fixed << std::setprecision(1)
       << renderer_->GetPrimaryFps() << " FPS\n";
  text << "DC " << (data_channel_ && data_channel_->IsCommandOpen() ? "OPEN" : "WAIT")
       << "  DRIVE " << (drive_enabled_ ? "ON" : "OFF")
       << "  RC " << last_command_ << "\n";
  text << input_state;
  const std::string telemetry = data_channel_ ? data_channel_->GetLastTelemetry() : "";
  if (!telemetry.empty()) {
    text << "\nTEL " << telemetry.substr(0, 64);
  }
  renderer_->SetOverlayText(text.str());
}
