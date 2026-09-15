import importlib.util
import contextlib
import io
import itertools
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
        result, writer, writes, errors = self.run_capacity_scenario("stop")
        self.assertEqual(1, result)
        self.assertTrue(writer.closed)
        self.assertEqual(1, len(writes))
        self.assertIn('"publication": "stopped"', errors)

    def test_main_continues_writing_after_capacity_event_when_opted_in(self):
        result, writer, writes, errors = self.run_capacity_scenario("continue")
        # Finite-run quality validation still fails; continuation does not claim capacity.
        self.assertEqual(1, result)
        self.assertTrue(writer.closed)
        self.assertGreater(len(writes), 1)
        self.assertIn('"state": "degraded"', errors)
        self.assertIn('"publication": "continuing_best_effort"', errors)
        self.assertNotIn('"publication": "stopped"', errors)

    def test_continue_policy_requires_adaptive_mode(self):
        with patch.object(MODULE, 'GpuArucoDetector') as detector, \
             contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
            MODULE.main(['--no-adaptive', '--capacity-policy', 'continue'])
        self.assertEqual(2, error.exception.code)
        detector.assert_not_called()

    def test_continue_policy_downgrades_to_10_and_preserves_overload_evidence(self):
        from MarkerDetectionRateController import AdaptiveDetectionRateController, DetectionWindow
        controller = AdaptiveDetectionRateController(hold_seconds=0)
        topology = MODULE.Mly2Topology(1, 10_000_000, 'green', 1, ('one',))
        output = io.StringIO()
        now = 0
        with contextlib.redirect_stderr(output):
            for expected_hz in (40, 33, 25, 20, 15, 10, 10):
                for _ in range(3):
                    now += 5
                    window = DetectionWindow(5, 1000 / controller.detection_hz, 0.2)
                    decision = controller.observe_window(window, now, True)
                    self.assertFalse(MODULE.enforce_detection_capacity(
                        decision, topology, window, 'continue',
                    ))
                self.assertEqual(expected_hz, decision.detection_hz)
        self.assertTrue(decision.capacity_exceeded)
        state = json.loads(output.getvalue())
        self.assertEqual(10, state['detectionHz'])
        self.assertEqual('degraded', state['state'])
        self.assertNotIn('restartCondition', state)
        healthy = controller.observe_window(DetectionWindow(5, 5, 0), now + 5, True)
        self.assertFalse(healthy.capacity_exceeded)
        self.assertEqual(10, healthy.detection_hz)

    def test_main_retries_upgrade_once_at_same_vehicle_ready_not_in_fixed_mode(self):
        from MarkerDetectionRateController import RateDecision
        for adaptive, expected_calls in ((True, 1), (False, 0)):
            with self.subTest(adaptive=adaptive):
                phases = ('green', 'green', 'ready', 'ready', 'countdown', 'green')
                topologies = [MODULE.Mly2Topology(1, 10_000_000, phase, 1, ('one',)) for phase in phases]
                calls = []
                controller = SimpleNamespace(
                    detection_hz=25,
                    reset_evidence=lambda: None,
                    observe_window=lambda *_args, **_kwargs: RateDecision(25, False, False, 'stable'),
                )
                def prepare(now):
                    calls.append(now)
                    controller.detection_hz = 33
                    return RateDecision(33, True, False, 'prepare_upgrade')
                controller.prepare = prepare
                _, writer, _, _ = self.run_capacity_scenario('auto', topologies, controller, adaptive)
                self.assertEqual(expected_calls, len(calls))
                self.assertEqual([33] if adaptive else [], writer.rates)

    def run_capacity_scenario(self, policy, topologies=None, controller=None, adaptive=True):
        from MarkerDetectionRateController import RateDecision
        topology = MODULE.Mly2Topology(1, 10_000_000, 'green', 1, ('one',))
        sampled = [SimpleNamespace(source_id='one', reason='no_video', eligible=False)]
        clock = iter(index * 0.1 for index in range(10_000))
        topologies = topologies or [topology]
        topology_stream = itertools.chain(topologies, itertools.repeat(topologies[-1]))
        reader = SimpleNamespace(frame_event_available=False, read_topology=lambda: next(topology_stream))
        reader_context = contextlib.nullcontext(reader)
        writes = []
        class Writer:
            closed = False
            def __init__(self): self.rates = []
            def __enter__(self): return self
            def __exit__(self, *_): self.closed = True
            def set_detection_hz(self, hz): self.rates.append(hz)
            def write(self, *args, **kwargs):
                self.assert_open()
                writes.append(args)
            def assert_open(self):
                if self.closed: raise AssertionError('write after close')
        writer = Writer()
        controller = controller or SimpleNamespace(
            detection_hz=25,
            reset_evidence=lambda: None,
            observe_window=lambda *_args, **_kwargs: RateDecision(25, False, True, 'capacity_exceeded'),
        )
        errors = io.StringIO()
        with patch.object(MODULE, 'GpuArucoDetector', return_value=SimpleNamespace(cp=None)), \
             patch.object(MODULE, 'AdaptiveDetectionRateController', return_value=controller), \
             patch.object(MODULE, 'open_reader', return_value=reader_context), \
             patch.object(MODULE, 'MarkerObservationSharedMemoryWriter', return_value=writer), \
             patch.object(MODULE, 'allocate_batches', return_value=(None, None, None)), \
             patch.object(MODULE, 'read_sampling_state', return_value=([None], {}, sampled)), \
             patch.object(MODULE.time, 'perf_counter', side_effect=lambda: next(clock)), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(errors):
            result = MODULE.main([
                '--warmup-iterations', '0', '--control-window-seconds', '0.01',
                '--duration-seconds', '10', '--capacity-policy', policy,
            ] + ([] if adaptive else ['--no-adaptive']))
        return result, writer, writes, errors.getvalue()

    def test_processing_duration_excludes_frame_wait(self):
        self.assertAlmostEqual(3.25, MODULE.processing_duration_ms(18.25, 15.0))

    def test_processing_duration_never_becomes_negative(self):
        self.assertEqual(0.0, MODULE.processing_duration_ms(4.0, 5.0))

    def test_fresh_frame_wait_does_not_trigger_minimum_rate_capacity_failure(self):
        from MarkerDetectionRateController import AdaptiveDetectionRateController, DetectionWindow

        controller = AdaptiveDetectionRateController(initial_detection_hz=25)
        # Real failure shape: a 47 ms epoch, of which 36 ms waits for the
        # other camera. Repeated boundary overruns used to stop publication.
        processing_ms = MODULE.processing_duration_ms(47.0, 36.0)
        missed = MODULE.processing_deadline_missed(0.007, processing_ms, 0.04)
        for index in range(3):
            decision = controller.observe_window(
                DetectionWindow(5, processing_ms, float(missed)),
                (index + 1) * 5, allow_downgrade=True,
            )
        self.assertFalse(decision.capacity_exceeded)
        self.assertEqual(25, decision.detection_hz)

    def test_processing_overrun_and_skipped_epoch_remain_deadline_misses(self):
        self.assertTrue(MODULE.processing_deadline_missed(0, 41.0, 0.04))
        self.assertTrue(MODULE.processing_deadline_missed(0.04, 3.0, 0.04))
        self.assertFalse(MODULE.processing_deadline_missed(0, 40.0, 0.04))

    def test_parser_defaults_to_strict_fresh_frame_coverage(self):
        args = MODULE.build_parser().parse_args([])

        self.assertEqual(5.0, args.fresh_frame_wait_ms)
        self.assertEqual(0.95, args.minimum_fresh_tick_ratio)
        self.assertEqual("sampled", args.profiling_mode)
        self.assertTrue(args.adaptive)
        self.assertEqual(50, args.initial_detection_hz)
        self.assertEqual("auto", args.capacity_policy)
        self.assertEqual("continue", MODULE.resolve_capacity_policy(args.capacity_policy, args.adaptive))

    def test_fixed_mode_keeps_stop_policy_and_lower_initial_rates_are_valid(self):
        args = MODULE.build_parser().parse_args(['--no-adaptive'])
        self.assertEqual("stop", MODULE.resolve_capacity_policy(args.capacity_policy, args.adaptive))
        self.assertEqual("stop", MODULE.resolve_capacity_policy("stop", True))
        for hz in (20, 15, 10):
            args = MODULE.build_parser().parse_args(['--initial-detection-hz', str(hz)])
            self.assertEqual(hz, args.initial_detection_hz)

    def test_same_vehicle_next_ready_is_preparation_not_every_ready_tick(self):
        def topology(phase, generation=1, sources=('one', 'two')):
            return MODULE.Mly2Topology(generation, 10_000_000, phase, 1, sources)
        for phase in ('green', 'finished', 'aborted', 'idle'):
            self.assertTrue(MODULE.is_preparation_transition(topology(phase), topology('ready')))
        for previous, current in (
            (None, topology('ready')),
            (topology('ready'), topology('ready')),
            (topology('ready'), topology('countdown')),
            (topology('countdown'), topology('green')),
            (topology('green'), topology('ready', generation=2)),
            (topology('green'), topology('ready', sources=('three',))),
        ):
            self.assertFalse(MODULE.is_preparation_transition(previous, current))

    def test_window_readiness_requires_all_sources_and_real_epoch_rate(self):
        from collections import Counter
        counts = Counter(one=125, two=125)
        ready = MODULE.window_input_ready
        self.assertTrue(ready(('one', 'two'), counts, 125, 5, 25, 0.95))
        self.assertFalse(ready(('one', 'two'), Counter(one=125), 125, 5, 25, 0.95))
        self.assertFalse(ready(('one', 'two'), counts, 125, 60, 25, 0.95))
        self.assertFalse(ready((), Counter(), 125, 5, 25, 0.95))

    def test_window_report_separates_measured_rate_from_next_profile_and_fresh_sources(self):
        from collections import Counter
        from MarkerDetectionRateController import RateDecision, DetectionWindow
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            MODULE.report_detection_window(
                MODULE.Mly2Topology(1, 10_000_000, 'green', 1, ('one', 'two')),
                DetectionWindow(5, 35, 0.2, input_ready=False),
                25, RateDecision(20, True, False, 'overload_downgrade'),
                120, Counter(one=100, two=110),
            )
        status = json.loads(output.getvalue())
        self.assertEqual(25, status['measuredDetectionHz'])
        self.assertEqual(20, status['detectionHz'])
        self.assertEqual(24, status['effectiveEpochHz'])
        self.assertEqual([20, 22], [s['effectiveDetectionHz'] for s in status['sources']])
        self.assertEqual('overload_downgrade', status['reason'])
        self.assertGreater(status['atUnixNs'], 0)
        self.assertFalse(status['inputReady'])

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
