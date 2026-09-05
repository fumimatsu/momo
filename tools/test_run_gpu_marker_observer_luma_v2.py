import importlib.util
import contextlib
import io
import json
import pathlib
import sys
import unittest
from unittest.mock import patch
from types import SimpleNamespace


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
    def test_capacity_failure_is_immediate_and_machine_readable(self):
        from MarkerDetectionRateController import RateDecision, DetectionWindow
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            stopped = MODULE.enforce_detection_capacity(
                RateDecision(25, False, True, 'capacity_exceeded'),
                MODULE.Mly2Topology(3, 10_000_000, 'green', 1, ('one',)),
                DetectionWindow(5, 39, 0.2),
            )
        self.assertTrue(stopped)
        state = json.loads(output.getvalue())
        self.assertEqual('failed', state['state'])
        self.assertEqual('capacity_exceeded', state['reason'])
        self.assertEqual('stopped', state['publication'])
        self.assertIn('restart', state['restartCondition'])

    def test_fixed_profile_overload_is_also_terminal(self):
        from MarkerDetectionRateController import RateDecision, DetectionWindow
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertTrue(MODULE.enforce_detection_capacity(
                RateDecision(50, False, False, 'downgrade_locked'),
                MODULE.Mly2Topology(1, 10_000_000, 'ready', 1, ('one',)),
                DetectionWindow(5, 19, 0.2),
            ))

    def test_adaptive_downgrade_does_not_stop_publication(self):
        from MarkerDetectionRateController import RateDecision, DetectionWindow
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            self.assertFalse(MODULE.enforce_detection_capacity(
                RateDecision(40, True, False, 'overload_downgrade'),
                MODULE.Mly2Topology(1, 10_000_000, 'green', 1, ('one',)),
                DetectionWindow(5, 19, 0.2),
            ))
        self.assertEqual('', output.getvalue())

    def test_main_closes_writer_and_returns_failure_after_capacity_event(self):
        from MarkerDetectionRateController import RateDecision
        topology = MODULE.Mly2Topology(1, 10_000_000, 'green', 1, ('one',))
        sampled = [SimpleNamespace(source_id='one', reason='no_video', eligible=False)]
        clock = iter(index * 0.1 for index in range(10_000))
        reader = SimpleNamespace(frame_event_available=False, read_topology=lambda: topology)
        reader_context = contextlib.nullcontext(reader)
        writes = []
        class Writer:
            closed = False
            def __enter__(self): return self
            def __exit__(self, *_): self.closed = True
            def write(self, *args, **kwargs):
                self.assert_open()
                writes.append(args)
            def assert_open(self):
                if self.closed: raise AssertionError('write after close')
        writer = Writer()
        controller = SimpleNamespace(
            detection_hz=25,
            observe_window=lambda *_args, **_kwargs: RateDecision(25, False, True, 'capacity_exceeded'),
        )
        with patch.object(MODULE, 'GpuArucoDetector', return_value=SimpleNamespace(cp=None)), \
             patch.object(MODULE, 'AdaptiveDetectionRateController', return_value=controller), \
             patch.object(MODULE, 'open_reader', return_value=reader_context), \
             patch.object(MODULE, 'MarkerObservationSharedMemoryWriter', return_value=writer), \
             patch.object(MODULE, 'allocate_batches', return_value=(None, None, None)), \
             patch.object(MODULE, 'read_sampling_state', return_value=([None], {}, sampled)), \
             patch.object(MODULE.time, 'perf_counter', side_effect=lambda: next(clock)), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = MODULE.main(['--warmup-iterations', '0', '--control-window-seconds', '0.01'])
        self.assertEqual(1, result)
        self.assertTrue(writer.closed)
        self.assertEqual(1, len(writes))

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
