"""Bounded, whole-run metrics for the long-lived Marker worker."""

from array import array
import math


class DurationDistribution:
    """Keep counts, not individual samples; quantiles round conservatively up.

    Values above 200 ms share an overflow bucket whose quantile is the exact
    observed maximum. This cannot conceal an overload in an old control window.
    """

    resolution_ms = 0.01
    limit_ms = 200.0
    bucket_count = 20_002

    def __init__(self):
        self.clear()

    def clear(self):
        self.buckets = array("Q", [0]) * self.bucket_count
        self.samples = 0
        self.maximum = 0.0

    def __len__(self):
        return self.samples

    def append(self, value):
        if not math.isfinite(value) or value < 0:
            raise ValueError("metric sample must be finite and non-negative")
        bucket = min(self.bucket_count - 1, math.ceil(value / self.resolution_ms))
        self.buckets[bucket] += 1
        self.samples += 1
        self.maximum = max(self.maximum, value)

    def percentile(self, percent):
        if not 0 < percent <= 100:
            raise ValueError("percentile must be in (0, 100]")
        if not self.samples:
            return 0.0
        rank = math.ceil(self.samples * percent / 100)
        count = 0
        for bucket, frequency in enumerate(self.buckets):
            count += frequency
            if count >= rank:
                if bucket == self.bucket_count - 1:
                    return self.maximum
                return min(self.maximum, bucket * self.resolution_ms)
        raise RuntimeError("metric histogram count mismatch")

    def report(self):
        return {
            "samples": self.samples,
            "p50": round(self.percentile(50), 3),
            "p95": round(self.percentile(95), 3),
            "p99": round(self.percentile(99), 3),
            "maximum": round(self.maximum, 3),
        }
