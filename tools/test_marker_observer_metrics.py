import pathlib
import sys
import unittest


sys.path.insert(0, str(pathlib.Path(__file__).parent))

from MarkerObserverMetrics import evaluate_capacity, summarize_ms


class MarkerObserverMetricsTest(unittest.TestCase):
    def test_capacity_passes_when_rate_and_every_source_are_ready(self):
        gate = evaluate_capacity(50, 48.0, 100, [100, 96, 95, 100])

        self.assertTrue(gate.input_ready)
        self.assertTrue(gate.throughput_passed)
        self.assertTrue(gate.passed)
        self.assertEqual(47.5, gate.required_publication_rate_hz)

    def test_capacity_fails_when_one_source_is_not_ready(self):
        gate = evaluate_capacity(50, 50.0, 100, [100, 100, 94, 100])

        self.assertFalse(gate.input_ready)
        self.assertTrue(gate.throughput_passed)
        self.assertFalse(gate.passed)

    def test_capacity_fails_when_publication_rate_is_low(self):
        gate = evaluate_capacity(50, 47.49, 100, [100, 100])

        self.assertTrue(gate.input_ready)
        self.assertFalse(gate.throughput_passed)
        self.assertFalse(gate.passed)

    def test_capacity_fails_without_batches_or_sources(self):
        gate = evaluate_capacity(50, 50.0, 0, [])

        self.assertFalse(gate.input_ready)
        self.assertFalse(gate.passed)

    def test_timing_summary_reports_percentiles(self):
        self.assertEqual(
            {"samples": 4, "p50": 2.0, "p95": 4.0, "p99": 4.0, "max": 4.0},
            summarize_ms([1.0, 2.0, 3.0, 4.0]),
        )


if __name__ == "__main__":
    unittest.main()
