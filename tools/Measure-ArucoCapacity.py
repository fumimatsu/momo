#!/usr/bin/env python3
"""Measure real-time H.264 decode plus ArUco detection capacity."""

from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

import cv2
import psutil


DEFAULT_DETECTION_HZ = 25.0
MINIMUM_RATE_FACTOR = 0.95


@dataclass
class WorkerResult:
    source_id: int
    decoded_frames: int = 0
    detections: int = 0
    marker_frames: int = 0
    marker_ids: dict[int, int] = field(default_factory=dict)
    detection_ms: list[float] = field(default_factory=list)
    read_errors: int = 0
    error: str | None = None


def percentile(values: list[float], value: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(value / 100 * len(ordered)) - 1))
    return ordered[index]


def parse_counts(value: str) -> list[int]:
    counts = [int(part.strip()) for part in value.split(",") if part.strip()]
    if not counts or any(count < 1 or count > 32 for count in counts):
        raise argparse.ArgumentTypeError("source counts must be comma-separated values in 1..32")
    if len(set(counts)) != len(counts):
        raise argparse.ArgumentTypeError("source counts must not contain duplicates")
    return counts


def make_detector() -> cv2.aruco.ArucoDetector:
    dictionary = cv2.aruco.getPredefinedDictionary(cv2.aruco.DICT_4X4_50)
    parameters = cv2.aruco.DetectorParameters()
    parameters.minDistanceToBorder = 0
    parameters.adaptiveThreshWinSizeMin = 5
    parameters.adaptiveThreshWinSizeMax = 23
    parameters.adaptiveThreshWinSizeStep = 4
    parameters.adaptiveThreshConstant = 7.0
    parameters.minMarkerPerimeterRate = 0.035
    parameters.maxMarkerPerimeterRate = 2.0
    parameters.minOtsuStdDev = 0.8
    parameters.errorCorrectionRate = 0.6
    return cv2.aruco.ArucoDetector(dictionary, parameters)


def run_worker(
    source_id: int,
    video_path: str,
    duration: float,
    detection_hz: float,
    quality: float,
    start_event: threading.Event,
    result: WorkerResult,
) -> None:
    capture = cv2.VideoCapture(video_path)
    if not capture.isOpened():
        result.error = "video_open_failed"
        return
    source_fps = capture.get(cv2.CAP_PROP_FPS)
    if source_fps <= 0:
        source_fps = 50.0
    detector = make_detector()
    start_event.wait()
    started_at = time.perf_counter()
    deadline = started_at + duration
    next_frame_at = started_at
    next_detection_at = started_at
    frame_interval = 1.0 / source_fps
    detection_interval = 1.0 / detection_hz

    try:
        while True:
            now = time.perf_counter()
            if now >= deadline:
                break
            if now < next_frame_at:
                time.sleep(min(next_frame_at - now, 0.002))
                continue
            ok, frame = capture.read()
            if not ok:
                result.read_errors += 1
                capture.set(cv2.CAP_PROP_POS_FRAMES, 0)
                ok, frame = capture.read()
                if not ok:
                    result.error = "video_read_failed"
                    break
            result.decoded_frames += 1
            next_frame_at += frame_interval
            if next_frame_at < now - frame_interval:
                next_frame_at = now

            if now < next_detection_at:
                continue
            next_detection_at += detection_interval
            if next_detection_at < now - detection_interval:
                next_detection_at = now

            gray = cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY)
            resized_height = max(1, int(gray.shape[0] * quality))
            resized_width = max(1, int(resized_height * 16.0 / 9.0))
            gray = cv2.resize(gray, (resized_width, resized_height), interpolation=cv2.INTER_LINEAR)
            detection_started = time.perf_counter()
            _, ids, _ = detector.detectMarkers(gray)
            result.detection_ms.append((time.perf_counter() - detection_started) * 1000)
            result.detections += 1
            if ids is not None:
                result.marker_frames += 1
                for marker_id in ids.flatten():
                    value = int(marker_id)
                    result.marker_ids[value] = result.marker_ids.get(value, 0) + 1
    except Exception as exc:  # pragma: no cover - diagnostic boundary
        result.error = f"{type(exc).__name__}: {exc}"
    finally:
        capture.release()


def read_exact(stream, size: int) -> bytes:
    chunks = bytearray()
    while len(chunks) < size:
        chunk = stream.read(size - len(chunks))
        if not chunk:
            break
        chunks.extend(chunk)
    return bytes(chunks)


def run_hardware_worker(
    source_id: int,
    video_path: str,
    duration: float,
    detection_hz: float,
    quality: float,
    ffmpeg_path: str,
    decoder: str,
    source_width: int,
    source_height: int,
    start_event: threading.Event,
    result: WorkerResult,
) -> None:
    resized_height = max(1, int(source_height * quality))
    resized_width = max(1, int(resized_height * 16.0 / 9.0))
    resized_height -= resized_height % 2
    resized_width -= resized_width % 2
    frame_bytes = resized_width * resized_height
    hardware_options = (
        ["-hwaccel", "qsv", "-hwaccel_output_format", "qsv"]
        if decoder == "qsv"
        else ["-hwaccel", "cuda", "-hwaccel_output_format", "cuda", "-c:v", "h264_cuvid"]
    )
    scale_filter = (
        f"vpp_qsv=w={resized_width}:h={resized_height}"
        if decoder == "qsv"
        else f"scale_cuda={resized_width}:{resized_height}"
    )
    command = [
        ffmpeg_path,
        "-hide_banner",
        "-loglevel",
        "error",
        "-re",
        "-stream_loop",
        "-1",
        *hardware_options,
        "-i",
        video_path,
        "-an",
        "-vf",
        (
            f"{scale_filter},hwdownload,format=nv12,fps={detection_hz:g},format=gray"
        ),
        "-f",
        "rawvideo",
        "-pix_fmt",
        "gray",
        "pipe:1",
    ]
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=frame_bytes * 2)
    detector = make_detector()
    start_event.wait()
    started_at = time.perf_counter()
    deadline = started_at + duration
    try:
        assert process.stdout is not None
        while time.perf_counter() < deadline:
            raw = read_exact(process.stdout, frame_bytes)
            if len(raw) != frame_bytes:
                result.read_errors += 1
                break
            gray = memoryview(raw)
            gray = __import__("numpy").frombuffer(gray, dtype="uint8").reshape((resized_height, resized_width))
            detection_started = time.perf_counter()
            _, ids, _ = detector.detectMarkers(gray)
            result.detection_ms.append((time.perf_counter() - detection_started) * 1000)
            result.decoded_frames += 1
            result.detections += 1
            if ids is not None:
                result.marker_frames += 1
                for marker_id in ids.flatten():
                    value = int(marker_id)
                    result.marker_ids[value] = result.marker_ids.get(value, 0) + 1
    except Exception as exc:  # pragma: no cover - diagnostic boundary
        result.error = f"{type(exc).__name__}: {exc}"
    finally:
        process.kill()
        try:
            _, stderr = process.communicate(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            _, stderr = process.communicate()
        if result.read_errors and not result.error:
            result.error = stderr.decode("utf-8", errors="replace").strip() or "hardware_video_read_failed"


def monitor_process(stop_event: threading.Event, samples: list[dict[str, float]]) -> None:
    process = psutil.Process(os.getpid())
    logical_cpus = psutil.cpu_count(logical=True) or 1
    known_processes: dict[int, psutil.Process] = {}
    while not stop_event.wait(0.5):
        current = [process]
        try:
            current.extend(process.children(recursive=True))
        except (psutil.NoSuchProcess, psutil.AccessDenied):
            pass
        cpu_percent = 0.0
        working_set = 0
        for item in current:
            if item.pid not in known_processes:
                known_processes[item.pid] = item
                try:
                    item.cpu_percent(interval=None)
                except (psutil.NoSuchProcess, psutil.AccessDenied):
                    pass
                continue
            try:
                cpu_percent += item.cpu_percent(interval=None)
                working_set += item.memory_info().rss
            except (psutil.NoSuchProcess, psutil.AccessDenied):
                pass
        samples.append(
            {
                "cpuPercent": cpu_percent / logical_cpus,
                "workingSetMB": working_set / (1024 * 1024),
            }
        )


def run_case(
    video_path: str,
    count: int,
    duration: float,
    detection_hz: float,
    quality: float,
    decoder: str,
    ffmpeg_path: str | None,
    source_width: int,
    source_height: int,
    max_cpu_percent: float,
) -> dict:
    start_event = threading.Event()
    stop_monitor = threading.Event()
    monitor_samples: list[dict[str, float]] = []
    results = [WorkerResult(source_id=index + 1) for index in range(count)]
    if decoder in ("qsv", "cuda"):
        assert ffmpeg_path is not None
        threads = [
            threading.Thread(
                target=run_hardware_worker,
                args=(
                    result.source_id,
                    video_path,
                    duration,
                    detection_hz,
                    quality,
                    ffmpeg_path,
                    decoder,
                    source_width,
                    source_height,
                    start_event,
                    result,
                ),
                name=f"aruco-source-{result.source_id:02d}",
            )
            for result in results
        ]
    else:
        threads = [
            threading.Thread(
                target=run_worker,
                args=(result.source_id, video_path, duration, detection_hz, quality, start_event, result),
                name=f"aruco-source-{result.source_id:02d}",
            )
            for result in results
        ]
    monitor = threading.Thread(target=monitor_process, args=(stop_monitor, monitor_samples), name="process-monitor")
    for thread in threads:
        thread.start()
    monitor.start()
    started_at = time.perf_counter()
    start_event.set()
    for thread in threads:
        thread.join()
    elapsed = time.perf_counter() - started_at
    stop_monitor.set()
    monitor.join()

    worker_summaries = []
    failures = []
    minimum_decode_fps = math.inf
    minimum_detection_fps = math.inf
    all_detection_ms: list[float] = []
    for result in results:
        decode_fps = result.decoded_frames / elapsed
        detection_fps = result.detections / elapsed
        minimum_decode_fps = min(minimum_decode_fps, decode_fps)
        minimum_detection_fps = min(minimum_detection_fps, detection_fps)
        all_detection_ms.extend(result.detection_ms)
        if result.error:
            failures.append(f"source {result.source_id}: {result.error}")
        minimum_required_decode_fps = (
            detection_hz * MINIMUM_RATE_FACTOR if decoder in ("qsv", "cuda") else 50.0 * MINIMUM_RATE_FACTOR
        )
        if decode_fps < minimum_required_decode_fps:
            failures.append(
                f"source {result.source_id}: output FPS {decode_fps:.2f} below {minimum_required_decode_fps:.2f}"
            )
        if detection_fps < detection_hz * MINIMUM_RATE_FACTOR:
            failures.append(
                f"source {result.source_id}: detection FPS {detection_fps:.2f} "
                f"below {detection_hz * MINIMUM_RATE_FACTOR:.2f}"
            )
        worker_summaries.append(
            {
                **{key: value for key, value in asdict(result).items() if key != "detection_ms"},
                "decodeFps": round(decode_fps, 3),
                "detectionFps": round(detection_fps, 3),
                "detectionMsP95": round(percentile(result.detection_ms, 95) or 0, 3),
            }
        )

    cpu_values = [sample["cpuPercent"] for sample in monitor_samples]
    memory_values = [sample["workingSetMB"] for sample in monitor_samples]
    detection_p95 = percentile(all_detection_ms, 95)
    if detection_p95 is not None and detection_p95 > 1000.0 / detection_hz:
        failures.append(
            f"detection latency p95 {detection_p95:.2f}ms exceeds one {detection_hz:g}Hz interval"
        )
    cpu_p95 = percentile(cpu_values, 95) or 0
    if cpu_p95 > max_cpu_percent:
        failures.append(f"process tree CPU p95 {cpu_p95:.2f}% exceeds {max_cpu_percent:.2f}%")
    return {
        "sourceCount": count,
        "passed": not failures,
        "elapsedSeconds": round(elapsed, 3),
        "minimumDecodeFps": round(minimum_decode_fps, 3),
        "minimumDetectionFps": round(minimum_detection_fps, 3),
        "detectionMsP95": round(detection_p95 or 0, 3),
        "cpuPercentP95": round(cpu_p95, 3),
        "workingSetMBMax": round(max(memory_values) if memory_values else 0, 3),
        "failures": failures,
        "workers": worker_summaries,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, help="upright H.264 recording")
    parser.add_argument("--source-counts", type=parse_counts, default=parse_counts("1,2,4,6,8,12,16,24,32"))
    parser.add_argument("--duration", type=float, default=30.0)
    parser.add_argument("--detection-hz", type=float, default=DEFAULT_DETECTION_HZ)
    parser.add_argument("--quality", type=float, default=0.6)
    parser.add_argument("--decoder", choices=("opencv", "qsv", "cuda"), default="opencv")
    parser.add_argument("--ffmpeg", help="FFmpeg executable with QSV/CUDA support")
    parser.add_argument("--max-cpu-percent", type=float, default=60.0)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.duration < 5:
        parser.error("--duration must be at least 5 seconds")
    if args.detection_hz <= 0 or args.detection_hz > 50:
        parser.error("--detection-hz must be in (0, 50]")
    if args.quality <= 0 or args.quality > 1:
        parser.error("--quality must be in (0, 1]")
    if args.max_cpu_percent <= 0 or args.max_cpu_percent > 100:
        parser.error("--max-cpu-percent must be in (0, 100]")
    if args.decoder in ("qsv", "cuda") and (not args.ffmpeg or not Path(args.ffmpeg).is_file()):
        parser.error("hardware decoder requires an existing --ffmpeg executable")
    input_path = Path(args.input).resolve()
    if not input_path.is_file():
        parser.error(f"input does not exist: {input_path}")

    capture = cv2.VideoCapture(str(input_path))
    metadata = {
        "width": int(capture.get(cv2.CAP_PROP_FRAME_WIDTH)),
        "height": int(capture.get(cv2.CAP_PROP_FRAME_HEIGHT)),
        "fps": capture.get(cv2.CAP_PROP_FPS),
        "codecFourCC": int(capture.get(cv2.CAP_PROP_FOURCC)),
    }
    capture.release()
    minimum_input_fps = args.detection_hz * MINIMUM_RATE_FACTOR
    if metadata["fps"] < minimum_input_fps:
        parser.error(
            f"input FPS {metadata['fps']:.3f} is below the required {minimum_input_fps:.3f} "
            f"for a {args.detection_hz:g}Hz test"
        )
    cases = []
    for count in args.source_counts:
        print(f"AruCo capacity: {count} sources", flush=True)
        case = run_case(
            str(input_path),
            count,
            args.duration,
            args.detection_hz,
            args.quality,
            args.decoder,
            str(Path(args.ffmpeg).resolve()) if args.ffmpeg else None,
            metadata["width"],
            metadata["height"],
            args.max_cpu_percent,
        )
        cases.append(case)
        print(
            f"  {'PASS' if case['passed'] else 'FAIL'} "
            f"decode={case['minimumDecodeFps']:.2f}fps detect={case['minimumDetectionFps']:.2f}fps "
            f"latency-p95={case['detectionMsP95']:.2f}ms cpu-p95={case['cpuPercentP95']:.2f}%",
            flush=True,
        )

    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    report = {
        "schemaVersion": 1,
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": os.environ.get("COMPUTERNAME", ""),
        "opencvVersion": cv2.__version__,
        "logicalCpuCount": psutil.cpu_count(logical=True),
        "input": str(input_path),
        "inputMetadata": metadata,
        "acceptance": {
            "minimumInputFps": minimum_input_fps,
            "minimumDetectionFps": args.detection_hz * MINIMUM_RATE_FACTOR,
            "maximumDetectionLatencyP95Ms": 1000.0 / args.detection_hz,
            "maximumProcessTreeCpuP95Percent": args.max_cpu_percent,
        },
        "detectionHz": args.detection_hz,
        "recognitionQuality": args.quality,
        "decoder": args.decoder,
        "maxCpuPercent": args.max_cpu_percent,
        "cases": cases,
    }
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"Report: {output}")
    return 0 if all(case["passed"] for case in cases) else 1


if __name__ == "__main__":
    raise SystemExit(main())
