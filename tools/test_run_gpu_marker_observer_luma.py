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

    def test_source_state_signature_includes_sequence_and_video_validity(self):
        sources = [
            MODULE.LumaSource("11.3", 0, 10, 100, True),
            MODULE.LumaSource("11.4", 1, 20, 200, False),
        ]

        self.assertEqual(((10, True), (20, False)), MODULE.source_state_signature(sources))

    def test_duplicate_poll_delay_backs_off_without_exceeding_five_ms(self):
        delays = [
            MODULE.duplicate_poll_delay_seconds(attempt, 0.02)
            for attempt in range(6)
        ]

        self.assertEqual([0.0005, 0.001, 0.002, 0.004, 0.005, 0.005], delays)

    def test_duplicate_poll_delay_respects_quarter_frame_interval(self):
        self.assertEqual(0.0025, MODULE.duplicate_poll_delay_seconds(8, 0.01))

    def test_duplicate_poll_delay_rejects_negative_attempt(self):
        with self.assertRaisesRegex(ValueError, "attempt must not be negative"):
            MODULE.duplicate_poll_delay_seconds(-1, 0.02)

    def test_sampled_profiling_profiles_only_when_due(self):
        selected, next_sample = MODULE.select_profile_frame("sampled", 10.0, 10.0, 1.0)
        skipped, unchanged = MODULE.select_profile_frame(
            "sampled",
            10.5,
            next_sample,
            1.0,
        )

        self.assertTrue(selected)
        self.assertEqual(11.0, next_sample)
        self.assertFalse(skipped)
        self.assertEqual(next_sample, unchanged)

    def test_full_and_off_profiling_do_not_move_sample_deadline(self):
        self.assertEqual(
            (True, 20.0),
            MODULE.select_profile_frame("full", 10.0, 20.0, 1.0),
        )
        self.assertEqual(
            (False, 20.0),
            MODULE.select_profile_frame("off", 30.0, 20.0, 1.0),
        )

    def test_sampled_profiling_rejects_non_positive_interval(self):
        with self.assertRaisesRegex(ValueError, "interval must be positive"):
            MODULE.select_profile_frame("sampled", 10.0, 10.0, 0.0)

    def test_parser_uses_sampled_profiling_by_default(self):
        args = MODULE.build_parser().parse_args([])

        self.assertEqual("sampled", args.profiling_mode)
        self.assertEqual(1.0, args.profiling_sample_interval_seconds)

    def test_run_requires_four_live_sources_at_target_rate(self):
        accepted = MODULE.evaluate_run(500, [500, 500, 500, 500], 49.8, 50, 18.0)
        self.assertTrue(accepted["passed"])
        self.assertTrue(accepted["inputReady"])
        self.assertTrue(accepted["throughputPassed"])
        self.assertEqual([], accepted["failureReasons"])

    def test_run_rejects_one_live_source(self):
        rejected = MODULE.evaluate_run(500, [0, 500, 0, 0], 49.8, 50, 18.0)
        self.assertFalse(rejected["passed"])
        self.assertFalse(rejected["inputReady"])
        self.assertTrue(rejected["throughputPassed"])
        self.assertIn("active source count 1 != 4", rejected["failureReasons"])

    def test_run_rejects_low_publication_rate(self):
        rejected = MODULE.evaluate_run(400, [400, 400, 400, 400], 40.0, 50, 18.0)
        self.assertFalse(rejected["passed"])
        self.assertTrue(rejected["inputReady"])
        self.assertFalse(rejected["throughputPassed"])
        self.assertTrue(
            any("publication rate" in reason for reason in rejected["failureReasons"])
        )

    def test_run_rejects_partial_source_coverage(self):
        rejected = MODULE.evaluate_run(500, [500, 500, 300, 500], 49.8, 50, 18.0)
        self.assertFalse(rejected["passed"])
        self.assertIn("source coverage below 0.950", rejected["failureReasons"])

    def test_run_rejects_slow_cycle_p95(self):
        rejected = MODULE.evaluate_run(500, [500, 500, 500, 500], 49.8, 50, 25.0)
        self.assertFalse(rejected["passed"])
        self.assertTrue(any("cycle p95" in reason for reason in rejected["failureReasons"]))

    def test_marker_parser_rejects_reserved_ids(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            MODULE.parse_marker_ids("2,17")

    def test_rolling_samples_keeps_only_latest_window(self):
        samples = MODULE.RollingSamples(3)
        for value in [1.0, 2.0, 3.0, 4.0]:
            samples.append(value)

        self.assertEqual(3.0, samples.percentile(50))
        self.assertEqual(
            {
                "samples": 4,
                "windowSamples": 3,
                "p50": 3.0,
                "p95": 4.0,
                "p99": 4.0,
                "max": 4.0,
            },
            samples.summarize_ms(),
        )

    def test_rolling_samples_preserves_all_run_maximum(self):
        samples = MODULE.RollingSamples(2)
        for value in [9.0, 2.0, 3.0]:
            samples.append(value)

        self.assertEqual(9.0, samples.summarize_ms()["max"])

    def test_rolling_samples_rejects_empty_capacity(self):
        with self.assertRaisesRegex(ValueError, "capacity must be positive"):
            MODULE.RollingSamples(0)

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

            destination = np.empty((1, 528, 960), dtype=np.uint8)
            with MODULE.SharedLumaReader(mapping_name) as reader:
                self.assertEqual(2, reader.latest_sequence())
                batch = reader.read_latest([1], destination)
            self.assertIsNotNone(batch)
            assert batch is not None
            self.assertEqual(["11.5", "11.6"], reader.source_ids)
            self.assertEqual(42, batch.sources[0].source_sequence)
            self.assertTrue(batch.sources[0].video_valid)
            self.assertEqual((1, 528, 960), batch.y_planes.shape)
            self.assertIs(destination, batch.y_planes)
            self.assertTrue(np.all(batch.y_planes == 73))
        finally:
            mapping.close()

    @unittest.skipUnless(MODULE.os.name == "nt", "Windows shared memory only")
    def test_reader_does_not_create_missing_mapping(self):
        with self.assertRaises(FileNotFoundError):
            MODULE.SharedLumaReader(r"Local\MomoObserverLumaMissingForTestV1")


if __name__ == "__main__":
    unittest.main()
