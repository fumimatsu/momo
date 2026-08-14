#!/usr/bin/env python3
"""Replay one or more videos through the GPU detector and publish marker observations."""

from __future__ import annotations

import argparse
import importlib.util
import json
from pathlib import Path
import sys
import time

from GpuArucoDetector import GpuArucoDetector
from MarkerObserverMetrics import (
    DEFAULT_REQUIRED_SOURCE_AVAILABILITY_RATIO,
    evaluate_capacity,
    percentile,
)
from MarkerObservationIpc import (
    DEFAULT_MAPPING_NAME,
    MarkerDetection,
    MarkerObservationSharedMemoryWriter,
    SourceObservation,
)


CAPACITY_MODULE_PATH = Path(__file__).with_name("Measure-ArucoCapacity.py")
CAPACITY_SPEC = importlib.util.spec_from_file_location(
    "measure_aruco_capacity_for_marker_observer",
    CAPACITY_MODULE_PATH,
)
CAPACITY = importlib.util.module_from_spec(CAPACITY_SPEC)
assert CAPACITY_SPEC.loader is not None
sys.modules[CAPACITY_SPEC.name] = CAPACITY
CAPACITY_SPEC.loader.exec_module(CAPACITY)

RESERVED_MARKER_IDS = frozenset({17, 34, 37})
DEFAULT_ALLOWED_MARKER_IDS = frozenset(set(range(50)) - RESERVED_MARKER_IDS)


def parse_source(value: str) -> tuple[str, Path]:
    source_id, separator, raw_path = value.partition("=")
    source_id = source_id.strip()
    if not separator or not source_id or not raw_path.strip():
        raise argparse.ArgumentTypeError("source must use SOURCE_ID=VIDEO_PATH")
    try:
        encoded_source_id = source_id.encode("utf-8")
    except UnicodeEncodeError as exc:
        raise argparse.ArgumentTypeError("source ID must be valid UTF-8") from exc
    if len(encoded_source_id) >= 32:
        raise argparse.ArgumentTypeError("source ID must contain 1..31 UTF-8 bytes")
    path = Path(raw_path.strip()).expanduser().resolve()
    if not path.is_file():
        raise argparse.ArgumentTypeError(f"source video does not exist: {path}")
    return source_id, path


def parse_marker_ids(value: str) -> frozenset[int]:
    try:
        marker_ids = frozenset(int(part.strip()) for part in value.split(",") if part.strip())
    except ValueError as exc:
        raise argparse.ArgumentTypeError("marker IDs must be comma-separated integers") from exc
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    reserved = marker_ids & RESERVED_MARKER_IDS
    if reserved:
        raise argparse.ArgumentTypeError(
            "reserved marker IDs are not allowed: " + ",".join(str(value) for value in sorted(reserved))
        )
    return marker_ids


def read_frame(decoder, video_path: Path, loop: bool):
    frames = decoder.get_batch_frames(1)
    if frames or not loop:
        return frames[0] if frames else None
    decoder.reconfigure_decoder(str(video_path), 0)
    frames = decoder.get_batch_frames(1)
    return frames[0] if frames else None


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--source",
        action="append",
        type=parse_source,
        required=True,
        help="Repeat SOURCE_ID=VIDEO_PATH for each simulated input.",
    )
    parser.add_argument("--detection-hz", type=int, default=50)
    parser.add_argument("--duration-seconds", type=float, default=30.0)
    parser.add_argument(
        "--allowed-marker-ids",
        type=parse_marker_ids,
        default=DEFAULT_ALLOWED_MARKER_IDS,
    )
    parser.add_argument("--mapping-name", default=DEFAULT_MAPPING_NAME)
    parser.add_argument("--output")
    parser.add_argument("--no-loop", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not 1 <= len(args.source) <= 32:
        parser.error("one to 32 sources are required")
    if len({source_id for source_id, _ in args.source}) != len(args.source):
        parser.error("source IDs must be unique")
    if args.detection_hz < 1 or args.detection_hz > 240:
        parser.error("--detection-hz must be in 1..240")
    if args.duration_seconds <= 0:
        parser.error("--duration-seconds must be positive")

    nvc = CAPACITY.load_nvcodec()
    detector = GpuArucoDetector(allowed_marker_ids=args.allowed_marker_ids)
    cp = detector.cp
    decoders = []
    dimensions: list[tuple[int, int]] = []
    for _, path in args.source:
        decoder = nvc.ThreadedDecoder(str(path), 8, gpu_id=0, use_device_memory=True)
        metadata = decoder.get_stream_metadata()
        dimensions.append((int(metadata.width), int(metadata.height)))
        decoders.append(decoder)
    if len(set(dimensions)) != 1:
        parser.error(f"all source videos must have the same dimensions: {dimensions}")

    warmup_started = time.perf_counter()
    width, height = dimensions[0]
    detector.warmup(len(decoders), height, width)
    warmup_ms = (time.perf_counter() - warmup_started) * 1000.0

    frame_interval = 1.0 / args.detection_hz
    frame_sequences = [0] * len(decoders)
    processing_ms: list[float] = []
    late_cycles = 0
    published_batches = 0
    observed_marker_instances = 0
    started_at = time.perf_counter()
    next_frame_at = started_at
    try:
        with MarkerObservationSharedMemoryWriter(args.mapping_name, args.detection_hz) as writer:
            while time.perf_counter() - started_at < args.duration_seconds:
                now = time.perf_counter()
                if now < next_frame_at:
                    time.sleep(min(next_frame_at - now, 0.002))
                    continue
                if now - next_frame_at >= frame_interval:
                    late_cycles += 1

                frame_received_at_unix_ns = time.time_ns()
                frames = [
                    read_frame(decoder, path, not args.no_loop)
                    for decoder, (_, path) in zip(decoders, args.source)
                ]
                valid_indices = [index for index, frame in enumerate(frames) if frame is not None]
                results_by_index = {}
                processing_started = time.perf_counter()
                if valid_indices:
                    luma_frames = []
                    for index in valid_indices:
                        width, height = dimensions[index]
                        luma_frames.append(cp.from_dlpack(frames[index])[:height, :width])
                    results = detector.detect_batch(cp.stack(luma_frames))
                    results_by_index = dict(zip(valid_indices, results))
                detected_at_unix_ns = time.time_ns()
                processing_ms.append((time.perf_counter() - processing_started) * 1000.0)

                observations = []
                for source_index, (source_id, _) in enumerate(args.source):
                    video_valid = source_index in results_by_index
                    if video_valid:
                        frame_sequences[source_index] += 1
                        result = results_by_index[source_index]
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
                        observed_marker_instances += len(detections)
                    else:
                        detections = []
                        candidate_count = 0
                    observations.append(
                        SourceObservation(
                            source_index=source_index,
                            source_id=source_id,
                            source_sequence=frame_sequences[source_index],
                            frame_received_at_unix_ns=frame_received_at_unix_ns,
                            detected_at_unix_ns=detected_at_unix_ns,
                            video_valid=video_valid,
                            candidate_count=candidate_count,
                            detections=detections,
                        )
                    )
                writer.write(detected_at_unix_ns, observations)
                published_batches += 1
                next_frame_at += frame_interval
                if next_frame_at < now - frame_interval:
                    next_frame_at = now
    finally:
        for decoder in decoders:
            decoder.end()

    elapsed_seconds = time.perf_counter() - started_at
    publication_rate = published_batches / max(elapsed_seconds, 1e-9)
    gate = evaluate_capacity(
        args.detection_hz,
        publication_rate,
        published_batches,
        frame_sequences,
    )
    report = {
        "schemaVersion": 1,
        "stage": "gpu_marker_observer_replay",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "mappingName": args.mapping_name,
        "sources": [
            {
                "sourceIndex": index,
                "sourceId": source_id,
                "input": str(path),
                "width": dimensions[index][0],
                "height": dimensions[index][1],
                "publishedFrames": frame_sequences[index],
                "availabilityRatio": round(gate.source_availability_ratios[index], 6),
            }
            for index, (source_id, path) in enumerate(args.source)
        ],
        "detectionHz": args.detection_hz,
        "durationSeconds": round(elapsed_seconds, 3),
        "warmupMs": round(warmup_ms, 3),
        "publishedBatches": published_batches,
        "publicationRateHz": round(publication_rate, 3),
        "requiredPublicationRateHz": round(gate.required_publication_rate_hz, 3),
        "requiredSourceAvailabilityRatio": DEFAULT_REQUIRED_SOURCE_AVAILABILITY_RATIO,
        "processingMsP50": round(percentile(processing_ms, 50), 3),
        "processingMsP95": round(percentile(processing_ms, 95), 3),
        "processingMsP99": round(percentile(processing_ms, 99), 3),
        "processingMsMax": round(max(processing_ms, default=0.0), 3),
        "lateCycles": late_cycles,
        "markerInstances": observed_marker_instances,
        "reservedMarkerIds": sorted(RESERVED_MARKER_IDS),
        "inputReady": gate.input_ready,
        "throughputPassed": gate.throughput_passed,
        "passed": gate.passed,
    }
    if args.output:
        output = Path(args.output).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"Report: {output}")
    print(
        f"{'PASS' if report['passed'] else 'FAIL'} sources={len(args.source)} "
        f"batches={published_batches} rate={publication_rate:.2f}Hz "
        f"processing-p95={report['processingMsP95']:.3f}ms"
    )
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
