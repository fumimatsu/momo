import importlib.util
import pathlib
import sys
import tempfile
import unittest

import numpy as np


MODULE_PATH = pathlib.Path(__file__).with_name("Measure-ArucoCapacity.py")
SPEC = importlib.util.spec_from_file_location("measure_aruco_capacity", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)

COMPARE_MODULE_PATH = pathlib.Path(__file__).with_name("Compare-ArucoBackends.py")
COMPARE_SPEC = importlib.util.spec_from_file_location("compare_aruco_backends", COMPARE_MODULE_PATH)
COMPARE_MODULE = importlib.util.module_from_spec(COMPARE_SPEC)
assert COMPARE_SPEC.loader is not None
sys.modules[COMPARE_SPEC.name] = COMPARE_MODULE
COMPARE_SPEC.loader.exec_module(COMPARE_MODULE)


class MeasureArucoCapacityTest(unittest.TestCase):
    def test_default_detection_rate_is_25_hz(self):
        self.assertEqual(25.0, MODULE.DEFAULT_DETECTION_HZ)
        self.assertEqual(47.5, 50.0 * MODULE.MINIMUM_RATE_FACTOR)

    def test_percentile_uses_ceiling_rank(self):
        self.assertEqual(4, MODULE.percentile([1, 2, 3, 4], 95))
        self.assertIsNone(MODULE.percentile([], 95))

    def test_parse_counts(self):
        self.assertEqual([1, 4, 32], MODULE.parse_counts("1,4,32"))
        with self.assertRaises(Exception):
            MODULE.parse_counts("0,4")
        with self.assertRaises(Exception):
            MODULE.parse_counts("4,4")

    def test_find_nvcodec_cuda_root_finds_pip_runtime(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            prefix = pathlib.Path(temporary_directory)
            cuda_root = prefix / "Lib" / "site-packages" / "nvidia" / "cuda_runtime"
            (cuda_root / "bin").mkdir(parents=True)
            (cuda_root / "bin" / "cudart64_12.dll").touch()
            self.assertEqual(cuda_root.resolve(), MODULE.find_nvcodec_cuda_root(prefix))

    def test_prepare_nvcodec_luma_ignores_nv12_chroma_planes(self):
        nv12 = np.vstack(
            (
                np.arange(32, dtype=np.uint8).reshape(4, 8),
                np.full((2, 8), 255, dtype=np.uint8),
            )
        )
        gray = MODULE.prepare_nvcodec_luma(nv12, source_width=8, source_height=4, quality=1.0)
        self.assertEqual((4, 6), gray.shape)
        self.assertLess(int(gray.max()), 255)

    def test_parse_nvidia_smi_sample(self):
        self.assertEqual(
            {"gpuPercent": 12.0, "decoderPercent": 34.0, "memoryUsedMB": 567.0},
            MODULE.parse_nvidia_smi_sample("12, 34, 567"),
        )
        self.assertIsNone(MODULE.parse_nvidia_smi_sample("N/A, 1, 2"))

    def test_record_marker_observation_tracks_raw_and_frame_presence(self):
        result = MODULE.WorkerResult(source_id=1)
        MODULE.record_marker_observation(result, [1, 1, 3])
        self.assertEqual(1, result.marker_frames)
        self.assertEqual({1: 2, 3: 1}, result.marker_ids)
        self.assertEqual({1: 1, 3: 1}, result.marker_id_frames)

    def test_group_detection_frames_tolerates_short_detection_gaps(self):
        self.assertEqual(
            [
                {"firstFrame": 10, "lastFrame": 19, "detectionFrames": 3},
                {"firstFrame": 40, "lastFrame": 40, "detectionFrames": 1},
            ],
            COMPARE_MODULE.group_detection_frames([10, 12, 19, 40], maximum_gap=10),
        )


if __name__ == "__main__":
    unittest.main()
