import argparse
import importlib.util
import pathlib
import sys
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("Run-GpuMarkerObserverReplay.py")
SPEC = importlib.util.spec_from_file_location("run_gpu_marker_observer_replay", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.path.insert(0, str(MODULE_PATH.parent))
SPEC.loader.exec_module(MODULE)


class GpuMarkerObserverReplayTest(unittest.TestCase):
    def test_source_parser_preserves_windows_drive_colon(self):
        with tempfile.NamedTemporaryFile(suffix=".mp4") as video:
            source_id, path = MODULE.parse_source(f"sim-01={video.name}")
        self.assertEqual("sim-01", source_id)
        self.assertEqual(pathlib.Path(video.name).resolve(), path)

    def test_source_parser_requires_existing_video(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            MODULE.parse_source("sim-01=Z:\\missing\\video.mp4")

    def test_reserved_marker_ids_are_rejected(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            MODULE.parse_marker_ids("1,17")

    def test_default_allowlist_excludes_reserved_ids(self):
        self.assertEqual({17, 34, 37}, set(range(50)) - set(MODULE.DEFAULT_ALLOWED_MARKER_IDS))


if __name__ == "__main__":
    unittest.main()
