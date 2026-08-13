import pathlib
import sys
import unittest

import cv2
import numpy as np


sys.path.insert(0, str(pathlib.Path(__file__).parent))
import GpuArucoDetector as MODULE


class GpuArucoDetectorTest(unittest.TestCase):
    def test_invalid_allowed_marker_id_is_rejected(self):
        with self.assertRaises(ValueError):
            MODULE.GpuArucoDetector(allowed_marker_ids=[50])

    def test_invalid_maximum_component_area_ratio_is_rejected(self):
        with self.assertRaises(ValueError):
            MODULE.GpuArucoDetector(maximum_component_area_ratio=0)

    def test_gpu_detector_finds_generated_marker_without_host_image_input(self):
        try:
            detector = MODULE.GpuArucoDetector(allowed_marker_ids=[17])
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        marker = cv2.aruco.generateImageMarker(dictionary, 17, 48)
        canvas = np.full((256, 256), 255, dtype=np.uint8)
        canvas[104:152, 104:152] = marker
        result = detector.detect(detector.cp.asarray(canvas))
        self.assertEqual([17], result.marker_ids)
        self.assertGreater(result.candidate_count, 0)

    def test_gpu_detector_preserves_multiple_physical_markers_with_same_id(self):
        try:
            detector = MODULE.GpuArucoDetector(allowed_marker_ids=[1])
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        marker = cv2.aruco.generateImageMarker(dictionary, 1, 48)
        canvas = np.full((256, 256), 255, dtype=np.uint8)
        canvas[48:96, 40:88] = marker
        canvas[152:200, 168:216] = marker
        result = detector.detect(detector.cp.asarray(canvas))
        self.assertEqual([1, 1], sorted(result.marker_ids))

    def test_gpu_batch_detector_keeps_results_separated_by_source(self):
        try:
            detector = MODULE.GpuArucoDetector(allowed_marker_ids=[1, 2])
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        canvases = []
        for source_index, marker_id in enumerate((1, 2)):
            marker = cv2.aruco.generateImageMarker(dictionary, marker_id, 48)
            canvas = np.full((256, 256), 255, dtype=np.uint8)
            canvas[104:152, 104:152] = marker
            if source_index == 0:
                canvas[32:80, 32:80] = marker
            canvases.append(canvas)
        results = detector.detect_batch(detector.cp.asarray(np.stack(canvases)))
        self.assertEqual([[1, 1], [2]], [sorted(result.marker_ids) for result in results])

    def test_gpu_detector_finds_all_dict_4x4_50_ids_including_16(self):
        try:
            detector = MODULE.GpuArucoDetector(allowed_marker_ids=range(50))
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        detected_ids = []
        for marker_id in range(50):
            marker = cv2.aruco.generateImageMarker(dictionary, marker_id, 48)
            canvas = np.full((256, 256), 255, dtype=np.uint8)
            canvas[104:152, 104:152] = marker
            result = detector.detect(detector.cp.asarray(canvas))
            if marker_id in result.marker_ids:
                detected_ids.append(marker_id)
        self.assertEqual(list(range(50)), detected_ids)

    def test_twenty_percent_limit_accepts_close_id_16_with_large_window(self):
        try:
            detector_10 = MODULE.GpuArucoDetector(
                adaptive_window_size=31,
                maximum_component_area_ratio=0.1,
                allowed_marker_ids=[16],
            )
            detector_20 = MODULE.GpuArucoDetector(
                adaptive_window_size=31,
                maximum_component_area_ratio=0.2,
                allowed_marker_ids=[16],
            )
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        marker = cv2.aruco.generateImageMarker(dictionary, 16, 144)
        canvas = np.full((256, 256), 255, dtype=np.uint8)
        canvas[56:200, 56:200] = marker
        self.assertNotIn(16, detector_10.detect(detector_10.cp.asarray(canvas)).marker_ids)
        self.assertIn(16, detector_20.detect(detector_20.cp.asarray(canvas)).marker_ids)


if __name__ == "__main__":
    unittest.main()
