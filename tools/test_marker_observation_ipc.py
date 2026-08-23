import pathlib
import struct
import sys
import unittest
import uuid


sys.path.insert(0, str(pathlib.Path(__file__).parent))
from MarkerObservationIpc import (
    BATCH_PARTIAL,
    HEADER_SIZE,
    MAGIC,
    MAX_DETECTIONS,
    SLOT_HEADER_SIZE,
    SOURCE_HEADER_SIZE,
    SOURCE_SIZE,
    MarkerDetection,
    MarkerObservationSharedMemoryWriter,
    SourceObservation,
    encode_batch,
    encode_header,
)


class MarkerObservationIpcTest(unittest.TestCase):
    def test_header_matches_version_one_contract(self):
        header = encode_header(7, 1234, 50, 9000)
        self.assertEqual(len(header), HEADER_SIZE)
        self.assertEqual(struct.unpack_from("<I", header, 0)[0], MAGIC)
        self.assertEqual(struct.unpack_from("<q", header, 24)[0], 7)
        self.assertEqual(struct.unpack_from("<I", header, 36)[0], 50)

    def test_batch_preserves_duplicate_markers_and_source_identity(self):
        payload = encode_batch(
            3,
            1000,
            [
                SourceObservation(
                    source_index=2,
                    source_id="sim-03",
                    source_sequence=19,
                    frame_received_at_unix_ns=800,
                    detected_at_unix_ns=900,
                    video_valid=True,
                    candidate_count=4,
                    detections=[
                        MarkerDetection(1, 0.25, 0.5, 0.1),
                        MarkerDetection(1, 0.75, 0.5, 0.2),
                    ],
                )
            ],
        )
        self.assertEqual(struct.unpack_from("<q", payload, 8)[0], 3)
        self.assertEqual(struct.unpack_from("<I", payload, 24)[0], 1)
        self.assertEqual(struct.unpack_from("<I", payload, 28)[0], 0)
        source_offset = SLOT_HEADER_SIZE
        self.assertEqual(payload[source_offset : source_offset + 6], b"sim-03")
        self.assertEqual(struct.unpack_from("<I", payload, source_offset + 32)[0], 2)
        self.assertEqual(struct.unpack_from("<I", payload, source_offset + 64)[0], 2)
        self.assertEqual(struct.unpack_from("<i", payload, source_offset + SOURCE_HEADER_SIZE)[0], 1)
        self.assertEqual(
            struct.unpack_from("<i", payload, source_offset + SOURCE_HEADER_SIZE + 16)[0],
            1,
        )

    def test_batch_marks_partial_updates_in_reserved_slot_flags(self):
        payload = encode_batch(
            1,
            1000,
            [SourceObservation(0, "sim", 1, 800, 900, True, 0, [])],
            batch_flags=BATCH_PARTIAL,
        )

        self.assertEqual(struct.unpack_from("<I", payload, 28)[0], BATCH_PARTIAL)

    def test_batch_rejects_unknown_batch_flags(self):
        with self.assertRaisesRegex(ValueError, "unknown batch flags"):
            encode_batch(1, 1, [], batch_flags=2)

    def test_batch_rejects_duplicate_source_index(self):
        source = SourceObservation(0, "sim", 1, 1, 1, True, 0, [])
        with self.assertRaisesRegex(ValueError, "duplicate source index"):
            encode_batch(1, 1, [source, source])

    def test_batch_marks_truncated_detection_list(self):
        detections = [MarkerDetection(1, 0.5, 0.5, 0.1)] * (MAX_DETECTIONS + 1)
        payload = encode_batch(
            1,
            1,
            [SourceObservation(0, "sim", 1, 1, 1, True, 20, detections)],
        )
        flags = struct.unpack_from("<I", payload, SLOT_HEADER_SIZE + 60)[0]
        count = struct.unpack_from("<I", payload, SLOT_HEADER_SIZE + 64)[0]
        self.assertEqual(flags, 3)
        self.assertEqual(count, MAX_DETECTIONS)
        self.assertEqual(len(payload), SLOT_HEADER_SIZE + 32 * SOURCE_SIZE)

    def test_batch_rejects_out_of_range_detection(self):
        source = SourceObservation(
            0,
            "sim",
            1,
            1,
            1,
            True,
            1,
            [MarkerDetection(50, 0.5, 0.5, 0.1)],
        )
        with self.assertRaisesRegex(ValueError, "marker ID"):
            encode_batch(1, 1, [source])

        source = SourceObservation(
            0,
            "sim",
            1,
            1,
            1,
            True,
            1,
            [MarkerDetection(1, 1.1, 0.5, 0.1)],
        )
        with self.assertRaisesRegex(ValueError, "finite values"):
            encode_batch(1, 1, [source])

    def test_writer_rejects_second_producer_for_same_mapping(self):
        mapping_name = rf"Local\MomoMarkerObservationTest-{uuid.uuid4()}"
        with MarkerObservationSharedMemoryWriter(mapping_name):
            with self.assertRaisesRegex(RuntimeError, "another Marker Observer producer"):
                MarkerObservationSharedMemoryWriter(mapping_name)

    def test_writer_updates_advertised_detection_rate(self):
        mapping_name = rf"Local\MomoMarkerObservationTest-{uuid.uuid4()}"
        with MarkerObservationSharedMemoryWriter(mapping_name, 50) as writer:
            writer.set_detection_hz(33)
            self.assertEqual(33, writer.detection_hz)
            self.assertEqual(33, struct.unpack_from("<I", writer.mapping, 36)[0])

            with self.assertRaisesRegex(ValueError, "positive"):
                writer.set_detection_hz(0)


if __name__ == "__main__":
    unittest.main()
