import argparse
import importlib.util
import pathlib
import sys
import unittest

import numpy as np


MODULE_PATH = pathlib.Path(__file__).with_name("Run-GpuMarkerObserverSharedFrame.py")
SPEC = importlib.util.spec_from_file_location("run_gpu_marker_observer_shared_frame", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.path.insert(0, str(MODULE_PATH.parent))
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class GpuMarkerObserverSharedFrameTest(unittest.TestCase):
    def test_source_parser_maps_source_to_fixed_slot(self):
        source = MODULE.parse_source("11.5=2")
        self.assertEqual("11.5", source.source_id)
        self.assertEqual(2, source.slot_index)

    def test_source_parser_rejects_slot_outside_composite(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            MODULE.parse_source("11.5=4")

    def test_validate_sources_rejects_duplicate_slots(self):
        with self.assertRaisesRegex(ValueError, "slot indices"):
            MODULE.validate_sources(
                [
                    MODULE.SourceSlot("11.4", 1),
                    MODULE.SourceSlot("11.5", 1),
                ]
            )

    def test_source_rectangles_exclude_green_slot_padding(self):
        self.assertEqual((0, 6, 960, 528), MODULE.source_rectangle(0))
        self.assertEqual((960, 546, 960, 528), MODULE.source_rectangle(3))

    def test_extract_source_frames_preserves_slot_order(self):
        frame = np.zeros((1080, 1920, 4), dtype=np.uint8)
        frame[6:534, 0:960, 0] = 10
        frame[546:1074, 960:1920, 0] = 40
        sources = [MODULE.SourceSlot("11.6", 3), MODULE.SourceSlot("11.3", 0)]
        extracted = MODULE.extract_source_frames(frame, sources)
        self.assertEqual((2, 528, 960, 4), extracted.shape)
        self.assertTrue(np.all(extracted[0, :, :, 0] == 40))
        self.assertTrue(np.all(extracted[1, :, :, 0] == 10))

    @unittest.skipUnless(MODULE.os.name == "nt", "Windows shared memory only")
    def test_reader_does_not_create_missing_mapping(self):
        with self.assertRaises(FileNotFoundError):
            MODULE.SharedFrameReader(r"Local\MomoObserverFrameMissingForTestV1")


if __name__ == "__main__":
    unittest.main()
