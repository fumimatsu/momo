#!/usr/bin/env python3
"""Compare the GPU-only ArUco ID path with the OpenCV validation oracle."""

from __future__ import annotations

import argparse
from collections import Counter
import importlib.util
import json
import math
import sys
import time
from pathlib import Path

import cv2
import numpy as np

from GpuArucoDetector import GpuArucoDetector


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
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    return marker_ids


def percentile(values: list[float], value: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * value / 100.0) - 1))
    return ordered[index]


def counter_overlap(reference: Counter[int], candidate: Counter[int]) -> tuple[int, int, int]:
    true_positives = sum((reference & candidate).values())
    false_positives = sum((candidate - reference).values())
    false_negatives = sum((reference - candidate).values())
    return true_positives, false_positives, false_negatives


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True)
    parser.add_argument("--frame-count", type=int, default=1500)
    parser.add_argument("--quality", type=float, default=0.6)
    parser.add_argument("--expected-marker-ids", type=parse_marker_ids, default={1, 2, 3})
    parser.add_argument("--adaptive-window-size", type=int, default=13)
    parser.add_argument("--maximum-component-area-ratio", type=float, default=0.1)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.frame_count < 1:
        parser.error("--frame-count must be positive")
    if args.quality <= 0 or args.quality > 1:
        parser.error("--quality must be in (0, 1]")
    if args.maximum_component_area_ratio <= 0 or args.maximum_component_area_ratio > 1:
        parser.error("--maximum-component-area-ratio must be in (0, 1]")
    if args.adaptive_window_size < 3 or args.adaptive_window_size % 2 == 0:
        parser.error("--adaptive-window-size must be an odd integer >= 3")

    input_path = Path(args.input).resolve()
    if not input_path.is_file():
        parser.error(f"input does not exist: {input_path}")

    nvc = CAPACITY.load_nvcodec()
    gpu_detector = GpuArucoDetector(
        allowed_marker_ids=args.expected_marker_ids,
        adaptive_window_size=args.adaptive_window_size,
        maximum_component_area_ratio=args.maximum_component_area_ratio,
    )
    cp = gpu_detector.cp
    cpu_detector = CAPACITY.make_detector()

    warmup_decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=True)
    metadata = warmup_decoder.get_stream_metadata()
    warmup_frames = warmup_decoder.get_batch_frames(1)
    if not warmup_frames:
        raise RuntimeError("input did not produce a warmup frame")
    warmup_gray = gpu_detector.decoder.resize_nv12_luma(
        cp.from_dlpack(warmup_frames[0]), int(metadata.width), int(metadata.height), args.quality
    )
    gpu_detector.detect(warmup_gray)
    cp.cuda.Stream.null.synchronize()
    del warmup_decoder

    decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=True)
    requested_frames = min(args.frame_count, int(metadata.num_frames))
    compared_frames = 0
    true_positives = 0
    false_positives = 0
    false_negatives = 0
    exact_frames = 0
    instance_true_positives = 0
    instance_false_positives = 0
    instance_false_negatives = 0
    exact_instance_frames = 0
    gpu_elapsed_ms = []
    gpu_wall_ms = []
    candidate_counts = []
    cpu_id_counts: Counter[int] = Counter()
    gpu_id_counts: Counter[int] = Counter()
    cpu_instance_counts: Counter[int] = Counter()
    gpu_instance_counts: Counter[int] = Counter()
    mismatches = []
    cpu_oracle_host_bytes = 0
    result_host_bytes = 0
    started_at = time.perf_counter()

    try:
        for frame_index in range(requested_frames):
            frames = decoder.get_batch_frames(1)
            if not frames:
                break
            gpu_wall_started = time.perf_counter()
            start_event = cp.cuda.Event()
            stop_event = cp.cuda.Event()
            start_event.record()
            gray_device = gpu_detector.decoder.resize_nv12_luma(
                cp.from_dlpack(frames[0]), int(metadata.width), int(metadata.height), args.quality
            )
            gpu_result = gpu_detector.detect(gray_device)
            stop_event.record()
            stop_event.synchronize()
            gpu_elapsed_ms.append(cp.cuda.get_elapsed_time(start_event, stop_event))
            gpu_wall_ms.append((time.perf_counter() - gpu_wall_started) * 1000.0)
            candidate_counts.append(gpu_result.candidate_count)
            result_host_bytes += len(gpu_result.marker_ids) * 2

            gray_host = cp.asnumpy(gray_device)
            cpu_oracle_host_bytes += gray_host.nbytes
            _, ids, _ = cpu_detector.detectMarkers(gray_host)
            cpu_instances = Counter(
                int(value)
                for value in ([] if ids is None else ids.flatten())
                if int(value) in args.expected_marker_ids
            )
            gpu_instances = Counter(gpu_result.marker_ids)
            cpu_ids = set(cpu_instances)
            gpu_ids = set(gpu_instances)
            cpu_id_counts.update(cpu_ids)
            gpu_id_counts.update(gpu_ids)
            cpu_instance_counts.update(cpu_instances)
            gpu_instance_counts.update(gpu_instances)
            true_positives += len(cpu_ids & gpu_ids)
            false_positives += len(gpu_ids - cpu_ids)
            false_negatives += len(cpu_ids - gpu_ids)
            instance_tp, instance_fp, instance_fn = counter_overlap(
                cpu_instances, gpu_instances
            )
            instance_true_positives += instance_tp
            instance_false_positives += instance_fp
            instance_false_negatives += instance_fn
            if cpu_instances == gpu_instances:
                exact_instance_frames += 1
            if cpu_ids == gpu_ids:
                exact_frames += 1
            if cpu_instances != gpu_instances and len(mismatches) < 100:
                mismatches.append(
                    {
                        "frameIndex": frame_index,
                        "cpuIds": sorted(cpu_ids),
                        "gpuIds": sorted(gpu_ids),
                        "cpuInstances": dict(sorted(cpu_instances.items())),
                        "gpuInstances": dict(sorted(gpu_instances.items())),
                        "candidateCount": gpu_result.candidate_count,
                    }
                )
            compared_frames += 1
    finally:
        del decoder

    precision = true_positives / max(1, true_positives + false_positives)
    recall = true_positives / max(1, true_positives + false_negatives)
    instance_precision = instance_true_positives / max(
        1, instance_true_positives + instance_false_positives
    )
    instance_recall = instance_true_positives / max(
        1, instance_true_positives + instance_false_negatives
    )
    gpu_p95 = percentile(gpu_elapsed_ms, 95)
    gpu_wall_p95 = percentile(gpu_wall_ms, 95)
    passed = (
        compared_frames == requested_frames
        and precision >= 0.98
        and recall >= 0.95
        and instance_precision >= 0.98
        and instance_recall >= 0.95
        and gpu_p95 < 20.0
        and gpu_wall_p95 < 20.0
    )
    report = {
        "schemaVersion": 1,
        "stage": "gpu_only_candidate_and_id_detection",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "input": str(input_path),
        "expectedMarkerIds": sorted(args.expected_marker_ids),
        "adaptiveWindowSize": args.adaptive_window_size,
        "maximumComponentAreaRatio": args.maximum_component_area_ratio,
        "requestedFrames": requested_frames,
        "comparedFrames": compared_frames,
        "exactFrameCount": exact_frames,
        "exactFrameRate": round(exact_frames / max(1, compared_frames), 6),
        "exactInstanceFrameCount": exact_instance_frames,
        "exactInstanceFrameRate": round(exact_instance_frames / max(1, compared_frames), 6),
        "truePositives": true_positives,
        "falsePositives": false_positives,
        "falseNegatives": false_negatives,
        "precision": round(precision, 6),
        "recall": round(recall, 6),
        "instanceTruePositives": instance_true_positives,
        "instanceFalsePositives": instance_false_positives,
        "instanceFalseNegatives": instance_false_negatives,
        "instancePrecision": round(instance_precision, 6),
        "instanceRecall": round(instance_recall, 6),
        "gpuProcessingMsP50": round(percentile(gpu_elapsed_ms, 50), 3),
        "gpuProcessingMsP95": round(gpu_p95, 3),
        "gpuProcessingMsMax": round(max(gpu_elapsed_ms, default=0.0), 3),
        "gpuPathWallMsP50": round(percentile(gpu_wall_ms, 50), 3),
        "gpuPathWallMsP95": round(gpu_wall_p95, 3),
        "gpuPathWallMsMax": round(max(gpu_wall_ms, default=0.0), 3),
        "gpuCandidateCountP95": int(percentile(candidate_counts, 95)),
        "cpuIdFrameCounts": {str(key): value for key, value in sorted(cpu_id_counts.items())},
        "gpuIdFrameCounts": {str(key): value for key, value in sorted(gpu_id_counts.items())},
        "cpuIdInstanceCounts": {
            str(key): value for key, value in sorted(cpu_instance_counts.items())
        },
        "gpuIdInstanceCounts": {
            str(key): value for key, value in sorted(gpu_instance_counts.items())
        },
        "deviceInput": True,
        "deviceCandidateExtraction": True,
        "deviceDictionaryDecode": True,
        "deviceInterop": "DLPack",
        "hostImageTransfer": "validation_cpu_oracle_only",
        "cpuOracleHostBytes": cpu_oracle_host_bytes,
        "resultHostBytes": result_host_bytes,
        "mismatches": mismatches,
        "acceptance": {
            "minimumPrecision": 0.98,
            "minimumRecall": 0.95,
            "minimumInstancePrecision": 0.98,
            "minimumInstanceRecall": 0.95,
            "maximumGpuP95Ms": 20.0,
            "maximumGpuPathWallP95Ms": 20.0,
        },
        "elapsedSeconds": round(time.perf_counter() - started_at, 3),
        "passed": passed,
    }
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(
        f"{'PASS' if passed else 'FAIL'} frames={compared_frames} precision={precision:.3%} "
        f"recall={recall:.3%} instance-precision={instance_precision:.3%} "
        f"instance-recall={instance_recall:.3%} gpu-p95={gpu_p95:.3f}ms "
        f"wall-p95={gpu_wall_p95:.3f}ms"
    )
    print(f"Report: {output}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
