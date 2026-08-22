from dataclasses import dataclass


DEFAULT_PROFILES_HZ = (50, 40, 33, 25)


@dataclass(frozen=True)
class DetectionWindow:
    duration_seconds: float
    cycle_p95_ms: float
    deadline_miss_ratio: float


@dataclass(frozen=True)
class RateDecision:
    detection_hz: int
    changed: bool
    capacity_exceeded: bool
    reason: str


class AdaptiveDetectionRateController:
    def __init__(
        self,
        profiles_hz: tuple[int, ...] = DEFAULT_PROFILES_HZ,
        overload_windows: int = 3,
        utilization_limit: float = 0.80,
        deadline_miss_limit: float = 0.05,
        hold_seconds: float = 10.0,
        upgrade_utilization_limit: float = 0.55,
        upgrade_deadline_miss_limit: float = 0.01,
        upgrade_healthy_seconds: float = 60.0,
        initial_detection_hz: int | None = None,
    ):
        if not profiles_hz or any(value <= 0 for value in profiles_hz):
            raise ValueError("profiles_hz must contain positive values")
        if tuple(sorted(profiles_hz, reverse=True)) != profiles_hz:
            raise ValueError("profiles_hz must be strictly descending")
        if len(set(profiles_hz)) != len(profiles_hz):
            raise ValueError("profiles_hz must not contain duplicates")
        if initial_detection_hz is not None and initial_detection_hz not in profiles_hz:
            raise ValueError("initial_detection_hz must be one of profiles_hz")
        if overload_windows < 1:
            raise ValueError("overload_windows must be positive")
        self.profiles_hz = profiles_hz
        self.overload_windows = overload_windows
        self.utilization_limit = utilization_limit
        self.deadline_miss_limit = deadline_miss_limit
        self.hold_seconds = hold_seconds
        self.upgrade_utilization_limit = upgrade_utilization_limit
        self.upgrade_deadline_miss_limit = upgrade_deadline_miss_limit
        self.upgrade_healthy_seconds = upgrade_healthy_seconds
        self.profile_index = (
            0
            if initial_detection_hz is None
            else profiles_hz.index(initial_detection_hz)
        )
        self.consecutive_overload_windows = 0
        self.healthy_seconds = 0.0
        self.hold_until = 0.0
        self.capacity_exceeded = False

    @property
    def detection_hz(self) -> int:
        return self.profiles_hz[self.profile_index]

    def observe_window(
        self,
        window: DetectionWindow,
        now_seconds: float,
        allow_downgrade: bool,
    ) -> RateDecision:
        self._validate_window(window)
        period_ms = 1000.0 / self.detection_hz
        utilization = window.cycle_p95_ms / period_ms
        overloaded = (
            utilization > self.utilization_limit
            or window.deadline_miss_ratio > self.deadline_miss_limit
        )
        healthy = (
            utilization < self.upgrade_utilization_limit
            and window.deadline_miss_ratio < self.upgrade_deadline_miss_limit
        )

        if overloaded:
            self.consecutive_overload_windows += 1
            self.healthy_seconds = 0.0
        else:
            self.consecutive_overload_windows = 0
            if healthy:
                self.healthy_seconds += window.duration_seconds
            else:
                self.healthy_seconds = 0.0
            if self.capacity_exceeded:
                self.capacity_exceeded = False

        if not overloaded or self.consecutive_overload_windows < self.overload_windows:
            return self._decision(False, "stable" if not overloaded else "overload_pending")
        if not allow_downgrade:
            return self._decision(False, "downgrade_locked")
        if now_seconds < self.hold_until:
            return self._decision(False, "hold")
        if self.profile_index + 1 >= len(self.profiles_hz):
            self.capacity_exceeded = True
            return self._decision(False, "capacity_exceeded")

        self.profile_index += 1
        self.consecutive_overload_windows = 0
        self.hold_until = now_seconds + self.hold_seconds
        self.capacity_exceeded = False
        return self._decision(True, "overload_downgrade")

    def prepare(self, now_seconds: float) -> RateDecision:
        if self.profile_index == 0 or self.healthy_seconds < self.upgrade_healthy_seconds:
            return self._decision(False, "prepare_keep")
        self.profile_index -= 1
        self.consecutive_overload_windows = 0
        self.healthy_seconds = 0.0
        self.hold_until = now_seconds + self.hold_seconds
        self.capacity_exceeded = False
        return self._decision(True, "prepare_upgrade")

    def _decision(self, changed: bool, reason: str) -> RateDecision:
        return RateDecision(
            detection_hz=self.detection_hz,
            changed=changed,
            capacity_exceeded=self.capacity_exceeded,
            reason=reason,
        )

    @staticmethod
    def _validate_window(window: DetectionWindow) -> None:
        if window.duration_seconds <= 0:
            raise ValueError("duration_seconds must be positive")
        if window.cycle_p95_ms < 0:
            raise ValueError("cycle_p95_ms must not be negative")
        if not 0 <= window.deadline_miss_ratio <= 1:
            raise ValueError("deadline_miss_ratio must be in [0, 1]")
