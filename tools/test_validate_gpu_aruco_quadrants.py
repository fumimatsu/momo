import importlib.util
import pathlib
import sys
import unittest

import numpy as np


MODULE_PATH = pathlib.Path(__file__).with_name("Validate-GpuArucoQuadrants.py")
sys.path.insert(0, str(MODULE_PATH.parent))
SPEC = importlib.util.spec_from_file_location("validate_gpu_aruco_quadrants", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class FakeCupy:
    @staticmethod
    def stack(values):
        return np.stack(values)


class ValidateGpuArucoQuadrantsTest(unittest.TestCase):
    def test_split_quadrants_uses_visual_order(self):
        image = np.array([[1, 1, 2, 2], [1, 1, 2, 2], [3, 3, 4, 4], [3, 3, 4, 4]])
        quadrants = MODULE.split_quadrants(FakeCupy, image, 4, 4)
        self.assertEqual([1, 2, 3, 4], [int(value[0, 0]) for value in quadrants])

    def test_group_frames_tolerates_short_gaps(self):
        self.assertEqual(
            [
                {"firstFrame": 10, "lastFrame": 19, "detectionFrames": 3},
                {"firstFrame": 40, "lastFrame": 40, "detectionFrames": 1},
            ],
            MODULE.group_frames([10, 12, 19, 40]),
        )


if __name__ == "__main__":
    unittest.main()
