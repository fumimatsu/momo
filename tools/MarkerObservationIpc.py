"""Windows shared-memory contract for Marker Observer frame observations."""

from __future__ import annotations

from dataclasses import dataclass, field
import ctypes
import math
import mmap
import os
import struct
import time
from typing import Iterable


MAGIC = 0x314F4D4D  # "MMO1"
VERSION = 1
HEADER_SIZE = 64
RING_CAPACITY = 16
MAX_SOURCES = 32
MAX_DETECTIONS = 16
DETECTION_SIZE = 16
SOURCE_HEADER_SIZE = 76
SOURCE_SIZE = SOURCE_HEADER_SIZE + MAX_DETECTIONS * DETECTION_SIZE
SLOT_HEADER_SIZE = 32
SLOT_SIZE = SLOT_HEADER_SIZE + MAX_SOURCES * SOURCE_SIZE
MAPPING_SIZE = HEADER_SIZE + RING_CAPACITY * SLOT_SIZE
DEFAULT_MAPPING_NAME = r"Local\MomoMarkerObservationsV1"
VIDEO_VALID = 1
DETECTIONS_TRUNCATED = 2


@dataclass(frozen=True)
class MarkerDetection:
    marker_id: int
    center_x: float
    center_y: float
    area: float


@dataclass(frozen=True)
class SourceObservation:
    source_index: int
    source_id: str
    source_sequence: int
    frame_received_at_unix_ns: int
    detected_at_unix_ns: int
    video_valid: bool
    candidate_count: int
    detections: list[MarkerDetection] = field(default_factory=list)


def encode_batch(sequence: int, published_at_unix_ns: int, sources: Iterable[SourceObservation]) -> bytes:
    source_list = list(sources)
    if sequence < 1:
        raise ValueError("sequence must be positive")
    if len(source_list) > MAX_SOURCES:
        raise ValueError(f"source count exceeds {MAX_SOURCES}")

    payload = bytearray(SLOT_SIZE)
    struct.pack_into("<qqqII", payload, 0, sequence, sequence, published_at_unix_ns, len(source_list), 0)
    seen_indices: set[int] = set()
    for record_index, source in enumerate(source_list):
        if source.source_index < 0 or source.source_index >= MAX_SOURCES:
            raise ValueError(f"source index must be in 0..{MAX_SOURCES - 1}")
        if source.source_index in seen_indices:
            raise ValueError(f"duplicate source index: {source.source_index}")
        seen_indices.add(source.source_index)
        source_id = source.source_id.encode("utf-8")
        if not source_id or len(source_id) >= 32:
            raise ValueError("source ID must contain 1..31 UTF-8 bytes")

        offset = SLOT_HEADER_SIZE + record_index * SOURCE_SIZE
        payload[offset : offset + len(source_id)] = source_id
        flags = VIDEO_VALID if source.video_valid else 0
        detections = source.detections[:MAX_DETECTIONS]
        if len(source.detections) > MAX_DETECTIONS:
            flags |= DETECTIONS_TRUNCATED
        struct.pack_into(
            "<IQQqIIII",
            payload,
            offset + 32,
            source.source_index,
            source.source_sequence,
            source.frame_received_at_unix_ns,
            source.detected_at_unix_ns,
            flags,
            len(detections),
            max(0, source.candidate_count),
            0,
        )
        detection_offset = offset + SOURCE_HEADER_SIZE
        for detection in detections:
            if detection.marker_id < 0 or detection.marker_id >= 50:
                raise ValueError("marker ID must be in 0..49")
            if not all(
                math.isfinite(value) and 0.0 <= value <= 1.0
                for value in (detection.center_x, detection.center_y, detection.area)
            ):
                raise ValueError("marker center and area must be finite values in 0..1")
            struct.pack_into(
                "<ifff",
                payload,
                detection_offset,
                detection.marker_id,
                detection.center_x,
                detection.center_y,
                detection.area,
            )
            detection_offset += DETECTION_SIZE
    return bytes(payload)


def encode_header(write_sequence: int, producer_pid: int, detection_hz: int, created_at_unix_ns: int) -> bytes:
    payload = bytearray(HEADER_SIZE)
    struct.pack_into(
        "<IHHIIIIqIIq",
        payload,
        0,
        MAGIC,
        VERSION,
        HEADER_SIZE,
        RING_CAPACITY,
        SLOT_SIZE,
        MAX_SOURCES,
        MAX_DETECTIONS,
        write_sequence,
        producer_pid,
        detection_hz,
        created_at_unix_ns,
    )
    return bytes(payload)


class MarkerObservationSharedMemoryWriter:
    def __init__(self, mapping_name: str = DEFAULT_MAPPING_NAME, detection_hz: int = 50):
        if os.name != "nt":
            raise RuntimeError("Marker observation shared memory is supported only on Windows")
        if detection_hz < 1:
            raise ValueError("detection_hz must be positive")
        self.mapping_name = mapping_name
        self.detection_hz = detection_hz
        self.sequence = 0
        self.mapping = mmap.mmap(-1, MAPPING_SIZE, tagname=mapping_name, access=mmap.ACCESS_WRITE)
        self.kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel32.CreateMutexW.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.c_wchar_p]
        self.kernel32.CreateMutexW.restype = ctypes.c_void_p
        self.kernel32.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_uint32]
        self.kernel32.WaitForSingleObject.restype = ctypes.c_uint32
        self.kernel32.ReleaseMutex.argtypes = [ctypes.c_void_p]
        self.kernel32.ReleaseMutex.restype = ctypes.c_int
        self.kernel32.CloseHandle.argtypes = [ctypes.c_void_p]
        self.kernel32.CloseHandle.restype = ctypes.c_int
        ctypes.set_last_error(0)
        self.producer_mutex = self.kernel32.CreateMutexW(None, 0, mapping_name + "-Producer")
        producer_mutex_error = ctypes.get_last_error()
        if not self.producer_mutex or producer_mutex_error == 183:
            if self.producer_mutex:
                self.kernel32.CloseHandle(self.producer_mutex)
            self.mapping.close()
            raise RuntimeError(f"another Marker Observer producer owns '{mapping_name}'")
        self.mutex = self.kernel32.CreateMutexW(None, 0, mapping_name + "-Mutex")
        if not self.mutex:
            self.kernel32.CloseHandle(self.producer_mutex)
            self.mapping.close()
            raise OSError(ctypes.get_last_error(), "CreateMutexW failed")
        self._lock()
        try:
            self.mapping.seek(0)
            self.mapping.write(encode_header(0, os.getpid(), detection_hz, time.time_ns()))
            self.mapping.write(b"\0" * (MAPPING_SIZE - HEADER_SIZE))
        finally:
            self._unlock()

    def _lock(self) -> None:
        status = self.kernel32.WaitForSingleObject(self.mutex, 5000)
        if status not in (0, 0x80):
            raise TimeoutError(f"marker observation mutex wait failed: 0x{status:08X}")

    def _unlock(self) -> None:
        if not self.kernel32.ReleaseMutex(self.mutex):
            raise OSError(ctypes.get_last_error(), "ReleaseMutex failed")

    def write(self, published_at_unix_ns: int, sources: Iterable[SourceObservation]) -> int:
        next_sequence = self.sequence + 1
        payload = encode_batch(next_sequence, published_at_unix_ns, sources)
        slot_index = (next_sequence - 1) % RING_CAPACITY
        slot_offset = HEADER_SIZE + slot_index * SLOT_SIZE
        self._lock()
        try:
            self.mapping[slot_offset : slot_offset + SLOT_SIZE] = payload
            struct.pack_into("<q", self.mapping, 24, next_sequence)
        finally:
            self._unlock()
        self.sequence = next_sequence
        return next_sequence

    def close(self) -> None:
        if getattr(self, "mapping", None) is not None:
            self.mapping.close()
            self.mapping = None
        if getattr(self, "mutex", None):
            self.kernel32.CloseHandle(self.mutex)
            self.mutex = None
        if getattr(self, "producer_mutex", None):
            self.kernel32.CloseHandle(self.producer_mutex)
            self.producer_mutex = None

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        self.close()
