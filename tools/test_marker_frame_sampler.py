import pathlib
import sys
import unittest


sys.path.insert(0, str(pathlib.Path(__file__).parent))
from MarkerFrameSampler import SourceFrameState, sample_latest_frames


class MarkerFrameSamplerTest(unittest.TestCase):
    def test_selects_fresh_latest_frames_on_one_tick(self):
        sampled = sample_latest_frames(
            [
                SourceFrameState("source-1", 10, 980, True),
                SourceFrameState("source-2", 20, 970, True),
            ],
            sample_tick=1000,
            last_detected_sequences={},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertEqual([True, True], [source.eligible for source in sampled])
        self.assertEqual([20, 30], [source.age_ticks for source in sampled])

    def test_stale_source_does_not_block_fresh_source(self):
        sampled = sample_latest_frames(
            [
                SourceFrameState("source-1", 10, 990, True),
                SourceFrameState("source-2", 20, 900, True),
            ],
            sample_tick=1000,
            last_detected_sequences={},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertEqual("selected", sampled[0].reason)
        self.assertEqual("stale", sampled[1].reason)

    def test_source_outside_batch_skew_is_invalid_for_only_that_tick(self):
        sampled = sample_latest_frames(
            [
                SourceFrameState("source-1", 10, 995, True),
                SourceFrameState("source-2", 20, 950, True),
            ],
            sample_tick=1000,
            last_detected_sequences={},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertTrue(sampled[0].eligible)
        self.assertFalse(sampled[1].eligible)
        self.assertEqual("skewed", sampled[1].reason)

    def test_does_not_detect_same_frozen_frame_twice(self):
        sampled = sample_latest_frames(
            [SourceFrameState("source-1", 10, 995, True)],
            sample_tick=1000,
            last_detected_sequences={"source-1": 10},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertFalse(sampled[0].eligible)
        self.assertEqual("duplicate_or_rollback", sampled[0].reason)

    def test_rejects_source_sequence_rollback_after_reconnect(self):
        sampled = sample_latest_frames(
            [SourceFrameState("source-1", 9, 995, True)],
            sample_tick=1000,
            last_detected_sequences={"source-1": 10},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertFalse(sampled[0].eligible)
        self.assertEqual("duplicate_or_rollback", sampled[0].reason)

    def test_missing_video_is_invalid_without_waiting(self):
        sampled = sample_latest_frames(
            [
                SourceFrameState("source-1", 0, 0, False),
                SourceFrameState("source-2", 20, 995, True),
            ],
            sample_tick=1000,
            last_detected_sequences={},
            maximum_age_ticks=60,
            maximum_skew_ticks=40,
        )

        self.assertEqual("no_video", sampled[0].reason)
        self.assertTrue(sampled[1].eligible)


if __name__ == "__main__":
    unittest.main()
