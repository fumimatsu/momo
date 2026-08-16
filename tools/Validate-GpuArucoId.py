#!/usr/bin/env python3
"""Validate GPU ArUco dictionary decoding using CPU-provided candidate corners."""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
import time
from pathlib import Path

import numpy as np

from GpuArucoId import GpuArucoIdDecoder, make_candidate_homographies


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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True)
    parser.add_argument("--frame-count", type=int, default=1500)
    parser.add_argument("--quality", type=float, default=0.6)
    parser.add_argument("--max-hamming", type=int, default=0)
    parser.add_argument("--expected-marker-ids", type=parse_marker_ids, default={1, 2, 3})
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.frame_count < 1:
        parser.error("--frame-count must be positive")
    if args.quality <= 0 or args.quality > 1:
        parser.error("--quality must be in (0, 1]")

    input_path = Path(args.input).resolve()
    if not input_path.is_file():
        parser.error(f"input does not exist: {input_path}")

    nvc = CAPACITY.load_nvcodec()
    gpu_decoder = GpuArucoIdDecoder(max_hamming=args.max_hamming)
    cp = gpu_decoder.cp
    decoder = nvc.SimpleDecoder(str(input_path), gpu_id=0, use_device_memory=True)
    metadata = decoder.get_stream_metadata()
    source_width = int(metadata.width)
    source_height = int(metadata.height)
    detector = CAPACITY.make_detector()
    requested_frames = min(args.frame_count, int(metadata.num_frames))
    compared_frames = 0
    candidate_count = 0
    compared_candidate_count = 0
    diagnostic_cpu_candidates = []
    matched_candidates = 0
    mismatches = []
    gpu_elapsed_ms = []
    cpu_oracle_host_bytes = 0
    result_host_bytes = 0
    started_at = time.perf_counter()

    try:
        for frame_index in range(requested_frames):
            frames = decoder.get_batch_frames(1)
            if not frames:
                break
            frame = frames[0]
            nv12_device = cp.from_dlpack(frame)
            start_event = cp.cuda.Event()
            stop_event = cp.cuda.Event()
            start_event.record()
            gray_device = gpu_decoder.resize_nv12_luma(
                nv12_device, source_width, source_height, args.quality
            )
            stop_event.record()
            stop_event.synchronize()
            frame_gpu_ms = cp.cuda.get_elapsed_time(start_event, stop_event)

            gray_host = cp.asnumpy(gray_device)
            cpu_oracle_host_bytes += gray_host.nbytes
            corners, ids, _ = detector.detectMarkers(gray_host)
            raw_expected_ids = [] if ids is None else [int(marker_id) for marker_id in ids.flatten()]
            candidate_count += len(raw_expected_ids)
            valid_candidates = []
            expected_ids = []
            for candidate_index, (candidate_corners, expected_id) in enumerate(
                zip(corners, raw_expected_ids)
            ):
                if expected_id in args.expected_marker_ids:
                    valid_candidates.append(candidate_corners)
                    expected_ids.append(expected_id)
                elif len(diagnostic_cpu_candidates) < 100:
                    diagnostic_cpu_candidates.append(
                        {
                            "frameIndex": frame_index,
                            "candidateIndex": candidate_index,
                            "reportedId": expected_id,
                        }
                    )
            candidate_corners = (
                np.empty((0, 4, 2), dtype=np.float32)
                if not valid_candidates
                else np.asarray(valid_candidates)
            )

            if len(candidate_corners):
                homographies = make_candidate_homographies(candidate_corners)
                start_event.record()
                results, marker_mask = gpu_decoder.decode(gray_device, homographies)
                stop_event.record()
                stop_event.synchronize()
                frame_gpu_ms += cp.cuda.get_elapsed_time(start_event, stop_event)
                actual_ids = [result.marker_id for result in results]
                result_host_bytes += len(results) * 6 + 8
            else:
                results = []
                marker_mask = 0
                actual_ids = []
                result_host_bytes += 8

            compared_frames += 1
            compared_candidate_count += len(expected_ids)
            for candidate_index, expected_id in enumerate(expected_ids):
                actual_id = actual_ids[candidate_index]
                if actual_id == expected_id:
                    matched_candidates += 1
                elif len(mismatches) < 100:
                    result = results[candidate_index]
                    mismatches.append(
                        {
                            "frameIndex": frame_index,
                            "candidateIndex": candidate_index,
                            "expectedId": expected_id,
                            "actualId": actual_id,
                            "hamming": result.hamming,
                            "borderErrors": result.border_errors,
                            "code": result.code,
                        }
                    )
            expected_mask = 0
            for expected_id in expected_ids:
                expected_mask |= 1 << expected_id
            if marker_mask != expected_mask and not expected_ids and len(mismatches) < 100:
                mismatches.append(
                    {"frameIndex": frame_index, "expectedMask": expected_mask, "actualMask": marker_mask}
                )
            gpu_elapsed_ms.append(frame_gpu_ms)
    finally:
        del decoder

    agreement = matched_candidates / compared_candidate_count if compared_candidate_count else 1.0
    passed = compared_frames == requested_frames and agreement == 1.0 and not mismatches
    sorted_gpu_ms = sorted(gpu_elapsed_ms)
    p95_index = max(0, min(len(sorted_gpu_ms) - 1, int(np.ceil(len(sorted_gpu_ms) * 0.95)) - 1))
    report = {
        "schemaVersion": 1,
        "stage": "gpu_dictionary_decode_with_cpu_candidate_oracle",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "input": str(input_path),
        "expectedMarkerIds": sorted(args.expected_marker_ids),
        "requestedFrames": requested_frames,
        "comparedFrames": compared_frames,
        "candidateCount": candidate_count,
        "comparedCandidateCount": compared_candidate_count,
        "diagnosticCpuCandidateCount": candidate_count - compared_candidate_count,
        "diagnosticCpuCandidates": diagnostic_cpu_candidates,
        "matchedCandidates": matched_candidates,
        "candidateIdAgreement": round(agreement, 6),
        "gpuProcessingMsP95": round(sorted_gpu_ms[p95_index] if sorted_gpu_ms else 0.0, 3),
        "deviceInput": True,
        "deviceInterop": "DLPack",
        "hostImageTransfer": "validation_cpu_candidate_oracle_only",
        "cpuOracleHostBytes": cpu_oracle_host_bytes,
        "resultHostBytes": result_host_bytes,
        "mismatches": mismatches,
        "elapsedSeconds": round(time.perf_counter() - started_at, 3),
        "passed": passed,
    }
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(
        f"{'PASS' if passed else 'FAIL'} frames={compared_frames} candidates={candidate_count} "
        f"agreement={agreement:.3%} gpu-p95={report['gpuProcessingMsP95']:.3f}ms"
    )
    print(f"Report: {output}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
