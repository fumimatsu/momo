#!/usr/bin/env python3
"""Validate one 2x2 composite video as four GPU ArUco detector sources."""

from __future__ import annotations

import argparse
from collections import Counter
import importlib.util
import json
import math
import sys
import time
from pathlib import Path

import numpy as np

from GpuArucoDetector import GpuArucoDetector


MODULE_PATH = Path(__file__).with_name("Measure-ArucoCapacity.py")
SPEC = importlib.util.spec_from_file_location("measure_aruco_capacity", MODULE_PATH)
CAPACITY = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = CAPACITY
SPEC.loader.exec_module(CAPACITY)


def parse_marker_ids(value: str) -> set[int]:
    marker_ids = {int(part.strip()) for part in value.split(",") if part.strip()}
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    return marker_ids


def percentile(values: list[float], value: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * value / 100.0) - 1))
    return ordered[index]


def group_frames(frames: list[int], maximum_gap: int = 10) -> list[dict[str, int]]:
    groups: list[list[int]] = []
    for frame in frames:
        if not groups or frame - groups[-1][-1] > maximum_gap:
            groups.append([frame])
        else:
            groups[-1].append(frame)
    return [
        {"firstFrame": group[0], "lastFrame": group[-1], "detectionFrames": len(group)}
        for group in groups
    ]


def split_quadrants(cp, luma_device, width: int, height: int):
    if width % 2 or height % 2:
        raise ValueError("composite dimensions must be even")
    half_width = width // 2
    half_height = height // 2
    return cp.stack(
        (
            luma_device[:half_height, :half_width],
            luma_device[:half_height, half_width:width],
            luma_device[half_height:height, :half_width],
            luma_device[half_height:height, half_width:width],
        )
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True)
    parser.add_argument("--frame-count", type=int, default=1800)
    parser.add_argument("--expected-marker-ids", type=parse_marker_ids, default={1})
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    input_path = Path(args.input).resolve()
    if not input_path.is_file():
        parser.error(f"input does not exist: {input_path}")
    if args.frame_count < 1:
        parser.error("--frame-count must be positive")

    nvc = CAPACITY.load_nvcodec()
    detector = GpuArucoDetector(allowed_marker_ids=args.expected_marker_ids)
    cp = detector.cp
    cpu_detector = CAPACITY.make_detector()
    decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=True)
    metadata = decoder.get_stream_metadata()
    width = int(metadata.width)
    height = int(metadata.height)
    fps = float(metadata.average_fps)
    requested_frames = min(args.frame_count, int(metadata.num_frames))
    if width != 1920 or height != 1080:
        parser.error(f"expected 1920x1080 composite input, got {width}x{height}")

    warmup = decoder.get_batch_frames(1)
    if not warmup:
        raise RuntimeError("input did not produce a warmup frame")
    warmup_luma = cp.from_dlpack(warmup[0])[:height, :width]
    detector.detect_batch(split_quadrants(cp, warmup_luma, width, height))
    cp.cuda.Stream.null.synchronize()
    del decoder

    decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=True)
    gpu_processing_ms: list[float] = []
    frame_wall_ms: list[float] = []
    cpu_oracle_ms: list[float] = []
    cpu_frames = [[] for _ in range(4)]
    gpu_frames = [[] for _ in range(4)]
    cpu_instances = [Counter() for _ in range(4)]
    gpu_instances = [Counter() for _ in range(4)]
    compared_frames = 0
    started_at = time.perf_counter()
    try:
        for frame_index in range(requested_frames):
            frame_started = time.perf_counter()
            frames = decoder.get_batch_frames(1)
            if not frames:
                break
            processing_started = time.perf_counter()
            luma_device = cp.from_dlpack(frames[0])[:height, :width]
            quadrants = split_quadrants(cp, luma_device, width, height)
            gpu_results = detector.detect_batch(quadrants)
            gpu_processing_ms.append((time.perf_counter() - processing_started) * 1000.0)
            frame_wall_ms.append((time.perf_counter() - frame_started) * 1000.0)

            quadrants_host = cp.asnumpy(quadrants)
            cpu_started = time.perf_counter()
            for quadrant_index, (gpu_result, gray_host) in enumerate(
                zip(gpu_results, quadrants_host)
            ):
                gpu_counter = Counter(gpu_result.marker_ids)
                gpu_instances[quadrant_index].update(gpu_counter)
                if gpu_counter:
                    gpu_frames[quadrant_index].append(frame_index)
                _, ids, _ = cpu_detector.detectMarkers(gray_host)
                cpu_counter = Counter(
                    int(value)
                    for value in ([] if ids is None else ids.flatten())
                    if int(value) in args.expected_marker_ids
                )
                cpu_instances[quadrant_index].update(cpu_counter)
                if cpu_counter:
                    cpu_frames[quadrant_index].append(frame_index)
            cpu_oracle_ms.append((time.perf_counter() - cpu_started) * 1000.0)
            compared_frames += 1
    finally:
        del decoder

    processing_p95 = percentile(gpu_processing_ms, 95)
    wall_p95 = percentile(frame_wall_ms, 95)
    processing_rate = compared_frames / max(1e-9, sum(gpu_processing_ms) / 1000.0)
    required_rate = fps * CAPACITY.MINIMUM_RATE_FACTOR
    maximum_interval_ms = 1000.0 / fps
    over_budget_frames = sum(value > maximum_interval_ms for value in frame_wall_ms)
    passed = (
        compared_frames == requested_frames
        and processing_p95 <= maximum_interval_ms
        and wall_p95 <= maximum_interval_ms
        and processing_rate >= required_rate
    )
    quadrants_report = []
    for quadrant_index in range(4):
        quadrants_report.append(
            {
                "quadrant": quadrant_index + 1,
                "cpuDetectionFrames": len(cpu_frames[quadrant_index]),
                "gpuDetectionFrames": len(gpu_frames[quadrant_index]),
                "cpuMarkerInstances": {
                    str(key): value for key, value in sorted(cpu_instances[quadrant_index].items())
                },
                "gpuMarkerInstances": {
                    str(key): value for key, value in sorted(gpu_instances[quadrant_index].items())
                },
                "cpuGroups": group_frames(cpu_frames[quadrant_index]),
                "gpuGroups": group_frames(gpu_frames[quadrant_index]),
            }
        )
    report = {
        "schemaVersion": 1,
        "stage": "gpu_aruco_2x2_composite_quadrants",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "input": str(input_path),
        "inputWidth": width,
        "inputHeight": height,
        "inputFps": fps,
        "quadrantWidth": width // 2,
        "quadrantHeight": height // 2,
        "requestedFrames": requested_frames,
        "comparedFrames": compared_frames,
        "expectedMarkerIds": sorted(args.expected_marker_ids),
        "gpuProcessingMsP50": round(percentile(gpu_processing_ms, 50), 3),
        "gpuProcessingMsP95": round(processing_p95, 3),
        "gpuProcessingMsP99": round(percentile(gpu_processing_ms, 99), 3),
        "gpuProcessingMsMax": round(max(gpu_processing_ms, default=0.0), 3),
        "frameWallMsP95": round(wall_p95, 3),
        "frameWallMsP99": round(percentile(frame_wall_ms, 99), 3),
        "frameWallMsMax": round(max(frame_wall_ms, default=0.0), 3),
        "frameWallOverBudgetCount": over_budget_frames,
        "frameWallOverBudgetRate": round(over_budget_frames / max(1, compared_frames), 6),
        "processingFramesPerSecond": round(processing_rate, 3),
        "cpuFourQuadrantMsP95": round(percentile(cpu_oracle_ms, 95), 3),
        "cpuFourQuadrantMsP99": round(percentile(cpu_oracle_ms, 99), 3),
        "acceptance": {
            "minimumFramesPerSecond": round(required_rate, 3),
            "maximumP95Ms": round(maximum_interval_ms, 3),
        },
        "deviceDecode": True,
        "deviceQuadrantSplit": True,
        "deviceBatchDetection": True,
        "cpuOracleOutsideGpuMeasurement": True,
        "quadrants": quadrants_report,
        "elapsedSeconds": round(time.perf_counter() - started_at, 3),
        "passed": passed,
    }
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(
        f"{'PASS' if passed else 'FAIL'} frames={compared_frames} "
        f"processing-p95={processing_p95:.3f}ms wall-p95={wall_p95:.3f}ms "
        f"processing-rate={processing_rate:.2f}fps"
    )
    print(f"Report: {output}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
