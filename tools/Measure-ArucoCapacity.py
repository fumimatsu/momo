#!/usr/bin/env python3
"""Measure real-time H.264 decode plus ArUco detection capacity."""

from __future__ import annotations

import argparse
import json
import math
import os
import shutil
import subprocess
import sys
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

import cv2
import numpy as np
import psutil


DEFAULT_DETECTION_HZ = 25.0
MINIMUM_RATE_FACTOR = 0.95
_DLL_DIRECTORY_HANDLES: list[object] = []
_NV_CODEC_MODULE = None
_NV_CODEC_LOCK = threading.Lock()


@dataclass
class WorkerResult:
    source_id: int
    decoded_frames: int = 0
    detections: int = 0
    marker_frames: int = 0
    marker_ids: dict[int, int] = field(default_factory=dict)
    marker_id_frames: dict[int, int] = field(default_factory=dict)
    detection_ms: list[float] = field(default_factory=list)
    processing_ms: list[float] = field(default_factory=list)
    active_elapsed_seconds: float = 0.0
    read_errors: int = 0
    error: str | None = None


def record_marker_observation(result: WorkerResult, marker_ids) -> None:
    values = [int(marker_id) for marker_id in marker_ids]
    if not values:
        return
    result.marker_frames += 1
    for value in values:
        result.marker_ids[value] = result.marker_ids.get(value, 0) + 1
    for value in set(values):
        result.marker_id_frames[value] = result.marker_id_frames.get(value, 0) + 1


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


def find_nvcodec_cuda_root(prefix: Path | None = None) -> Path | None:
    candidates: list[Path] = []
    if prefix is not None:
        candidates.append(prefix / "Lib" / "site-packages" / "nvidia" / "cuda_runtime")
    cuda_path = os.environ.get("CUDA_PATH")
    if cuda_path:
        candidates.append(Path(cuda_path))
    candidates.append(Path(sys.prefix) / "Lib" / "site-packages" / "nvidia" / "cuda_runtime")
    for candidate in candidates:
        if (candidate / "bin" / "cudart64_12.dll").is_file():
            return candidate.resolve()
    return None


def load_nvcodec():
    global _NV_CODEC_MODULE
    with _NV_CODEC_LOCK:
        if _NV_CODEC_MODULE is not None:
            return _NV_CODEC_MODULE
        if os.name == "nt":
            cuda_root = find_nvcodec_cuda_root()
            if cuda_root is not None:
                os.environ["CUDA_PATH"] = str(cuda_root)
                _DLL_DIRECTORY_HANDLES.append(os.add_dll_directory(str(cuda_root / "bin")))
        try:
            import PyNvVideoCodec as nvc
        except (ImportError, OSError) as exc:
            raise RuntimeError(
                "nvcodec backend requires PyNvVideoCodec and the CUDA 12 runtime; "
                "run Initialize-ArucoCapacity.ps1 -IncludeNvCodec"
            ) from exc
        _NV_CODEC_MODULE = nvc
        return _NV_CODEC_MODULE


def prepare_nvcodec_luma(nv12: np.ndarray, source_width: int, source_height: int, quality: float) -> np.ndarray:
    if nv12.ndim != 2 or nv12.shape[0] < source_height or nv12.shape[1] < source_width:
        raise ValueError(f"unexpected NV12 frame shape: {nv12.shape}")
    resized_height = max(1, int(source_height * quality))
    resized_width = max(1, int(resized_height * 16.0 / 9.0))
    resized_height -= resized_height % 2
    resized_width -= resized_width % 2
    luma = nv12[:source_height, :source_width]
    return cv2.resize(luma, (resized_width, resized_height), interpolation=cv2.INTER_LINEAR)


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
                record_marker_observation(result, ids.flatten())
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
                record_marker_observation(result, ids.flatten())
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


def run_nvcodec_worker(
    source_id: int,
    video_path: str,
    duration: float,
    detection_hz: float,
    quality: float,
    source_width: int,
    source_height: int,
    start_event: threading.Event,
    ready_event: threading.Event,
    result: WorkerResult,
) -> None:
    decoder = None
    try:
        nvc = load_nvcodec()
        decoder = nvc.ThreadedDecoder(video_path, 8, gpu_id=0, use_device_memory=False)
        metadata = decoder.get_stream_metadata()
        source_fps = float(metadata.average_fps) if metadata.average_fps > 0 else 50.0
        detector = make_detector()
        ready_event.set()
        start_event.wait()
        started_at = time.perf_counter()
        deadline = started_at + duration
        next_frame_at = started_at
        frame_interval = 1.0 / min(source_fps, detection_hz)
        while time.perf_counter() < deadline:
            now = time.perf_counter()
            if now < next_frame_at:
                time.sleep(min(next_frame_at - now, 0.002))
                continue
            frames = decoder.get_batch_frames(1)
            if not frames:
                decoder.reconfigure_decoder(video_path, 0)
                continue
            processing_started = time.perf_counter()
            nv12 = np.from_dlpack(frames[0])
            gray = prepare_nvcodec_luma(nv12, source_width, source_height, quality)
            detection_started = time.perf_counter()
            _, ids, _ = detector.detectMarkers(gray)
            result.detection_ms.append((time.perf_counter() - detection_started) * 1000)
            result.processing_ms.append((time.perf_counter() - processing_started) * 1000)
            result.decoded_frames += 1
            result.detections += 1
            next_frame_at += frame_interval
            if next_frame_at < now - frame_interval:
                next_frame_at = now
            if ids is not None:
                record_marker_observation(result, ids.flatten())
    except Exception as exc:  # pragma: no cover - diagnostic boundary
        result.error = f"{type(exc).__name__}: {exc}"
    finally:
        ready_event.set()
        if decoder is not None:
            decoder.end()


def run_nvcodec_gpu_worker(
    source_id: int,
    video_path: str,
    duration: float,
    detection_hz: float,
    quality: float,
    source_width: int,
    source_height: int,
    start_event: threading.Event,
    ready_event: threading.Event,
    result: WorkerResult,
) -> None:
    decoder = None
    try:
        from GpuArucoDetector import GpuArucoDetector

        nvc = load_nvcodec()
        gpu_detector = GpuArucoDetector(allowed_marker_ids=range(50))
        cp = gpu_detector.cp
        decoder = nvc.ThreadedDecoder(video_path, 8, gpu_id=0, use_device_memory=True)
        metadata = decoder.get_stream_metadata()
        source_fps = float(metadata.average_fps) if metadata.average_fps > 0 else 50.0
        ready_event.set()
        start_event.wait()
        started_at = time.perf_counter()
        deadline = started_at + duration
        next_frame_at = started_at
        frame_interval = 1.0 / min(source_fps, detection_hz)
        while time.perf_counter() < deadline:
            now = time.perf_counter()
            if now < next_frame_at:
                time.sleep(min(next_frame_at - now, 0.002))
                continue
            frames = decoder.get_batch_frames(1)
            if not frames:
                decoder.reconfigure_decoder(video_path, 0)
                continue
            processing_started = time.perf_counter()
            nv12_device = cp.from_dlpack(frames[0])
            gray_device = gpu_detector.decoder.resize_nv12_luma(
                nv12_device, source_width, source_height, quality
            )
            detection_started = time.perf_counter()
            detection = gpu_detector.detect(gray_device)
            result.detection_ms.append((time.perf_counter() - detection_started) * 1000)
            result.processing_ms.append((time.perf_counter() - processing_started) * 1000)
            result.decoded_frames += 1
            result.detections += 1
            next_frame_at += frame_interval
            if next_frame_at < now - frame_interval:
                next_frame_at = now
            if detection.marker_ids:
                record_marker_observation(result, detection.marker_ids)
    except Exception as exc:  # pragma: no cover - diagnostic boundary
        result.error = f"{type(exc).__name__}: {exc}"
    finally:
        ready_event.set()
        if decoder is not None:
            decoder.end()


def run_nvcodec_gpu_batch_worker(
    video_path: str,
    duration: float,
    detection_hz: float,
    quality: float,
    source_width: int,
    source_height: int,
    start_event: threading.Event,
    ready_events: list[threading.Event],
    results: list[WorkerResult],
) -> None:
    decoders = []
    try:
        from GpuArucoDetector import GpuArucoDetector

        nvc = load_nvcodec()
        gpu_detector = GpuArucoDetector(allowed_marker_ids=range(50))
        cp = gpu_detector.cp
        for _ in results:
            decoder = nvc.ThreadedDecoder(video_path, 8, gpu_id=0, use_device_memory=True)
            decoders.append(decoder)
        metadata = decoders[0].get_stream_metadata()
        source_fps = float(metadata.average_fps) if metadata.average_fps > 0 else 50.0
        for ready_event in ready_events:
            ready_event.set()
        start_event.wait()
        started_at = time.perf_counter()
        deadline = started_at + duration
        next_frame_at = started_at
        frame_interval = 1.0 / min(source_fps, detection_hz)
        while time.perf_counter() < deadline:
            now = time.perf_counter()
            if now < next_frame_at:
                time.sleep(min(next_frame_at - now, 0.002))
                continue
            frames = []
            for decoder in decoders:
                batch = decoder.get_batch_frames(1)
                if not batch:
                    decoder.reconfigure_decoder(video_path, 0)
                    frames = []
                    break
                frames.append(batch[0])
            if len(frames) != len(decoders):
                continue

            processing_started = time.perf_counter()
            resized_frames = [
                gpu_detector.decoder.resize_nv12_luma(
                    cp.from_dlpack(frame), source_width, source_height, quality
                )
                for frame in frames
            ]
            gray_batch = cp.stack(resized_frames)
            detection_started = time.perf_counter()
            detections = gpu_detector.detect_batch(gray_batch)
            detection_elapsed_ms = (time.perf_counter() - detection_started) * 1000
            processing_elapsed_ms = (time.perf_counter() - processing_started) * 1000
            for result, detection in zip(results, detections):
                result.detection_ms.append(detection_elapsed_ms)
                result.processing_ms.append(processing_elapsed_ms)
                result.decoded_frames += 1
                result.detections += 1
                if detection.marker_ids:
                    record_marker_observation(result, detection.marker_ids)
            next_frame_at += frame_interval
            if next_frame_at < now - frame_interval:
                next_frame_at = now
        active_elapsed = time.perf_counter() - started_at
        for result in results:
            result.active_elapsed_seconds = active_elapsed
    except Exception as exc:  # pragma: no cover - diagnostic boundary
        message = f"{type(exc).__name__}: {exc}"
        for result in results:
            result.error = message
    finally:
        for ready_event in ready_events:
            ready_event.set()
        for decoder in decoders:
            decoder.end()


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


def find_nvidia_smi() -> str | None:
    command = shutil.which("nvidia-smi.exe") or shutil.which("nvidia-smi")
    if command:
        return command
    windows_directory = os.environ.get("WINDIR")
    if windows_directory:
        candidate = Path(windows_directory) / "System32" / "nvidia-smi.exe"
        if candidate.is_file():
            return str(candidate)
    return None


def parse_nvidia_smi_sample(line: str) -> dict[str, float] | None:
    parts = [part.strip() for part in line.split(",")]
    if len(parts) != 3:
        return None
    try:
        return {
            "gpuPercent": float(parts[0]),
            "decoderPercent": float(parts[1]),
            "memoryUsedMB": float(parts[2]),
        }
    except ValueError:
        return None


def monitor_nvidia_smi(
    executable: str,
    stop_event: threading.Event,
    samples: list[dict[str, float]],
) -> None:
    command = [
        executable,
        "--id=0",
        "--query-gpu=utilization.gpu,utilization.decoder,memory.used",
        "--format=csv,noheader,nounits",
        "--loop-ms=500",
    ]
    creation_flags = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
        creationflags=creation_flags,
    )
    try:
        assert process.stdout is not None
        while not stop_event.is_set():
            line = process.stdout.readline()
            if not line:
                break
            sample = parse_nvidia_smi_sample(line)
            if sample is not None:
                samples.append(sample)
    finally:
        process.kill()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()


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
    nvidia_smi: str | None,
) -> dict:
    start_event = threading.Event()
    stop_monitor = threading.Event()
    monitor_samples: list[dict[str, float]] = []
    gpu_monitor_samples: list[dict[str, float]] = []
    results = [WorkerResult(source_id=index + 1) for index in range(count)]
    ready_events = [threading.Event() for _ in results]
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
    elif decoder == "nvcodec":
        threads = [
            threading.Thread(
                target=run_nvcodec_worker,
                args=(
                    result.source_id,
                    video_path,
                    duration,
                    detection_hz,
                    quality,
                    source_width,
                    source_height,
                    start_event,
                    ready_event,
                    result,
                ),
                name=f"aruco-source-{result.source_id:02d}",
            )
            for result, ready_event in zip(results, ready_events)
        ]
    elif decoder == "nvcodec-gpu":
        threads = [
            threading.Thread(
                target=run_nvcodec_gpu_worker,
                args=(
                    result.source_id,
                    video_path,
                    duration,
                    detection_hz,
                    quality,
                    source_width,
                    source_height,
                    start_event,
                    ready_event,
                    result,
                ),
                name=f"gpu-aruco-source-{result.source_id:02d}",
            )
            for result, ready_event in zip(results, ready_events)
        ]
    elif decoder == "nvcodec-gpu-batch":
        threads = [
            threading.Thread(
                target=run_nvcodec_gpu_batch_worker,
                args=(
                    video_path,
                    duration,
                    detection_hz,
                    quality,
                    source_width,
                    source_height,
                    start_event,
                    ready_events,
                    results,
                ),
                name="gpu-aruco-batch-scheduler",
            )
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
    gpu_monitor = (
        threading.Thread(
            target=monitor_nvidia_smi,
            args=(nvidia_smi, stop_monitor, gpu_monitor_samples),
            name="nvidia-smi-monitor",
        )
        if nvidia_smi and decoder in ("nvcodec", "nvcodec-gpu", "nvcodec-gpu-batch")
        else None
    )
    for thread in threads:
        thread.start()
    if decoder in ("nvcodec", "nvcodec-gpu", "nvcodec-gpu-batch"):
        for ready_event in ready_events:
            if not ready_event.wait(30):
                start_event.set()
                for thread in threads:
                    thread.join()
                raise RuntimeError("NVDEC worker initialization timed out")
    monitor.start()
    if gpu_monitor is not None:
        gpu_monitor.start()
    started_at = time.perf_counter()
    start_event.set()
    for thread in threads:
        thread.join()
    elapsed = time.perf_counter() - started_at
    stop_monitor.set()
    monitor.join()
    if gpu_monitor is not None:
        gpu_monitor.join()

    worker_summaries = []
    failures = []
    minimum_decode_fps = math.inf
    minimum_detection_fps = math.inf
    all_detection_ms: list[float] = []
    all_processing_ms: list[float] = []
    for result in results:
        active_elapsed = result.active_elapsed_seconds or elapsed
        decode_fps = result.decoded_frames / active_elapsed
        detection_fps = result.detections / active_elapsed
        minimum_decode_fps = min(minimum_decode_fps, decode_fps)
        minimum_detection_fps = min(minimum_detection_fps, detection_fps)
        all_detection_ms.extend(result.detection_ms)
        all_processing_ms.extend(result.processing_ms)
        if result.error:
            failures.append(f"source {result.source_id}: {result.error}")
        minimum_required_decode_fps = (
            detection_hz * MINIMUM_RATE_FACTOR
            if decoder in ("qsv", "cuda", "nvcodec", "nvcodec-gpu", "nvcodec-gpu-batch")
            else 50.0 * MINIMUM_RATE_FACTOR
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
                **{
                    key: value
                    for key, value in asdict(result).items()
                    if key not in ("detection_ms", "processing_ms")
                },
                "decodeFps": round(decode_fps, 3),
                "detectionFps": round(detection_fps, 3),
                "detectionMsP95": round(percentile(result.detection_ms, 95) or 0, 3),
                "processingMsP95": round(
                    percentile(result.processing_ms or result.detection_ms, 95) or 0, 3
                ),
                "processingMsMax": round(
                    max(result.processing_ms or result.detection_ms, default=0), 3
                ),
            }
        )

    cpu_values = [sample["cpuPercent"] for sample in monitor_samples]
    memory_values = [sample["workingSetMB"] for sample in monitor_samples]
    detection_p95 = percentile(all_detection_ms, 95)
    processing_p95 = percentile(all_processing_ms or all_detection_ms, 95)
    if processing_p95 is not None and processing_p95 > 1000.0 / detection_hz:
        failures.append(
            f"processing latency p95 {processing_p95:.2f}ms exceeds one {detection_hz:g}Hz interval"
        )
    cpu_p95 = percentile(cpu_values, 95) or 0
    if cpu_p95 > max_cpu_percent:
        failures.append(f"process tree CPU p95 {cpu_p95:.2f}% exceeds {max_cpu_percent:.2f}%")
    gpu_values = [sample["gpuPercent"] for sample in gpu_monitor_samples]
    decoder_values = [sample["decoderPercent"] for sample in gpu_monitor_samples]
    gpu_memory_values = [sample["memoryUsedMB"] for sample in gpu_monitor_samples]
    return {
        "sourceCount": count,
        "passed": not failures,
        "elapsedSeconds": round(elapsed, 3),
        "minimumDecodeFps": round(minimum_decode_fps, 3),
        "minimumDetectionFps": round(minimum_detection_fps, 3),
        "detectionMsP95": round(detection_p95 or 0, 3),
        "processingMsP95": round(processing_p95 or 0, 3),
        "cpuPercentP95": round(cpu_p95, 3),
        "workingSetMBMax": round(max(memory_values) if memory_values else 0, 3),
        "gpuPercentP95": round(percentile(gpu_values, 95) or 0, 3),
        "decoderPercentP95": round(percentile(decoder_values, 95) or 0, 3),
        "gpuMemoryUsedMBMax": round(max(gpu_memory_values) if gpu_memory_values else 0, 3),
        "gpuMonitorSamples": len(gpu_monitor_samples),
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
    parser.add_argument(
        "--decoder",
        choices=(
            "opencv",
            "qsv",
            "cuda",
            "nvcodec",
            "nvcodec-gpu",
            "nvcodec-gpu-batch",
        ),
        default="opencv",
    )
    parser.add_argument("--ffmpeg", help="FFmpeg executable with QSV/CUDA support")
    parser.add_argument("--max-cpu-percent", type=float, default=60.0)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if args.duration < 5:
        parser.error("--duration must be at least 5 seconds")
    if args.detection_hz <= 0 or args.detection_hz > 60:
        parser.error("--detection-hz must be in (0, 60]")
    if args.quality <= 0 or args.quality > 1:
        parser.error("--quality must be in (0, 1]")
    if args.max_cpu_percent <= 0 or args.max_cpu_percent > 100:
        parser.error("--max-cpu-percent must be in (0, 100]")
    if args.decoder in ("qsv", "cuda") and (not args.ffmpeg or not Path(args.ffmpeg).is_file()):
        parser.error("hardware decoder requires an existing --ffmpeg executable")
    if args.decoder in ("nvcodec", "nvcodec-gpu", "nvcodec-gpu-batch"):
        try:
            load_nvcodec()
        except RuntimeError as exc:
            parser.error(str(exc))
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
    if args.decoder in ("nvcodec-gpu", "nvcodec-gpu-batch"):
        try:
            from GpuArucoDetector import GpuArucoDetector

            prewarm = GpuArucoDetector(allowed_marker_ids=range(50))
            prewarm_height, prewarm_width = prewarm.decoder.resized_shape(
                metadata["height"], args.quality
            )
            blank = prewarm.cp.full(
                (prewarm_height, prewarm_width), 255, dtype=prewarm.cp.uint8
            )
            if args.decoder == "nvcodec-gpu-batch":
                prewarm.detect_batch(blank[None])
            else:
                prewarm.detect(blank)
            prewarm.cp.cuda.Stream.null.synchronize()
        except RuntimeError as exc:
            parser.error(str(exc))
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
            find_nvidia_smi(),
        )
        cases.append(case)
        print(
            f"  {'PASS' if case['passed'] else 'FAIL'} "
            f"decode={case['minimumDecodeFps']:.2f}fps detect={case['minimumDetectionFps']:.2f}fps "
            f"processing-p95={case['processingMsP95']:.2f}ms cpu-p95={case['cpuPercentP95']:.2f}%",
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
