#!/usr/bin/env python3
"""Detect dynamic MLY2 marker sources with one adaptive GPU batch worker."""

from __future__ import annotations

import argparse
from collections import Counter, deque
from dataclasses import dataclass
import json
import math
from pathlib import Path
import time

import numpy as np

from GpuArucoDetector import GpuArucoDetector
from MarkerDetectionRateController import (
    AdaptiveDetectionRateController,
    DetectionWindow,
)
from MarkerFrameSampler import SourceFrameState, sample_latest_frames
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
    DEFAULT_MAPPING_NAME,
    MarkerDetection,
    MarkerObservationSharedMemoryWriter,
    SourceObservation,
)


RESERVED_MARKER_IDS = frozenset({17, 34, 37})
DEFAULT_ALLOWED_MARKER_IDS = frozenset(set(range(50)) - RESERVED_MARKER_IDS)
DEFAULT_PROFILES = (50, 40, 33, 25)


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
    age_ms: deque[float] | None = None

    def __post_init__(self):
        if self.age_ms is None:
            self.age_ms = deque(maxlen=3000)


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
    parser.add_argument("--control-window-seconds", type=float, default=5.0)
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


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.duration_seconds < 0 or args.wait_for_mapping_seconds < 0:
        parser.error("duration and mapping wait must be zero or positive")
    if args.status_interval_seconds <= 0 or args.control_window_seconds <= 0:
        parser.error("status and control window intervals must be positive")
    if args.required_source_count < 0 or args.required_source_count > MAX_SOURCES:
        parser.error(f"--required-source-count must be in 0..{MAX_SOURCES}")
    if args.maximum_frame_age_ms < 0 or args.maximum_batch_skew_ms < 0:
        parser.error("frame age and batch skew must not be negative")

    detector = GpuArucoDetector(allowed_marker_ids=args.allowed_marker_ids)
    cp = detector.cp
    controller = AdaptiveDetectionRateController(
        profiles_hz=DEFAULT_PROFILES,
        initial_detection_hz=args.initial_detection_hz,
    )
    metrics: dict[str, SourceMetrics] = {}
    profile_history = [
        {"atSeconds": 0.0, "detectionHz": controller.detection_hz, "reason": "initial"}
    ]
    reason_counts: Counter[str] = Counter()
    cycle_window: list[float] = []
    all_cycle_ms: list[float] = []
    deadline_misses = 0
    tick_count = 0
    published_batches = 0
    marker_instances = 0
    capacity_exceeded = False
    topology_changes = 0
    measured_started_at = time.monotonic()
    final_topology: Mly2Topology | None = None

    try:
        with open_reader(
            args.input_mapping_name, args.wait_for_mapping_seconds
        ) as reader, MarkerObservationSharedMemoryWriter(
            args.output_mapping_name, controller.detection_hz
        ) as writer:
            topology: Mly2Topology | None = None
            pinned_owner = None
            host_planes = None
            device_planes = None
            last_detected_sequences: dict[str, int] = {}
            next_tick = time.monotonic()
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
                or time.monotonic() - measured_started_at < args.duration_seconds
            ):
                now = time.monotonic()
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
                    for source_id in topology.source_ids:
                        metrics.setdefault(source_id, SourceMetrics())
                    if topology_changes > 1:
                        decision = controller.prepare(now)
                        if decision.changed:
                            writer.set_detection_hz(decision.detection_hz)
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
                if schedule_late >= period:
                    deadline_misses += 1
                    skipped_periods = math.floor(schedule_late / period)
                    next_tick = scheduled_at + (skipped_periods + 1) * period
                else:
                    next_tick = scheduled_at + period
                tick_count += 1
                cycle_started = time.perf_counter()

                snapshots = reader.read_sources(topology)
                sample_tick = reader.query_performance_counter()
                states = []
                snapshots_by_id: dict[str, Mly2SourceSnapshot] = {}
                for slot_index, source_id in enumerate(topology.source_ids):
                    snapshot = snapshots[slot_index]
                    if snapshot is None:
                        states.append(SourceFrameState(source_id, 0, 0, False))
                    else:
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
                    int(topology.qpc_frequency * args.maximum_frame_age_ms / 1000.0),
                    int(topology.qpc_frequency * args.maximum_batch_skew_ms / 1000.0),
                )

                eligible: list[Mly2SourceSnapshot] = []
                for selection in sampled:
                    if not selection.eligible:
                        reason_counts[selection.reason] += 1
                        continue
                    snapshot = snapshots_by_id.get(selection.source_id)
                    if snapshot is None or host_planes is None:
                        reason_counts["unstable_metadata"] += 1
                        continue
                    destination = host_planes[len(eligible)]
                    if not reader.copy_plane(topology, snapshot, destination):
                        reason_counts["changed_during_copy"] += 1
                        continue
                    eligible.append(snapshot)

                results_by_source = {}
                if eligible:
                    device_planes[: len(eligible)].set(host_planes[: len(eligible)])
                    results = detector.detect_batch(device_planes[: len(eligible)])
                    cp.cuda.Stream.null.synchronize()
                    results_by_source = {
                        snapshot.source_id: result
                        for snapshot, result in zip(eligible, results, strict=True)
                    }

                detected_at_unix_ns = time.time_ns()
                observations = []
                for slot_index, source_id in enumerate(topology.source_ids):
                    snapshot = snapshots[slot_index]
                    result = results_by_source.get(source_id)
                    if snapshot is None:
                        source_sequence = 0
                        frame_received_at_unix_ns = 0
                    else:
                        source_sequence = snapshot.source_sequence
                        frame_received_at_unix_ns = snapshot.received_unix_ns
                    if result is None:
                        detections = []
                        candidate_count = 0
                    else:
                        detections = [
                            MarkerDetection(
                                marker.marker_id,
                                marker.center_x,
                                marker.center_y,
                                marker.area,
                            )
                            for marker in result.markers
                        ]
                        candidate_count = result.candidate_count
                        marker_instances += len(detections)
                        last_detected_sequences[source_id] = source_sequence
                        source_metrics = metrics[source_id]
                        source_metrics.detected_frames += 1
                        source_metrics.last_sequence = source_sequence
                        source_metrics.last_eligible_at = now
                        source_metrics.last_marker_ids = tuple(
                            detection.marker_id for detection in detections
                        )
                        source_metrics.age_ms.append(
                            max(
                                0.0,
                                (
                                    detected_at_unix_ns
                                    - frame_received_at_unix_ns
                                )
                                / 1_000_000.0,
                            )
                        )
                    observations.append(
                        SourceObservation(
                            source_index=slot_index,
                            source_id=source_id,
                            source_sequence=source_sequence,
                            frame_received_at_unix_ns=frame_received_at_unix_ns,
                            detected_at_unix_ns=detected_at_unix_ns,
                            video_valid=result is not None,
                            candidate_count=candidate_count,
                            detections=detections,
                        )
                    )
                writer.write(detected_at_unix_ns, observations)
                published_batches += 1

                cycle_ms = (time.perf_counter() - cycle_started) * 1000.0
                cycle_window.append(cycle_ms)
                all_cycle_ms.append(cycle_ms)
                if cycle_ms > period * 1000.0:
                    deadline_misses += 1

                now = time.monotonic()
                window_duration = now - window_started
                if window_duration >= args.control_window_seconds:
                    decision = controller.observe_window(
                        DetectionWindow(
                            duration_seconds=window_duration,
                            cycle_p95_ms=percentile(cycle_window, 95),
                            deadline_miss_ratio=(
                                deadline_misses / max(1, tick_count)
                            ),
                        ),
                        now,
                        allow_downgrade=args.adaptive,
                    )
                    capacity_exceeded = capacity_exceeded or decision.capacity_exceeded
                    if decision.changed:
                        writer.set_detection_hz(decision.detection_hz)
                        profile_history.append(
                            {
                                "atSeconds": round(now - measured_started_at, 3),
                                "detectionHz": decision.detection_hz,
                                "reason": decision.reason,
                            }
                        )
                        next_tick = now + 1.0 / decision.detection_hz
                    cycle_window.clear()
                    deadline_misses = 0
                    tick_count = 0
                    window_started = now

                if now - last_status_at >= args.status_interval_seconds:
                    print(
                        f"sources={len(topology.source_ids)} "
                        f"eligible={len(eligible)} hz={controller.detection_hz} "
                        f"cycle={cycle_ms:.2f}ms phase={topology.phase}",
                        flush=True,
                    )
                    last_status_at = now
    except KeyboardInterrupt:
        pass

    elapsed = time.monotonic() - measured_started_at
    source_ids = list(final_topology.source_ids) if final_topology else []
    required_count = args.required_source_count or len(source_ids)
    finished_at = time.monotonic()
    active_sources = sum(
        metrics[source_id].last_eligible_at > 0
        and finished_at - metrics[source_id].last_eligible_at <= 2.0
        for source_id in source_ids
    )
    rate = published_batches / max(elapsed, 1e-9)
    minimum_rate = controller.detection_hz * 0.90
    failure_reasons = []
    if len(source_ids) != required_count:
        failure_reasons.append(
            f"configured source count {len(source_ids)} != {required_count}"
        )
    if active_sources != required_count:
        failure_reasons.append(f"active source count {active_sources} != {required_count}")
    if rate < minimum_rate:
        failure_reasons.append(
            f"publication rate {rate:.3f} Hz < {minimum_rate:.3f} Hz"
        )
    if capacity_exceeded:
        failure_reasons.append("capacity exceeded at 25 Hz")

    report = {
        "schemaVersion": 1,
        "stage": "gpu_marker_observer_mly2",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "inputMappingName": args.input_mapping_name,
        "outputMappingName": args.output_mapping_name,
        "durationSeconds": round(elapsed, 3),
        "configuredSources": len(source_ids),
        "activeSources": active_sources,
        "publishedBatches": published_batches,
        "publicationRateHz": round(rate, 3),
        "effectiveDetectionHz": controller.detection_hz,
        "adaptive": args.adaptive,
        "capacityExceeded": capacity_exceeded,
        "topologyChanges": topology_changes,
        "profileHistory": profile_history,
        "samplingReasons": dict(sorted(reason_counts.items())),
        "markerInstances": marker_instances,
        "cycleTimeMs": {
            "p50": round(percentile(all_cycle_ms, 50), 3),
            "p95": round(percentile(all_cycle_ms, 95), 3),
            "p99": round(percentile(all_cycle_ms, 99), 3),
            "maximum": round(max(all_cycle_ms, default=0.0), 3),
        },
        "sources": [
            {
                "sourceId": source_id,
                "detectedFrames": metrics[source_id].detected_frames,
                "lastSequence": metrics[source_id].last_sequence,
                "lastMarkerIds": list(metrics[source_id].last_marker_ids),
                "lastEligibleAgoMs": round(
                    max(0.0, finished_at - metrics[source_id].last_eligible_at)
                    * 1000.0,
                    3,
                )
                if metrics[source_id].last_eligible_at > 0
                else None,
                "frameAgeP95Ms": round(percentile(metrics[source_id].age_ms, 95), 3),
            }
            for source_id in source_ids
        ],
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
        f"active={active_sources} rate={rate:.2f}Hz "
        f"effective={controller.detection_hz}Hz",
        flush=True,
    )
    for reason in failure_reasons:
        print(f"  gate: {reason}", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
