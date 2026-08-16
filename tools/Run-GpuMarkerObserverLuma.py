#!/usr/bin/env python3
"""Detect markers from Native Observer Y-plane batches and publish observations."""

from __future__ import annotations

import argparse
from collections import deque
from collections.abc import Iterable
import ctypes
from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
import struct
import time

import numpy as np

from GpuArucoDetector import GpuArucoDetector
from MarkerObservationIpc import (
    DEFAULT_MAPPING_NAME,
    MarkerDetection,
    MarkerObservationSharedMemoryWriter,
    SourceObservation,
)


SHARED_LUMA_MAGIC = 0x31594C4D  # "MLY1"
SHARED_LUMA_VERSION = 1
SHARED_LUMA_HEADER_SIZE = 64
SHARED_LUMA_PIXEL_FORMAT = 0x30303859  # "Y800"
SHARED_LUMA_WIDTH = 960
SHARED_LUMA_HEIGHT = 528
SHARED_LUMA_STRIDE = SHARED_LUMA_WIDTH
SHARED_LUMA_BUFFER_COUNT = 3
SHARED_LUMA_MAX_SOURCES = 4
SHARED_LUMA_SOURCE_ID_SIZE = 32
SHARED_LUMA_SOURCE_TABLE_SIZE = SHARED_LUMA_MAX_SOURCES * SHARED_LUMA_SOURCE_ID_SIZE
SHARED_LUMA_SOURCE_METADATA_SIZE = 24
SHARED_LUMA_METADATA_SIZE = SHARED_LUMA_MAX_SOURCES * SHARED_LUMA_SOURCE_METADATA_SIZE
SHARED_LUMA_PLANE_SIZE = SHARED_LUMA_STRIDE * SHARED_LUMA_HEIGHT
SHARED_LUMA_BUFFER_SIZE = (
    SHARED_LUMA_METADATA_SIZE + SHARED_LUMA_MAX_SOURCES * SHARED_LUMA_PLANE_SIZE
)
SHARED_LUMA_PREFIX_SIZE = SHARED_LUMA_HEADER_SIZE + SHARED_LUMA_SOURCE_TABLE_SIZE
SHARED_LUMA_MAPPING_SIZE = (
    SHARED_LUMA_PREFIX_SIZE + SHARED_LUMA_BUFFER_COUNT * SHARED_LUMA_BUFFER_SIZE
)
SHARED_LUMA_VALID = 1
RESERVED_MARKER_IDS = frozenset({17, 34, 37})
DEFAULT_ALLOWED_MARKER_IDS = frozenset(set(range(50)) - RESERVED_MARKER_IDS)
METRICS_WINDOW_SECONDS = 60


@dataclass(frozen=True)
class LumaSource:
    source_id: str
    slot_index: int
    source_sequence: int
    timestamp_unix_ns: int
    video_valid: bool


@dataclass(frozen=True)
class LumaBatch:
    sequence: int
    timestamp_unix_ns: int
    sources: list[LumaSource]
    y_planes: np.ndarray


def parse_marker_ids(value: str) -> frozenset[int]:
    try:
        marker_ids = frozenset(int(part.strip()) for part in value.split(",") if part.strip())
    except ValueError as exc:
        raise argparse.ArgumentTypeError("marker IDs must be comma-separated integers") from exc
    if not marker_ids or any(marker_id < 0 or marker_id >= 50 for marker_id in marker_ids):
        raise argparse.ArgumentTypeError("marker IDs must be in 0..49")
    reserved = marker_ids & RESERVED_MARKER_IDS
    if reserved:
        values = ",".join(str(marker_id) for marker_id in sorted(reserved))
        raise argparse.ArgumentTypeError(f"reserved marker IDs are not allowed: {values}")
    return marker_ids


def percentile(values: Iterable[float], percent: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * percent / 100.0) - 1))
    return ordered[index]


def summarize_ms(values: Iterable[float]) -> dict[str, float | int]:
    samples = list(values)
    return {
        "samples": len(samples),
        "p50": round(percentile(samples, 50), 3),
        "p95": round(percentile(samples, 95), 3),
        "p99": round(percentile(samples, 99), 3),
        "max": round(max(samples, default=0.0), 3),
    }


class RollingSamples:
    def __init__(self, capacity: int):
        if capacity < 1:
            raise ValueError("capacity must be positive")
        self._values: deque[float] = deque(maxlen=capacity)
        self.sample_count = 0
        self.maximum = 0.0

    def append(self, value: float) -> None:
        self._values.append(value)
        self.sample_count += 1
        self.maximum = max(self.maximum, value)

    def percentile(self, percent: float) -> float:
        return percentile(self._values, percent)

    def summarize_ms(self) -> dict[str, float | int]:
        summary = summarize_ms(self._values)
        summary["samples"] = self.sample_count
        summary["windowSamples"] = len(self._values)
        summary["max"] = round(self.maximum, 3)
        return summary


def select_source_slots(configured: list[str], requested: list[str] | None) -> list[int]:
    if not configured:
        raise ValueError("Native Observer did not configure any luma sources")
    if not requested:
        return list(range(len(configured)))
    if len(requested) != len(set(requested)):
        raise ValueError("source IDs must be unique")
    missing = [source_id for source_id in requested if source_id not in configured]
    if missing:
        raise ValueError("source IDs are not configured by Native Observer: " + ",".join(missing))
    return [configured.index(source_id) for source_id in requested]


def evaluate_run(
    published_batches: int,
    published_per_source: list[int],
    publication_rate_hz: float,
    detection_hz: int,
    cycle_p95_ms: float,
    required_source_count: int = 4,
    minimum_rate_ratio: float = 0.95,
    minimum_source_coverage: float = 0.95,
    maximum_cycle_p95_ms: float = 20.0,
) -> dict[str, object]:
    active_source_count = sum(count > 0 for count in published_per_source)
    minimum_rate_hz = detection_hz * minimum_rate_ratio
    source_coverages = [
        count / published_batches if published_batches > 0 else 0.0
        for count in published_per_source
    ]
    input_reasons = []
    throughput_reasons = []
    if len(published_per_source) != required_source_count:
        input_reasons.append(
            f"configured source count {len(published_per_source)} != {required_source_count}"
        )
    if active_source_count != required_source_count:
        input_reasons.append(
            f"active source count {active_source_count} != {required_source_count}"
        )
    if any(coverage < minimum_source_coverage for coverage in source_coverages):
        input_reasons.append(
            f"source coverage below {minimum_source_coverage:.3f}"
        )
    if publication_rate_hz < minimum_rate_hz:
        throughput_reasons.append(
            f"publication rate {publication_rate_hz:.3f} Hz < {minimum_rate_hz:.3f} Hz"
        )
    if cycle_p95_ms > maximum_cycle_p95_ms:
        throughput_reasons.append(
            f"cycle p95 {cycle_p95_ms:.3f} ms > {maximum_cycle_p95_ms:.3f} ms"
        )
    input_ready = not input_reasons
    throughput_passed = not throughput_reasons
    reasons = input_reasons + throughput_reasons
    return {
        "inputReady": input_ready,
        "throughputPassed": throughput_passed,
        "passed": input_ready and throughput_passed,
        "requiredSourceCount": required_source_count,
        "activeSourceCount": active_source_count,
        "minimumPublicationRateHz": round(minimum_rate_hz, 3),
        "minimumSourceCoverage": minimum_source_coverage,
        "maximumCycleP95Ms": maximum_cycle_p95_ms,
        "sourceCoverages": [round(value, 6) for value in source_coverages],
        "inputFailureReasons": input_reasons,
        "throughputFailureReasons": throughput_reasons,
        "failureReasons": reasons,
    }


class SharedLumaReader:
    _HEADER = struct.Struct("<IHHIIIIIIIiqq8x")
    _SOURCE_METADATA = struct.Struct("<QqII")
    _FILE_MAP_READ = 0x0004

    def __init__(self, mapping_name: str):
        if os.name != "nt":
            raise RuntimeError("Native Observer shared luma is supported only on Windows")
        self.mapping_name = mapping_name
        self.kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel32.OpenFileMappingW.argtypes = [ctypes.c_uint32, ctypes.c_int, ctypes.c_wchar_p]
        self.kernel32.OpenFileMappingW.restype = ctypes.c_void_p
        self.kernel32.MapViewOfFile.argtypes = [
            ctypes.c_void_p,
            ctypes.c_uint32,
            ctypes.c_uint32,
            ctypes.c_uint32,
            ctypes.c_size_t,
        ]
        self.kernel32.MapViewOfFile.restype = ctypes.c_void_p
        self.kernel32.UnmapViewOfFile.argtypes = [ctypes.c_void_p]
        self.kernel32.UnmapViewOfFile.restype = ctypes.c_int
        self.kernel32.CloseHandle.argtypes = [ctypes.c_void_p]
        self.kernel32.CloseHandle.restype = ctypes.c_int
        self.mapping_handle = self.kernel32.OpenFileMappingW(self._FILE_MAP_READ, 0, mapping_name)
        if not self.mapping_handle:
            error = ctypes.get_last_error()
            raise FileNotFoundError(error, f"shared luma mapping was not found: {mapping_name}")
        self.view = self.kernel32.MapViewOfFile(
            self.mapping_handle,
            self._FILE_MAP_READ,
            0,
            0,
            SHARED_LUMA_MAPPING_SIZE,
        )
        if not self.view:
            error = ctypes.get_last_error()
            self.kernel32.CloseHandle(self.mapping_handle)
            self.mapping_handle = None
            raise OSError(error, f"MapViewOfFile failed: {mapping_name}")
        self.buffer = (ctypes.c_ubyte * SHARED_LUMA_MAPPING_SIZE).from_address(self.view)
        self.array = np.ctypeslib.as_array(self.buffer)
        try:
            self.source_ids = self._validate_and_read_sources()
        except Exception:
            self.close()
            raise

    def _read_header(self) -> tuple[int, ...]:
        return self._HEADER.unpack_from(self.buffer, 0)

    def latest_sequence(self) -> int:
        return struct.unpack_from("<q", self.buffer, 40)[0]

    def _validate_and_read_sources(self) -> list[str]:
        for _ in range(8):
            header = self._read_header()
            sequence = header[11]
            if sequence & 1:
                time.sleep(0)
                continue
            actual = header[:10]
            expected = (
                SHARED_LUMA_MAGIC,
                SHARED_LUMA_VERSION,
                SHARED_LUMA_HEADER_SIZE,
                SHARED_LUMA_WIDTH,
                SHARED_LUMA_HEIGHT,
                SHARED_LUMA_STRIDE,
                SHARED_LUMA_PIXEL_FORMAT,
                SHARED_LUMA_BUFFER_COUNT,
                SHARED_LUMA_MAX_SOURCES,
                header[9],
            )
            if actual != expected or header[9] < 1 or header[9] > SHARED_LUMA_MAX_SOURCES:
                raise RuntimeError(f"unexpected shared luma contract: {actual}")
            source_ids = []
            for index in range(header[9]):
                offset = SHARED_LUMA_HEADER_SIZE + index * SHARED_LUMA_SOURCE_ID_SIZE
                encoded = bytes(self.buffer[offset : offset + SHARED_LUMA_SOURCE_ID_SIZE])
                source_id = encoded.split(b"\0", 1)[0].decode("utf-8")
                if not source_id:
                    raise RuntimeError(f"shared luma source {index} has no ID")
                source_ids.append(source_id)
            if sequence == struct.unpack_from("<q", self.buffer, 40)[0]:
                if len(source_ids) != len(set(source_ids)):
                    raise RuntimeError("shared luma source IDs are not unique")
                return source_ids
        raise RuntimeError("shared luma source table remained unstable")

    def read_latest(
        self,
        slot_indices: list[int],
        destination: np.ndarray | None = None,
    ) -> LumaBatch | None:
        expected_shape = (len(slot_indices), SHARED_LUMA_HEIGHT, SHARED_LUMA_WIDTH)
        if destination is None:
            planes = np.empty(expected_shape, dtype=np.uint8)
        else:
            if (
                destination.shape != expected_shape
                or destination.dtype != np.uint8
                or not destination.flags.c_contiguous
            ):
                raise ValueError(
                    f"destination must be contiguous uint8 with shape {expected_shape}"
                )
            planes = destination
        for _ in range(4):
            first_sequence = struct.unpack_from("<q", self.buffer, 40)[0]
            if first_sequence <= 0 or first_sequence & 1:
                time.sleep(0)
                continue
            active_buffer = struct.unpack_from("<i", self.buffer, 36)[0]
            timestamp_unix_ns = struct.unpack_from("<q", self.buffer, 48)[0]
            if active_buffer < 0 or active_buffer >= SHARED_LUMA_BUFFER_COUNT:
                continue
            buffer_offset = SHARED_LUMA_PREFIX_SIZE + active_buffer * SHARED_LUMA_BUFFER_SIZE
            sources = []
            for output_index, slot_index in enumerate(slot_indices):
                metadata_offset = buffer_offset + slot_index * SHARED_LUMA_SOURCE_METADATA_SIZE
                source_sequence, source_timestamp, flags, _ = self._SOURCE_METADATA.unpack_from(
                    self.buffer, metadata_offset
                )
                plane_offset = (
                    buffer_offset
                    + SHARED_LUMA_METADATA_SIZE
                    + slot_index * SHARED_LUMA_PLANE_SIZE
                )
                planes[output_index] = self.array[
                    plane_offset : plane_offset + SHARED_LUMA_PLANE_SIZE
                ].reshape(SHARED_LUMA_HEIGHT, SHARED_LUMA_STRIDE)[:, :SHARED_LUMA_WIDTH]
                sources.append(
                    LumaSource(
                        source_id=self.source_ids[slot_index],
                        slot_index=slot_index,
                        source_sequence=source_sequence,
                        timestamp_unix_ns=source_timestamp,
                        video_valid=bool(flags & SHARED_LUMA_VALID),
                    )
                )
            second_sequence = struct.unpack_from("<q", self.buffer, 40)[0]
            if first_sequence == second_sequence and not second_sequence & 1:
                return LumaBatch(second_sequence, timestamp_unix_ns, sources, planes)
        return None

    def close(self) -> None:
        self.array = None
        self.buffer = None
        if getattr(self, "view", None):
            self.kernel32.UnmapViewOfFile(self.view)
            self.view = None
        if getattr(self, "mapping_handle", None):
            self.kernel32.CloseHandle(self.mapping_handle)
            self.mapping_handle = None

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        self.close()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--source-id",
        action="append",
        help="Optional source ID to select; repeat to preserve the requested order.",
    )
    parser.add_argument("--input-mapping-name", default=r"Local\MomoObserverLumaV1")
    parser.add_argument("--output-mapping-name", default=DEFAULT_MAPPING_NAME)
    parser.add_argument("--detection-hz", type=int, default=50)
    parser.add_argument("--duration-seconds", type=float, default=0.0)
    parser.add_argument("--wait-for-mapping-seconds", type=float, default=20.0)
    parser.add_argument("--status-interval-seconds", type=float, default=1.0)
    parser.add_argument("--required-source-count", type=int, default=4)
    parser.add_argument("--minimum-rate-ratio", type=float, default=0.95)
    parser.add_argument("--minimum-source-coverage", type=float, default=0.95)
    parser.add_argument("--maximum-cycle-p95-ms", type=float, default=20.0)
    parser.add_argument(
        "--allowed-marker-ids",
        type=parse_marker_ids,
        default=DEFAULT_ALLOWED_MARKER_IDS,
    )
    parser.add_argument("--output")
    return parser


def open_shared_luma_reader(mapping_name: str, timeout_seconds: float) -> SharedLumaReader:
    deadline = time.monotonic() + timeout_seconds
    last_error: Exception | None = None
    while True:
        try:
            return SharedLumaReader(mapping_name)
        except (FileNotFoundError, OSError, RuntimeError) as exc:
            last_error = exc
        if time.monotonic() >= deadline:
            raise RuntimeError(
                f"shared luma mapping '{mapping_name}' was not ready within {timeout_seconds:g}s"
            ) from last_error
        time.sleep(0.1)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.detection_hz < 1 or args.detection_hz > 240:
        parser.error("--detection-hz must be in 1..240")
    if args.duration_seconds < 0:
        parser.error("--duration-seconds must be zero or positive")
    if args.wait_for_mapping_seconds < 0:
        parser.error("--wait-for-mapping-seconds must be zero or positive")
    if args.status_interval_seconds <= 0:
        parser.error("--status-interval-seconds must be positive")
    if args.required_source_count < 1 or args.required_source_count > SHARED_LUMA_MAX_SOURCES:
        parser.error(f"--required-source-count must be in 1..{SHARED_LUMA_MAX_SOURCES}")
    if not 0 < args.minimum_rate_ratio <= 1:
        parser.error("--minimum-rate-ratio must be in (0, 1]")
    if not 0 < args.minimum_source_coverage <= 1:
        parser.error("--minimum-source-coverage must be in (0, 1]")
    if args.maximum_cycle_p95_ms <= 0:
        parser.error("--maximum-cycle-p95-ms must be positive")

    detector = GpuArucoDetector(allowed_marker_ids=args.allowed_marker_ids)
    cp = detector.cp
    metrics_window_capacity = args.detection_hz * METRICS_WINDOW_SECONDS
    processing_ms = RollingSamples(metrics_window_capacity)
    stage_samples: dict[str, RollingSamples] = {}
    source_age_samples: dict[str, RollingSamples] = {}
    published_batches = 0
    marker_instances = 0
    skipped_unstable_batches = 0
    skipped_duplicate_batches = 0
    last_shared_sequence = 0
    measured_started_at = time.monotonic()
    frame_interval = 1.0 / args.detection_hz

    def add_stage(name: str, value_ms: float) -> None:
        samples = stage_samples.get(name)
        if samples is None:
            samples = RollingSamples(metrics_window_capacity)
            stage_samples[name] = samples
        samples.append(value_ms)

    try:
        with open_shared_luma_reader(
            args.input_mapping_name,
            args.wait_for_mapping_seconds,
        ) as reader:
            try:
                slot_indices = select_source_slots(reader.source_ids, args.source_id)
            except ValueError as exc:
                parser.error(str(exc))
            source_ids = [reader.source_ids[index] for index in slot_indices]
            batch_shape = (len(slot_indices), SHARED_LUMA_HEIGHT, SHARED_LUMA_WIDTH)
            batch_elements = int(np.prod(batch_shape))
            pinned_owner = cp.cuda.alloc_pinned_memory(batch_elements)
            host_planes = np.frombuffer(
                pinned_owner,
                dtype=np.uint8,
                count=batch_elements,
            ).reshape(batch_shape)
            device_planes = cp.empty(batch_shape, dtype=cp.uint8)

            warmup_deadline = time.monotonic() + max(args.wait_for_mapping_seconds, 1.0)
            warmup_batch = None
            warmup_source_count = min(args.required_source_count, len(slot_indices))
            while time.monotonic() < warmup_deadline:
                candidate = reader.read_latest(slot_indices, host_planes)
                if (
                    candidate is not None
                    and sum(source.video_valid for source in candidate.sources)
                    >= warmup_source_count
                ):
                    warmup_batch = candidate
                    break
                time.sleep(0.01)
            if warmup_batch is None:
                raise RuntimeError(
                    f"Native Observer did not publish {warmup_source_count} live luma "
                    "sources for warmup"
                )

            device_planes.set(warmup_batch.y_planes)
            for _ in range(2):
                warmup_timings: dict[str, float] = {}
                detector.detect_batch(device_planes, warmup_timings)
                cp.cuda.Stream.null.synchronize()
            last_shared_sequence = warmup_batch.sequence

            measured_started_at = time.monotonic()
            next_detection_at = measured_started_at
            last_status_at = measured_started_at
            published_per_source = [0] * len(slot_indices)
            last_marker_ids: list[list[int]] = [[] for _ in slot_indices]

            with MarkerObservationSharedMemoryWriter(
                args.output_mapping_name,
                args.detection_hz,
            ) as writer:
                print(
                    f"Live Marker Observer Y-plane: input={args.input_mapping_name} "
                    f"output={args.output_mapping_name} sources=" + ",".join(source_ids),
                    flush=True,
                )
                while (
                    args.duration_seconds == 0
                    or time.monotonic() - measured_started_at < args.duration_seconds
                ):
                    now = time.monotonic()
                    if now < next_detection_at:
                        time.sleep(min(next_detection_at - now, 0.002))
                        continue
                    scheduled_at = next_detection_at

                    cycle_started = time.perf_counter()
                    current_sequence = reader.latest_sequence()
                    if (
                        current_sequence <= 0
                        or current_sequence & 1
                        or current_sequence == last_shared_sequence
                    ):
                        skipped_duplicate_batches += 1
                        time.sleep(0.0005)
                        continue
                    read_started = time.perf_counter()
                    batch = reader.read_latest(slot_indices, host_planes)
                    read_ms = (time.perf_counter() - read_started) * 1000.0
                    if batch is None:
                        skipped_unstable_batches += 1
                        time.sleep(0.0005)
                        continue
                    if batch.sequence == last_shared_sequence:
                        skipped_duplicate_batches += 1
                        time.sleep(0.0005)
                        continue
                    last_shared_sequence = batch.sequence
                    add_stage("sharedReadHostCopyMs", read_ms)
                    add_stage("scheduleLateMs", max(0.0, (now - scheduled_at) * 1000.0))

                    processing_started = time.perf_counter()
                    selection_started = time.perf_counter()
                    valid_indices = [
                        index for index, source in enumerate(batch.sources) if source.video_valid
                    ]
                    add_stage(
                        "sourceSelectionMs",
                        (time.perf_counter() - selection_started) * 1000.0,
                    )

                    results_by_index = {}
                    if valid_indices:
                        h2d_started = time.perf_counter()
                        h2d_event_started = cp.cuda.Event()
                        h2d_event_finished = cp.cuda.Event()
                        h2d_event_started.record()
                        device_planes.set(batch.y_planes)
                        h2d_event_finished.record()
                        h2d_event_finished.synchronize()
                        add_stage("h2dWallMs", (time.perf_counter() - h2d_started) * 1000.0)
                        add_stage(
                            "h2dGpuMs",
                            cp.cuda.get_elapsed_time(h2d_event_started, h2d_event_finished),
                        )

                        detector_timings: dict[str, float] = {}
                        detect_started = time.perf_counter()
                        results = detector.detect_batch(device_planes, detector_timings)
                        add_stage("detectWallMs", (time.perf_counter() - detect_started) * 1000.0)
                        for name, value in detector_timings.items():
                            add_stage(name, value)
                        results_by_index = {
                            index: results[index] for index in valid_indices
                        }
                    detected_at_unix_ns = time.time_ns()
                    processing_ms.append((time.perf_counter() - processing_started) * 1000.0)

                    observation_started = time.perf_counter()
                    observations = []
                    for source_position, source in enumerate(batch.sources):
                        result = results_by_index.get(source_position)
                        if result is None:
                            detections = []
                            candidate_count = 0
                            last_marker_ids[source_position] = []
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
                            published_per_source[source_position] += 1
                            last_marker_ids[source_position] = [
                                item.marker_id for item in detections
                            ]
                            age_samples = source_age_samples.get(source.source_id)
                            if age_samples is None:
                                age_samples = RollingSamples(metrics_window_capacity)
                                source_age_samples[source.source_id] = age_samples
                            age_samples.append(
                                max(
                                    0.0,
                                    (detected_at_unix_ns - source.timestamp_unix_ns) / 1_000_000.0,
                                )
                            )
                        observations.append(
                            SourceObservation(
                                source_index=source.slot_index,
                                source_id=source.source_id,
                                source_sequence=source.source_sequence,
                                frame_received_at_unix_ns=source.timestamp_unix_ns,
                                detected_at_unix_ns=detected_at_unix_ns,
                                video_valid=result is not None,
                                candidate_count=candidate_count,
                                detections=detections,
                            )
                        )
                    add_stage(
                        "observationFormatMs",
                        (time.perf_counter() - observation_started) * 1000.0,
                    )
                    ipc_started = time.perf_counter()
                    writer.write(detected_at_unix_ns, observations)
                    add_stage("ipcWriteMs", (time.perf_counter() - ipc_started) * 1000.0)
                    add_stage("cycleMs", (time.perf_counter() - cycle_started) * 1000.0)
                    published_batches += 1
                    next_detection_at = max(
                        scheduled_at + frame_interval,
                        time.monotonic(),
                    )

                    if now - last_status_at >= args.status_interval_seconds:
                        elapsed = max(now - measured_started_at, 1e-9)
                        status = ", ".join(
                            f"{source.source_id}:valid={source.video_valid} "
                            f"seq={source.source_sequence} ids={last_marker_ids[index]}"
                            for index, source in enumerate(batch.sources)
                        )
                        print(
                            f"rate={published_batches / elapsed:.2f}Hz "
                            f"cycle-p95={stage_samples['cycleMs'].percentile(95):.2f}ms "
                            f"{status}",
                            flush=True,
                        )
                        last_status_at = now
    except KeyboardInterrupt:
        pass

    elapsed_seconds = time.monotonic() - measured_started_at
    publication_rate = published_batches / max(elapsed_seconds, 1e-9)
    cycle_samples = stage_samples.get("cycleMs")
    cycle_p95_ms = cycle_samples.percentile(95) if cycle_samples is not None else 0.0
    acceptance = evaluate_run(
        published_batches,
        published_per_source,
        publication_rate,
        args.detection_hz,
        cycle_p95_ms,
        required_source_count=args.required_source_count,
        minimum_rate_ratio=args.minimum_rate_ratio,
        minimum_source_coverage=args.minimum_source_coverage,
        maximum_cycle_p95_ms=args.maximum_cycle_p95_ms,
    )
    report = {
        "schemaVersion": 2,
        "stage": "gpu_marker_observer_native_luma",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "inputMappingName": args.input_mapping_name,
        "outputMappingName": args.output_mapping_name,
        "hostInputMode": "reused_pinned_full_batch",
        "schedulingMode": "latest_frame_bounded_rate",
        "sources": [
            {
                "sourceIndex": slot_indices[index],
                "sourceId": source_id,
                "publishedFrames": published_per_source[index],
                "lastMarkerIds": last_marker_ids[index],
                "frameAgeMs": (
                    source_age_samples[source_id].summarize_ms()
                    if source_id in source_age_samples
                    else summarize_ms([])
                ),
            }
            for index, source_id in enumerate(source_ids)
        ],
        "detectionHz": args.detection_hz,
        "durationSeconds": round(elapsed_seconds, 3),
        "publishedBatches": published_batches,
        "publicationRateHz": round(publication_rate, 3),
        "metricsWindowSeconds": METRICS_WINDOW_SECONDS,
        "processingMsP50": round(processing_ms.percentile(50), 3),
        "processingMsP95": round(processing_ms.percentile(95), 3),
        "processingMsP99": round(processing_ms.percentile(99), 3),
        "processingMsMax": round(processing_ms.maximum, 3),
        "stageMetricsMs": {
            name: values.summarize_ms() for name, values in sorted(stage_samples.items())
        },
        "markerInstances": marker_instances,
        "activeSources": sum(count > 0 for count in published_per_source),
        "skippedUnstableBatches": skipped_unstable_batches,
        "skippedDuplicateBatches": skipped_duplicate_batches,
        "acceptance": acceptance,
        "inputReady": acceptance["inputReady"],
        "throughputPassed": acceptance["throughputPassed"],
        "passed": acceptance["passed"],
    }
    if args.output:
        output = Path(args.output).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"Report: {output}")
    print(
        f"{'PASS' if report['passed'] else 'FAIL'} sources={len(source_ids)} "
        f"batches={published_batches} rate={publication_rate:.2f}Hz "
        f"cycle-p95={cycle_p95_ms:.3f}ms",
        flush=True,
    )
    for reason in acceptance["failureReasons"]:
        print(f"  gate: {reason}", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
