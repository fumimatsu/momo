"""Bound unreadable MLY2 metadata without manufacturing an image observation."""

from collections import Counter, deque
from dataclasses import dataclass
import time


@dataclass
class MetadataGap:
    started_tick: int
    invalidated: bool = False


class MetadataGapTracker:
    def __init__(self):
        self.gaps = {}
        self.snapshots = {}
        self.last_invalid_reason = {}
        self.counts = Counter()
        self.recent = deque(maxlen=32)
        self.unreported = 0
        self.generation = 0

    def record(self, source_id, reason, tick, sequence=0):
        self.counts[reason] += 1
        self.recent.append(dict(sourceId=source_id, reason=reason,
                                atQpc=tick or None, sourceSequence=sequence,
                                generation=self.generation, atUnixNs=time.time_ns()))
        self.unreported += 1

    def classify(self, source_id, snapshot, tick, maximum_gap_ticks):
        gap = self.gaps.get(source_id)
        if snapshot is None and gap is None:
            gap = self.gaps[source_id] = MetadataGap(tick)
            previous = self.snapshots.get(source_id)
            self.record(source_id, "metadata_pending", tick,
                        previous.source_sequence if previous else 0)
        if snapshot is not None:
            self.snapshots[source_id] = snapshot
        if gap is None:
            return None
        elapsed = tick - gap.started_tick
        # A late recovery must publish invalidation BEFORE accepting its image.
        # Keep this debt until the writer acknowledges a successful write.
        if not gap.invalidated and (elapsed < 0 or elapsed >= maximum_gap_ticks):
            return "metadata_timeout"
        if snapshot is None:
            return "metadata_pending"
        del self.gaps[source_id]
        self.record(source_id, "metadata_recovered_after_reset" if gap.invalidated
                    else "metadata_recovered", tick, snapshot.source_sequence)
        return None

    def acknowledge_invalid(self, observations, sampled):
        reasons = {s.source_id: s.reason for s in sampled}
        for observation in observations:
            source_id = observation.source_id
            reason = reasons[source_id]
            gap = self.gaps.get(source_id)
            if gap is not None:
                gap.invalidated = True
            if self.last_invalid_reason.get(source_id) != reason:
                self.record(source_id, reason, 0, observation.source_sequence)
                self.last_invalid_reason[source_id] = reason

    def acknowledge_valid(self, source_ids):
        for source_id in source_ids:
            if self.last_invalid_reason.pop(source_id, None) is not None:
                snapshot = self.snapshots.get(source_id)
                self.record(source_id, "valid_observation_resumed", 0,
                            snapshot.source_sequence if snapshot else 0)

    def change_generation(self, generation):
        self.gaps.clear()
        self.snapshots.clear()
        self.last_invalid_reason.clear()
        self.generation = generation

    def take_report(self, generation, at_unix_ns):
        if not self.unreported:
            return None
        report = dict(type="marker_input_health", version=1,
                      generation=generation, atUnixNs=at_unix_ns,
                      counts=dict(self.counts), recent=list(self.recent),
                      omittedEvents=max(0, self.unreported - len(self.recent)))
        self.recent.clear()
        self.unreported = 0
        return report
