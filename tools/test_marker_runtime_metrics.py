import math
import unittest

from MarkerRuntimeMetrics import DurationDistribution


class DurationDistributionTest(unittest.TestCase):
    def test_storage_is_constant_and_old_overload_is_not_forgotten(self):
        metric = DurationDistribution()
        storage = metric.buckets.buffer_info()[1] * metric.buckets.itemsize
        for _ in range(10_000):
            metric.append(30.0)
        for _ in range(90_000):
            metric.append(2.0)
        self.assertEqual(storage, metric.buckets.buffer_info()[1] * metric.buckets.itemsize)
        self.assertEqual(100_000, metric.report()["samples"])
        self.assertEqual(30.0, metric.percentile(95))
        self.assertEqual(2.0, metric.percentile(50))

    def test_quantiles_never_underestimate_samples(self):
        metric = DurationDistribution()
        samples = [0.0, 0.009, 12.011, 16.001, 19.999, 31.999, 199.995]
        for sample in samples:
            metric.append(sample)
        for percent in (50, 95, 99, 100):
            exact = samples[math.ceil(len(samples) * percent / 100) - 1]
            self.assertGreaterEqual(metric.percentile(percent), exact)
            self.assertLessEqual(metric.percentile(percent), exact + 0.01)

    def test_overflow_is_conservative_and_preserves_maximum(self):
        metric = DurationDistribution()
        for sample in (201, 500, 1000):
            metric.append(sample)
        self.assertEqual(1000, metric.percentile(50))
        self.assertEqual(1000, metric.report()["maximum"])

    def test_clear_starts_a_new_control_window(self):
        metric = DurationDistribution()
        metric.append(50)
        metric.clear()
        self.assertEqual(0, len(metric))
        self.assertEqual(0, metric.percentile(95))
        metric.append(2)
        self.assertEqual(2, metric.report()["maximum"])

    def test_invalid_sample_cannot_become_a_healthy_value(self):
        metric = DurationDistribution()
        for sample in (-1, float('nan'), float('inf')):
            with self.assertRaises(ValueError):
                metric.append(sample)
        self.assertEqual(0, len(metric))


if __name__ == '__main__':
    unittest.main()
