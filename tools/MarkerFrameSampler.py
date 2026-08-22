from dataclasses import dataclass
from typing import Mapping, Sequence


@dataclass(frozen=True)
class SourceFrameState:
    source_id: str
    source_sequence: int
    received_tick: int
    video_valid: bool


@dataclass(frozen=True)
class SampledSource:
    source_id: str
    source_sequence: int
    eligible: bool
    age_ticks: int | None
    reason: str


def sample_latest_frames(
    sources: Sequence[SourceFrameState],
    sample_tick: int,
    last_detected_sequences: Mapping[str, int],
    maximum_age_ticks: int,
    maximum_skew_ticks: int,
) -> list[SampledSource]:
    if sample_tick < 0:
        raise ValueError("sample_tick must not be negative")
    if maximum_age_ticks < 0 or maximum_skew_ticks < 0:
        raise ValueError("age and skew limits must not be negative")

    seen_source_ids: set[str] = set()
    preliminary: list[SampledSource] = []
    newest_eligible_tick: int | None = None
    for source in sources:
        if not source.source_id or source.source_id in seen_source_ids:
            raise ValueError("source_id must be non-empty and unique")
        seen_source_ids.add(source.source_id)

        age_ticks = sample_tick - source.received_tick
        if not source.video_valid or source.source_sequence <= 0:
            preliminary.append(
                SampledSource(source.source_id, source.source_sequence, False, None, "no_video")
            )
        elif age_ticks < 0:
            preliminary.append(
                SampledSource(source.source_id, source.source_sequence, False, None, "future_timestamp")
            )
        elif source.source_sequence <= last_detected_sequences.get(source.source_id, 0):
            preliminary.append(
                SampledSource(
                    source.source_id,
                    source.source_sequence,
                    False,
                    age_ticks,
                    "duplicate_or_rollback",
                )
            )
        elif age_ticks > maximum_age_ticks:
            preliminary.append(
                SampledSource(source.source_id, source.source_sequence, False, age_ticks, "stale")
            )
        else:
            preliminary.append(
                SampledSource(source.source_id, source.source_sequence, True, age_ticks, "selected")
            )
            if newest_eligible_tick is None or source.received_tick > newest_eligible_tick:
                newest_eligible_tick = source.received_tick

    if newest_eligible_tick is None:
        return preliminary

    sampled: list[SampledSource] = []
    for source, result in zip(sources, preliminary, strict=True):
        if result.eligible and newest_eligible_tick - source.received_tick > maximum_skew_ticks:
            sampled.append(
                SampledSource(
                    source.source_id,
                    source.source_sequence,
                    False,
                    result.age_ticks,
                    "skewed",
                )
            )
        else:
            sampled.append(result)
    return sampled
