#!/usr/bin/env python3
"""Detect markers in the live Native Observer shared frame and publish observations."""

from __future__ import annotations

import argparse
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


SHARED_FRAME_MAGIC = 0x3146504D  # "MFP1"
SHARED_FRAME_VERSION = 1
SHARED_FRAME_HEADER_SIZE = 64
SHARED_FRAME_PIXEL_FORMAT = 0x41524742  # "BGRA"
SHARED_FRAME_WIDTH = 1920
SHARED_FRAME_HEIGHT = 1080
SHARED_FRAME_STRIDE = SHARED_FRAME_WIDTH * 4
SHARED_FRAME_BUFFER_COUNT = 3
SHARED_FRAME_SIZE = SHARED_FRAME_STRIDE * SHARED_FRAME_HEIGHT
SHARED_FRAME_MAPPING_SIZE = (
    SHARED_FRAME_HEADER_SIZE + SHARED_FRAME_SIZE * SHARED_FRAME_BUFFER_COUNT
)
SOURCE_WIDTH = 960
SOURCE_HEIGHT = 528
SLOT_HEIGHT = 540
SOURCE_VERTICAL_OFFSET = (SLOT_HEIGHT - SOURCE_HEIGHT) // 2
RESERVED_MARKER_IDS = frozenset({17, 34, 37})
DEFAULT_ALLOWED_MARKER_IDS = frozenset(set(range(50)) - RESERVED_MARKER_IDS)


@dataclass(frozen=True)
class SourceSlot:
    source_id: str
    slot_index: int


@dataclass(frozen=True)
class SharedFrame:
    sequence: int
    timestamp_unix_ns: int
    bgra: np.ndarray


def parse_source(value: str) -> SourceSlot:
    source_id, separator, raw_slot = value.partition("=")
    source_id = source_id.strip()
    if not separator or not source_id or not raw_slot.strip():
        raise argparse.ArgumentTypeError("source must use SOURCE_ID=SLOT_INDEX")
    try:
        encoded_source_id = source_id.encode("utf-8")
    except UnicodeEncodeError as exc:
        raise argparse.ArgumentTypeError("source ID must be valid UTF-8") from exc
    if len(encoded_source_id) >= 32:
        raise argparse.ArgumentTypeError("source ID must contain 1..31 UTF-8 bytes")
    try:
        slot_index = int(raw_slot.strip())
    except ValueError as exc:
        raise argparse.ArgumentTypeError("slot index must be an integer in 0..3") from exc
    if slot_index < 0 or slot_index > 3:
        raise argparse.ArgumentTypeError("slot index must be in 0..3")
    return SourceSlot(source_id, slot_index)


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


def validate_sources(sources: list[SourceSlot]) -> None:
    source_ids = [source.source_id for source in sources]
    slot_indices = [source.slot_index for source in sources]
    if len(source_ids) != len(set(source_ids)):
        raise ValueError("source IDs must be unique")
    if len(slot_indices) != len(set(slot_indices)):
        raise ValueError("slot indices must be unique")


def source_rectangle(slot_index: int) -> tuple[int, int, int, int]:
    if slot_index < 0 or slot_index > 3:
        raise ValueError("slot index must be in 0..3")
    left = (slot_index % 2) * SOURCE_WIDTH
    top = (slot_index // 2) * SLOT_HEIGHT + SOURCE_VERTICAL_OFFSET
    return left, top, SOURCE_WIDTH, SOURCE_HEIGHT


def percentile(values: list[float], percent: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * percent / 100.0) - 1))
    return ordered[index]


class SharedFrameReader:
    _HEADER = struct.Struct("<IHHIIIIIii4xqq8x")
    _FILE_MAP_READ = 0x0004

    def __init__(self, mapping_name: str):
        if os.name != "nt":
            raise RuntimeError("Native Observer shared frames are supported only on Windows")
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
        self.mapping_handle = self.kernel32.OpenFileMappingW(
            self._FILE_MAP_READ,
            0,
            mapping_name,
        )
        if not self.mapping_handle:
            error = ctypes.get_last_error()
            raise FileNotFoundError(error, f"shared frame mapping was not found: {mapping_name}")
        self.view = self.kernel32.MapViewOfFile(
            self.mapping_handle,
            self._FILE_MAP_READ,
            0,
            0,
            SHARED_FRAME_MAPPING_SIZE,
        )
        if not self.view:
            error = ctypes.get_last_error()
            self.kernel32.CloseHandle(self.mapping_handle)
            self.mapping_handle = None
            raise OSError(error, f"MapViewOfFile failed: {mapping_name}")
        self.buffer = (ctypes.c_ubyte * SHARED_FRAME_MAPPING_SIZE).from_address(self.view)
        self.array = np.ctypeslib.as_array(self.buffer)
        try:
            self._validate_header()
        except Exception:
            self.close()
            raise

    def _read_header(self) -> tuple[int, ...]:
        return self._HEADER.unpack_from(self.buffer, 0)

    def _validate_header(self) -> None:
        (
            magic,
            version,
            header_size,
            width,
            height,
            stride,
            pixel_format,
            buffer_count,
            _,
            _,
            _,
            _,
        ) = self._read_header()
        actual = (magic, version, header_size, width, height, stride, pixel_format, buffer_count)
        expected = (
            SHARED_FRAME_MAGIC,
            SHARED_FRAME_VERSION,
            SHARED_FRAME_HEADER_SIZE,
            SHARED_FRAME_WIDTH,
            SHARED_FRAME_HEIGHT,
            SHARED_FRAME_STRIDE,
            SHARED_FRAME_PIXEL_FORMAT,
            SHARED_FRAME_BUFFER_COUNT,
        )
        if actual != expected:
            raise RuntimeError(f"unexpected shared frame contract: {actual}")

    def read_latest(self, sources: list[SourceSlot]) -> SharedFrame | None:
        for _ in range(4):
            first_sequence = struct.unpack_from("<q", self.buffer, 40)[0]
            if first_sequence <= 0 or first_sequence & 1:
                time.sleep(0)
                continue
            active_buffer = struct.unpack_from("<i", self.buffer, 28)[0]
            timestamp_unix_ns = struct.unpack_from("<q", self.buffer, 48)[0]
            if active_buffer < 0 or active_buffer >= SHARED_FRAME_BUFFER_COUNT:
                continue
            frame_offset = SHARED_FRAME_HEADER_SIZE + active_buffer * SHARED_FRAME_SIZE
            frame = self.array[frame_offset : frame_offset + SHARED_FRAME_SIZE].reshape(
                SHARED_FRAME_HEIGHT,
                SHARED_FRAME_WIDTH,
                4,
            )
            source_frames = extract_source_frames(frame, sources)
            second_sequence = struct.unpack_from("<q", self.buffer, 40)[0]
            if first_sequence == second_sequence and not second_sequence & 1:
                return SharedFrame(second_sequence, timestamp_unix_ns, source_frames)
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


def extract_source_frames(frame: np.ndarray, sources: list[SourceSlot]) -> np.ndarray:
    frames = []
    for source in sources:
        left, top, width, height = source_rectangle(source.slot_index)
        frames.append(frame[top : top + height, left : left + width])
    return np.stack(frames)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--source",
        action="append",
        type=parse_source,
        required=True,
        help="Repeat SOURCE_ID=SLOT_INDEX for each Native Observer slot.",
    )
    parser.add_argument("--input-mapping-name", default=r"Local\MomoObserverFrameV1")
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


def open_shared_frame_reader(mapping_name: str, timeout_seconds: float) -> SharedFrameReader:
    deadline = time.monotonic() + timeout_seconds
    last_error: Exception | None = None
    while True:
        try:
            return SharedFrameReader(mapping_name)
        except (FileNotFoundError, OSError, RuntimeError) as exc:
            last_error = exc
        if time.monotonic() >= deadline:
            raise RuntimeError(
                f"shared frame mapping '{mapping_name}' was not ready within {timeout_seconds:g}s"
            ) from last_error
        time.sleep(0.1)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        validate_sources(args.source)
    except ValueError as exc:
        parser.error(str(exc))
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
    frame_interval = 1.0 / args.detection_hz
    source_sequences = [0] * len(args.source)
    processing_ms: list[float] = []
    published_batches = 0
    marker_instances = 0
    skipped_unstable_frames = 0
    skipped_duplicate_frames = 0
    last_shared_sequence = 0
    last_status_at = time.monotonic()
    last_marker_ids: list[list[int]] = [[] for _ in args.source]
    started_at = time.monotonic()
    next_detection_at = started_at

    try:
        with open_shared_frame_reader(
            args.input_mapping_name,
            args.wait_for_mapping_seconds,
        ) as reader, MarkerObservationSharedMemoryWriter(
            args.output_mapping_name,
            args.detection_hz,
        ) as writer:
            print(
                f"Live Marker Observer: input={args.input_mapping_name} "
                f"output={args.output_mapping_name} sources="
                + ",".join(f"{source.source_id}@{source.slot_index}" for source in args.source),
                flush=True,
            )
            while args.duration_seconds == 0 or time.monotonic() - started_at < args.duration_seconds:
                now = time.monotonic()
                if now < next_detection_at:
                    time.sleep(min(next_detection_at - now, 0.002))
                    continue
                next_detection_at += frame_interval
                if next_detection_at < now - frame_interval:
                    next_detection_at = now

                shared_frame = reader.read_latest(args.source)
                if shared_frame is None:
                    skipped_unstable_frames += 1
                    continue
                if shared_frame.sequence == last_shared_sequence:
                    skipped_duplicate_frames += 1
                    continue
                last_shared_sequence = shared_frame.sequence

                processing_started = time.perf_counter()
                source_bgra = cp.asarray(shared_frame.bgra)
                green_ratio = cp.mean(
                    (source_bgra[:, :, :, 0] == 0)
                    & (source_bgra[:, :, :, 1] == 255)
                    & (source_bgra[:, :, :, 2] == 0),
                    axis=(1, 2),
                )
                valid_mask = cp.asnumpy(green_ratio < 0.95)
                gray_batch = (
                    source_bgra[:, :, :, 0].astype(cp.uint16) * 29
                    + source_bgra[:, :, :, 1].astype(cp.uint16) * 150
                    + source_bgra[:, :, :, 2].astype(cp.uint16) * 77
                    + 128
                ) >> 8
                valid_indices = [index for index, valid in enumerate(valid_mask) if valid]
                results_by_index = {}
                if valid_indices:
                    results = detector.detect_batch(gray_batch[valid_indices].astype(cp.uint8))
                    results_by_index = dict(zip(valid_indices, results))
                detected_at_unix_ns = time.time_ns()
                processing_ms.append((time.perf_counter() - processing_started) * 1000.0)

                observations = []
                for source_position, source in enumerate(args.source):
                    result = results_by_index.get(source_position)
                    video_valid = result is not None
                    if video_valid:
                        source_sequences[source_position] += 1
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
                        last_marker_ids[source_position] = [item.marker_id for item in detections]
                    else:
                        detections = []
                        candidate_count = 0
                        last_marker_ids[source_position] = []
                    observations.append(
                        SourceObservation(
                            source_index=source.slot_index,
                            source_id=source.source_id,
                            source_sequence=source_sequences[source_position],
                            frame_received_at_unix_ns=shared_frame.timestamp_unix_ns,
                            detected_at_unix_ns=detected_at_unix_ns,
                            video_valid=video_valid,
                            candidate_count=candidate_count,
                            detections=detections,
                        )
                    )
                writer.write(detected_at_unix_ns, observations)
                published_batches += 1

                if now - last_status_at >= args.status_interval_seconds:
                    elapsed = max(now - started_at, 1e-9)
                    status = ", ".join(
                        f"{source.source_id}:valid={bool(valid_mask[index])} ids={last_marker_ids[index]}"
                        for index, source in enumerate(args.source)
                    )
                    print(
                        f"rate={published_batches / elapsed:.2f}Hz p95={percentile(processing_ms, 95):.2f}ms "
                        f"{status}",
                        flush=True,
                    )
                    last_status_at = now
    except KeyboardInterrupt:
        pass

    elapsed_seconds = time.monotonic() - started_at
    publication_rate = published_batches / max(elapsed_seconds, 1e-9)
    report = {
        "schemaVersion": 1,
        "stage": "gpu_marker_observer_shared_frame",
        "measuredAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "inputMappingName": args.input_mapping_name,
        "outputMappingName": args.output_mapping_name,
        "sources": [
            {
                "sourceIndex": source.slot_index,
                "sourceId": source.source_id,
                "publishedFrames": source_sequences[index],
                "lastMarkerIds": last_marker_ids[index],
            }
            for index, source in enumerate(args.source)
        ],
        "detectionHz": args.detection_hz,
        "durationSeconds": round(elapsed_seconds, 3),
        "publishedBatches": published_batches,
        "publicationRateHz": round(publication_rate, 3),
        "processingMsP50": round(percentile(processing_ms, 50), 3),
        "processingMsP95": round(percentile(processing_ms, 95), 3),
        "processingMsP99": round(percentile(processing_ms, 99), 3),
        "processingMsMax": round(max(processing_ms, default=0.0), 3),
        "markerInstances": marker_instances,
        "skippedUnstableFrames": skipped_unstable_frames,
        "skippedDuplicateFrames": skipped_duplicate_frames,
        "passed": published_batches > 0 and all(source_sequences),
    }
    if args.output:
        output = Path(args.output).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"Report: {output}")
    print(
        f"{'PASS' if report['passed'] else 'FAIL'} sources={len(args.source)} "
        f"batches={published_batches} rate={publication_rate:.2f}Hz "
        f"processing-p95={report['processingMsP95']:.3f}ms",
        flush=True,
    )
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
