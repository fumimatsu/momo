#ifndef PILOT_INPUT_H_
#define PILOT_INPUT_H_

#include <memory>
#include <string>

#include <boost/asio/steady_timer.hpp>

#include <SDL3/SDL.h>

class PilotDataChannel;
class SDLRenderer;

// Web Pilot の Input 設定と互換の JSON を読む。
struct PilotInputConfig {
  int index = 0;
  int steering_axis = 0;
  bool steering_invert = false;
  double steering_gain = 1.0;
  double steering_deadzone = 0.03;
  double steering_center = 0.0;
  double steering_left = -1.0;
  double steering_right = 1.0;
  int throttle_axis = 5;
  bool throttle_invert = false;
  double throttle_idle = 1.0;
  double throttle_pressed = -1.0;
  int brake_axis = 6;
  bool brake_invert = false;
  double brake_idle = 1.0;
  double brake_pressed = -1.0;
  double pedal_deadzone = 0.05;
  int drive_button = 8;
};

bool LoadPilotInputConfig(const std::string& path,
                          PilotInputConfig* config,
                          std::string* error);

// ハンコンが無い、または Drive Off のときは 50 Hz でニュートラルを送る。
class PilotInput {
 public:
  PilotInput(boost::asio::io_context& ioc,
             std::shared_ptr<PilotDataChannel> data_channel,
             PilotInputConfig config,
             SDLRenderer* renderer,
             std::string device_label);
  ~PilotInput();

  void Start();
  void Stop();

 private:
  void Tick(const boost::system::error_code& ec);
  bool EnsureJoystick();
  double GetAxis(int index) const;
  bool GetButton(int index) const;
  double NormalizeSteering(double value) const;
  double NormalizePedal(double value,
                        bool invert,
                        double idle,
                        double pressed) const;
  static double ApplyDeadzone(double value, double deadzone);
  void SendNeutral();
  void SendCurrentCommand();
  void UpdateOverlay(const std::string& input_state) const;

  boost::asio::steady_timer timer_;
  std::shared_ptr<PilotDataChannel> data_channel_;
  PilotInputConfig config_;
  SDLRenderer* renderer_;
  std::string device_label_;
  std::string last_command_ = "S:1500,T:1500";
  SDL_Joystick* joystick_ = nullptr;
  bool drive_enabled_ = false;
  bool previous_drive_button_ = false;
  bool joystick_was_available_ = false;
  bool stopped_ = false;
  int last_joystick_count_ = 0;
};

#endif  // PILOT_INPUT_H_
