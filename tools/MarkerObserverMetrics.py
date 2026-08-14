#!/usr/bin/env python3
"""Shared capacity gates and timing summaries for Marker Observer tools."""

from __future__ import annotations

from dataclasses import dataclass
import math


DEFAULT_REQUIRED_RATE_RATIO = 0.95
DEFAULT_REQUIRED_SOURCE_AVAILABILITY_RATIO = 0.95


def percentile(values: list[float], percent: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * percent / 100.0) - 1))
    return ordered[index]


def summarize_ms(values: list[float]) -> dict[str, float | int]:
    return {
        "samples": len(values),
        "p50": round(percentile(values, 50), 3),
        "p95": round(percentile(values, 95), 3),
        "p99": round(percentile(values, 99), 3),
        "max": round(max(values, default=0.0), 3),
    }


@dataclass(frozen=True)
class CapacityGate:
    required_publication_rate_hz: float
    source_availability_ratios: tuple[float, ...]
    input_ready: bool
    throughput_passed: bool
    passed: bool


def evaluate_capacity(
    detection_hz: float,
    publication_rate_hz: float,
    published_batches: int,
    published_per_source: list[int],
    required_rate_ratio: float = DEFAULT_REQUIRED_RATE_RATIO,
    required_source_availability_ratio: float = DEFAULT_REQUIRED_SOURCE_AVAILABILITY_RATIO,
) -> CapacityGate:
    required_publication_rate_hz = detection_hz * required_rate_ratio
    denominator = max(published_batches, 1)
    source_availability_ratios = tuple(
        published_frames / denominator for published_frames in published_per_source
    )
    input_ready = (
        published_batches > 0
        and bool(source_availability_ratios)
        and all(
            ratio >= required_source_availability_ratio
            for ratio in source_availability_ratios
        )
    )
    throughput_passed = publication_rate_hz >= required_publication_rate_hz
    return CapacityGate(
        required_publication_rate_hz=required_publication_rate_hz,
        source_availability_ratios=source_availability_ratios,
        input_ready=input_ready,
        throughput_passed=throughput_passed,
        passed=input_ready and throughput_passed,
    )
