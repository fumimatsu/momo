#!/usr/bin/env python3
"""Build an offline impact report from Relay telemetry NDJSON logs.

This tool never changes Relay state or vehicle HP. The shadow result is an
analysis aid for comparing the current magnitude/jerk classifier with the
provisional time-window impact classifier.
"""

from __future__ import annotations

import argparse
import bisect
import csv
import glob
import html
import json
import math
import sys
from collections import Counter, defaultdict
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from statistics import median
from typing import Any, Iterable


CURRENT_WEAK_MAGNITUDE = 10.0
CURRENT_STRONG_MAGNITUDE = 12.0
CURRENT_STRONG_JERK = 250.0
CURRENT_SEVERE_MAGNITUDE = 15.0
CURRENT_SEVERE_JERK = 750.0
CURRENT_DAMAGE = {"": 0.0, "weak": 0.0, "strong": 12.0, "severe": 20.0}
WINDOW_SHADOW_ALGORITHM = "vertical-window-v2"
WINDOW_MS = 300.0
WINDOW_COVERAGE_TOLERANCE_MS = 50.0
COLLISION_VERTICAL_SHARE_MAX = 0.20
BASELINE_GUARD_MS = 100.0
HORIZONTAL_ACTIVE_MPS2 = 3.0
VERTICAL_REVERSAL_MPS2 = 1.0


@dataclass(slots=True)
class MotionSample:
    session_id: str
    time_ms: float
    received_at: str
    source_id: str
    car_id: str
    boot: str
    sequence: int
    device_time_us: int
    forward_mps2: float
    lateral_mps2: float
    vertical_mps2: float
    derived_jerk_mps3: float | None
    yaw_rate_rad_s: float | None


@dataclass(slots=True)
class ImpactCandidate:
    session_id: str
    time_ms: float
    received_at: str
    source_id: str
    car_id: str
    race_run_id: str
    boot: str
    sequence: int
    device_time_us: int
    event_id: str
    magnitude_mps2: float
    jerk_mps3: float
    axis_forward: float
    axis_lateral: float
    axis_vertical: float
    vertical_share: float
    horizontal_share: float
    current_class: str
    current_damage: float
    shadow_class: str
    shadow_damage: float
    shadow_action: str
    window_shadow_algorithm: str = ""
    window_shadow_axis_kind: str = ""
    window_shadow_kind: str = ""
    window_shadow_damage_allowed: bool | None = None
    window_shadow_ffb_allowed: bool | None = None
    window_shadow_complete: bool | None = None
    window_shadow_before_ms: float | None = None
    window_shadow_after_ms: float | None = None
    window_shadow_samples: int | None = None
    window_shadow_horizontal_active_ms: float | None = None
    window_shadow_baseline_forward_mps2: float | None = None
    window_shadow_baseline_lateral_mps2: float | None = None
    window_shadow_peak_horizontal_delta_mps2: float | None = None
    window_shadow_horizontal_delta_active_ms: float | None = None
    window_shadow_vertical_reversals: int | None = None
    window_shadow_reasons: str = ""
    confirmed_class: str = ""
    confirmed_damage_applied: bool | None = None
    confirmed_damage: float | None = None
    confirmed_suppression_reason: str = ""
    confirmed_hp_before: float | None = None
    confirmed_hp_after: float | None = None
    runtime_shadow_algorithm: str = ""
    runtime_shadow_axis_kind: str = ""
    runtime_shadow_kind: str = ""
    runtime_shadow_damage_allowed: bool | None = None
    runtime_shadow_ffb_allowed: bool | None = None
    runtime_shadow_window_complete: bool | None = None
    runtime_shadow_samples: int | None = None
    runtime_shadow_reasons: str = ""


@dataclass(slots=True)
class WindowShadowFeatures:
    complete: bool = False
    before_ms: float = 0.0
    after_ms: float = 0.0
    horizontal_active_ms: float = 0.0
    baseline_forward_mps2: float = 0.0
    baseline_lateral_mps2: float = 0.0
    peak_horizontal_delta_mps2: float = 0.0
    horizontal_delta_active_ms: float = 0.0
    vertical_reversals: int = 0


def finite_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    return result if math.isfinite(result) else None


def integer(value: Any) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        return None
    return value


def record_time_ms(record: dict[str, Any], fallback: float) -> float:
    epoch_ms = finite_number(record.get("epoch_ms"))
    if epoch_ms is not None:
        return epoch_ms
    received = record.get("relayReceivedAt")
    if isinstance(received, str):
        try:
            parsed = datetime.fromisoformat(received.replace("Z", "+00:00"))
            return parsed.timestamp() * 1000.0
        except ValueError:
            pass
    elapsed_us = finite_number(record.get("relayElapsedUs"))
    if elapsed_us is not None:
        return elapsed_us / 1000.0
    return fallback


def record_received_at(record: dict[str, Any], time_ms: float) -> str:
    received = record.get("relayReceivedAt")
    if isinstance(received, str) and received:
        return received
    if time_ms > 1_000_000_000_000:
        return datetime.fromtimestamp(time_ms / 1000.0, timezone.utc).isoformat()
    return ""


def classify_current(magnitude: float, jerk: float) -> str:
    if magnitude < CURRENT_WEAK_MAGNITUDE:
        return ""
    if magnitude >= CURRENT_SEVERE_MAGNITUDE and jerk >= CURRENT_SEVERE_JERK:
        return "severe"
    if magnitude >= CURRENT_STRONG_MAGNITUDE and jerk >= CURRENT_STRONG_JERK:
        return "strong"
    return "weak"


def axis_shares(axis: Iterable[float]) -> tuple[float, float]:
    forward, lateral, vertical = axis
    norm = math.sqrt((forward * forward) + (lateral * lateral) + (vertical * vertical))
    if norm <= 0:
        return 0.0, 0.0
    return abs(vertical) / norm, math.hypot(forward, lateral) / norm


def classify_shadow(
    current_class: str,
    vertical_share: float,
    horizontal_share: float,
    minimum_vertical_share: float,
    maximum_horizontal_share: float,
) -> tuple[str, float, str]:
    damage = CURRENT_DAMAGE[current_class]
    mixed_vertical = (
        vertical_share >= minimum_vertical_share
        and horizontal_share > maximum_horizontal_share
    )
    if mixed_vertical:
        return current_class, damage, "observe_mixed_vertical_candidate"
    vertical_dominant = (
        vertical_share >= minimum_vertical_share
        and horizontal_share <= maximum_horizontal_share
    )
    if not vertical_dominant:
        return current_class, damage, "unchanged"
    if damage > 0:
        return current_class, 0.0, "suppress_vertical_surface_candidate"
    return current_class, 0.0, "observe_vertical_surface_candidate"


def significant_vertical_sign(value: float) -> int:
    if abs(value) < VERTICAL_REVERSAL_MPS2:
        return 0
    return 1 if value > 0 else -1


def calculate_window_shadow_features(
    samples: list[MotionSample],
    event_time_ms: float,
) -> WindowShadowFeatures:
    features = WindowShadowFeatures()
    if not samples:
        return features

    features.before_ms = max(0.0, event_time_ms - samples[0].time_ms)
    features.after_ms = max(0.0, samples[-1].time_ms - event_time_ms)
    minimum_coverage = WINDOW_MS - WINDOW_COVERAGE_TOLERANCE_MS
    features.complete = (
        features.before_ms >= minimum_coverage
        and features.after_ms >= minimum_coverage
    )

    baseline_samples = [
        sample for sample in samples
        if sample.time_ms <= event_time_ms - BASELINE_GUARD_MS
    ]
    if baseline_samples:
        features.baseline_forward_mps2 = median(
            sample.forward_mps2 for sample in baseline_samples
        )
        features.baseline_lateral_mps2 = median(
            sample.lateral_mps2 for sample in baseline_samples
        )

    previous_sign = 0
    for index, sample in enumerate(samples):
        horizontal = math.hypot(sample.forward_mps2, sample.lateral_mps2)
        horizontal_delta = math.hypot(
            sample.forward_mps2 - features.baseline_forward_mps2,
            sample.lateral_mps2 - features.baseline_lateral_mps2,
        )
        features.peak_horizontal_delta_mps2 = max(
            features.peak_horizontal_delta_mps2,
            horizontal_delta,
        )
        vertical_sign = significant_vertical_sign(sample.vertical_mps2)
        if vertical_sign:
            if previous_sign and vertical_sign != previous_sign:
                features.vertical_reversals += 1
            previous_sign = vertical_sign

        if index == 0:
            continue
        previous = samples[index - 1]
        interval_ms = sample.time_ms - previous.time_ms
        if interval_ms <= 0 or interval_ms > 100.0:
            continue
        previous_horizontal = math.hypot(previous.forward_mps2, previous.lateral_mps2)
        if (horizontal + previous_horizontal) / 2 >= HORIZONTAL_ACTIVE_MPS2:
            features.horizontal_active_ms += interval_ms
        previous_delta = math.hypot(
            previous.forward_mps2 - features.baseline_forward_mps2,
            previous.lateral_mps2 - features.baseline_lateral_mps2,
        )
        if (horizontal_delta + previous_delta) / 2 >= HORIZONTAL_ACTIVE_MPS2:
            features.horizontal_delta_active_ms += interval_ms
    return features


def classify_window_shadow(
    current_class: str,
    vertical_share: float,
    horizontal_share: float,
    features: WindowShadowFeatures,
) -> tuple[str, str, bool, bool, list[str]]:
    axis_kind = "ambiguous"
    if vertical_share > COLLISION_VERTICAL_SHARE_MAX:
        axis_kind = "road_impact"
    elif horizontal_share > 0:
        axis_kind = "collision"

    kind = "ambiguous"
    reasons: list[str] = []
    if vertical_share > COLLISION_VERTICAL_SHARE_MAX and not features.complete:
        reasons.extend(["window_incomplete", "vertical_axis_candidate"])
    elif vertical_share > COLLISION_VERTICAL_SHARE_MAX and features.vertical_reversals > 0:
        kind = "road_impact"
        reasons.extend(["vertical_axis_candidate", "vertical_rebound"])
        if features.horizontal_active_ms > 0:
            reasons.append("horizontal_load_context_only")
    elif vertical_share > COLLISION_VERTICAL_SHARE_MAX:
        reasons.extend(["vertical_axis_candidate", "vertical_rebound_missing"])
    elif current_class in {"strong", "severe"}:
        kind = "collision"
        reasons.extend(["horizontal_axis_candidate", "damage_threshold_met"])
    else:
        reasons.extend(["horizontal_axis_candidate", "below_damage_threshold"])

    damage_allowed = kind == "collision" and current_class in {"strong", "severe"}
    ffb_allowed = kind in {"road_impact", "collision"} or bool(current_class)
    return axis_kind, kind, damage_allowed, ffb_allowed, reasons


def expand_inputs(values: list[str]) -> list[Path]:
    paths: list[Path] = []
    for value in values:
        matches = [Path(item) for item in glob.glob(value)]
        if not matches:
            matches = [Path(value)]
        for path in matches:
            resolved = path.resolve()
            if not resolved.is_file():
                raise FileNotFoundError(f"Relay log was not found: {path}")
            if resolved not in paths:
                paths.append(resolved)
    return paths


def analyze_logs(
    paths: list[Path],
    minimum_vertical_share: float = 0.75,
    maximum_horizontal_share: float = 0.45,
) -> dict[str, Any]:
    samples: list[MotionSample] = []
    candidates: list[ImpactCandidate] = []
    confirmed_by_id: dict[str, dict[str, Any]] = {}
    runtime_shadow_by_id: dict[str, dict[str, Any]] = {}
    counters: Counter[str] = Counter()
    sequence_state: dict[tuple[str, str, str, str], int] = {}
    stream_integrity: dict[tuple[str, str, str, str], Counter[str]] = defaultdict(Counter)
    previous_motion: dict[tuple[str, str, str], tuple[int, list[float]]] = {}

    for path in paths:
        file_session_id = path.stem
        file_source_id = ""
        file_car_id = ""
        with path.open("r", encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, start=1):
                counters["lines"] += 1
                stripped = line.strip()
                if not stripped:
                    counters["empty_lines"] += 1
                    continue
                try:
                    record = json.loads(stripped)
                except json.JSONDecodeError:
                    counters["malformed_lines"] += 1
                    continue
                if not isinstance(record, dict):
                    counters["invalid_records"] += 1
                    continue
                counters["records"] += 1
                record_type = str(record.get("type") or record.get("kind") or "unknown")
                counters[f"record_type:{record_type}"] += 1
                record_session_id = record.get("relaySessionId") or record.get("run_id")
                if isinstance(record_session_id, str) and record_session_id:
                    file_session_id = record_session_id
                if record.get("schema") == "momo-fpv-cpu-shadow-capture/v1" and record_type == "run_start":
                    viewer = record.get("viewer")
                    identity = viewer.get("source_identity") if isinstance(viewer, dict) else None
                    if isinstance(identity, dict):
                        file_source_id = str(identity.get("relay_device") or identity.get("room_id") or "")
                        file_car_id = str(identity.get("race_car_id") or "")
                    if not file_source_id:
                        file_source_id = str(record.get("run_id") or path.stem)
                stats = record.get("stats")
                if isinstance(stats, dict):
                    counters["queue_drops"] += int(stats.get("queueDrops") or 0)
                    counters["write_errors"] += int(stats.get("writeErrors") or 0)

                time_ms = record_time_ms(record, float(counters["records"]))
                received_at = record_received_at(record, time_ms)
                if record_type == "vehicle_event":
                    event = record.get("vehicleEvent")
                    if isinstance(event, dict) and isinstance(event.get("eventId"), str):
                        confirmed_by_id[event["eventId"]] = event
                    continue
                if record_type == "impact_shadow":
                    shadow = record.get("impactShadow")
                    if isinstance(shadow, dict) and isinstance(shadow.get("eventId"), str):
                        runtime_shadow_by_id[shadow["eventId"]] = shadow
                    continue
                if record_type != "telemetry":
                    continue
                raw = record.get("raw")
                if not isinstance(raw, str) and record.get("schema") == "momo-fpv-cpu-shadow-capture/v1":
                    raw = record.get("raw_message")
                if not isinstance(raw, str) or not raw.startswith("TEL:"):
                    counters["non_tel_telemetry"] += 1
                    continue
                try:
                    payload = json.loads(raw[4:])
                except json.JSONDecodeError:
                    counters["malformed_telemetry"] += 1
                    continue
                if not isinstance(payload, dict) or payload.get("v") != 2:
                    counters["non_v2_telemetry"] += 1
                    continue

                source_id = str(record.get("sourceId") or file_source_id)
                car_id = str(record.get("carId") or file_car_id)
                telemetry_source = str(payload.get("src") or record.get("telemetrySource") or "")
                boot = str(payload.get("boot") or "")
                sequence = integer(payload.get("seq"))
                device_time_us = integer(payload.get("t_us"))
                if sequence is None or device_time_us is None:
                    counters["invalid_v2_envelope"] += 1
                    continue

                stream_key = (file_session_id, source_id, telemetry_source, boot)
                integrity = stream_integrity[stream_key]
                integrity["messages"] += 1
                previous_sequence = sequence_state.get(stream_key)
                if previous_sequence is not None:
                    if sequence == previous_sequence:
                        integrity["duplicates"] += 1
                    elif sequence < previous_sequence:
                        integrity["regressions"] += 1
                    elif sequence > previous_sequence + 1:
                        integrity["missing_sequences"] += sequence - previous_sequence - 1
                sequence_state[stream_key] = sequence

                kind = payload.get("k")
                if kind == "s" and telemetry_source == "imu0":
                    motion = payload.get("m")
                    axis = motion.get("a") if isinstance(motion, dict) else None
                    if not isinstance(axis, list) or len(axis) != 3:
                        counters["invalid_motion_states"] += 1
                        continue
                    values = [finite_number(item) for item in axis]
                    if any(item is None for item in values):
                        counters["invalid_motion_states"] += 1
                        continue
                    yaw = finite_number(motion.get("y"))
                    motion_key = (file_session_id, source_id, boot)
                    derived_jerk = None
                    previous = previous_motion.get(motion_key)
                    if previous is not None:
                        elapsed_seconds = (device_time_us - previous[0]) / 1_000_000.0
                        if 0 < elapsed_seconds <= 1.0:
                            derived_jerk = math.sqrt(sum(
                                (values[index] - previous[1][index]) ** 2 for index in range(3)
                            )) / elapsed_seconds
                    previous_motion[motion_key] = (device_time_us, values)
                    samples.append(MotionSample(
                        session_id=file_session_id,
                        time_ms=time_ms,
                        received_at=received_at,
                        source_id=source_id,
                        car_id=car_id,
                        boot=boot,
                        sequence=sequence,
                        device_time_us=device_time_us,
                        forward_mps2=values[0],
                        lateral_mps2=values[1],
                        vertical_mps2=values[2],
                        derived_jerk_mps3=derived_jerk,
                        yaw_rate_rad_s=yaw,
                    ))
                    continue

                event = payload.get("e")
                if kind != "e" or not isinstance(event, dict) or event.get("n") != "impact_candidate":
                    continue
                magnitude = finite_number(event.get("m"))
                jerk = finite_number(event.get("j"))
                axis = event.get("a")
                if magnitude is None or jerk is None or not isinstance(axis, list) or len(axis) != 3:
                    counters["invalid_impact_candidates"] += 1
                    continue
                axis_values = [finite_number(item) for item in axis]
                if any(item is None for item in axis_values):
                    counters["invalid_impact_candidates"] += 1
                    continue
                vertical_share, horizontal_share = axis_shares(axis_values)
                current_class = classify_current(magnitude, jerk)
                shadow_class, shadow_damage, shadow_action = classify_shadow(
                    current_class,
                    vertical_share,
                    horizontal_share,
                    minimum_vertical_share,
                    maximum_horizontal_share,
                )
                event_id = f"{car_id or source_id}:{boot}:{sequence}"
                candidates.append(ImpactCandidate(
                    session_id=file_session_id,
                    time_ms=time_ms,
                    received_at=received_at,
                    source_id=source_id,
                    car_id=car_id,
                    race_run_id=str(record.get("raceRunId") or ""),
                    boot=boot,
                    sequence=sequence,
                    device_time_us=device_time_us,
                    event_id=event_id,
                    magnitude_mps2=magnitude,
                    jerk_mps3=jerk,
                    axis_forward=axis_values[0],
                    axis_lateral=axis_values[1],
                    axis_vertical=axis_values[2],
                    vertical_share=vertical_share,
                    horizontal_share=horizontal_share,
                    current_class=current_class,
                    current_damage=CURRENT_DAMAGE[current_class],
                    shadow_class=shadow_class,
                    shadow_damage=shadow_damage,
                    shadow_action=shadow_action,
                ))

    samples.sort(key=lambda item: (item.time_ms, item.source_id, item.sequence))
    candidates.sort(key=lambda item: (item.time_ms, item.source_id, item.sequence))

    samples_by_stream: dict[tuple[str, str, str], list[MotionSample]] = defaultdict(list)
    for sample in samples:
        samples_by_stream[(sample.session_id, sample.car_id or sample.source_id, sample.boot)].append(sample)
    times_by_stream = {
        key: [sample.time_ms for sample in values]
        for key, values in samples_by_stream.items()
    }

    for candidate in candidates:
        stream_key = (candidate.session_id, candidate.car_id or candidate.source_id, candidate.boot)
        stream_samples = samples_by_stream.get(stream_key, [])
        stream_times = times_by_stream.get(stream_key, [])
        left = bisect.bisect_left(stream_times, candidate.time_ms - WINDOW_MS)
        right = bisect.bisect_right(stream_times, candidate.time_ms + WINDOW_MS)
        window_samples = stream_samples[left:right]
        features = calculate_window_shadow_features(window_samples, candidate.time_ms)
        axis_kind, kind, damage_allowed, ffb_allowed, reasons = classify_window_shadow(
            candidate.current_class,
            candidate.vertical_share,
            candidate.horizontal_share,
            features,
        )
        candidate.window_shadow_algorithm = WINDOW_SHADOW_ALGORITHM
        candidate.window_shadow_axis_kind = axis_kind
        candidate.window_shadow_kind = kind
        candidate.window_shadow_damage_allowed = damage_allowed
        candidate.window_shadow_ffb_allowed = ffb_allowed
        candidate.window_shadow_complete = features.complete
        candidate.window_shadow_before_ms = features.before_ms
        candidate.window_shadow_after_ms = features.after_ms
        candidate.window_shadow_samples = len(window_samples)
        candidate.window_shadow_horizontal_active_ms = features.horizontal_active_ms
        candidate.window_shadow_baseline_forward_mps2 = features.baseline_forward_mps2
        candidate.window_shadow_baseline_lateral_mps2 = features.baseline_lateral_mps2
        candidate.window_shadow_peak_horizontal_delta_mps2 = features.peak_horizontal_delta_mps2
        candidate.window_shadow_horizontal_delta_active_ms = features.horizontal_delta_active_ms
        candidate.window_shadow_vertical_reversals = features.vertical_reversals
        candidate.window_shadow_reasons = ",".join(reasons)

        confirmed = confirmed_by_id.get(candidate.event_id)
        if confirmed:
            candidate.confirmed_class = str(confirmed.get("impactClass") or "")
            applied = confirmed.get("damageApplied")
            candidate.confirmed_damage_applied = applied if isinstance(applied, bool) else None
            candidate.confirmed_damage = finite_number(confirmed.get("damage"))
            candidate.confirmed_suppression_reason = str(confirmed.get("suppressionReason") or "")
            candidate.confirmed_hp_before = finite_number(confirmed.get("hpBefore"))
            candidate.confirmed_hp_after = finite_number(confirmed.get("hpAfter"))
        shadow = runtime_shadow_by_id.get(candidate.event_id)
        if not shadow:
            continue
        candidate.runtime_shadow_algorithm = str(shadow.get("algorithmVersion") or "")
        candidate.runtime_shadow_axis_kind = str(shadow.get("axisProposalKind") or "")
        candidate.runtime_shadow_kind = str(shadow.get("proposedKind") or "")
        damage_allowed = shadow.get("proposedDamageAllowed")
        candidate.runtime_shadow_damage_allowed = damage_allowed if isinstance(damage_allowed, bool) else None
        ffb_allowed = shadow.get("proposedFfbAllowed")
        candidate.runtime_shadow_ffb_allowed = ffb_allowed if isinstance(ffb_allowed, bool) else None
        window_complete = shadow.get("windowComplete")
        candidate.runtime_shadow_window_complete = window_complete if isinstance(window_complete, bool) else None
        motion_samples = shadow.get("motionSamples")
        candidate.runtime_shadow_samples = motion_samples if isinstance(motion_samples, int) and motion_samples >= 0 else None
        reasons = shadow.get("reasons")
        if isinstance(reasons, list):
            candidate.runtime_shadow_reasons = ",".join(str(item) for item in reasons)

    per_vehicle: dict[str, dict[str, Any]] = {}
    vehicle_keys = sorted({sample.car_id or sample.source_id for sample in samples}
                          | {event.car_id or event.source_id for event in candidates})
    for vehicle_key in vehicle_keys:
        vehicle_samples = [item for item in samples if (item.car_id or item.source_id) == vehicle_key]
        vehicle_events = [item for item in candidates if (item.car_id or item.source_id) == vehicle_key]
        session_samples: dict[str, list[MotionSample]] = defaultdict(list)
        for sample in vehicle_samples:
            session_samples[sample.session_id].append(sample)
        duration_seconds = sum(
            max(0.0, (values[-1].time_ms - values[0].time_ms) / 1000.0)
            for values in session_samples.values() if len(values) >= 2
        )
        motion_intervals = sum(max(0, len(values) - 1) for values in session_samples.values())
        per_vehicle[vehicle_key] = {
            "sessions": len(session_samples),
            "motionSamples": len(vehicle_samples),
            "impactCandidates": len(vehicle_events),
            "durationSeconds": duration_seconds,
            "effectiveMotionHz": (motion_intervals / duration_seconds) if duration_seconds > 0 else 0.0,
            "currentClasses": dict(Counter(item.current_class or "none" for item in vehicle_events)),
            "shadowActions": dict(Counter(item.shadow_action for item in vehicle_events)),
            "windowShadowKinds": dict(Counter(item.window_shadow_kind or "missing" for item in vehicle_events)),
            "runtimeShadowKinds": dict(Counter(item.runtime_shadow_kind or "missing" for item in vehicle_events)),
        }

    integrity_rows = []
    for key, values in sorted(stream_integrity.items()):
        integrity_rows.append({
            "sessionId": key[0],
            "sourceId": key[1],
            "telemetrySource": key[2],
            "boot": key[3],
            **dict(values),
        })

    return {
        "inputs": [str(path) for path in paths],
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "currentClassifier": {
            "weakMagnitudeMps2": CURRENT_WEAK_MAGNITUDE,
            "strongMagnitudeMps2": CURRENT_STRONG_MAGNITUDE,
            "strongJerkMps3": CURRENT_STRONG_JERK,
            "severeMagnitudeMps2": CURRENT_SEVERE_MAGNITUDE,
            "severeJerkMps3": CURRENT_SEVERE_JERK,
            "strongDamage": CURRENT_DAMAGE["strong"],
            "severeDamage": CURRENT_DAMAGE["severe"],
        },
        "shadowConfig": {
            "minimumVerticalShare": minimum_vertical_share,
            "maximumHorizontalShare": maximum_horizontal_share,
            "windowAlgorithm": WINDOW_SHADOW_ALGORITHM,
            "windowMs": WINDOW_MS,
            "collisionVerticalShareMax": COLLISION_VERTICAL_SHARE_MAX,
            "baselineGuardMs": BASELINE_GUARD_MS,
            "runtimeBehaviorChanged": False,
        },
        "counters": dict(counters),
        "streamIntegrity": integrity_rows,
        "perVehicle": per_vehicle,
        "samples": samples,
        "candidates": candidates,
        "unmatchedConfirmedEvents": max(0, len(confirmed_by_id) - sum(1 for item in candidates if item.event_id in confirmed_by_id)),
        "unmatchedRuntimeShadows": max(0, len(runtime_shadow_by_id) - sum(1 for item in candidates if item.event_id in runtime_shadow_by_id)),
    }


def write_csv(path: Path, rows: Iterable[dict[str, Any]], fieldnames: list[str]) -> None:
    with path.open("w", encoding="utf-8-sig", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fieldnames, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(rows)


def decimate(samples: list[MotionSample], limit: int = 3000) -> list[MotionSample]:
    if len(samples) <= limit:
        return samples
    step = len(samples) / limit
    return [samples[min(len(samples) - 1, int(index * step))] for index in range(limit)]


def build_report_html(result: dict[str, Any]) -> str:
    series: dict[str, Any] = {}
    for vehicle in result["perVehicle"]:
        session_ids = sorted({
            item.session_id for item in result["samples"]
            if (item.car_id or item.source_id) == vehicle
        } | {
            item.session_id for item in result["candidates"]
            if (item.car_id or item.source_id) == vehicle
        })
        for session_id in session_ids:
            samples = [item for item in result["samples"] if (item.car_id or item.source_id) == vehicle and item.session_id == session_id]
            events = [item for item in result["candidates"] if (item.car_id or item.source_id) == vehicle and item.session_id == session_id]
            reduced = decimate(samples)
            origin = reduced[0].time_ms if reduced else (events[0].time_ms if events else 0.0)
            label = f"{vehicle} / {session_id}"
            series[label] = {
                "samples": [[
                    (item.time_ms - origin) / 1000.0,
                    item.forward_mps2,
                    item.lateral_mps2,
                    item.vertical_mps2,
                    item.derived_jerk_mps3,
                ] for item in reduced],
                "events": [[(item.time_ms - origin) / 1000.0, item.current_class or "none", item.shadow_action] for item in events],
            }
    payload = json.dumps(series, ensure_ascii=False, separators=(",", ":")).replace("<", "\\u003c")
    vehicle_rows = "".join(
        "<tr>"
        f"<td>{html.escape(vehicle)}</td>"
        f"<td>{values['motionSamples']}</td>"
        f"<td>{values['effectiveMotionHz']:.1f}</td>"
        f"<td>{values['impactCandidates']}</td>"
        f"<td>{html.escape(json.dumps(values['shadowActions'], ensure_ascii=False))}</td>"
        f"<td>{html.escape(json.dumps(values['windowShadowKinds'], ensure_ascii=False))}</td>"
        "</tr>"
        for vehicle, values in result["perVehicle"].items()
    ) or '<tr><td colspan="6">No motion telemetry found</td></tr>'
    event_rows = "".join(
        "<tr>"
        f"<td>{html.escape(item.received_at)}</td><td>{html.escape(item.car_id or item.source_id)}</td>"
        f"<td>{item.magnitude_mps2:.1f}</td><td>{item.jerk_mps3:.0f}</td>"
        f"<td>{item.vertical_share:.3f}</td><td>{item.horizontal_share:.3f}</td>"
        f"<td>{html.escape(item.current_class or 'none')}</td><td>{html.escape(item.shadow_action)}</td>"
        f"<td>{html.escape(item.window_shadow_kind or 'missing')}</td>"
        f"<td>{html.escape(item.runtime_shadow_kind or 'missing')}</td>"
        f"<td>{html.escape(item.confirmed_suppression_reason or ('applied' if item.confirmed_damage_applied else 'unmatched'))}</td>"
        "</tr>"
        for item in result["candidates"]
    ) or '<tr><td colspan="11">No impact candidates found</td></tr>'
    chart_blocks = "".join(
        f'<section class="panel"><h2>{html.escape(vehicle)}</h2><canvas data-vehicle="{html.escape(vehicle, quote=True)}"></canvas></section>'
        for vehicle in series
    )
    counters = result["counters"]
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Relay impact analysis</title>
<style>
:root{{--bg:#0b0f12;--panel:#12191e;--line:#2b3942;--text:#e7edf0;--muted:#91a1aa;--cyan:#37c7e8;--magenta:#ec6bb7;--yellow:#f5d547;}}
*{{box-sizing:border-box}} body{{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 Segoe UI,Arial,sans-serif}}
main{{max-width:1500px;margin:auto;padding:20px}} h1{{font-size:22px;margin:0 0 4px}} h2{{font-size:15px;margin:0 0 10px}}
.muted{{color:var(--muted)}} .summary{{display:grid;grid-template-columns:repeat(4,minmax(140px,1fr));gap:8px;margin:16px 0}}
.metric,.panel{{background:var(--panel);border:1px solid var(--line);border-radius:4px;padding:12px}} .metric strong{{display:block;font-size:22px}}
table{{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}} th,td{{padding:7px 8px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}}
.table-wrap{{overflow:auto}} canvas{{width:100%;height:260px;display:block}} .legend{{display:flex;gap:16px;color:var(--muted);margin:10px 0}}
.legend b:nth-child(1){{color:var(--cyan)}} .legend b:nth-child(2){{color:var(--magenta)}} .legend b:nth-child(3){{color:var(--yellow)}}
@media(max-width:800px){{.summary{{grid-template-columns:repeat(2,1fr)}} main{{padding:10px}}}}
</style></head><body><main>
<h1>Relay impact analysis</h1><div class="muted">Offline shadow report. Runtime HP behavior was not changed.</div>
<div class="summary">
<div class="metric"><span>Motion samples</span><strong>{len(result['samples'])}</strong></div>
<div class="metric"><span>Impact candidates</span><strong>{len(result['candidates'])}</strong></div>
<div class="metric"><span>Malformed lines</span><strong>{counters.get('malformed_lines', 0)}</strong></div>
<div class="metric"><span>Queue drops</span><strong>{counters.get('queue_drops', 0)}</strong></div>
</div>
<section class="panel"><h2>Vehicle summary</h2><div class="table-wrap"><table><thead><tr><th>Vehicle</th><th>Samples</th><th>Effective Hz</th><th>Events</th><th>Axis shadow</th><th>Window v2</th></tr></thead><tbody>{vehicle_rows}</tbody></table></div></section>
<div class="legend"><b>Forward G</b><b>Lateral G</b><b>Vertical G</b><span>Derived jerk (lower band)</span></div>
{chart_blocks}
<section class="panel"><h2>Impact candidates</h2><div class="table-wrap"><table><thead><tr><th>Received</th><th>Vehicle</th><th>Magnitude</th><th>Jerk</th><th>Vertical</th><th>Horizontal</th><th>Current</th><th>Axis shadow</th><th>Window v2</th><th>Runtime shadow</th><th>Confirmed</th></tr></thead><tbody>{event_rows}</tbody></table></div></section>
<script>const series={payload};
const colors=['#37c7e8','#ec6bb7','#f5d547'];
function draw(canvas,data){{const dpr=devicePixelRatio||1,box=canvas.getBoundingClientRect();canvas.width=Math.max(1,Math.floor(box.width*dpr));canvas.height=Math.max(1,Math.floor(box.height*dpr));const c=canvas.getContext('2d');c.scale(dpr,dpr);const w=box.width,h=box.height,p=28,s=data.samples;if(!s.length){{c.fillStyle='#91a1aa';c.fillText('No samples',p,p);return}}const t0=s[0][0],t1=Math.max(t0+.001,s[s.length-1][0]),split=h*.72,zero=split/2;let peak=1,jerkPeak=1;for(const row of s){{for(let i=1;i<4;i++)peak=Math.max(peak,Math.abs(row[i]));if(Number.isFinite(row[4]))jerkPeak=Math.max(jerkPeak,row[4])}}const x=t=>p+(t-t0)/(t1-t0)*(w-p*1.5);c.strokeStyle='#2b3942';c.beginPath();c.moveTo(p,zero);c.lineTo(w-p/2,zero);c.moveTo(p,split);c.lineTo(w-p/2,split);c.stroke();for(const event of data.events){{c.strokeStyle=event[2].startsWith('suppress')?'#ff6b6b':'#64747d';c.beginPath();c.moveTo(x(event[0]),p);c.lineTo(x(event[0]),h-p);c.stroke()}}for(let axis=1;axis<4;axis++){{c.strokeStyle=colors[axis-1];c.lineWidth=1.4;c.beginPath();s.forEach((row,index)=>{{const xx=x(row[0]),v=row[axis],yy=zero-v/peak*(zero-p);if(index)c.lineTo(xx,yy);else c.moveTo(xx,yy)}});c.stroke()}}c.strokeStyle='#c8d1d6';c.lineWidth=1;c.beginPath();let started=false;for(const row of s){{if(!Number.isFinite(row[4]))continue;const yy=h-p-(row[4]/jerkPeak)*(h-split-p);if(started)c.lineTo(x(row[0]),yy);else{{c.moveTo(x(row[0]),yy);started=true}}}}c.stroke();c.fillStyle='#91a1aa';c.fillText(peak.toFixed(1)+' m/s2',3,p);c.fillText((-peak).toFixed(1),3,split-p);c.fillText(jerkPeak.toFixed(0)+' m/s3',3,split+13)}}
function redraw(){{document.querySelectorAll('canvas[data-vehicle]').forEach(canvas=>draw(canvas,series[canvas.dataset.vehicle]))}}redraw();addEventListener('resize',redraw);</script>
</main></body></html>"""


def write_outputs(result: dict[str, Any], output_dir: Path, window_ms: float) -> None:
    output_dir.mkdir(parents=True, exist_ok=True)
    summary = {key: value for key, value in result.items() if key not in {"samples", "candidates"}}
    summary["impactCandidates"] = [asdict(item) for item in result["candidates"]]
    (output_dir / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")

    sample_fields = list(MotionSample.__dataclass_fields__)
    event_fields = list(ImpactCandidate.__dataclass_fields__)
    write_csv(output_dir / "motion-samples.csv", (asdict(item) for item in result["samples"]), sample_fields)
    write_csv(output_dir / "impact-events.csv", (asdict(item) for item in result["candidates"]), event_fields)

    window_fields = ["event_id", "offset_ms", *sample_fields]
    window_rows = []
    samples_by_vehicle: dict[tuple[str, str], list[MotionSample]] = defaultdict(list)
    for sample in result["samples"]:
        samples_by_vehicle[(sample.session_id, sample.car_id or sample.source_id)].append(sample)
    for event in result["candidates"]:
        vehicle = event.car_id or event.source_id
        for sample in samples_by_vehicle.get((event.session_id, vehicle), []):
            offset = sample.time_ms - event.time_ms
            if abs(offset) <= window_ms:
                window_rows.append({"event_id": event.event_id, "offset_ms": offset, **asdict(sample)})
    write_csv(output_dir / "event-windows.csv", window_rows, window_fields)
    (output_dir / "report.html").write_text(build_report_html(result), encoding="utf-8")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Analyze Relay telemetry NDJSON impact candidates offline.")
    parser.add_argument("input", nargs="+", help="One or more telemetry NDJSON paths or glob patterns")
    parser.add_argument("--output-dir", required=True, help="Directory for CSV, JSON, and HTML output")
    parser.add_argument("--window-ms", type=float, default=500.0, help="Motion window around each impact candidate")
    parser.add_argument("--minimum-vertical-share", type=float, default=0.75)
    parser.add_argument("--maximum-horizontal-share", type=float, default=0.45)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    if args.window_ms < 0:
        raise ValueError("--window-ms must not be negative")
    for name, value in [
        ("--minimum-vertical-share", args.minimum_vertical_share),
        ("--maximum-horizontal-share", args.maximum_horizontal_share),
    ]:
        if not 0 <= value <= 1:
            raise ValueError(f"{name} must be between 0 and 1")
    paths = expand_inputs(args.input)
    result = analyze_logs(paths, args.minimum_vertical_share, args.maximum_horizontal_share)
    output_dir = Path(args.output_dir).resolve()
    write_outputs(result, output_dir, args.window_ms)
    print(f"Relay impact analysis: {len(result['samples'])} motion samples, {len(result['candidates'])} candidates")
    print(f"Report: {output_dir / 'report.html'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
