import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("Analyze-RelayImpactLog.py")
SPEC = importlib.util.spec_from_file_location("analyze_relay_impact_log", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


def record(elapsed_us, raw, record_type="telemetry", **extra):
    value = {
        "type": record_type,
        "schemaVersion": 1,
        "relaySessionId": "session-a",
        "relayReceivedAt": f"2026-08-24T00:00:{elapsed_us / 1_000_000:06.3f}Z",
        "relayElapsedUs": elapsed_us,
        "sourceId": "source-1",
        "carId": "CAR-1",
        "raw": raw,
    }
    value.update(extra)
    return value


def telemetry(payload):
    return "TEL:" + json.dumps(payload, separators=(",", ":"))


class RelayImpactAnalysisTests(unittest.TestCase):
    def test_analyzes_vertical_shadow_and_writes_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            log = root / "telemetry-test.ndjson"
            records = [
                record(0, telemetry({"v": 2, "k": "s", "src": "imu0", "boot": "boot-a", "seq": 1, "t_us": 0, "m": {"a": [0.1, 0.2, 0.0], "y": 0.01}})),
                record(100_000, telemetry({"v": 2, "k": "s", "src": "imu0", "boot": "boot-a", "seq": 2, "t_us": 100_000, "m": {"a": [0.3, 0.2, 5.0], "y": 0.02}})),
                record(120_000, telemetry({"v": 2, "k": "e", "src": "imu0", "boot": "boot-a", "seq": 3, "t_us": 120_000, "e": {"n": "impact_candidate", "m": 16.0, "a": [0.1, 0.1, 0.99], "j": 800.0}})),
                record(140_000, telemetry({"v": 2, "k": "s", "src": "imu0", "boot": "boot-a", "seq": 4, "t_us": 140_000, "m": {"a": [0.1, 0.1, -4.0], "y": 0.01}})),
                record(220_000, telemetry({"v": 2, "k": "e", "src": "imu0", "boot": "boot-a", "seq": 5, "t_us": 220_000, "e": {"n": "impact_candidate", "m": 13.0, "a": [0.98, 0.1, 0.1], "j": 300.0}})),
                record(121_000, "", "vehicle_event", vehicleEvent={
                    "type": "vehicle_event", "version": 1, "eventId": "CAR-1:boot-a:3",
                    "impactClass": "severe", "damageApplied": True, "damage": 20,
                    "hpBefore": 100, "hpAfter": 80,
                }),
                record(421_000, "", "impact_shadow", impactShadow={
                    "eventId": "CAR-1:boot-a:3",
                    "algorithmVersion": "vertical-window-v1",
                    "axisProposalKind": "road_impact",
                    "proposedKind": "road_impact",
                    "proposedDamageAllowed": False,
                    "windowComplete": True,
                    "motionSamples": 19,
                    "reasons": ["vertical_rebound", "horizontal_brief"],
                }),
                record(521_000, "", "impact_shadow", impactShadow={
                    "eventId": "CAR-1:boot-a:5",
                    "algorithmVersion": "vertical-window-v1",
                    "axisProposalKind": "collision",
                    "proposedKind": "collision",
                    "proposedDamageAllowed": True,
                    "windowComplete": True,
                    "motionSamples": 18,
                    "reasons": ["horizontal_axis_candidate"],
                }),
                {"type": "relay_session_end", "stats": {"queueDrops": 2, "writeErrors": 0}},
            ]
            log.write_text("\n".join(json.dumps(item) for item in records) + "\n{broken\n", encoding="utf-8")

            result = MODULE.analyze_logs([log])
            self.assertEqual(len(result["samples"]), 3)
            self.assertEqual(len(result["candidates"]), 2)
            self.assertGreater(result["samples"][1].derived_jerk_mps3, 0)
            vertical, horizontal = result["candidates"]
            self.assertEqual(vertical.current_class, "severe")
            self.assertEqual(vertical.shadow_action, "suppress_vertical_surface_candidate")
            self.assertEqual(vertical.shadow_damage, 0)
            self.assertTrue(vertical.confirmed_damage_applied)
            self.assertEqual(vertical.runtime_shadow_algorithm, "vertical-window-v1")
            self.assertEqual(vertical.runtime_shadow_axis_kind, "road_impact")
            self.assertEqual(vertical.runtime_shadow_kind, "road_impact")
            self.assertFalse(vertical.runtime_shadow_damage_allowed)
            self.assertTrue(vertical.runtime_shadow_window_complete)
            self.assertEqual(vertical.runtime_shadow_samples, 19)
            self.assertEqual(vertical.runtime_shadow_reasons, "vertical_rebound,horizontal_brief")
            self.assertEqual(horizontal.current_class, "strong")
            self.assertEqual(horizontal.shadow_action, "unchanged")
            self.assertEqual(horizontal.runtime_shadow_kind, "collision")
            self.assertTrue(horizontal.runtime_shadow_damage_allowed)
            self.assertEqual(result["counters"]["malformed_lines"], 1)
            self.assertEqual(result["counters"]["queue_drops"], 2)

            output = root / "report"
            MODULE.write_outputs(result, output, 500)
            for name in ["summary.json", "motion-samples.csv", "impact-events.csv", "event-windows.csv", "report.html"]:
                self.assertTrue((output / name).is_file(), name)
            self.assertIn("suppress_vertical_surface_candidate", (output / "report.html").read_text(encoding="utf-8"))

    def test_current_thresholds_match_relay(self):
        self.assertEqual(MODULE.classify_current(9.99, 1000), "")
        self.assertEqual(MODULE.classify_current(13, 120), "weak")
        self.assertEqual(MODULE.classify_current(13, 300), "strong")
        self.assertEqual(MODULE.classify_current(15, 750), "severe")

    def test_mixed_vertical_candidate_is_observed_without_suppression(self):
        shadow_class, shadow_damage, shadow_action = MODULE.classify_shadow(
            "strong", 0.86, 0.52, 0.75, 0.45
        )
        self.assertEqual(shadow_class, "strong")
        self.assertEqual(shadow_damage, 12.0)
        self.assertEqual(shadow_action, "observe_mixed_vertical_candidate")

    def test_accepts_cpu_shadow_capture_telemetry(self):
        with tempfile.TemporaryDirectory() as temporary:
            log = Path(temporary) / "cpu-shadow.jsonl"
            records = [
                {
                    "schema": "momo-fpv-cpu-shadow-capture/v1", "kind": "run_start",
                    "run_id": "run-a", "epoch_ms": 1_785_501_058_000,
                    "viewer": {"source_identity": {"relay_device": "11.5", "race_car_id": "CP-3"}},
                },
                {
                    "schema": "momo-fpv-cpu-shadow-capture/v1", "kind": "telemetry",
                    "epoch_ms": 1_785_501_058_100,
                    "raw_message": telemetry({"v": 2, "k": "s", "src": "imu0", "boot": "boot-b", "seq": 10, "t_us": 100_000, "m": {"a": [0.2, 0.1, 0.3], "y": 0.01}}),
                },
                {
                    "schema": "momo-fpv-cpu-shadow-capture/v1", "kind": "telemetry",
                    "epoch_ms": 1_785_501_058_120,
                    "raw_message": telemetry({"v": 2, "k": "e", "src": "imu0", "boot": "boot-b", "seq": 11, "t_us": 120_000, "e": {"n": "impact_candidate", "m": 13.0, "a": [0.1, 0.1, 0.99], "j": 300.0}}),
                },
            ]
            log.write_text("\n".join(json.dumps(item) for item in records), encoding="utf-8")
            result = MODULE.analyze_logs([log])
            self.assertEqual(len(result["samples"]), 1)
            self.assertEqual(len(result["candidates"]), 1)
            self.assertEqual(result["samples"][0].source_id, "11.5")
            self.assertEqual(result["samples"][0].car_id, "CP-3")
            self.assertEqual(result["samples"][0].session_id, "run-a")
            self.assertEqual(result["candidates"][0].event_id, "CP-3:boot-b:11")


if __name__ == "__main__":
    unittest.main()
