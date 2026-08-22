import pathlib
import sys
import unittest


sys.path.insert(0, str(pathlib.Path(__file__).parent))
from MarkerDetectionRateController import (
    AdaptiveDetectionRateController,
    DetectionWindow,
)


class MarkerDetectionRateControllerTest(unittest.TestCase):
    def test_can_start_at_a_lower_profile_for_capacity_validation(self):
        controller = AdaptiveDetectionRateController(initial_detection_hz=25)

        self.assertEqual(25, controller.detection_hz)

    def test_rejects_an_initial_rate_outside_the_profiles(self):
        with self.assertRaisesRegex(ValueError, "initial_detection_hz"):
            AdaptiveDetectionRateController(initial_detection_hz=30)

    def test_downgrades_one_profile_after_three_overload_windows(self):
        controller = AdaptiveDetectionRateController()
        overloaded = DetectionWindow(5.0, 17.0, 0.0)

        self.assertEqual("overload_pending", controller.observe_window(overloaded, 5, True).reason)
        self.assertEqual("overload_pending", controller.observe_window(overloaded, 10, True).reason)
        decision = controller.observe_window(overloaded, 15, True)

        self.assertTrue(decision.changed)
        self.assertEqual(40, decision.detection_hz)
        self.assertEqual("overload_downgrade", decision.reason)

    def test_does_not_downgrade_when_green_adaptation_is_locked(self):
        controller = AdaptiveDetectionRateController()
        overloaded = DetectionWindow(5.0, 17.0, 0.0)

        for now in (5, 10, 15, 20):
            decision = controller.observe_window(overloaded, now, False)

        self.assertEqual(50, decision.detection_hz)
        self.assertEqual("downgrade_locked", decision.reason)

    def test_hold_prevents_rapid_repeated_downgrade(self):
        controller = AdaptiveDetectionRateController()
        overloaded_50 = DetectionWindow(5.0, 17.0, 0.0)
        for now in (5, 10, 15):
            controller.observe_window(overloaded_50, now, True)

        overloaded_40 = DetectionWindow(2.0, 21.0, 0.0)
        for now in (17, 19, 21):
            decision = controller.observe_window(overloaded_40, now, True)

        self.assertEqual(40, decision.detection_hz)
        self.assertEqual("hold", decision.reason)

    def test_reports_capacity_exceeded_at_25_hz(self):
        controller = AdaptiveDetectionRateController(hold_seconds=0)
        now = 0.0
        for expected_hz in (40, 33, 25):
            for _ in range(3):
                now += 5
                period_ms = 1000.0 / controller.detection_hz
                decision = controller.observe_window(
                    DetectionWindow(5.0, period_ms * 0.9, 0.0),
                    now,
                    True,
                )
            self.assertEqual(expected_hz, decision.detection_hz)

        for _ in range(3):
            now += 5
            decision = controller.observe_window(DetectionWindow(5.0, 35.0, 0.1), now, True)

        self.assertFalse(decision.changed)
        self.assertTrue(decision.capacity_exceeded)
        self.assertEqual("capacity_exceeded", decision.reason)

    def test_upgrades_only_when_prepare_is_called_after_healthy_period(self):
        controller = AdaptiveDetectionRateController(hold_seconds=0)
        overloaded = DetectionWindow(5.0, 17.0, 0.0)
        for now in (5, 10, 15):
            controller.observe_window(overloaded, now, True)
        self.assertEqual(40, controller.detection_hz)

        healthy = DetectionWindow(5.0, 5.0, 0.0)
        for now in range(20, 80, 5):
            decision = controller.observe_window(healthy, now, True)
        self.assertEqual(40, decision.detection_hz)

        decision = controller.prepare(80)
        self.assertTrue(decision.changed)
        self.assertEqual(50, decision.detection_hz)
        self.assertEqual("prepare_upgrade", decision.reason)


if __name__ == "__main__":
    unittest.main()
