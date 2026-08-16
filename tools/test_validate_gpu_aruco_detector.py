from collections import Counter
import importlib.util
import pathlib
import sys
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("Validate-GpuArucoDetector.py")
sys.path.insert(0, str(MODULE_PATH.parent))
SPEC = importlib.util.spec_from_file_location("validate_gpu_aruco_detector", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class ValidateGpuArucoDetectorTest(unittest.TestCase):
    def test_counter_overlap_preserves_physical_marker_multiplicity(self):
        self.assertEqual(
            (2, 1, 1),
            MODULE.counter_overlap(Counter({1: 3}), Counter({1: 2, 17: 1})),
        )


if __name__ == "__main__":
    unittest.main()
