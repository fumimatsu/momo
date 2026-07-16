#ifndef ARUCO_DETECTOR_H_
#define ARUCO_DETECTOR_H_

#include <vector>

#include <opencv2/core.hpp>
#include <opencv2/objdetect/aruco_detector.hpp>

// WebRTC
#include <api/video/i420_buffer.h>

class ArucoDetector {
 public:
  ArucoDetector();

  // I420 を BGR の cv::Mat に変換し、検出したマーカーを描画する。
  void DetectAndDraw(const webrtc::I420BufferInterface& buffer,
                     cv::Mat& bgr, bool flip_vertical = false,
                     bool flip_horizontal = false);

 private:
  cv::aruco::ArucoDetector detector_;
  std::vector<int> last_detected_ids_;
};

#endif  // ARUCO_DETECTOR_H_
