import importlib.util
import pathlib
import sys
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("Run-GpuMarkerObserverLumaV2.py")
SPEC = importlib.util.spec_from_file_location(
    "run_gpu_marker_observer_luma_v2", MODULE_PATH
)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.path.insert(0, str(MODULE_PATH.parent))
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class GpuMarkerObserverLumaV2Test(unittest.TestCase):
    def test_processing_duration_excludes_frame_wait(self):
        self.assertAlmostEqual(3.25, MODULE.processing_duration_ms(18.25, 15.0))

    def test_processing_duration_never_becomes_negative(self):
        self.assertEqual(0.0, MODULE.processing_duration_ms(4.0, 5.0))

    def test_parser_defaults_to_strict_fresh_frame_coverage(self):
        args = MODULE.build_parser().parse_args([])

        self.assertEqual(5.0, args.fresh_frame_wait_ms)
        self.assertEqual(0.95, args.minimum_fresh_tick_ratio)
        self.assertEqual("sampled", args.profiling_mode)

    def test_micro_batch_waits_for_live_duplicate_sources(self):
        sampled = [
            type("Sample", (), {"source_id": "one", "reason": "selected"})(),
            type("Sample", (), {"source_id": "two", "reason": "duplicate_or_rollback"})(),
            type("Sample", (), {"source_id": "three", "reason": "no_video"})(),
        ]

        self.assertEqual(1, MODULE.pending_fresh_source_count(sampled))

    def test_micro_batch_does_not_wait_for_missing_video(self):
        sampled = [type("Sample", (), {"source_id": "one", "reason": "no_video"})()]

        self.assertEqual(0, MODULE.pending_fresh_source_count(sampled))

    def test_pending_source_count_ignores_sources_already_processed_in_epoch(self):
        sampled = [
            type(
                "Sample",
                (),
                {"source_id": "one", "reason": "duplicate_or_rollback"},
            )()
        ]

        self.assertEqual(
            0,
            MODULE.pending_fresh_source_count(sampled, {"one"}),
        )

    def test_invalid_video_is_published_explicitly(self):
        topology = MODULE.Mly2Topology(1, 10_000_000, "green", 1, ("one",))
        snapshot = MODULE.Mly2SourceSnapshot("one", 0, 7, 10, 20, 7, 0, 0)
        sampled = [
            type("Sample", (), {"source_id": "one", "reason": "no_video"})()
        ]

        observations = MODULE.build_invalid_observations(
            topology,
            {"one": snapshot},
            sampled,
            30,
        )

        self.assertEqual(1, len(observations))
        self.assertEqual(7, observations[0].source_sequence)
        self.assertFalse(observations[0].video_valid)

    def test_non_invalidating_sampling_reason_is_omitted(self):
        topology = MODULE.Mly2Topology(1, 10_000_000, "green", 1, ("one",))
        sampled = [
            type(
                "Sample",
                (),
                {"source_id": "one", "reason": "duplicate_or_rollback"},
            )()
        ]

        observations = MODULE.build_invalid_observations(
            topology,
            {},
            sampled,
            30,
        )

        self.assertEqual([], observations)


if __name__ == "__main__":
    unittest.main()
