import argparse
import importlib.util
import mmap
import pathlib
import struct
import sys
import unittest

import numpy as np


MODULE_PATH = pathlib.Path(__file__).with_name("Run-GpuMarkerObserverLuma.py")
SPEC = importlib.util.spec_from_file_location("run_gpu_marker_observer_luma", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.path.insert(0, str(MODULE_PATH.parent))
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class GpuMarkerObserverLumaTest(unittest.TestCase):
    def test_contract_sizes_match_native_layout(self):
        self.assertEqual(64, MODULE.SharedLumaReader._HEADER.size)
        self.assertEqual(24, MODULE.SharedLumaReader._SOURCE_METADATA.size)
        self.assertEqual(6_083_040, MODULE.SHARED_LUMA_MAPPING_SIZE)

    def test_source_selection_defaults_to_all_configured_slots(self):
        self.assertEqual([0, 1, 2], MODULE.select_source_slots(["11.3", "11.4", "11.5"], None))

    def test_source_selection_preserves_requested_order(self):
        self.assertEqual(
            [2, 0],
            MODULE.select_source_slots(["11.3", "11.4", "11.5"], ["11.5", "11.3"]),
        )

    def test_source_selection_rejects_missing_source(self):
        with self.assertRaisesRegex(ValueError, "not configured"):
            MODULE.select_source_slots(["11.5"], ["11.6"])

    def test_marker_parser_rejects_reserved_ids(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            MODULE.parse_marker_ids("2,17")

    @unittest.skipUnless(MODULE.os.name == "nt", "Windows shared memory only")
    def test_reader_reads_selected_native_luma_slot(self):
        mapping_name = rf"Local\MomoObserverLumaTest{MODULE.os.getpid()}"
        mapping = mmap.mmap(
            -1,
            MODULE.SHARED_LUMA_MAPPING_SIZE,
            tagname=mapping_name,
            access=mmap.ACCESS_WRITE,
        )
        try:
            mapping[:] = b"\0" * MODULE.SHARED_LUMA_MAPPING_SIZE
            MODULE.SharedLumaReader._HEADER.pack_into(
                mapping,
                0,
                MODULE.SHARED_LUMA_MAGIC,
                MODULE.SHARED_LUMA_VERSION,
                MODULE.SHARED_LUMA_HEADER_SIZE,
                MODULE.SHARED_LUMA_WIDTH,
                MODULE.SHARED_LUMA_HEIGHT,
                MODULE.SHARED_LUMA_STRIDE,
                MODULE.SHARED_LUMA_PIXEL_FORMAT,
                MODULE.SHARED_LUMA_BUFFER_COUNT,
                MODULE.SHARED_LUMA_MAX_SOURCES,
                2,
                0,
                2,
                123456,
            )
            mapping[64:68] = b"11.5"
            mapping[96:100] = b"11.6"
            buffer_offset = MODULE.SHARED_LUMA_PREFIX_SIZE
            MODULE.SharedLumaReader._SOURCE_METADATA.pack_into(
                mapping,
                buffer_offset + MODULE.SHARED_LUMA_SOURCE_METADATA_SIZE,
                42,
                987654,
                MODULE.SHARED_LUMA_VALID,
                0,
            )
            plane_offset = (
                buffer_offset
                + MODULE.SHARED_LUMA_METADATA_SIZE
                + MODULE.SHARED_LUMA_PLANE_SIZE
            )
            mapping[plane_offset : plane_offset + MODULE.SHARED_LUMA_PLANE_SIZE] = bytes(
                [73]
            ) * MODULE.SHARED_LUMA_PLANE_SIZE

            with MODULE.SharedLumaReader(mapping_name) as reader:
                batch = reader.read_latest([1])
            self.assertIsNotNone(batch)
            assert batch is not None
            self.assertEqual(["11.5", "11.6"], reader.source_ids)
            self.assertEqual(42, batch.sources[0].source_sequence)
            self.assertTrue(batch.sources[0].video_valid)
            self.assertEqual((1, 528, 960), batch.y_planes.shape)
            self.assertTrue(np.all(batch.y_planes == 73))
        finally:
            mapping.close()

    @unittest.skipUnless(MODULE.os.name == "nt", "Windows shared memory only")
    def test_reader_does_not_create_missing_mapping(self):
        with self.assertRaises(FileNotFoundError):
            MODULE.SharedLumaReader(r"Local\MomoObserverLumaMissingForTestV1")


if __name__ == "__main__":
    unittest.main()
