import importlib.util
import pathlib
import sys
import unittest
import contextlib
import io
from unittest.mock import patch
from types import SimpleNamespace

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from MarkerMetadataGap import MetadataGapTracker
from MarkerLumaV2 import SOURCE_CONNECTED, VIDEO_VALID, Mly2SourceSnapshot, Mly2Topology

spec = importlib.util.spec_from_file_location("metadata_gap_worker", pathlib.Path(__file__).with_name("Run-GpuMarkerObserverLumaV2.py"))
worker = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = worker
spec.loader.exec_module(worker)


class MetadataGapTest(unittest.TestCase):
    def setUp(self):
        self.gaps = MetadataGapTracker()
        self.topology = Mly2Topology(1, 1000, "green", 1, ("one", "two"))

    def snapshot(self, tick, sequence=10, flags=SOURCE_CONNECTED | VIDEO_VALID, source_id="one"):
        return Mly2SourceSnapshot(source_id, 0, sequence, tick, tick * 1_000_000, sequence, 0, flags)

    def sample(self, tick, snapshot, previous=None):
        reader = SimpleNamespace(read_sources=lambda _: [snapshot, self.snapshot(tick, source_id="two")],
                                 query_performance_counter=lambda: tick)
        return worker.read_sampling_state(reader, self.topology, previous or {}, 60, 40, self.gaps)

    def publish(self, by_id, sampled):
        written = []
        writer = SimpleNamespace(write=lambda *args, **kwargs: written.extend(args[1]))
        worker.publish_invalid_batch(writer, self.topology, by_id, sampled, self.gaps)
        return written

    def test_short_gap_omits_only_affected_source_then_detects_fresh_recovery(self):
        self.sample(100, self.snapshot(100))
        _, by_id, sampled = self.sample(110, None)
        self.assertEqual(["metadata_pending", "selected"], [s.reason for s in sampled])
        self.assertEqual([], self.publish(by_id, sampled))
        self.assertEqual(10, by_id["one"].source_sequence)
        self.assertFalse(sampled[0].eligible)
        _, _, sampled = self.sample(169, self.snapshot(169, 11))
        self.assertTrue(sampled[0].eligible)
        self.assertEqual(1, self.gaps.counts["metadata_recovered"])

    def test_timeout_debt_survives_retries_and_requires_successful_write(self):
        self.sample(100, self.snapshot(100))
        self.sample(110, None)
        for tick in (170, 180, 210):
            _, by_id, sampled = self.sample(tick, self.snapshot(tick, 11))
            self.assertEqual("metadata_timeout", sampled[0].reason)
            self.assertFalse(sampled[0].eligible)
        def fail(*args, **kwargs):
            raise OSError("writer unavailable")
        with self.assertRaises(OSError):
            worker.publish_invalid_batch(SimpleNamespace(write=fail), self.topology, by_id, sampled, self.gaps)
        self.assertFalse(self.gaps.gaps["one"].invalidated)
        invalid = self.publish(by_id, sampled)
        self.assertEqual(0, invalid[0].source_sequence)
        self.assertFalse(invalid[0].video_valid)
        self.assertTrue(self.sample(211, self.snapshot(211, 11))[2][0].eligible)

    def test_next_epoch_late_recovery_cannot_bypass_timeout_at_any_profile(self):
        for hz in (50, 40, 33, 25, 20, 15, 10):
            with self.subTest(hz=hz):
                self.gaps = MetadataGapTracker()
                self.sample(100, None)
                tick = 100 + max(60, round(1000 / hz))
                _, by_id, sampled = self.sample(tick, self.snapshot(tick))
                self.assertEqual("metadata_timeout", sampled[0].reason)
                self.assertEqual(1, len(self.publish(by_id, sampled)))

    def test_continued_unreadability_invalidates_once_without_synthetic_empty_frames(self):
        self.sample(100, self.snapshot(100))
        self.sample(110, None)
        _, by_id, sampled = self.sample(170, None)
        invalid = self.publish(by_id, sampled)
        self.assertEqual(0, invalid[0].source_sequence)
        for tick in range(180, 400):
            _, by_id, sampled = self.sample(tick, None)
            self.assertFalse(sampled[0].eligible)
            self.assertEqual([], self.publish(by_id, sampled))
        self.assertEqual(1, self.gaps.counts["metadata_timeout"])

    def test_stable_disconnected_stale_future_and_frozen_frames_remain_invalid(self):
        for snapshot, expected, previous in (
            (self.snapshot(115, flags=0), "no_video", {}),
            (self.snapshot(1), "stale", {}),
            (self.snapshot(1), "stale", {"one": 10}),
            (self.snapshot(130), "future_timestamp", {}),
        ):
            with self.subTest(expected=expected, previous=previous):
                self.gaps = MetadataGapTracker()
                self.sample(100, None)
                _, by_id, sampled = self.sample(120, snapshot, previous)
                self.assertEqual(expected, sampled[0].reason)
                self.assertEqual(1, len(self.publish(by_id, sampled)))

    def test_duplicate_recovery_is_not_an_absent_or_detection_observation(self):
        self.sample(100, self.snapshot(100))
        self.sample(105, None)
        _, by_id, sampled = self.sample(110, self.snapshot(100), {"one": 10})
        self.assertEqual("duplicate_or_rollback", sampled[0].reason)
        self.assertFalse(sampled[0].eligible)
        self.assertEqual([], self.publish(by_id, sampled))

    def test_generation_reset_discards_cached_metadata_and_gap_deadline(self):
        self.sample(100, self.snapshot(100))
        self.sample(105, None)
        self.gaps.change_generation(2)
        self.assertFalse(self.gaps.snapshots)
        self.assertTrue(self.sample(500, self.snapshot(500, 1))[2][0].eligible)

    def test_qpc_rollback_and_zero_hold_limit_fail_closed(self):
        self.gaps.classify("one", None, 100, 60)
        self.assertEqual("metadata_timeout", self.gaps.classify("one", self.snapshot(1), 1, 60))
        self.assertEqual("metadata_timeout", self.gaps.classify("two", None, 100, 0))

    def test_diagnostics_are_bounded_and_report_only_when_changed(self):
        for tick in range(100):
            self.gaps.classify("one", None, tick * 10, 60)
            self.gaps.classify("one", self.snapshot(tick * 10 + 1), tick * 10 + 1, 60)
        report = self.gaps.take_report(1, 999)
        self.assertEqual(32, len(report["recent"]))
        self.assertEqual(168, report["omittedEvents"])
        self.assertEqual(100, report["counts"]["metadata_recovered"])
        self.assertIsNone(self.gaps.take_report(1, 1000))

    def run_loop(self, gap_end=0, generation_change=False, topology_gap=False):
        class Clock:
            now = 0.0
            def read(self):
                self.now += 0.0001
                return self.now
            def sleep(self, seconds): self.now += seconds
        clock = Clock()
        writes = []
        def topology():
            if topology_gap and 0.025 < clock.now < gap_end:
                return None
            return Mly2Topology(2 if generation_change and clock.now > 0.07 else 1,
                                1_000_000_000, "green", 1, ("one", "two"))
        def read_sources(current):
            seq = int(clock.now / 0.02) + 1
            return [None if source_id == "one" and 0.025 < clock.now < gap_end else
                    Mly2SourceSnapshot(source_id, index, seq, int(clock.now * 1e9),
                                       int(clock.now * 1e9), seq, 0, 3)
                    for index, source_id in enumerate(current.source_ids)]
        def wait(seconds):
            clock.sleep(min(seconds, 0.005))
            return True
        reader = SimpleNamespace(frame_event_available=True, read_topology=topology,
            read_sources=read_sources, query_performance_counter=lambda: int(clock.now * 1e9),
            copy_plane=lambda *_: True, wait_for_frame=wait)
        writer = SimpleNamespace(write=lambda at, observations, **kwargs: writes.append((clock.now, list(observations))))
        def detect(cp, detector, output, current, eligible, host, device, metrics, previous,
                   sampled_at, profile, additional=None):
            if not eligible: return None
            observations = list(additional or [])
            for snapshot in eligible:
                previous[snapshot.source_id] = snapshot.source_sequence
                metric = metrics[snapshot.source_id]
                metric.detected_frames += 1
                metric.last_sequence = snapshot.source_sequence
                metric.last_eligible_at = sampled_at
                observations.append(worker.SourceObservation(snapshot.slot_index, snapshot.source_id,
                    snapshot.source_sequence, snapshot.received_unix_ns, snapshot.received_unix_ns,
                    True, 0, []))
            output.write(0, observations, batch_flags=worker.BATCH_PARTIAL)
            return worker.DetectionBatchExecution(frozenset(s.source_id for s in eligible), clock.now, 0, {}, {})
        with patch.object(worker, "GpuArucoDetector", return_value=SimpleNamespace(cp=None)), \
             patch.object(worker, "open_reader", return_value=contextlib.nullcontext(reader)), \
             patch.object(worker, "MarkerObservationSharedMemoryWriter", return_value=contextlib.nullcontext(writer)), \
             patch.object(worker, "allocate_batches", return_value=(None, [None, None], object())), \
             patch.object(worker, "execute_detection_batch", side_effect=detect), \
             patch.object(worker.time, "perf_counter", side_effect=clock.read), \
             patch.object(worker.time, "sleep", side_effect=clock.sleep), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            worker.main(["--no-adaptive", "--duration-seconds", "0.25", "--warmup-iterations", "0"])
        return [(at, observation) for at, batch in writes for observation in batch]

    def test_worker_loop_short_gap_recovers_in_microbatch_without_invalidating_other_car(self):
        output = self.run_loop(0.046)
        self.assertTrue(output)
        self.assertTrue(all(o.video_valid for _, o in output))
        recovered = [at for at, o in output if o.source_id == "one" and 0.045 < at < 0.06]
        self.assertTrue(recovered, "short gap did not recover in the late micro-batch")

    def test_worker_loop_timeout_and_topology_unreadability_reset_before_recovery(self):
        for topology_gap in (False, True):
            with self.subTest(topology_gap=topology_gap):
                output = self.run_loop(0.16, topology_gap=topology_gap)
                invalid = [(at, o) for at, o in output if o.source_id == "one" and not o.video_valid]
                self.assertEqual(1, len(invalid))
                self.assertEqual(0, invalid[0][1].source_sequence)
                self.assertGreaterEqual(invalid[0][0], 0.1)
                self.assertTrue(any(at > 0.16 and o.source_id == "one" and o.video_valid for at, o in output))
                if not topology_gap:
                    self.assertTrue(all(o.video_valid for _, o in output if o.source_id == "two"))

    def test_worker_loop_generation_boundary_resets_both_sources_before_new_detections(self):
        output = self.run_loop(generation_change=True)
        invalid = [(at, o) for at, o in output if not o.video_valid]
        self.assertEqual({"one", "two"}, {o.source_id for _, o in invalid})
        self.assertTrue(all(o.source_sequence == 0 for _, o in invalid))
        for _, invalid_source in invalid:
            position = next(i for i, (_, o) in enumerate(output) if o is invalid_source)
            self.assertTrue(any(o.video_valid and o.source_id == invalid_source.source_id for _, o in output[position + 1:]))


if __name__ == "__main__":
    unittest.main()
