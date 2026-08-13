#!/usr/bin/env python3
"""Compare OpenCV and direct NVDEC ArUco results on identical frame indices."""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
import time
from pathlib import Path

import cv2
import numpy as np


MODULE_PATH = Path(__file__).with_name("Measure-ArucoCapacity.py")
SPEC = importlib.util.spec_from_file_location("measure_aruco_capacity", MODULE_PATH)
CAPACITY = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = CAPACITY
SPEC.loader.exec_module(CAPACITY)


def parse_marker_ids(value: str) -> set[int]:
    try:
        marker_ids = {int(part.strip()) for part in value.split(",") if part.strip()}
    except ValueError as exc:
        raise argparse.ArgumentTypeError("marker IDs must be comma-separated integers") from exc
    if not marker_ids or any(marker_id < 0 or marker_id > 49 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    return marker_ids


def detect_ids(detector, gray: np.ndarray) -> set[int]:
    _, ids, _ = detector.detectMarkers(gray)
    return set() if ids is None else {int(marker_id) for marker_id in ids.flatten()}


def group_detection_frames(frames: list[int], maximum_gap: int = 10) -> list[dict[str, int]]:
    if not frames:
        return []
    groups = []
    start = previous = frames[0]
    count = 1
    for frame_index in frames[1:]:
        if frame_index - previous > maximum_gap:
            groups.append({"firstFrame": start, "lastFrame": previous, "detectionFrames": count})
            start = frame_index
            count = 0
        previous = frame_index
        count += 1
    groups.append({"firstFrame": start, "lastFrame": previous, "detectionFrames": count})
    return groups


def resize_cpu_frame(frame: np.ndarray, quality: float) -> np.ndarray:
    gray = cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY)
    resized_height = max(1, int(gray.shape[0] * quality))
    resized_width = max(1, int(resized_height * 16.0 / 9.0))
    resized_height -= resized_height % 2
    resized_width -= resized_width % 2
    return cv2.resize(gray, (resized_width, resized_height), interpolation=cv2.INTER_LINEAR)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, help="upright H.264 recording")
    parser.add_argument("--frame-count", type=int, default=1500)
    parser.add_argument("--quality", type=float, default=0.6)
    parser.add_argument("--expected-marker-ids", type=parse_marker_ids, default=parse_marker_ids("1,2,3"))
    parser.add_argument("--minimum-expected-agreement", type=float, default=0.99)
    parser.add_argument("--minimum-group-detections", type=int, default=3)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.frame_count < 1:
        parser.error("--frame-count must be positive")
    if args.quality <= 0 or args.quality > 1:
        parser.error("--quality must be in (0, 1]")
    if args.minimum_expected_agreement <= 0 or args.minimum_expected_agreement > 1:
        parser.error("--minimum-expected-agreement must be in (0, 1]")
    if args.minimum_group_detections < 1:
        parser.error("--minimum-group-detections must be positive")

    input_path = Path(args.input).resolve()
    if not input_path.is_file():
        parser.error(f"input does not exist: {input_path}")
    capture = cv2.VideoCapture(str(input_path))
    if not capture.isOpened():
        parser.error("OpenCV could not open input")

    nvc = CAPACITY.load_nvcodec()
    decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=False)
    metadata = decoder.get_stream_metadata()
    source_width = int(metadata.width)
    source_height = int(metadata.height)
    detector = CAPACITY.make_detector()
    compared = 0
    exact_matches = 0
    expected_matches = 0
    disagreements: list[dict] = []
    cpu_counts: dict[int, int] = {}
    nvcodec_counts: dict[int, int] = {}
    cpu_detection_frames = {marker_id: [] for marker_id in args.expected_marker_ids}
    nvcodec_detection_frames = {marker_id: [] for marker_id in args.expected_marker_ids}
    started_at = time.perf_counter()
    try:
        for frame_index in range(min(args.frame_count, int(metadata.num_frames))):
            ok, cpu_frame = capture.read()
            nvcodec_frames = decoder.get_batch_frames(1)
            if not ok or not nvcodec_frames:
                break
            cpu_ids = detect_ids(detector, resize_cpu_frame(cpu_frame, args.quality))
            nv12 = np.from_dlpack(nvcodec_frames[0])
            nvcodec_gray = CAPACITY.prepare_nvcodec_luma(
                nv12, source_width, source_height, args.quality
            )
            nvcodec_ids = detect_ids(detector, nvcodec_gray)
            compared += 1
            for marker_id in cpu_ids:
                cpu_counts[marker_id] = cpu_counts.get(marker_id, 0) + 1
                if marker_id in cpu_detection_frames:
                    cpu_detection_frames[marker_id].append(frame_index)
            for marker_id in nvcodec_ids:
                nvcodec_counts[marker_id] = nvcodec_counts.get(marker_id, 0) + 1
                if marker_id in nvcodec_detection_frames:
                    nvcodec_detection_frames[marker_id].append(frame_index)
            exact_match = cpu_ids == nvcodec_ids
            expected_match = (cpu_ids & args.expected_marker_ids) == (
                nvcodec_ids & args.expected_marker_ids
            )
            exact_matches += int(exact_match)
            expected_matches += int(expected_match)
            if not exact_match and len(disagreements) < 100:
                disagreements.append(
                    {
                        "frameIndex": frame_index,
                        "cpuIds": sorted(cpu_ids),
                        "nvcodecIds": sorted(nvcodec_ids),
                        "expectedIdsAgree": expected_match,
                    }
                )
    finally:
        capture.release()
        del decoder

    expected_agreement = expected_matches / compared if compared else 0.0
    exact_agreement = exact_matches / compared if compared else 0.0
    detection_groups = {}
    qualified_group_counts_match = True
    for marker_id in sorted(args.expected_marker_ids):
        cpu_groups = group_detection_frames(cpu_detection_frames[marker_id])
        nvcodec_groups = group_detection_frames(nvcodec_detection_frames[marker_id])
        cpu_qualified = [
            group for group in cpu_groups if group["detectionFrames"] >= args.minimum_group_detections
        ]
        nvcodec_qualified = [
            group for group in nvcodec_groups if group["detectionFrames"] >= args.minimum_group_detections
        ]
        qualified_group_counts_match &= len(cpu_qualified) == len(nvcodec_qualified)
        detection_groups[str(marker_id)] = {
            "cpu": cpu_groups,
            "nvcodec": nvcodec_groups,
            "cpuQualified": cpu_qualified,
            "nvcodecQualified": nvcodec_qualified,
        }
    passed = compared == min(args.frame_count, int(metadata.num_frames)) and (
        expected_agreement >= args.minimum_expected_agreement
    ) and qualified_group_counts_match
    report = {
        "schemaVersion": 1,
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "input": str(input_path),
        "requestedFrames": args.frame_count,
        "comparedFrames": compared,
        "quality": args.quality,
        "expectedMarkerIds": sorted(args.expected_marker_ids),
        "minimumExpectedAgreement": args.minimum_expected_agreement,
        "minimumGroupDetections": args.minimum_group_detections,
        "expectedMarkerAgreement": round(expected_agreement, 6),
        "exactAllIdAgreement": round(exact_agreement, 6),
        "qualifiedGroupCountsMatch": qualified_group_counts_match,
        "cpuMarkerFrameCounts": {str(key): cpu_counts[key] for key in sorted(cpu_counts)},
        "nvcodecMarkerFrameCounts": {
            str(key): nvcodec_counts[key] for key in sorted(nvcodec_counts)
        },
        "expectedMarkerDetectionGroups": detection_groups,
        "sampleDisagreements": disagreements,
        "elapsedSeconds": round(time.perf_counter() - started_at, 3),
        "passed": passed,
    }
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(
        f"{'PASS' if passed else 'FAIL'} frames={compared} "
        f"expected-agreement={expected_agreement:.3%} exact-agreement={exact_agreement:.3%}"
    )
    print(f"Report: {output}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
