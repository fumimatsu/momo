#!/usr/bin/env python3
"""Compose comparable CPU OpenCV and GPU-only ArUco capacity reports."""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path


def parse_marker_ids(value: str) -> set[int]:
    marker_ids = {int(part.strip()) for part in value.split(",") if part.strip()}
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    return marker_ids


def aggregate_marker_counts(case: dict, field: str) -> dict[int, int]:
    counts: dict[int, int] = {}
    for worker in case.get("workers", []):
        source = worker.get(field, {})
        for marker_id, count in source.items():
            value = int(marker_id)
            counts[value] = counts.get(value, 0) + int(count)
    return counts


def index_cases(report: dict) -> dict[int, dict]:
    return {int(case["sourceCount"]): case for case in report.get("cases", [])}


def build_comparison(cpu_report: dict, gpu_report: dict, expected_ids: set[int]) -> dict:
    for field in ("input", "detectionHz", "recognitionQuality"):
        if cpu_report.get(field) != gpu_report.get(field):
            raise ValueError(f"CPU/GPU report mismatch for {field}")
    if cpu_report.get("decoder") != "nvcodec":
        raise ValueError("CPU report must use decoder=nvcodec")
    if gpu_report.get("decoder") not in ("nvcodec-gpu", "nvcodec-gpu-batch"):
        raise ValueError("GPU report must use decoder=nvcodec-gpu or nvcodec-gpu-batch")

    cpu_cases = index_cases(cpu_report)
    gpu_cases = index_cases(gpu_report)
    if set(cpu_cases) != set(gpu_cases):
        raise ValueError("CPU/GPU source counts differ")

    comparisons = []
    for source_count in sorted(cpu_cases):
        cpu = cpu_cases[source_count]
        gpu = gpu_cases[source_count]
        cpu_processing = float(cpu.get("processingMsP95", cpu.get("detectionMsP95", 0)))
        gpu_processing = float(gpu.get("processingMsP95", gpu.get("detectionMsP95", 0)))
        cpu_percent = float(cpu.get("cpuPercentP95", 0))
        gpu_cpu_percent = float(gpu.get("cpuPercentP95", 0))
        cpu_marker_counts = aggregate_marker_counts(cpu, "marker_ids")
        gpu_marker_counts = aggregate_marker_counts(gpu, "marker_ids")
        cpu_marker_frame_counts = aggregate_marker_counts(cpu, "marker_id_frames")
        gpu_marker_frame_counts = aggregate_marker_counts(gpu, "marker_id_frames")
        comparisons.append(
            {
                "sourceCount": source_count,
                "cpuPassed": bool(cpu.get("passed")),
                "gpuPassed": bool(gpu.get("passed")),
                "cpuMinimumDetectionFps": cpu.get("minimumDetectionFps", 0),
                "gpuMinimumDetectionFps": gpu.get("minimumDetectionFps", 0),
                "cpuProcessingMsP95": cpu_processing,
                "gpuProcessingMsP95": gpu_processing,
                "processingSpeedup": round(cpu_processing / gpu_processing, 3)
                if gpu_processing > 0
                else 0,
                "cpuPercentP95": cpu_percent,
                "gpuPathCpuPercentP95": gpu_cpu_percent,
                "cpuReductionPercent": round((1.0 - gpu_cpu_percent / cpu_percent) * 100.0, 3)
                if cpu_percent > 0
                else 0,
                "cpuPathGpuPercentP95": cpu.get("gpuPercentP95", 0),
                "gpuPathGpuPercentP95": gpu.get("gpuPercentP95", 0),
                "cpuPathDecoderPercentP95": cpu.get("decoderPercentP95", 0),
                "gpuPathDecoderPercentP95": gpu.get("decoderPercentP95", 0),
                "cpuPathGpuMemoryUsedMBMax": cpu.get("gpuMemoryUsedMBMax", 0),
                "gpuPathGpuMemoryUsedMBMax": gpu.get("gpuMemoryUsedMBMax", 0),
                "expectedMarkerInstanceCounts": {
                    str(marker_id): {
                        "cpu": cpu_marker_counts.get(marker_id, 0),
                        "gpu": gpu_marker_counts.get(marker_id, 0),
                    }
                    for marker_id in sorted(expected_ids)
                },
                "expectedMarkerFrameCounts": {
                    str(marker_id): {
                        "cpu": cpu_marker_frame_counts.get(marker_id, 0),
                        "gpu": gpu_marker_frame_counts.get(marker_id, 0),
                    }
                    for marker_id in sorted(expected_ids)
                },
                "gpuDiagnosticMarkerInstanceCounts": {
                    str(marker_id): count
                    for marker_id, count in sorted(gpu_marker_counts.items())
                    if marker_id not in expected_ids
                },
                "gpuDiagnosticMarkerFrameCounts": {
                    str(marker_id): count
                    for marker_id, count in sorted(gpu_marker_frame_counts.items())
                    if marker_id not in expected_ids
                },
            }
        )

    cpu_frontier = max(
        (case["sourceCount"] for case in comparisons if case["cpuPassed"]), default=0
    )
    gpu_frontier = max(
        (case["sourceCount"] for case in comparisons if case["gpuPassed"]), default=0
    )
    return {
        "schemaVersion": 1,
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": cpu_report.get("host", ""),
        "input": cpu_report.get("input"),
        "detectionHz": cpu_report.get("detectionHz"),
        "recognitionQuality": cpu_report.get("recognitionQuality"),
        "expectedMarkerIds": sorted(expected_ids),
        "cpuBackend": "nvcodec_host_luma_opencv_aruco",
        "gpuBackend": gpu_report.get("decoder"),
        "largestPassedCpuSourceCount": cpu_frontier,
        "largestPassedGpuSourceCount": gpu_frontier,
        "comparisons": comparisons,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpu-report", required=True)
    parser.add_argument("--gpu-report", required=True)
    parser.add_argument("--expected-marker-ids", type=parse_marker_ids, default={1, 2, 3})
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    cpu_report = json.loads(Path(args.cpu_report).read_text(encoding="utf-8"))
    gpu_report = json.loads(Path(args.gpu_report).read_text(encoding="utf-8"))
    comparison = build_comparison(cpu_report, gpu_report, args.expected_marker_ids)
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(comparison, ensure_ascii=False, indent=2), encoding="utf-8")
    print(
        f"CPU frontier={comparison['largestPassedCpuSourceCount']} "
        f"GPU frontier={comparison['largestPassedGpuSourceCount']}"
    )
    print(f"Report: {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
