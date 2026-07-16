#include "aruco_detector.h"

#include <algorithm>
#include <cstring>
#include <sstream>
#include <string>
#include <vector>

#include <opencv2/imgproc.hpp>
#include <rtc_base/logging.h>

ArucoDetector::ArucoDetector()
    : detector_(
          cv::aruco::getPredefinedDictionary(cv::aruco::DICT_4X4_50),
          [] {
            cv::aruco::DetectorParameters parameters;
            parameters.adaptiveThreshWinSizeMin = 5;
            parameters.adaptiveThreshWinSizeMax = 23;
            parameters.adaptiveThreshWinSizeStep = 4;
            parameters.adaptiveThreshConstant = 7.0;
            parameters.minMarkerPerimeterRate = 0.035;
            parameters.maxMarkerPerimeterRate = 2.0;
            parameters.minOtsuStdDev = 0.8;
            // この映像では ArUco3 検出を有効にすると、DICT_4X4_50 の
            // 小さいマーカーを取りこぼすため、標準検出を使用する。
            parameters.useAruco3Detection = false;
            return parameters;
          }(),
          cv::aruco::RefineParameters(10.0f, 3.0f, true)) {}

void ArucoDetector::DetectAndDraw(const webrtc::I420BufferInterface& buffer,
                                  cv::Mat& bgr, bool flip_vertical,
                                  bool flip_horizontal) {
  const int width = buffer.width();
  const int height = buffer.height();
  cv::Mat i420(height + height / 2, width, CV_8UC1);

  for (int y = 0; y < height; ++y) {
    std::memcpy(i420.ptr(y), buffer.DataY() + y * buffer.StrideY(), width);
  }
  for (int y = 0; y < height / 2; ++y) {
    auto* u_row = i420.ptr(height + y / 2) + (y % 2) * width / 2;
    auto* v_row = i420.ptr(height + height / 4 + y / 2) + (y % 2) * width / 2;
    std::memcpy(u_row, buffer.DataU() + y * buffer.StrideU(), width / 2);
    std::memcpy(v_row, buffer.DataV() + y * buffer.StrideV(), width / 2);
  }

  cv::cvtColor(i420, bgr, cv::COLOR_YUV2BGR_I420);

  std::vector<std::vector<cv::Point2f>> corners;
  std::vector<int> ids;
  detector_.detectMarkers(bgr, corners, ids);

  std::vector<int> sorted_ids = ids;
  std::sort(sorted_ids.begin(), sorted_ids.end());
  if (sorted_ids != last_detected_ids_) {
    std::ostringstream message;
    message << "ArUco detected IDs:";
    if (sorted_ids.empty()) {
      message << " none";
    } else {
      for (const int id : sorted_ids) {
        message << " " << id;
      }
    }
    RTC_LOG(LS_INFO) << message.str();
    last_detected_ids_ = std::move(sorted_ids);
  }

  std::vector<std::vector<cv::Point2f>> display_corners = corners;
  if (flip_vertical || flip_horizontal) {
    // 検出は元画像で行い、描画用の画像と角だけを反転する。
    // 先に画像を反転するとマーカー自体が鏡像になり、ID を取りこぼす。
    const int flip_code = flip_vertical && flip_horizontal
                              ? -1
                              : (flip_vertical ? 0 : 1);
    cv::flip(bgr, bgr, flip_code);
    for (auto& marker_corners : display_corners) {
      for (auto& point : marker_corners) {
        if (flip_vertical) {
          point.y = static_cast<float>(height - 1) - point.y;
        }
        if (flip_horizontal) {
          point.x = static_cast<float>(width - 1) - point.x;
        }
      }
    }
  }

  if (ids.empty()) {
    return;
  }

  cv::aruco::drawDetectedMarkers(bgr, display_corners, ids,
                                 cv::Scalar(0, 255, 0));
  for (size_t i = 0; i < ids.size(); ++i) {
    const auto anchor = std::min_element(
        display_corners[i].begin(), display_corners[i].end(),
        [](const cv::Point2f& lhs, const cv::Point2f& rhs) {
          return lhs.y < rhs.y;
        });
    cv::putText(bgr, "ID: " + std::to_string(ids[i]),
                cv::Point(static_cast<int>(anchor->x),
                          std::max(20, static_cast<int>(anchor->y) - 8)),
                cv::FONT_HERSHEY_SIMPLEX, 0.7, cv::Scalar(0, 255, 0), 2,
                cv::LINE_AA);
  }
}
