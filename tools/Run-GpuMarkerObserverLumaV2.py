#!/usr/bin/env python3
"""Detect dynamic MLY2 marker sources with one adaptive GPU batch worker."""

from __future__ import annotations

import argparse
from collections import Counter, deque
from dataclasses import dataclass, field
import json
import math
from pathlib import Path
import sys
import time

import numpy as np

from GpuArucoDetector import GpuArucoDetector
from MarkerDetectionRateController import (
    AdaptiveDetectionRateController,
    DetectionWindow,
)
from MarkerFrameSampler import SourceFrameState, sample_latest_frames
from MarkerRuntimeMetrics import DurationDistribution
from MarkerLumaV2 import (
    HEIGHT,
    MAX_SOURCES,
    VIDEO_VALID,
    WIDTH,
    Mly2SourceSnapshot,
    Mly2Topology,
    open_reader,
)
from MarkerObservationIpc import (
    BATCH_PARTIAL,
    DEFAULT_MAPPING_NAME,
    MarkerDetection,
    MarkerObservationSharedMemoryWriter,
    SourceObservation,
)


RESERVED_MARKER_IDS = frozenset({17, 34, 37})
DEFAULT_ALLOWED_MARKER_IDS = frozenset(set(range(50)) - RESERVED_MARKER_IDS)
DEFAULT_PROFILES = (50, 40, 33, 25)
DEFAULT_FRESH_FRAME_WAIT_MS = 5.0
DEFAULT_MINIMUM_FRESH_TICK_RATIO = 0.95
WAITABLE_SAMPLING_REASONS = frozenset(
    {"duplicate_or_rollback", "skewed"}
)
INVALIDATING_SAMPLING_REASONS = frozenset(
    {"no_video", "stale", "future_timestamp"}
)


def parse_marker_ids(value: str) -> frozenset[int]:
    try:
        marker_ids = frozenset(
            int(part.strip()) for part in value.split(",") if part.strip()
        )
    except ValueError as exc:
        raise argparse.ArgumentTypeError(
            "marker IDs must be comma-separated integers"
        ) from exc
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    reserved = marker_ids & RESERVED_MARKER_IDS
    if reserved:
        raise argparse.ArgumentTypeError(
            "reserved marker IDs are not allowed: "
            + ",".join(str(marker_id) for marker_id in sorted(reserved))
        )
    return marker_ids


def percentile(values, percent: float) -> float:
    if isinstance(values, DurationDistribution):
        return values.percentile(percent)
    ordered = sorted(values)
    if not ordered:
        return 0.0
    index = max(
        0,
        min(len(ordered) - 1, math.ceil(len(ordered) * percent / 100.0) - 1),
    )
    return ordered[index]


@dataclass
class SourceMetrics:
    detected_frames: int = 0
    last_sequence: int = 0
    last_marker_ids: tuple[int, ...] = ()
    last_eligible_at: float = 0.0
    age_ms: DurationDistribution = field(default_factory=DurationDistribution)
    marker_instances_by_id: Counter[int] = field(default_factory=Counter)
    marker_id_frames: Counter[int] = field(default_factory=Counter)
    sampling_reasons: Counter[str] = field(default_factory=Counter)
    first_receiver_frame_count: int | None = None
    last_receiver_frame_count: int = 0
    first_receiver_seen_at: float = 0.0
    last_receiver_seen_at: float = 0.0

    def observe_receiver(self, snapshot: Mly2SourceSnapshot, observed_at: float) -> None:
        if self.first_receiver_frame_count is None:
            self.first_receiver_frame_count = snapshot.frame_count
            self.first_receiver_seen_at = observed_at
        self.last_receiver_frame_count = snapshot.frame_count
        self.last_receiver_seen_at = observed_at


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--input-mapping-name", default=r"Local\MomoMarkerLumaV2"
    )
    parser.add_argument("--output-mapping-name", default=DEFAULT_MAPPING_NAME)
    parser.add_argument("--duration-seconds", type=float, default=0.0)
    parser.add_argument("--wait-for-mapping-seconds", type=float, default=20.0)
    parser.add_argument("--status-interval-seconds", type=float, default=1.0)
    parser.add_argument("--required-source-count", type=int, default=0)
    parser.add_argument("--maximum-frame-age-ms", type=float, default=60.0)
    parser.add_argument("--maximum-batch-skew-ms", type=float, default=40.0)
    parser.add_argument(
        "--fresh-frame-wait-ms",
        type=float,
        default=DEFAULT_FRESH_FRAME_WAIT_MS,
        help="Bounded wait when every live source still has only a previously detected frame",
    )
    parser.add_argument("--control-window-seconds", type=float, default=5.0)
    parser.add_argument("--warmup-iterations", type=int, default=2)
    parser.add_argument(
        "--profiling-mode",
        choices=("off", "sampled", "full"),
        default="sampled",
    )
    parser.add_argument("--profile-every-ticks", type=int, default=50)
    parser.add_argument(
        "--minimum-fresh-tick-ratio",
        type=float,
        default=DEFAULT_MINIMUM_FRESH_TICK_RATIO,
    )
    parser.add_argument(
        "--initial-detection-hz",
        type=int,
        choices=DEFAULT_PROFILES,
        default=DEFAULT_PROFILES[0],
    )
    parser.add_argument(
        "--adaptive",
        action=argparse.BooleanOptionalAction,
        default=True,
    )
    parser.add_argument(
        "--allowed-marker-ids",
        type=parse_marker_ids,
        default=DEFAULT_ALLOWED_MARKER_IDS,
    )
    parser.add_argument("--output")
    return parser


def allocate_batches(cp, source_count: int):
    if source_count < 1:
        return None, None, None
    elements = source_count * HEIGHT * WIDTH
    pinned_owner = cp.cuda.alloc_pinned_memory(elements)
    host = np.frombuffer(pinned_owner, dtype=np.uint8, count=elements).reshape(
        source_count, HEIGHT, WIDTH
    )
    device = cp.empty((source_count, HEIGHT, WIDTH), dtype=cp.uint8)
    return pinned_owner, host, device


def distribution(values) -> dict[str, float | int]:
    if isinstance(values, DurationDistribution):
        return values.report()
    return {
        "samples": len(values),
        "p50": round(percentile(values, 50), 3),
        "p95": round(percentile(values, 95), 3),
        "p99": round(percentile(values, 99), 3),
        "maximum": round(max(values, default=0.0), 3),
    }


def enforce_detection_capacity(decision, topology, window):
    """Emit a machine-readable terminal failure before any further IPC write."""
    if not decision.capacity_exceeded and decision.reason != "downgrade_locked":
        return False
    print(json.dumps({
        "type": "marker_worker_status",
        "version": 1,
        "state": "failed",
        "reason": "capacity_exceeded",
        "phase": topology.phase,
        "generation": topology.generation,
        "sourceCount": len(topology.source_ids),
        "detectionHz": decision.detection_hz,
        "processingP95Ms": round(window.cycle_p95_ms, 3),
        "deadlineMissRatio": window.deadline_miss_ratio,
        "publication": "stopped",
        "restartCondition": "reduce_sources_or_add_marker_node_then_restart",
    }), file=sys.stderr, flush=True)
    return True


def processing_duration_ms(cycle_ms: float, wait_ms: float) -> float:
    """Return active processing time after removing bounded frame wait time."""
    return max(0.0, cycle_ms - wait_ms)


def pending_fresh_source_count(sampled, excluded_source_ids=frozenset()) -> int:
    return sum(
        selection.source_id not in excluded_source_ids
        and selection.reason in WAITABLE_SAMPLING_REASONS
        for selection in sampled
    )


def read_sampling_state(
    reader,
    topology: Mly2Topology,
    last_detected_sequences: dict[str, int],
    maximum_age_ticks: int,
    maximum_skew_ticks: int,
):
    snapshots = reader.read_sources(topology)
    sample_tick = reader.query_performance_counter()
    states = []
    snapshots_by_id: dict[str, Mly2SourceSnapshot] = {}
    for slot_index, source_id in enumerate(topology.source_ids):
        snapshot = snapshots[slot_index]
        if snapshot is None:
            states.append(SourceFrameState(source_id, 0, 0, False))
            continue
        snapshots_by_id[source_id] = snapshot
        states.append(
            SourceFrameState(
                source_id=source_id,
                source_sequence=snapshot.source_sequence,
                received_tick=snapshot.received_qpc,
                video_valid=snapshot.connected
                and snapshot.video_valid
                and bool(snapshot.flags & VIDEO_VALID),
            )
        )
    sampled = sample_latest_frames(
        states,
        sample_tick,
        last_detected_sequences,
        maximum_age_ticks,
        maximum_skew_ticks,
    )
    return snapshots, snapshots_by_id, sampled


def build_invalid_observations(
    topology: Mly2Topology,
    snapshots_by_id: dict[str, Mly2SourceSnapshot],
    sampled,
    detected_at_unix_ns: int,
) -> list[SourceObservation]:
    source_indexes = {
        source_id: slot_index
        for slot_index, source_id in enumerate(topology.source_ids)
    }
    observations = []
    for selection in sampled:
        if selection.reason not in INVALIDATING_SAMPLING_REASONS:
            continue
        snapshot = snapshots_by_id.get(selection.source_id)
        observations.append(
            SourceObservation(
                source_index=source_indexes[selection.source_id],
                source_id=selection.source_id,
                source_sequence=(snapshot.source_sequence if snapshot else 0),
                frame_received_at_unix_ns=(
                    snapshot.received_unix_ns if snapshot else 0
                ),
                detected_at_unix_ns=detected_at_unix_ns,
                video_valid=False,
                candidate_count=0,
                detections=[],
            )
        )
    return observations


@dataclass(frozen=True)
class DetectionBatchExecution:
    processed_source_ids: frozenset[str]
    detected_at_monotonic: float
    marker_instances: int
    stage_ms: dict[str, float]
    profiled_stage_ms: dict[str, float]


def execute_detection_batch(
    cp,
    detector,
    writer,
    topology: Mly2Topology,
    eligible: list[Mly2SourceSnapshot],
    host_planes,
    device_planes,
    metrics: dict[str, SourceMetrics],
    last_detected_sequences: dict[str, int],
    sampled_at: float,
    profile_cycle: bool,
    additional_observations: list[SourceObservation] | None = None,
) -> DetectionBatchExecution | None:
    if not eligible or host_planes is None or device_planes is None:
        return None

    batch_stage_ms: dict[str, float] = {}
    profiled: dict[str, float] = {}
    h2d_started = time.perf_counter()
    h2d_started_event = cp.cuda.Event() if profile_cycle else None
    h2d_finished_event = cp.cuda.Event() if profile_cycle else None
    if profile_cycle:
        h2d_started_event.record()
    device_planes[: len(eligible)].set(host_planes[: len(eligible)])
    if profile_cycle:
        h2d_finished_event.record()
        h2d_finished_event.synchronize()
        profiled["h2dGpu"] = cp.cuda.get_elapsed_time(
            h2d_started_event, h2d_finished_event
        )
    batch_stage_ms["h2dWall"] = (time.perf_counter() - h2d_started) * 1000.0

    detector_timings = {} if profile_cycle else None
    detector_started = time.perf_counter()
    results = detector.detect_batch(
        device_planes[: len(eligible)], detector_timings
    )
    batch_stage_ms["detectorWall"] = (
        time.perf_counter() - detector_started
    ) * 1000.0
    if detector_timings is not None:
        profiled.update(detector_timings)

    detected_at_unix_ns = time.time_ns()
    detected_at_monotonic = time.perf_counter()
    observation_started = time.perf_counter()
    source_indexes = {
        source_id: slot_index
        for slot_index, source_id in enumerate(topology.source_ids)
    }
    observations = list(additional_observations or ())
    marker_instances = 0
    for snapshot, result in zip(eligible, results, strict=True):
        detections = [
            MarkerDetection(
                marker.marker_id,
                marker.center_x,
                marker.center_y,
                marker.area,
            )
            for marker in result.markers
        ]
        marker_instances += len(detections)
        source_id = snapshot.source_id
        source_sequence = snapshot.source_sequence
        last_detected_sequences[source_id] = source_sequence
        source_metrics = metrics[source_id]
        source_metrics.detected_frames += 1
        source_metrics.last_sequence = source_sequence
        source_metrics.last_eligible_at = sampled_at
        source_metrics.last_marker_ids = tuple(
            detection.marker_id for detection in detections
        )
        marker_ids = [detection.marker_id for detection in detections]
        source_metrics.marker_instances_by_id.update(marker_ids)
        source_metrics.marker_id_frames.update(set(marker_ids))
        source_metrics.age_ms.append(
            max(
                0.0,
                (detected_at_unix_ns - snapshot.received_unix_ns) / 1_000_000.0,
            )
        )
        observations.append(
            SourceObservation(
                source_index=source_indexes[source_id],
                source_id=source_id,
                source_sequence=source_sequence,
                frame_received_at_unix_ns=snapshot.received_unix_ns,
                detected_at_unix_ns=detected_at_unix_ns,
                video_valid=True,
                candidate_count=result.candidate_count,
                detections=detections,
            )
        )
    batch_stage_ms["observationBuild"] = (
        time.perf_counter() - observation_started
    ) * 1000.0

    ipc_started = time.perf_counter()
    writer.write(
        detected_at_unix_ns,
        observations,
        batch_flags=BATCH_PARTIAL,
    )
    batch_stage_ms["ipcWrite"] = (time.perf_counter() - ipc_started) * 1000.0
    return DetectionBatchExecution(
        processed_source_ids=frozenset(snapshot.source_id for snapshot in eligible),
        detected_at_monotonic=detected_at_monotonic,
        marker_instances=marker_instances,
        stage_ms=batch_stage_ms,
        profiled_stage_ms=profiled,
    )


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.duration_seconds < 0 or args.wait_for_mapping_seconds < 0:
        parser.error("duration and mapping wait must be zero or positive")
    if args.status_interval_seconds <= 0 or args.control_window_seconds <= 0:
        parser.error("status and control window intervals must be positive")
    if args.required_source_count < 0 or args.required_source_count > MAX_SOURCES:
        parser.error(f"--required-source-count must be in 0..{MAX_SOURCES}")
    if (
        args.maximum_frame_age_ms < 0
        or args.maximum_batch_skew_ms < 0
        or args.fresh_frame_wait_ms < 0
    ):
        parser.error(
            "frame age, batch skew, and fresh-frame wait must not be negative"
        )
    if args.warmup_iterations < 0:
        parser.error("warmup iterations must not be negative")
    if args.profile_every_ticks < 1:
        parser.error("profile interval must be positive")
    if not 0 < args.minimum_fresh_tick_ratio <= 1:
        parser.error("minimum fresh tick ratio must be in (0, 1]")

    detector = GpuArucoDetector(allowed_marker_ids=args.allowed_marker_ids)
    cp = detector.cp
    controller = AdaptiveDetectionRateController(
        profiles_hz=DEFAULT_PROFILES,
        initial_detection_hz=args.initial_detection_hz,
    )
    metrics: dict[str, SourceMetrics] = {}
    profile_history = deque([
        {"atSeconds": 0.0, "detectionHz": controller.detection_hz, "reason": "initial"}
    ], maxlen=128)
    profile_history_count = 1
    reason_counts: Counter[str] = Counter()
    processing_window = DurationDistribution()
    all_cycle_ms = DurationDistribution()
    all_processing_ms = DurationDistribution()
    deadline_misses = 0
    tick_count = 0
    detection_epochs = 0
    published_batches = 0
    marker_instances = 0
    capacity_exceeded = False
    topology_changes = 0
    warmed_generation: int | None = None
    warmup_duration_ms = 0.0
    fresh_frame_waits = 0
    fresh_frame_wait_ms = DurationDistribution()
    micro_batch_waits = 0
    micro_batch_wait_ms = DurationDistribution()
    micro_batch_added_sources = 0
    frame_event_signals = 0
    frame_event_timeouts = 0
    eligible_source_samples = 0
    initial_eligible_source_samples = 0
    total_source_samples = 0
    total_deadline_misses = 0
    batches_per_epoch = DurationDistribution()
    inter_batch_skew_ms = DurationDistribution()
    stage_ms = {
        "metadataRead": DurationDistribution(),
        "freshFrameWait": fresh_frame_wait_ms,
        "microBatchWait": micro_batch_wait_ms,
        "sharedPlaneCopy": DurationDistribution(),
        "h2dWall": DurationDistribution(),
        "detectorWall": DurationDistribution(),
        "observationBuild": DurationDistribution(),
        "ipcWrite": DurationDistribution(),
    }
    profiled_stage_ms: dict[str, DurationDistribution] = {}
    profiled_cycles = 0
    measured_started_at = time.perf_counter()
    final_topology: Mly2Topology | None = None
    frame_event_available = False

    try:
        with open_reader(
            args.input_mapping_name, args.wait_for_mapping_seconds
        ) as reader, MarkerObservationSharedMemoryWriter(
            args.output_mapping_name, controller.detection_hz
        ) as writer:
            frame_event_available = reader.frame_event_available
            topology: Mly2Topology | None = None
            pinned_owner = None
            host_planes = None
            device_planes = None
            last_detected_sequences: dict[str, int] = {}
            next_tick = time.perf_counter()
            window_started = next_tick
            last_status_at = next_tick
            measured_started_at = next_tick

            print(
                f"Dynamic Marker Observer: input={args.input_mapping_name} "
                f"output={args.output_mapping_name} adaptive={args.adaptive}",
                flush=True,
            )

            while (
                args.duration_seconds == 0
                or time.perf_counter() - measured_started_at < args.duration_seconds
            ):
                now = time.perf_counter()
                if now < next_tick:
                    time.sleep(min(next_tick - now, 0.002))
                    continue

                current_topology = reader.read_topology()
                if current_topology is None:
                    reason_counts["unstable_topology"] += 1
                    next_tick = now + 1.0 / controller.detection_hz
                    continue
                if topology is None or current_topology.generation != topology.generation:
                    topology = current_topology
                    final_topology = topology
                    topology_changes += 1
                    last_detected_sequences.clear()
                    pinned_owner, host_planes, device_planes = allocate_batches(
                        cp, len(topology.source_ids)
                    )
                    # Retire metrics for removed sources at the generation boundary.
                    metrics = {
                        source_id: metrics[source_id] if source_id in metrics else SourceMetrics()
                        for source_id in topology.source_ids
                    }
                    if topology_changes > 1:
                        decision = controller.prepare(now)
                        if decision.changed:
                            writer.set_detection_hz(decision.detection_hz)
                            profile_history_count += 1
                            profile_history.append(
                                {
                                    "atSeconds": round(now - measured_started_at, 3),
                                    "detectionHz": decision.detection_hz,
                                    "reason": decision.reason,
                                }
                            )
                    print(
                        f"MLY2 generation={topology.generation} "
                        f"phase={topology.phase} sources={len(topology.source_ids)}",
                        flush=True,
                    )
                else:
                    topology = current_topology
                    final_topology = topology

                period = 1.0 / controller.detection_hz
                scheduled_at = next_tick
                schedule_late = max(0.0, now - scheduled_at)
                tick_missed_deadline = schedule_late >= period
                if tick_missed_deadline:
                    deadline_misses += 1
                    total_deadline_misses += 1
                    skipped_periods = math.floor(schedule_late / period)
                    next_tick = scheduled_at + (skipped_periods + 1) * period
                else:
                    next_tick = scheduled_at + period
                tick_count += 1
                detection_epochs += 1
                cycle_started = time.perf_counter()

                maximum_age_ticks = int(
                    topology.qpc_frequency * args.maximum_frame_age_ms / 1000.0
                )
                maximum_skew_ticks = int(
                    topology.qpc_frequency * args.maximum_batch_skew_ms / 1000.0
                )
                metadata_read_ms = 0.0
                waited_for_fresh_frame = False
                current_fresh_wait_ms = 0.0
                current_micro_batch_wait_ms = 0.0
                initial_eligible_count: int | None = None
                fresh_wait_deadline = min(
                    next_tick,
                    time.perf_counter() + args.fresh_frame_wait_ms / 1000.0,
                )
                while True:
                    metadata_started = time.perf_counter()
                    snapshots, snapshots_by_id, sampled = read_sampling_state(
                        reader,
                        topology,
                        last_detected_sequences,
                        maximum_age_ticks,
                        maximum_skew_ticks,
                    )
                    metadata_read_ms += (
                        time.perf_counter() - metadata_started
                    ) * 1000.0
                    eligible_count = sum(
                        selection.eligible for selection in sampled
                    )
                    if initial_eligible_count is None:
                        initial_eligible_count = eligible_count
                    pending_count = pending_fresh_source_count(sampled)
                    if eligible_count > 0:
                        break
                    else:
                        if pending_count == 0:
                            break
                        wait_deadline = fresh_wait_deadline
                    remaining = wait_deadline - time.perf_counter()
                    if remaining <= 0:
                        break
                    waited_for_fresh_frame = True
                    event_wait_started = time.perf_counter()
                    signaled = reader.wait_for_frame(remaining)
                    event_wait_ms = (
                        time.perf_counter() - event_wait_started
                    ) * 1000.0
                    current_fresh_wait_ms += event_wait_ms
                    if signaled:
                        frame_event_signals += 1
                    else:
                        frame_event_timeouts += 1
                sampled_at = time.perf_counter()

                eligible: list[Mly2SourceSnapshot] = []
                copy_started = time.perf_counter()
                for selection in sampled:
                    if not selection.eligible:
                        continue
                    snapshot = snapshots_by_id.get(selection.source_id)
                    if snapshot is None or host_planes is None:
                        reason_counts["unstable_metadata"] += 1
                        metrics[selection.source_id].sampling_reasons[
                            "unstable_metadata"
                        ] += 1
                        continue
                    destination = host_planes[len(eligible)]
                    if not reader.copy_plane(topology, snapshot, destination):
                        reason_counts["changed_during_copy"] += 1
                        metrics[selection.source_id].sampling_reasons[
                            "changed_during_copy"
                        ] += 1
                        continue
                    eligible.append(snapshot)
                shared_copy_ms = (time.perf_counter() - copy_started) * 1000.0

                if warmed_generation != topology.generation:
                    if args.warmup_iterations > 0:
                        if not eligible or device_planes is None:
                            continue
                        warmup_started = time.perf_counter()
                        device_planes[: len(eligible)].set(host_planes[: len(eligible)])
                        for _ in range(args.warmup_iterations):
                            detector.detect_batch(device_planes[: len(eligible)])
                        cp.cuda.Stream.null.synchronize()
                        warmup_duration_ms += (
                            time.perf_counter() - warmup_started
                        ) * 1000.0
                        for snapshot in eligible:
                            last_detected_sequences[snapshot.source_id] = (
                                snapshot.source_sequence
                            )
                    warmed_generation = topology.generation
                    if published_batches == 0:
                        reason_counts.clear()
                        for source_id in topology.source_ids:
                            metrics[source_id] = SourceMetrics()
                        processing_window.clear()
                        all_cycle_ms.clear()
                        all_processing_ms.clear()
                        deadline_misses = 0
                        tick_count = 0
                        detection_epochs = 0
                        total_deadline_misses = 0
                        frame_event_signals = 0
                        frame_event_timeouts = 0
                        micro_batch_waits = 0
                        micro_batch_added_sources = 0
                        measured_started_at = time.perf_counter()
                        next_tick = measured_started_at
                        window_started = measured_started_at
                        last_status_at = measured_started_at
                    if args.warmup_iterations > 0:
                        continue

                for slot_index, source_id in enumerate(topology.source_ids):
                    snapshot = snapshots[slot_index]
                    if snapshot is not None:
                        metrics[source_id].observe_receiver(snapshot, sampled_at)
                for selection in sampled:
                    if selection.eligible:
                        continue
                    reason_counts[selection.reason] += 1
                    metrics[selection.source_id].sampling_reasons[
                        selection.reason
                    ] += 1

                epoch_published_batches = 0
                invalid_detected_at_unix_ns = time.time_ns()
                invalid_observations = build_invalid_observations(
                    topology,
                    snapshots_by_id,
                    sampled,
                    invalid_detected_at_unix_ns,
                )
                stage_ms["metadataRead"].append(metadata_read_ms)
                stage_ms["sharedPlaneCopy"].append(shared_copy_ms)
                if waited_for_fresh_frame:
                    fresh_frame_waits += 1
                    fresh_frame_wait_ms.append(current_fresh_wait_ms)

                total_source_samples += len(topology.source_ids)
                initial_eligible_source_samples += initial_eligible_count or 0
                profile_cycle = args.profiling_mode == "full" or (
                    args.profiling_mode == "sampled"
                    and published_batches % args.profile_every_ticks == 0
                )
                executions: list[DetectionBatchExecution] = []
                execution = execute_detection_batch(
                    cp,
                    detector,
                    writer,
                    topology,
                    eligible,
                    host_planes,
                    device_planes,
                    metrics,
                    last_detected_sequences,
                    sampled_at,
                    profile_cycle,
                    invalid_observations,
                )
                if execution is not None:
                    executions.append(execution)
                    published_batches += 1
                    epoch_published_batches += 1
                elif invalid_observations:
                    invalid_ipc_started = time.perf_counter()
                    writer.write(
                        invalid_detected_at_unix_ns,
                        invalid_observations,
                        batch_flags=BATCH_PARTIAL,
                    )
                    stage_ms["ipcWrite"].append(
                        (time.perf_counter() - invalid_ipc_started) * 1000.0
                    )
                    published_batches += 1
                    epoch_published_batches += 1

                processed_source_ids = set(
                    execution.processed_source_ids if execution is not None else ()
                )
                late_metadata_read_ms = 0.0
                late_shared_copy_ms = 0.0
                late_wait_ms = 0.0
                late_sampled = sampled
                late_snapshots_by_id = snapshots_by_id
                while (
                    len(processed_source_ids) < len(topology.source_ids)
                    and time.perf_counter() < next_tick
                ):
                    late_metadata_started = time.perf_counter()
                    late_snapshots, late_snapshots_by_id, late_sampled = (
                        read_sampling_state(
                            reader,
                            topology,
                            last_detected_sequences,
                            maximum_age_ticks,
                            maximum_skew_ticks,
                        )
                    )
                    late_metadata_read_ms += (
                        time.perf_counter() - late_metadata_started
                    ) * 1000.0
                    late_selected = [
                        selection
                        for selection in late_sampled
                        if selection.eligible
                        and selection.source_id not in processed_source_ids
                    ]
                    if late_selected:
                        late_eligible: list[Mly2SourceSnapshot] = []
                        late_sampled_at = time.perf_counter()
                        late_copy_started = time.perf_counter()
                        for selection in late_selected:
                            snapshot = late_snapshots_by_id.get(selection.source_id)
                            if snapshot is None or host_planes is None:
                                reason_counts["unstable_metadata"] += 1
                                metrics[selection.source_id].sampling_reasons[
                                    "unstable_metadata"
                                ] += 1
                                continue
                            destination = host_planes[len(late_eligible)]
                            if not reader.copy_plane(topology, snapshot, destination):
                                reason_counts["changed_during_copy"] += 1
                                metrics[selection.source_id].sampling_reasons[
                                    "changed_during_copy"
                                ] += 1
                                continue
                            late_eligible.append(snapshot)
                        late_shared_copy_ms += (
                            time.perf_counter() - late_copy_started
                        ) * 1000.0
                        if late_eligible:
                            late_execution = execute_detection_batch(
                                cp,
                                detector,
                                writer,
                                topology,
                                late_eligible,
                                host_planes,
                                device_planes,
                                metrics,
                                last_detected_sequences,
                                late_sampled_at,
                                profile_cycle,
                            )
                            if late_execution is not None:
                                executions.append(late_execution)
                                published_batches += 1
                                epoch_published_batches += 1
                                processed_source_ids.update(
                                    late_execution.processed_source_ids
                                )
                            continue
                    pending_count = pending_fresh_source_count(
                        late_sampled, processed_source_ids
                    )
                    if pending_count == 0:
                        break
                    remaining = next_tick - time.perf_counter()
                    if remaining <= 0:
                        break
                    wait_started = time.perf_counter()
                    signaled = reader.wait_for_frame(remaining)
                    event_wait_ms = (time.perf_counter() - wait_started) * 1000.0
                    late_wait_ms += event_wait_ms
                    current_fresh_wait_ms += event_wait_ms
                    current_micro_batch_wait_ms += event_wait_ms
                    if signaled:
                        frame_event_signals += 1
                    else:
                        frame_event_timeouts += 1
                        break

                if late_metadata_read_ms > 0:
                    stage_ms["metadataRead"].append(late_metadata_read_ms)
                if late_shared_copy_ms > 0:
                    stage_ms["sharedPlaneCopy"].append(late_shared_copy_ms)
                if late_wait_ms > 0:
                    fresh_frame_waits += 1
                    fresh_frame_wait_ms.append(late_wait_ms)
                    micro_batch_waits += 1
                    micro_batch_wait_ms.append(late_wait_ms)
                if len(executions) > 1:
                    micro_batch_added_sources += sum(
                        len(batch.processed_source_ids)
                        for batch in executions[1:]
                    )
                eligible_source_samples += sum(
                    len(batch.processed_source_ids) for batch in executions
                )
                for batch in executions:
                    marker_instances += batch.marker_instances
                    for name, value in batch.stage_ms.items():
                        stage_ms[name].append(value)
                    if batch.profiled_stage_ms:
                        profiled_cycles += 1
                        for name, value in batch.profiled_stage_ms.items():
                            if name not in profiled_stage_ms:
                                profiled_stage_ms[name] = DurationDistribution()
                            profiled_stage_ms[name].append(value)
                batches_per_epoch.append(epoch_published_batches)
                if len(executions) > 1:
                    inter_batch_skew_ms.append(
                        (
                            executions[-1].detected_at_monotonic
                            - executions[0].detected_at_monotonic
                        )
                        * 1000.0
                    )

                cycle_ms = (time.perf_counter() - cycle_started) * 1000.0
                processing_ms = processing_duration_ms(
                    cycle_ms, current_fresh_wait_ms
                )
                processing_window.append(processing_ms)
                all_cycle_ms.append(cycle_ms)
                all_processing_ms.append(processing_ms)
                if cycle_ms > period * 1000.0 and not tick_missed_deadline:
                    deadline_misses += 1
                    total_deadline_misses += 1

                now = time.perf_counter()
                window_duration = now - window_started
                if window_duration >= args.control_window_seconds:
                    window = DetectionWindow(
                        duration_seconds=window_duration,
                        cycle_p95_ms=percentile(processing_window, 95),
                        deadline_miss_ratio=deadline_misses / max(1, tick_count),
                    )
                    decision = controller.observe_window(
                        window,
                        now,
                        allow_downgrade=args.adaptive,
                    )
                    capacity_exceeded = capacity_exceeded or decision.capacity_exceeded
                    if enforce_detection_capacity(decision, topology, window):
                        capacity_exceeded = True
                        break
                    if decision.changed:
                        writer.set_detection_hz(decision.detection_hz)
                        profile_history_count += 1
                        profile_history.append(
                            {
                                "atSeconds": round(now - measured_started_at, 3),
                                "detectionHz": decision.detection_hz,
                                "reason": decision.reason,
                            }
                        )
                        next_tick = now + 1.0 / decision.detection_hz
                    processing_window.clear()
                    deadline_misses = 0
                    tick_count = 0
                    window_started = now

                if now - last_status_at >= args.status_interval_seconds:
                    print(
                        f"sources={len(topology.source_ids)} "
                        f"eligible={len(eligible)} hz={controller.detection_hz} "
                        f"cycle={cycle_ms:.2f}ms process={processing_ms:.2f}ms "
                        f"phase={topology.phase}",
                        flush=True,
                    )
                    last_status_at = now
    except KeyboardInterrupt:
        pass

    elapsed = time.perf_counter() - measured_started_at
    source_ids = list(final_topology.source_ids) if final_topology else []
    required_count = args.required_source_count or len(source_ids)
    finished_at = time.perf_counter()
    active_sources = sum(
        metrics[source_id].last_eligible_at > 0
        and finished_at - metrics[source_id].last_eligible_at <= 2.0
        for source_id in source_ids
    )
    publication_rate = published_batches / max(elapsed, 1e-9)
    epoch_rate = detection_epochs / max(elapsed, 1e-9)
    minimum_rate = controller.detection_hz * 0.95
    cycle_time = distribution(all_cycle_ms)
    processing_time = distribution(all_processing_ms)
    source_reports = []
    for source_id in source_ids:
        source_metrics = metrics[source_id]
        receiver_duration = max(
            0.0,
            source_metrics.last_receiver_seen_at
            - source_metrics.first_receiver_seen_at,
        )
        first_receiver_frame = source_metrics.first_receiver_frame_count
        receiver_frame_delta = (
            max(0, source_metrics.last_receiver_frame_count - first_receiver_frame)
            if first_receiver_frame is not None
            else 0
        )
        receiver_rate = receiver_frame_delta / max(receiver_duration, 1e-9)
        detection_rate = source_metrics.detected_frames / max(elapsed, 1e-9)
        fresh_tick_ratio = source_metrics.detected_frames / max(
            detection_epochs, 1
        )
        source_reports.append(
            {
                "sourceId": source_id,
                "receiverFrames": receiver_frame_delta,
                "receiverFrameRateHz": round(receiver_rate, 3),
                "detectedFrames": source_metrics.detected_frames,
                "effectiveSourceDetectionHz": round(detection_rate, 3),
                "freshTickRatio": round(fresh_tick_ratio, 5),
                "detectedToReceiverFrameRatio": round(
                    source_metrics.detected_frames / max(receiver_frame_delta, 1),
                    5,
                ),
                "lastSequence": source_metrics.last_sequence,
                "lastMarkerIds": list(source_metrics.last_marker_ids),
                "samplingReasons": dict(
                    sorted(source_metrics.sampling_reasons.items())
                ),
                "markerInstancesById": dict(
                    sorted(source_metrics.marker_instances_by_id.items())
                ),
                "markerIdFrames": dict(
                    sorted(source_metrics.marker_id_frames.items())
                ),
                "lastEligibleAgoMs": round(
                    max(0.0, finished_at - source_metrics.last_eligible_at)
                    * 1000.0,
                    3,
                )
                if source_metrics.last_eligible_at > 0
                else None,
                "frameAgeP95Ms": round(
                    percentile(source_metrics.age_ms, 95), 3
                ),
            }
        )

    input_ready = all(
        source["receiverFrameRateHz"] >= minimum_rate for source in source_reports
    )
    source_coverage_passed = all(
        source["freshTickRatio"] >= args.minimum_fresh_tick_ratio
        for source in source_reports
    )
    cycle_headroom_limit_ms = 1000.0 / controller.detection_hz * 0.80
    throughput_passed = (
        epoch_rate >= minimum_rate
        and processing_time["p95"] <= cycle_headroom_limit_ms
    )
    failure_reasons = []
    if len(source_ids) != required_count:
        failure_reasons.append(
            f"configured source count {len(source_ids)} != {required_count}"
        )
    if active_sources != required_count:
        failure_reasons.append(f"active source count {active_sources} != {required_count}")
    if not input_ready:
        below = [
            f"{source['sourceId']}={source['receiverFrameRateHz']:.3f}Hz"
            for source in source_reports
            if source["receiverFrameRateHz"] < minimum_rate
        ]
        failure_reasons.append(
            "receiver input below target: " + ", ".join(below)
        )
    if epoch_rate < minimum_rate:
        failure_reasons.append(
            f"detection epoch rate {epoch_rate:.3f} Hz < {minimum_rate:.3f} Hz"
        )
    if processing_time["p95"] > cycle_headroom_limit_ms:
        failure_reasons.append(
            f"processing p95 {processing_time['p95']:.3f} ms > "
            f"{cycle_headroom_limit_ms:.3f} ms headroom limit"
        )
    if not source_coverage_passed:
        below = [
            f"{source['sourceId']}={source['freshTickRatio']:.3f}"
            for source in source_reports
            if source["freshTickRatio"] < args.minimum_fresh_tick_ratio
        ]
        failure_reasons.append(
            "fresh source coverage below target: " + ", ".join(below)
        )
    if capacity_exceeded:
        failure_reasons.append(f"capacity exceeded at {controller.detection_hz} Hz; publication stopped")

    report = {
        "schemaVersion": 4,
        "metricRetention": {
            "scope": "whole_run",
            "method": "bounded_upper_bound_histogram",
            "resolutionMs": DurationDistribution.resolution_ms,
            "limitMs": DurationDistribution.limit_ms,
            "overflowQuantile": "observed_maximum",
            "profileHistoryLimit": profile_history.maxlen,
            "profileHistoryDropped": profile_history_count - len(profile_history),
        },
        "stage": "gpu_marker_observer_mly2",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "inputMappingName": args.input_mapping_name,
        "outputMappingName": args.output_mapping_name,
        "durationSeconds": round(elapsed, 3),
        "configuredSources": len(source_ids),
        "activeSources": active_sources,
        "detectionEpochs": detection_epochs,
        "detectionEpochRateHz": round(epoch_rate, 3),
        "publishedBatches": published_batches,
        "publicationRateHz": round(publication_rate, 3),
        "batchUpdateMode": "partial",
        "batchesPerEpoch": distribution(batches_per_epoch),
        "interBatchDetectionSkewMs": distribution(inter_batch_skew_ms),
        "effectiveDetectionHz": controller.detection_hz,
        "adaptive": args.adaptive,
        "profilingMode": args.profiling_mode,
        "profiledCycles": profiled_cycles,
        "capacityExceeded": capacity_exceeded,
        "inputReady": input_ready,
        "throughputPassed": throughput_passed,
        "sourceCoveragePassed": source_coverage_passed,
        "topologyChanges": topology_changes,
        "warmupIterations": args.warmup_iterations,
        "warmupDurationMs": round(warmup_duration_ms, 3),
        "freshFrameWaitMs": args.fresh_frame_wait_ms,
        "freshFrameWaits": fresh_frame_waits,
        "microBatchWaitStrategy": "opportunistic_partial_batches_until_epoch_deadline",
        "microBatchWaits": micro_batch_waits,
        "microBatchAddedSources": micro_batch_added_sources,
        "frameEventAvailable": frame_event_available,
        "frameEventSignals": frame_event_signals,
        "frameEventTimeouts": frame_event_timeouts,
        "minimumFreshTickRatio": args.minimum_fresh_tick_ratio,
        "eligibleSourceRatio": round(
            eligible_source_samples / max(total_source_samples, 1), 5
        ),
        "initialEligibleSourceRatio": round(
            initial_eligible_source_samples / max(total_source_samples, 1), 5
        ),
        "deadlineMisses": total_deadline_misses,
        "profileHistory": list(profile_history),
        "samplingReasons": dict(sorted(reason_counts.items())),
        "markerInstances": marker_instances,
        "cycleTimeMs": cycle_time,
        "processingTimeMs": processing_time,
        "processingHeadroomLimitMs": round(cycle_headroom_limit_ms, 3),
        "stageTimeMs": {
            name: distribution(values) for name, values in stage_ms.items()
        },
        "profiledStageTimeMs": {
            name: distribution(values)
            for name, values in sorted(profiled_stage_ms.items())
        },
        "sources": source_reports,
        "failureReasons": failure_reasons,
        "passed": not failure_reasons,
    }
    if args.output:
        output = Path(args.output).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8"
        )
        print(f"Report: {output}", flush=True)
    print(
        f"{'PASS' if report['passed'] else 'FAIL'} sources={len(source_ids)} "
        f"active={active_sources} epochs={epoch_rate:.2f}Hz "
        f"batches={publication_rate:.2f}Hz "
        f"effective={controller.detection_hz}Hz",
        flush=True,
    )
    for reason in failure_reasons:
        print(f"  gate: {reason}", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
