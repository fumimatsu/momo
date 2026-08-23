"""Reader and layout constants for the source-local MLY2 marker frame IPC."""

from __future__ import annotations

import ctypes
from dataclasses import dataclass
import os
import struct
import time

import numpy as np


MAGIC = 0x32594C4D  # "MLY2"
VERSION = 2
HEADER_SIZE = 128
MAX_SOURCES = 32
SOURCE_ID_SIZE = 32
SOURCE_METADATA_SIZE = 64
WIDTH = 960
HEIGHT = 528
STRIDE = WIDTH
PIXEL_FORMAT_Y800 = 0x30303859
PLANE_SIZE = STRIDE * HEIGHT
SOURCE_TABLE_SIZE = MAX_SOURCES * SOURCE_ID_SIZE
ALL_METADATA_SIZE = MAX_SOURCES * SOURCE_METADATA_SIZE
PLANE_OFFSET = HEADER_SIZE + SOURCE_TABLE_SIZE + ALL_METADATA_SIZE
MAPPING_SIZE = PLANE_OFFSET + MAX_SOURCES * PLANE_SIZE

SOURCE_CONNECTED = 1 << 0
VIDEO_VALID = 1 << 1

RACE_PHASES = {
    0: "idle",
    1: "ready",
    2: "countdown",
    3: "green",
    4: "finished",
    5: "aborted",
}

_HEADER = struct.Struct("<IHH12IqqqQq32s")
_SOURCE_METADATA = struct.Struct("<qQqqQQIIII")


@dataclass(frozen=True)
class Mly2Topology:
    generation: int
    qpc_frequency: int
    phase: str
    manifest_revision_hash: int
    source_ids: tuple[str, ...]


@dataclass(frozen=True)
class Mly2SourceSnapshot:
    source_id: str
    slot_index: int
    source_sequence: int
    received_qpc: int
    received_unix_ns: int
    frame_count: int
    replaced_frame_count: int
    flags: int

    @property
    def connected(self) -> bool:
        return bool(self.flags & SOURCE_CONNECTED)

    @property
    def video_valid(self) -> bool:
        return bool(self.flags & VIDEO_VALID)


def _read_topology_once(buffer) -> Mly2Topology | None:
    first_guard = struct.unpack_from("<q", buffer, 72)[0]
    if first_guard & 1:
        return None
    header = _HEADER.unpack_from(buffer, 0)
    actual = (
        header[0],
        header[1],
        header[2],
        header[3],
        header[4],
        header[6],
        header[7],
        header[8],
        header[9],
        header[10],
        header[11],
        header[12],
    )
    expected = (
        MAGIC,
        VERSION,
        HEADER_SIZE,
        MAPPING_SIZE,
        MAX_SOURCES,
        WIDTH,
        HEIGHT,
        STRIDE,
        PIXEL_FORMAT_Y800,
        SOURCE_ID_SIZE,
        SOURCE_METADATA_SIZE,
        PLANE_SIZE,
    )
    if actual != expected:
        raise RuntimeError(f"unexpected MLY2 contract: {actual}")
    active_sources = header[5]
    if active_sources < 0 or active_sources > MAX_SOURCES:
        raise RuntimeError(f"invalid MLY2 active source count: {active_sources}")
    source_ids = []
    for index in range(active_sources):
        offset = HEADER_SIZE + index * SOURCE_ID_SIZE
        encoded = bytes(buffer[offset : offset + SOURCE_ID_SIZE])
        source_id = encoded.split(b"\0", 1)[0].decode("utf-8")
        if not source_id:
            raise RuntimeError(f"MLY2 source {index} has no ID")
        source_ids.append(source_id)
    second_guard = struct.unpack_from("<q", buffer, 72)[0]
    second_generation = struct.unpack_from("<q", buffer, 56)[0]
    if (
        first_guard != second_guard
        or second_guard & 1
        or header[15] != second_generation
    ):
        return None
    if len(source_ids) != len(set(source_ids)):
        raise RuntimeError("MLY2 source IDs are not unique")
    return Mly2Topology(
        generation=header[15],
        qpc_frequency=header[16],
        phase=RACE_PHASES.get(header[13], "unknown"),
        manifest_revision_hash=header[18],
        source_ids=tuple(source_ids),
    )


def read_topology_from_buffer(buffer, attempts: int = 8) -> Mly2Topology | None:
    for _ in range(attempts):
        topology = _read_topology_once(buffer)
        if topology is not None:
            if topology.qpc_frequency <= 0:
                raise RuntimeError("MLY2 QPC frequency must be positive")
            return topology
        time.sleep(0)
    return None


def read_source_from_buffer(
    buffer,
    topology: Mly2Topology,
    slot_index: int,
    attempts: int = 4,
) -> Mly2SourceSnapshot | None:
    if slot_index < 0 or slot_index >= len(topology.source_ids):
        raise IndexError("MLY2 source slot is outside the active topology")
    offset = HEADER_SIZE + SOURCE_TABLE_SIZE + slot_index * SOURCE_METADATA_SIZE
    for _ in range(attempts):
        first_guard = struct.unpack_from("<q", buffer, offset)[0]
        if first_guard & 1:
            time.sleep(0)
            continue
        metadata = _SOURCE_METADATA.unpack_from(buffer, offset)
        second_guard = struct.unpack_from("<q", buffer, offset)[0]
        if first_guard == second_guard and not second_guard & 1:
            if metadata[7:] != (WIDTH, HEIGHT, STRIDE) and metadata[1] > 0:
                raise RuntimeError(
                    f"MLY2 source {slot_index} has invalid dimensions {metadata[7:]}"
                )
            return Mly2SourceSnapshot(
                source_id=topology.source_ids[slot_index],
                slot_index=slot_index,
                source_sequence=metadata[1],
                received_qpc=metadata[2],
                received_unix_ns=metadata[3],
                frame_count=metadata[4],
                replaced_frame_count=metadata[5],
                flags=metadata[6],
            )
    return None


class MarkerLumaV2Reader:
    _FILE_MAP_READ = 0x0004
    _SYNCHRONIZE = 0x00100000
    _WAIT_OBJECT_0 = 0x00000000
    _WAIT_TIMEOUT = 0x00000102

    def __init__(self, mapping_name: str):
        if os.name != "nt":
            raise RuntimeError("MLY2 shared luma is supported only on Windows")
        self.mapping_name = mapping_name
        self.kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel32.OpenFileMappingW.argtypes = [
            ctypes.c_uint32,
            ctypes.c_int,
            ctypes.c_wchar_p,
        ]
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
        self.kernel32.OpenEventW.argtypes = [
            ctypes.c_uint32,
            ctypes.c_int,
            ctypes.c_wchar_p,
        ]
        self.kernel32.OpenEventW.restype = ctypes.c_void_p
        self.kernel32.WaitForSingleObject.argtypes = [
            ctypes.c_void_p,
            ctypes.c_uint32,
        ]
        self.kernel32.WaitForSingleObject.restype = ctypes.c_uint32
        self.kernel32.CloseHandle.restype = ctypes.c_int
        self.kernel32.QueryPerformanceCounter.argtypes = [ctypes.POINTER(ctypes.c_int64)]
        self.kernel32.QueryPerformanceCounter.restype = ctypes.c_int

        self.mapping_handle = self.kernel32.OpenFileMappingW(
            self._FILE_MAP_READ, 0, mapping_name
        )
        if not self.mapping_handle:
            raise FileNotFoundError(
                ctypes.get_last_error(),
                f"MLY2 mapping was not found: {mapping_name}",
            )
        self.view = self.kernel32.MapViewOfFile(
            self.mapping_handle,
            self._FILE_MAP_READ,
            0,
            0,
            MAPPING_SIZE,
        )
        if not self.view:
            error = ctypes.get_last_error()
            self.kernel32.CloseHandle(self.mapping_handle)
            self.mapping_handle = None
            raise OSError(error, f"MapViewOfFile failed: {mapping_name}")
        self.buffer = (ctypes.c_ubyte * MAPPING_SIZE).from_address(self.view)
        self.array = np.ctypeslib.as_array(self.buffer)
        self.frame_event_handle = self.kernel32.OpenEventW(
            self._SYNCHRONIZE, 0, f"{mapping_name}-FrameReady"
        )
        try:
            if self.read_topology() is None:
                raise RuntimeError("MLY2 topology remained unstable")
        except Exception:
            self.close()
            raise

    def query_performance_counter(self) -> int:
        value = ctypes.c_int64()
        if not self.kernel32.QueryPerformanceCounter(ctypes.byref(value)):
            raise OSError(ctypes.get_last_error(), "QueryPerformanceCounter failed")
        return value.value

    def read_topology(self) -> Mly2Topology | None:
        return read_topology_from_buffer(self.buffer)

    @property
    def frame_event_available(self) -> bool:
        return bool(self.frame_event_handle)

    def wait_for_frame(self, timeout_seconds: float) -> bool:
        if timeout_seconds <= 0:
            return False
        if not self.frame_event_handle:
            time.sleep(min(timeout_seconds, 0.001))
            return False
        timeout_ms = max(1, int(timeout_seconds * 1000.0 + 0.999))
        result = self.kernel32.WaitForSingleObject(
            self.frame_event_handle, timeout_ms
        )
        if result == self._WAIT_OBJECT_0:
            return True
        if result == self._WAIT_TIMEOUT:
            return False
        raise OSError(ctypes.get_last_error(), "WaitForSingleObject failed")

    def wait_for_frame_precise(self, timeout_seconds: float) -> bool:
        """Poll the frame event until a QPC deadline without coarse timer overshoot."""
        if timeout_seconds <= 0:
            return False
        deadline = time.perf_counter() + timeout_seconds
        while True:
            if self.frame_event_handle:
                result = self.kernel32.WaitForSingleObject(
                    self.frame_event_handle, 0
                )
                if result == self._WAIT_OBJECT_0:
                    return True
                if result != self._WAIT_TIMEOUT:
                    raise OSError(
                        ctypes.get_last_error(), "WaitForSingleObject failed"
                    )
            if time.perf_counter() >= deadline:
                return False

    def read_sources(
        self, topology: Mly2Topology
    ) -> list[Mly2SourceSnapshot | None]:
        current = self.read_topology()
        if current is None or current.generation != topology.generation:
            return [None] * len(topology.source_ids)
        return [
            read_source_from_buffer(self.buffer, topology, slot)
            for slot in range(len(topology.source_ids))
        ]

    def copy_plane(
        self,
        topology: Mly2Topology,
        snapshot: Mly2SourceSnapshot,
        destination: np.ndarray,
    ) -> bool:
        if destination.shape != (HEIGHT, WIDTH) or destination.dtype != np.uint8:
            raise ValueError(f"destination must be uint8 with shape {(HEIGHT, WIDTH)}")
        metadata_offset = (
            HEADER_SIZE + SOURCE_TABLE_SIZE + snapshot.slot_index * SOURCE_METADATA_SIZE
        )
        first_guard = struct.unpack_from("<q", self.buffer, metadata_offset)[0]
        if first_guard & 1:
            return False
        source_sequence = struct.unpack_from("<Q", self.buffer, metadata_offset + 8)[0]
        if source_sequence != snapshot.source_sequence:
            return False
        plane_offset = PLANE_OFFSET + snapshot.slot_index * PLANE_SIZE
        destination[:] = self.array[plane_offset : plane_offset + PLANE_SIZE].reshape(
            HEIGHT, STRIDE
        )[:, :WIDTH]
        second_guard = struct.unpack_from("<q", self.buffer, metadata_offset)[0]
        generation = struct.unpack_from("<q", self.buffer, 56)[0]
        return (
            first_guard == second_guard
            and not second_guard & 1
            and generation == topology.generation
            and source_sequence
            == struct.unpack_from("<Q", self.buffer, metadata_offset + 8)[0]
        )

    def close(self) -> None:
        self.array = None
        self.buffer = None
        if getattr(self, "frame_event_handle", None):
            self.kernel32.CloseHandle(self.frame_event_handle)
            self.frame_event_handle = None
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


def open_reader(mapping_name: str, timeout_seconds: float) -> MarkerLumaV2Reader:
    deadline = time.monotonic() + timeout_seconds
    last_error: Exception | None = None
    while True:
        try:
            return MarkerLumaV2Reader(mapping_name)
        except (FileNotFoundError, OSError, RuntimeError) as exc:
            last_error = exc
        if time.monotonic() >= deadline:
            raise RuntimeError(
                f"MLY2 mapping '{mapping_name}' was not ready within "
                f"{timeout_seconds:g}s"
            ) from last_error
        time.sleep(0.1)
