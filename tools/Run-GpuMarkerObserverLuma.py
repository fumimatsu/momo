#!/usr/bin/env python3
"""Detect markers from Native Observer Y-plane batches and publish observations."""

from __future__ import annotations

import argparse
import ctypes
from dataclasses import dataclass
import json
import os
from pathlib import Path
import struct
import time

import numpy as np

from GpuArucoDetector import GpuArucoDetector
from MarkerObserverMetrics import (
    DEFAULT_REQUIRED_SOURCE_AVAILABILITY_RATIO,
    evaluate_capacity,
    percentile,
    summarize_ms,
)
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

    def read_latest(self, slot_indices: list[int]) -> LumaBatch | None:
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
            planes = np.empty((len(slot_indices), SHARED_LUMA_HEIGHT, SHARED_LUMA_WIDTH), dtype=np.uint8)
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

    detector = GpuArucoDetector(allowed_marker_ids=args.allowed_marker_ids)
    cp = detector.cp
    processing_ms: list[float] = []
    stage_samples_ms: dict[str, list[float]] = {
        "sharedRead": [],
        "sourceSelection": [],
        "h2dSubmit": [],
        "h2dDevice": [],
        "detectorWall": [],
        "observationBuild": [],
        "ipcWrite": [],
        "sourceAge": [],
    }
    published_batches = 0
    marker_instances = 0
    skipped_unstable_batches = 0
    skipped_duplicate_batches = 0
    late_cycles = 0
    last_shared_sequence = 0
    last_status_at = time.monotonic()
    measured_started_at = time.monotonic()
    warmup_ms = 0.0
    frame_interval = 1.0 / args.detection_hz

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

            # Compile and allocate the CUDA path before timing or publishing live results.
            warmup_started = time.perf_counter()
            detector.warmup(len(slot_indices), SHARED_LUMA_HEIGHT, SHARED_LUMA_WIDTH)
            warmup_ms = (time.perf_counter() - warmup_started) * 1000.0
            measured_started_at = time.monotonic()
            next_detection_at = measured_started_at
            last_status_at = measured_started_at
            published_per_source = [0] * len(slot_indices)
            last_marker_ids: list[list[int]] = [[] for _ in slot_indices]
            h2d_start_event = cp.cuda.Event()
            h2d_stop_event = cp.cuda.Event()

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
                    if now - next_detection_at >= frame_interval:
                        late_cycles += 1
                    next_detection_at += frame_interval
                    if next_detection_at < now - frame_interval:
                        next_detection_at = now

                    shared_read_started = time.perf_counter()
                    batch = reader.read_latest(slot_indices)
                    stage_samples_ms["sharedRead"].append(
                        (time.perf_counter() - shared_read_started) * 1000.0
                    )
                    if batch is None:
                        skipped_unstable_batches += 1
                        continue
                    if batch.sequence == last_shared_sequence:
                        skipped_duplicate_batches += 1
                        continue
                    last_shared_sequence = batch.sequence

                    processing_started = time.perf_counter()
                    selection_started = time.perf_counter()
                    valid_indices = [
                        index for index, source in enumerate(batch.sources) if source.video_valid
                    ]
                    selected_planes = batch.y_planes[valid_indices] if valid_indices else None
                    stage_samples_ms["sourceSelection"].append(
                        (time.perf_counter() - selection_started) * 1000.0
                    )
                    results_by_index = {}
                    if valid_indices:
                        h2d_start_event.record()
                        h2d_started = time.perf_counter()
                        gray_batch = cp.asarray(selected_planes)
                        stage_samples_ms["h2dSubmit"].append(
                            (time.perf_counter() - h2d_started) * 1000.0
                        )
                        h2d_stop_event.record()
                        detector_started = time.perf_counter()
                        results = detector.detect_batch(gray_batch)
                        stage_samples_ms["detectorWall"].append(
                            (time.perf_counter() - detector_started) * 1000.0
                        )
                        h2d_stop_event.synchronize()
                        stage_samples_ms["h2dDevice"].append(
                            cp.cuda.get_elapsed_time(h2d_start_event, h2d_stop_event)
                        )
                        results_by_index = dict(zip(valid_indices, results))
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
                            if source.timestamp_unix_ns > 0:
                                stage_samples_ms["sourceAge"].append(
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
                    stage_samples_ms["observationBuild"].append(
                        (time.perf_counter() - observation_started) * 1000.0
                    )
                    ipc_started = time.perf_counter()
                    writer.write(detected_at_unix_ns, observations)
                    stage_samples_ms["ipcWrite"].append(
                        (time.perf_counter() - ipc_started) * 1000.0
                    )
                    published_batches += 1

                    if now - last_status_at >= args.status_interval_seconds:
                        elapsed = max(now - measured_started_at, 1e-9)
                        status = ", ".join(
                            f"{source.source_id}:valid={source.video_valid} "
                            f"seq={source.source_sequence} ids={last_marker_ids[index]}"
                            for index, source in enumerate(batch.sources)
                        )
                        print(
                            f"rate={published_batches / elapsed:.2f}Hz "
                            f"p95={percentile(processing_ms, 95):.2f}ms {status}",
                            flush=True,
                        )
                        last_status_at = now
    except KeyboardInterrupt:
        pass

    elapsed_seconds = time.monotonic() - measured_started_at
    publication_rate = published_batches / max(elapsed_seconds, 1e-9)
    gate = evaluate_capacity(
        args.detection_hz,
        publication_rate,
        published_batches,
        published_per_source,
    )
    report = {
        "schemaVersion": 1,
        "stage": "gpu_marker_observer_native_luma",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "inputMappingName": args.input_mapping_name,
        "outputMappingName": args.output_mapping_name,
        "sources": [
            {
                "sourceIndex": slot_indices[index],
                "sourceId": source_id,
                "publishedFrames": published_per_source[index],
                "availabilityRatio": round(gate.source_availability_ratios[index], 6),
                "lastMarkerIds": last_marker_ids[index],
            }
            for index, source_id in enumerate(source_ids)
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
        "markerInstances": marker_instances,
        "activeSources": sum(count > 0 for count in published_per_source),
        "lateCycles": late_cycles,
        "skippedUnstableBatches": skipped_unstable_batches,
        "skippedDuplicateBatches": skipped_duplicate_batches,
        "stageTimingsMs": {
            name: summarize_ms(samples) for name, samples in stage_samples_ms.items()
        },
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
        f"{'PASS' if report['passed'] else 'FAIL'} sources={len(source_ids)} "
        f"batches={published_batches} rate={publication_rate:.2f}Hz "
        f"processing-p95={report['processingMsP95']:.3f}ms",
        flush=True,
    )
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
