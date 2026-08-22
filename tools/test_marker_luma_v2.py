import pathlib
import struct
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from MarkerLumaV2 import (
    ALL_METADATA_SIZE,
    HEADER_SIZE,
    HEIGHT,
    MAGIC,
    MAPPING_SIZE,
    MAX_SOURCES,
    PIXEL_FORMAT_Y800,
    PLANE_OFFSET,
    PLANE_SIZE,
    SOURCE_CONNECTED,
    SOURCE_ID_SIZE,
    SOURCE_METADATA_SIZE,
    SOURCE_TABLE_SIZE,
    STRIDE,
    VERSION,
    VIDEO_VALID,
    WIDTH,
    read_source_from_buffer,
    read_topology_from_buffer,
)


def make_mapping(source_ids=("source-1", "source-2"), generation=3):
    payload = bytearray(MAPPING_SIZE)
    struct.pack_into(
        "<IHH12IqqqQq32s",
        payload,
        0,
        MAGIC,
        VERSION,
        HEADER_SIZE,
        MAPPING_SIZE,
        MAX_SOURCES,
        len(source_ids),
        WIDTH,
        HEIGHT,
        STRIDE,
        PIXEL_FORMAT_Y800,
        SOURCE_ID_SIZE,
        SOURCE_METADATA_SIZE,
        PLANE_SIZE,
        3,
        0,
        generation,
        10_000_000,
        2,
        1234,
        99,
        b"",
    )
    for index, source_id in enumerate(source_ids):
        encoded = source_id.encode("utf-8")
        offset = HEADER_SIZE + index * SOURCE_ID_SIZE
        payload[offset : offset + len(encoded)] = encoded
        metadata_offset = HEADER_SIZE + SOURCE_TABLE_SIZE + index * SOURCE_METADATA_SIZE
        struct.pack_into(
            "<qQqqQQIIII",
            payload,
            metadata_offset,
            2,
            index + 10,
            9_000_000 + index,
            100 + index,
            20 + index,
            19 + index,
            SOURCE_CONNECTED | VIDEO_VALID,
            WIDTH,
            HEIGHT,
            STRIDE,
        )
    return payload


class MarkerLumaV2Test(unittest.TestCase):
    def test_layout_is_fixed_and_bounded(self):
        self.assertEqual(HEADER_SIZE + SOURCE_TABLE_SIZE + ALL_METADATA_SIZE, PLANE_OFFSET)
        self.assertEqual(PLANE_OFFSET + MAX_SOURCES * PLANE_SIZE, MAPPING_SIZE)
        self.assertLess(MAPPING_SIZE, 17 * 1024 * 1024)

    def test_reads_topology_and_source_metadata(self):
        payload = make_mapping()
        topology = read_topology_from_buffer(payload)
        self.assertIsNotNone(topology)
        self.assertEqual(("source-1", "source-2"), topology.source_ids)
        self.assertEqual(3, topology.generation)
        self.assertEqual("green", topology.phase)
        source = read_source_from_buffer(payload, topology, 1)
        self.assertEqual(11, source.source_sequence)
        self.assertTrue(source.connected)
        self.assertTrue(source.video_valid)

    def test_rejects_unstable_topology_and_source(self):
        payload = make_mapping()
        struct.pack_into("<q", payload, 72, 3)
        self.assertIsNone(read_topology_from_buffer(payload, attempts=1))

        payload = make_mapping()
        topology = read_topology_from_buffer(payload)
        metadata_offset = HEADER_SIZE + SOURCE_TABLE_SIZE
        struct.pack_into("<q", payload, metadata_offset, 3)
        self.assertIsNone(
            read_source_from_buffer(payload, topology, 0, attempts=1)
        )

    def test_plane_offsets_do_not_overlap(self):
        offsets = [PLANE_OFFSET + index * PLANE_SIZE for index in range(MAX_SOURCES)]
        self.assertEqual(len(offsets), len(set(offsets)))
        self.assertEqual(MAPPING_SIZE, offsets[-1] + PLANE_SIZE)


if __name__ == "__main__":
    unittest.main()
