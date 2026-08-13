import pathlib
import sys
import unittest

import cv2
import numpy as np


sys.path.insert(0, str(pathlib.Path(__file__).parent))
import GpuArucoId as MODULE


class GpuArucoIdTest(unittest.TestCase):
    def test_dictionary_patterns_cover_all_ids_and_rotations(self):
        patterns = MODULE.build_dictionary_patterns()
        self.assertEqual((50, 4), patterns.shape)
        self.assertEqual(np.uint16, patterns.dtype)
        self.assertEqual(200, len(set(int(value) for value in patterns.flat)))

    def test_marker_ids_to_mask_deduplicates_ids(self):
        self.assertEqual((1 << 1) | (1 << 49), MODULE.marker_ids_to_mask([1, 49, 1]))
        with self.assertRaises(ValueError):
            MODULE.marker_ids_to_mask([50])

    def test_candidate_homography_maps_patch_corners(self):
        corners = np.array([[[10, 20], [34, 21], [35, 45], [9, 44]]], dtype=np.float32)
        homography = MODULE.make_candidate_homographies(corners)[0]
        patch_corners = np.array(
            [[[0, 0], [MODULE.PATCH_SIZE - 1, 0], [MODULE.PATCH_SIZE - 1, MODULE.PATCH_SIZE - 1], [0, MODULE.PATCH_SIZE - 1]]],
            dtype=np.float32,
        )
        mapped = cv2.perspectiveTransform(patch_corners, homography)
        np.testing.assert_allclose(corners, mapped, atol=1e-4)

    def test_gpu_decoder_identifies_generated_marker(self):
        try:
            decoder = MODULE.GpuArucoIdDecoder()
        except RuntimeError as exc:
            self.skipTest(str(exc))
        dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
        marker = cv2.aruco.generateImageMarker(dictionary, 17, 192)
        canvas = np.full((256, 256), 255, dtype=np.uint8)
        canvas[32:224, 32:224] = marker
        gray_device = decoder.cp.asarray(canvas)
        corners = np.array([[[32, 32], [223, 32], [223, 223], [32, 223]]], dtype=np.float32)
        results, marker_mask = decoder.decode(
            gray_device, MODULE.make_candidate_homographies(corners)
        )
        self.assertEqual(17, results[0].marker_id)
        self.assertEqual(1 << 17, marker_mask)


if __name__ == "__main__":
    unittest.main()
