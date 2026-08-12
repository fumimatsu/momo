import importlib.util
import pathlib
import sys
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("Measure-ArucoCapacity.py")
SPEC = importlib.util.spec_from_file_location("measure_aruco_capacity", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class MeasureArucoCapacityTest(unittest.TestCase):
    def test_percentile_uses_ceiling_rank(self):
        self.assertEqual(4, MODULE.percentile([1, 2, 3, 4], 95))
        self.assertIsNone(MODULE.percentile([], 95))

    def test_parse_counts(self):
        self.assertEqual([1, 4, 32], MODULE.parse_counts("1,4,32"))
        with self.assertRaises(Exception):
            MODULE.parse_counts("0,4")
        with self.assertRaises(Exception):
            MODULE.parse_counts("4,4")


if __name__ == "__main__":
    unittest.main()
