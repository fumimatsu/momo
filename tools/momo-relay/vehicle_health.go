package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	vehicleHealthMaximum             = 100.0
	vehicleHealthStrongDamage        = 12.0
	vehicleHealthSevereDamage        = 20.0
	vehicleHealthDamageEpisodeWindow = 1500 * time.Millisecond
	vehicleHealthRecoveryDelay       = 4 * time.Second
	vehicleHealthForwardCommandGrace = 350 * time.Millisecond
	vehicleHealthPublishInterval     = 100 * time.Millisecond
	vehicleFuelMaximum               = 100.0
	vehicleFuelRecoveryAmount        = 10.0
	vehicleFuelDefaultDriveDuration  = 120 * time.Second
	vehicleFuelVariationTimeConstant = 1500 * time.Millisecond
	vehicleFuelVariationGrace        = 500 * time.Millisecond
	vehicleFuelVariationDeadzone     = 0.4
	vehicleFuelVariationFullPenalty  = 2.0
	vehicleFuelRoughMaxMultiplier    = 1.6
	vehicleGearOneForwardMaximum     = 1600
	vehicleFuelEmptyForwardPWM       = vehicleGearOneForwardMaximum - 10
	vehicleFuelEmptyReversePWM       = 1500 - (vehicleFuelEmptyForwardPWM - 1500)
	vehicleBoostMaximum              = 100.0
	vehicleBoostDuration             = 2500 * time.Millisecond
	vehicleBoostFallbackCharge       = 30 * time.Second
	vehicleBoostLeaderCharge         = 45 * time.Second
	vehicleBoostCloseGapCharge       = 40 * time.Second
	vehicleBoostFarGapCharge         = 20 * time.Second
	vehicleBoostFullGapBenefit       = 8 * time.Second
	vehicleBoostOneLapDownCharge     = 16 * time.Second
	vehicleBoostMinimumCharge        = 12 * time.Second
	vehicleNormalGearMaximum         = 3
	vehicleBoostGear                 = 4
	relayImpactWeakMagnitudeMPS2     = 10.0
	relayImpactStrongMagnitudeMPS2   = 12.0
	relayImpactStrongJerkMPS3        = 250.0
	relayImpactSevereMagnitudeMPS2   = 15.0
	relayImpactSevereJerkMPS3        = 750.0
)

type vehicleHealthSnapshot struct {
	HP                 float64 `json:"hp"`
	SpeedCap           float64 `json:"speedCap"`
	Mode               string  `json:"mode"`
	Fuel               float64 `json:"fuel"`
	FuelState          string  `json:"fuelState"`
	Boost              float64 `json:"boost"`
	BoostState         string  `json:"boostState"`
	BoostRemainingMS   int64   `json:"boostRemainingMs"`
	Gear               int     `json:"gear"`
	NormalGearMax      int     `json:"normalGearMax"`
	Position           int     `json:"position"`
	FieldSize          int     `json:"fieldSize"`
	FuelRatePerSec     float64 `json:"fuelRatePerSecond"`
	FuelRateMultiplier float64 `json:"fuelRateMultiplier"`
	ThrottleVariation  float64 `json:"throttleVariationPerSecond"`
	RequestedThrottle  float64 `json:"requestedThrottle"`
	EffectiveThrottle  float64 `json:"effectiveThrottle"`
	SessionType        string  `json:"sessionType"`
	RaceGapKnown       bool    `json:"raceGapKnown"`
	GapToAheadMS       *int64  `json:"gapToAheadMs,omitempty"`
	LapDeltaToAhead    *int    `json:"lapDeltaToAhead,omitempty"`
	BoostChargeMS      int64   `json:"boostChargeMs"`
	ServerTimeMS       int64   `json:"serverTimeMs"`
}

type vehicleRaceGap struct {
	Known             bool
	IntervalToAheadMS int64
	LapDeltaToAhead   int
}

type pitRecoveryApplyResult struct {
	Status              string
	RecoveredAmount     float64
	FuelRecoveredAmount float64
	Snapshot            vehicleHealthSnapshot
}

type pitRecoveryApplyError struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
}

type pitRecoveryReceipt struct {
	Fingerprint         string
	RecoveredAmount     float64
	FuelRecoveredAmount float64
	Snapshot            vehicleHealthSnapshot
}

// vehicleHealth は Relay ごとの車体状態である。操縦上限を Viewer に預けると
// URL を直接開いた別の Pilot が制限を回避できるため、Relay 境界で適用する。
type vehicleHealth struct {
	mu sync.Mutex

	hp                     float64
	fuel                   float64
	boost                  float64
	boostActiveUntil       time.Time
	requestedGear          int
	position               int
	fieldSize              int
	raceGapKnown           bool
	gapToAheadMS           int64
	lapDeltaToAhead        int
	fuelDriveDuration      time.Duration
	fuelRatePerSec         float64
	throttleVariation      float64
	lastThrottleSample     float64
	lastThrottleSampleAt   time.Time
	hasThrottleSample      bool
	requestedThrottle      float64
	effectiveThrottle      float64
	driveEnabled           bool
	pitPresent             bool
	raceConnected          bool
	lastRaceStateAt        time.Time
	lastUpdatedAt          time.Time
	lastUnsafeAt           time.Time
	damageEpisodeStartedAt time.Time
	damageEpisodeDamage    float64
	lastForwardAt          time.Time
	lastPublishedAt        time.Time
	lastRacePhase          string
	lastSessionType        string
	recoveryMode           vehicleHealthRecoveryMode
	activeRaceRunID        string
	pitEntryID             string
	lastPitTick            int
	lastPitAt              time.Time
	pitReceipts            map[string]pitRecoveryReceipt
	pitSeenEntries         map[string]struct{}
	impactSeen             map[string]struct{}
}

func newVehicleHealth(now time.Time) *vehicleHealth {
	return newVehicleHealthWithFuelDuration(now, vehicleFuelDefaultDriveDuration)
}

func newVehicleHealthWithFuelDuration(now time.Time, fuelDriveDuration time.Duration) *vehicleHealth {
	if fuelDriveDuration <= 0 {
		fuelDriveDuration = vehicleFuelDefaultDriveDuration
	}
	return &vehicleHealth{
		hp:                vehicleHealthMaximum,
		fuel:              vehicleFuelMaximum,
		requestedGear:     1,
		fuelDriveDuration: fuelDriveDuration,
		lastUpdatedAt:     now,
		recoveryMode:      vehicleHealthRecoveryDefault,
		pitReceipts:       make(map[string]pitRecoveryReceipt),
		pitSeenEntries:    make(map[string]struct{}),
		impactSeen:        make(map[string]struct{}),
	}
}

func (health *vehicleHealth) setRecoveryMode(mode vehicleHealthRecoveryMode) {
	if health == nil {
		return
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.recoveryMode = mode
	health.resetPitRecoveryLocked()
}

func (health *vehicleHealth) recoveryModeSnapshot() vehicleHealthRecoveryMode {
	if health == nil {
		return vehicleHealthRecoveryDefault
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.recoveryMode
}

func (health *vehicleHealth) observeRaceRun(raceRunID string, now time.Time) {
	if health == nil {
		return
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	if raceRunID == health.activeRaceRunID {
		return
	}
	health.activeRaceRunID = raceRunID
	health.hp = vehicleHealthMaximum
	health.fuel = vehicleFuelMaximum
	health.boost = 0
	health.boostActiveUntil = time.Time{}
	health.requestedGear = 1
	health.requestedThrottle = 0
	health.effectiveThrottle = 0
	health.pitPresent = false
	health.lastForwardAt = time.Time{}
	health.lastUnsafeAt = time.Time{}
	health.resetDamageEpisodeLocked()
	health.lastPublishedAt = now
	health.fuelRatePerSec = 0
	health.resetThrottleVariationLocked()
	health.lastUpdatedAt = now
	health.lastRaceStateAt = now
	health.raceGapKnown = false
	health.gapToAheadMS = 0
	health.lapDeltaToAhead = 0
	health.resetPitRecoveryLocked()
	health.resetImpactDedupeLocked()
}

func (health *vehicleHealth) snapshot(now time.Time) vehicleHealthSnapshot {
	if health == nil {
		return defaultVehicleHealthSnapshot(now)
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	return health.snapshotLocked(now)
}

func (health *vehicleHealth) observeRacePhase(phase string, now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), false
	}
	phase = strings.ToLower(strings.TrimSpace(phase))
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	health.raceConnected = true
	health.lastRaceStateAt = now

	if phase == health.lastRacePhase {
		return health.snapshotLocked(now), false
	}
	health.lastRacePhase = phase
	if phase != "ready" {
		changed := false
		if phase != "green" {
			changed = health.restoreDamageLocked(now)
		}
		return health.snapshotLocked(now), changed
	}

	health.hp = vehicleHealthMaximum
	health.fuel = vehicleFuelMaximum
	health.boost = 0
	health.boostActiveUntil = time.Time{}
	health.requestedGear = 1
	health.fuelRatePerSec = 0
	health.resetThrottleVariationLocked()
	health.requestedThrottle = 0
	health.effectiveThrottle = 0
	health.pitPresent = false
	health.lastUpdatedAt = now
	health.lastUnsafeAt = time.Time{}
	health.resetDamageEpisodeLocked()
	health.lastForwardAt = time.Time{}
	health.lastPublishedAt = now
	health.resetPitRecoveryLocked()
	health.resetImpactDedupeLocked()
	return health.snapshotLocked(now), true
}

func (health *vehicleHealth) observeRaceState(connected bool, raceRunID string, phase string, position int, fieldSize int, now time.Time, sessionTypes ...string) (vehicleHealthSnapshot, bool) {
	return health.observeRaceStateWithGap(connected, raceRunID, phase, position, fieldSize, vehicleRaceGap{}, now, sessionTypes...)
}

func (health *vehicleHealth) observeRaceStateWithGap(connected bool, raceRunID string, phase string, position int, fieldSize int, gap vehicleRaceGap, now time.Time, sessionTypes ...string) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), false
	}
	health.mu.Lock()
	previousConnected := health.raceConnected
	previousPhase := health.lastRacePhase
	previousRunID := health.activeRaceRunID
	previousPosition := health.position
	previousFieldSize := health.fieldSize
	previousGapKnown := health.raceGapKnown
	previousGapToAheadMS := health.gapToAheadMS
	previousLapDeltaToAhead := health.lapDeltaToAhead
	previousSessionType := health.lastSessionType
	health.mu.Unlock()
	health.observeRaceRun(raceRunID, now)
	snapshot, changed := health.observeRacePhase(phase, now)
	health.mu.Lock()
	defer health.mu.Unlock()
	health.raceConnected = connected
	health.lastRaceStateAt = now
	health.position = position
	health.fieldSize = fieldSize
	health.raceGapKnown = gap.Known
	health.gapToAheadMS = maxInt64(0, gap.IntervalToAheadMS)
	health.lapDeltaToAhead = maxInt(0, gap.LapDeltaToAhead)
	health.lastSessionType = normalizeRaceSessionType(firstString(sessionTypes))
	if !connected && health.restoreDamageLocked(now) {
		changed = true
	}
	if previousConnected != connected || previousPhase != strings.ToLower(strings.TrimSpace(phase)) || previousRunID != raceRunID ||
		previousPosition != position || previousFieldSize != fieldSize || previousSessionType != health.lastSessionType ||
		previousGapKnown != health.raceGapKnown || previousGapToAheadMS != health.gapToAheadMS || previousLapDeltaToAhead != health.lapDeltaToAhead ||
		snapshot.BoostState != health.boostStateLocked(now) {
		changed = true
	}
	return health.snapshotLocked(now), changed
}

func (health *vehicleHealth) markRaceDisconnected(now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), false
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	changed := health.raceConnected
	health.raceConnected = false
	if health.restoreDamageLocked(now) {
		changed = true
	}
	return health.snapshotLocked(now), changed
}

func (health *vehicleHealth) restoreDamageLocked(now time.Time) bool {
	if health.hp >= vehicleHealthMaximum {
		return false
	}
	health.hp = vehicleHealthMaximum
	health.lastUnsafeAt = time.Time{}
	health.resetDamageEpisodeLocked()
	health.lastUpdatedAt = now
	return true
}

func (health *vehicleHealth) applyPitRecovery(command pitRecoveryCommand, now time.Time) (pitRecoveryApplyResult, *pitRecoveryApplyError) {
	if health == nil {
		return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
			StatusCode: 409,
			Code:       "health_unavailable",
			Message:    "vehicle health is unavailable",
		}
	}
	health.mu.Lock()
	defer health.mu.Unlock()

	if !health.recoveryMode.allowsPitRecovery() {
		return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
			StatusCode: 409,
			Code:       "recovery_mode_not_allowed",
			Message:    fmt.Sprintf("vehicle health recovery mode is %q", health.recoveryMode),
		}
	}
	if health.activeRaceRunID == "" || command.RaceRunID != health.activeRaceRunID {
		return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
			StatusCode: 409,
			Code:       "race_run_mismatch",
			Message:    "raceRunId does not match the vehicle health run",
		}
	}

	fingerprint := command.fingerprint()
	if receipt, ok := health.pitReceipts[command.CommandID]; ok {
		if receipt.Fingerprint != fingerprint {
			return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
				StatusCode: 409,
				Code:       "command_id_conflict",
				Message:    "commandId was already used for a different recovery tick",
			}
		}
		return pitRecoveryApplyResult{
			Status:              "duplicate",
			RecoveredAmount:     receipt.RecoveredAmount,
			FuelRecoveredAmount: receipt.FuelRecoveredAmount,
			Snapshot:            receipt.Snapshot,
		}, nil
	}

	if health.pitEntryID == command.EntryID {
		if command.Tick != health.lastPitTick+1 {
			return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
				StatusCode: 409,
				Code:       "tick_out_of_sequence",
				Message:    fmt.Sprintf("tick must be %d for the current entry", health.lastPitTick+1),
			}
		}
	} else {
		if command.Tick != 1 {
			return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
				StatusCode: 409,
				Code:       "tick_out_of_sequence",
				Message:    "a new entryId must start at tick 1",
			}
		}
		if _, seen := health.pitSeenEntries[command.EntryID]; seen {
			return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
				StatusCode: 409,
				Code:       "entry_id_reused",
				Message:    "entryId was already used in this race run",
			}
		}
	}
	if !health.lastPitAt.IsZero() {
		elapsed := now.Sub(health.lastPitAt)
		if elapsed < pitRecoveryMinimumInterval {
			return pitRecoveryApplyResult{}, &pitRecoveryApplyError{
				StatusCode: 429,
				Code:       "recovery_too_soon",
				Message:    "the previous recovery tick was accepted less than 1 second ago",
				RetryAfter: pitRecoveryMinimumInterval - elapsed,
			}
		}
	}

	health.advanceRecoveryLocked(now)
	previousHP := health.hp
	previousFuel := health.fuel
	health.hp = math.Min(vehicleHealthMaximum, health.hp+pitRecoveryAmount)
	health.fuel = math.Min(vehicleFuelMaximum, health.fuel+vehicleFuelRecoveryAmount)
	recoveredAmount := health.hp - previousHP
	fuelRecoveredAmount := health.fuel - previousFuel
	health.lastUpdatedAt = now
	health.lastPublishedAt = now
	health.pitEntryID = command.EntryID
	health.pitSeenEntries[command.EntryID] = struct{}{}
	health.lastPitTick = command.Tick
	health.lastPitAt = now
	snapshot := health.snapshotLocked(now)
	health.recordPitReceiptLocked(command.CommandID, pitRecoveryReceipt{
		Fingerprint:         fingerprint,
		RecoveredAmount:     recoveredAmount,
		FuelRecoveredAmount: fuelRecoveredAmount,
		Snapshot:            snapshot,
	})
	return pitRecoveryApplyResult{
		Status:              "applied",
		RecoveredAmount:     recoveredAmount,
		FuelRecoveredAmount: fuelRecoveredAmount,
		Snapshot:            snapshot,
	}, nil
}

func (health *vehicleHealth) recordPitReceiptLocked(commandID string, receipt pitRecoveryReceipt) {
	health.pitReceipts[commandID] = receipt
}

func (health *vehicleHealth) resetPitRecoveryLocked() {
	health.pitEntryID = ""
	health.lastPitTick = 0
	health.lastPitAt = time.Time{}
	health.pitReceipts = make(map[string]pitRecoveryReceipt)
	health.pitSeenEntries = make(map[string]struct{})
}

func (health *vehicleHealth) resetImpactDedupeLocked() {
	health.impactSeen = make(map[string]struct{})
}

func (health *vehicleHealth) resetDamageEpisodeLocked() {
	health.damageEpisodeStartedAt = time.Time{}
	health.damageEpisodeDamage = 0
}

func (health *vehicleHealth) rememberImpactLocked(eventID string) bool {
	if eventID == "" {
		return false
	}
	if _, exists := health.impactSeen[eventID]; exists {
		return false
	}
	health.impactSeen[eventID] = struct{}{}
	return true
}

// ingestTelemetry returns a new state only when it should be sent to Viewer
// and Observer. Raw state frames clock recovery; impact events change HP.
func (health *vehicleHealth) ingestTelemetry(raw string, carID string, now time.Time) (vehicleHealthSnapshot, bool, *vehicleImpactEvent) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), false, nil
	}
	health.mu.Lock()
	defer health.mu.Unlock()

	changed := health.advanceRecoveryLocked(now)
	var confirmed *vehicleImpactEvent
	if candidate, ok := parseRelayImpactCandidate(raw); ok {
		impactClass := classifyRelayImpactCandidate(candidate)
		eventID := fmt.Sprintf("%s:%s:%d", carID, candidate.Boot, candidate.Sequence)
		if impactClass != "" && health.rememberImpactLocked(eventID) {
			hpBefore := health.hp
			damage := relayImpactDamage(impactClass)
			damageApplied := false
			suppressionReason := ""
			if damage > 0 && !health.raceGameplayActiveLocked() {
				damage = 0
				suppressionReason = "race_inactive"
			} else if damage > 0 && health.boostStateLocked(now) == "active" {
				damage = 0
				suppressionReason = "boost_active"
			} else if damage > 0 {
				health.lastUnsafeAt = now
				if health.damageEpisodeStartedAt.IsZero() || now.Before(health.damageEpisodeStartedAt) ||
					now.Sub(health.damageEpisodeStartedAt) >= vehicleHealthDamageEpisodeWindow {
					health.damageEpisodeStartedAt = now
					health.damageEpisodeDamage = 0
				}
				if damage > health.damageEpisodeDamage {
					// 同じ衝突の追撃候補は合計ダメージを最上位クラスまでだけ引き上げる。
					damage -= health.damageEpisodeDamage
					health.damageEpisodeDamage += damage
					health.hp = math.Max(0, health.hp-damage)
					health.lastUpdatedAt = now
					changed = true
					damageApplied = true
				} else {
					damage = 0
					suppressionReason = "impact_episode"
				}
			} else {
				suppressionReason = "below_damage_threshold"
			}
			confirmed = &vehicleImpactEvent{
				Type:              "vehicle_event",
				Version:           1,
				EventID:           eventID,
				RaceRunID:         health.activeRaceRunID,
				CarID:             carID,
				ImpactClass:       impactClass,
				MagnitudeMPS2:     candidate.Magnitude,
				JerkMPS3:          candidate.Jerk,
				Axis:              candidate.Axis,
				DamageApplied:     damageApplied,
				Damage:            damage,
				SuppressionReason: suppressionReason,
				HPBefore:          hpBefore,
				HPAfter:           health.hp,
				ServerTimeMS:      now.UnixMilli(),
			}
		}
	}

	snapshot := health.snapshotLocked(now)
	if changed || health.lastPublishedAt.IsZero() || now.Sub(health.lastPublishedAt) >= vehicleHealthPublishInterval {
		health.lastPublishedAt = now
		return snapshot, true, confirmed
	}
	return snapshot, false, confirmed
}

func (health *vehicleHealth) limitCommand(message string, now time.Time) string {
	if health == nil {
		return message
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)

	parts := strings.Split(message, ",")
	for index, part := range parts {
		trimmed := strings.TrimSpace(part)
		if !strings.HasPrefix(trimmed, "T:") {
			continue
		}
		throttle, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "T:")))
		if err != nil {
			return message
		}
		gear := health.effectiveGearLocked(now)
		requestedThrottle := normalizeForwardThrottle(throttle, gear)
		health.observeThrottleVariationLocked(requestedThrottle, now)
		health.requestedThrottle = requestedThrottle
		limited := throttle
		if throttle > 1500 {
			limited = minInt(throttle, vehicleGearForwardMaximum(gear))
			limited = 1500 + int(math.Round(float64(limited-1500)*vehicleHealthSpeedCap(health.hp)))
			if health.fuel <= 0 {
				limited = minInt(limited, vehicleFuelEmptyForwardPWM)
			}
		} else if throttle < 1500 && health.fuel <= 0 {
			// Damage may require full reverse to escape an obstacle. Empty fuel alone
			// receives a symmetric limp limit so reverse cannot bypass fuel gameplay.
			limited = maxInt(throttle, vehicleFuelEmptyReversePWM)
		}
		health.effectiveThrottle = normalizeForwardThrottle(limited, gear)
		if limited > 1500 {
			health.lastForwardAt = now
		}
		if limited == throttle {
			return message
		}
		leading := part[:len(part)-len(strings.TrimLeft(part, " \t"))]
		trailing := part[len(strings.TrimRight(part, " \t\r\n")):]
		parts[index] = leading + fmt.Sprintf("T:%d", limited) + trailing
		return strings.Join(parts, ",")
	}
	return message
}

func (health *vehicleHealth) advanceRecoveryLocked(now time.Time) bool {
	if health.lastUpdatedAt.IsZero() {
		health.lastUpdatedAt = now
		return false
	}
	elapsed := now.Sub(health.lastUpdatedAt).Seconds()
	health.lastUpdatedAt = now
	if elapsed <= 0 {
		return false
	}

	changed := false
	activelyDriving := health.isActivelyDrivingLocked(now)
	raceDriving := activelyDriving && health.raceGameplayActiveLocked()
	if health.recoveryMode.allowsDrivingRecovery() && raceDriving && health.hp < vehicleHealthMaximum &&
		(health.lastUnsafeAt.IsZero() || now.Sub(health.lastUnsafeAt) >= vehicleHealthRecoveryDelay) {
		rate := vehicleHealthRecoveryRate(health.hp)
		previous := health.hp
		health.hp = math.Min(vehicleHealthMaximum, health.hp+(rate*elapsed))
		changed = changed || health.hp != previous
	}

	if raceDriving && health.fuel > 0 && health.fuelConsumptionEnabledLocked() {
		rate := health.fuelRateLocked()
		previous := health.fuel
		health.fuel = math.Max(0, health.fuel-(rate*elapsed))
		health.fuelRatePerSec = rate
		changed = changed || health.fuel != previous
	} else {
		health.fuelRatePerSec = 0
	}

	if !health.boostActiveUntil.IsZero() {
		remaining := health.boostActiveUntil.Sub(now)
		if remaining <= 0 || health.fuel <= 0 {
			health.cancelBoostLocked()
			changed = true
		} else {
			previous := health.boost
			health.boost = vehicleBoostMaximum * remaining.Seconds() / vehicleBoostDuration.Seconds()
			changed = changed || math.Abs(previous-health.boost) >= 0.01
		}
	} else if activelyDriving && health.fuel > 0 && health.boost < vehicleBoostMaximum {
		previous := health.boost
		health.boost = math.Min(vehicleBoostMaximum, health.boost+(vehicleBoostMaximum/health.boostChargeDurationLocked().Seconds()*elapsed))
		if vehicleBoostMaximum-health.boost < 0.001 {
			health.boost = vehicleBoostMaximum
		}
		changed = changed || health.boost != previous
	}
	return changed
}

func (health *vehicleHealth) snapshotLocked(now time.Time) vehicleHealthSnapshot {
	snapshot := vehicleHealthSnapshot{
		HP:                 health.hp,
		SpeedCap:           vehicleHealthSpeedCap(health.hp),
		Mode:               vehicleHealthMode(health.hp),
		Fuel:               health.fuel,
		FuelState:          vehicleFuelState(health.fuel),
		Boost:              health.boost,
		BoostState:         health.boostStateLocked(now),
		BoostRemainingMS:   maxInt64(0, health.boostActiveUntil.Sub(now).Milliseconds()),
		Gear:               health.effectiveGearLocked(now),
		NormalGearMax:      vehicleNormalGearMaximum,
		Position:           health.position,
		FieldSize:          health.fieldSize,
		FuelRatePerSec:     health.fuelRatePerSec,
		FuelRateMultiplier: health.fuelRateMultiplierLocked(),
		ThrottleVariation:  health.throttleVariation,
		RequestedThrottle:  health.requestedThrottle,
		EffectiveThrottle:  health.effectiveThrottle,
		SessionType:        vehicleSessionTypeState(health.lastSessionType),
		RaceGapKnown:       health.raceGapKnown,
		BoostChargeMS:      health.boostChargeDurationLocked().Milliseconds(),
		ServerTimeMS:       now.UnixMilli(),
	}
	if health.raceGapKnown && health.position > 1 {
		if health.lapDeltaToAhead > 0 {
			lapDelta := health.lapDeltaToAhead
			snapshot.LapDeltaToAhead = &lapDelta
		} else {
			gapMS := health.gapToAheadMS
			snapshot.GapToAheadMS = &gapMS
		}
	}
	return snapshot
}

func defaultVehicleHealthSnapshot(now time.Time) vehicleHealthSnapshot {
	return vehicleHealthSnapshot{
		HP: vehicleHealthMaximum, SpeedCap: 1, Mode: "healthy",
		Fuel: vehicleFuelMaximum, FuelState: "normal", BoostState: "charging",
		Gear: 1, NormalGearMax: vehicleNormalGearMaximum, FuelRateMultiplier: 1, SessionType: "unknown", ServerTimeMS: now.UnixMilli(),
	}
}

func (health *vehicleHealth) setDriveEnabled(enabled bool, now time.Time) vehicleHealthSnapshot {
	if health == nil {
		return defaultVehicleHealthSnapshot(now)
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	health.driveEnabled = enabled
	if !enabled {
		health.requestedThrottle = 0
		health.effectiveThrottle = 0
		health.lastForwardAt = time.Time{}
		health.resetThrottleVariationLocked()
	}
	return health.snapshotLocked(now)
}

func (health *vehicleHealth) setPitPresent(present bool, now time.Time) vehicleHealthSnapshot {
	if health == nil {
		return defaultVehicleHealthSnapshot(now)
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	health.pitPresent = present
	if present {
		health.cancelBoostLocked()
	}
	return health.snapshotLocked(now)
}

func (health *vehicleHealth) setRequestedGear(gear int, now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), gear >= 1 && gear <= vehicleNormalGearMaximum
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	if gear < 1 || gear > vehicleNormalGearMaximum || !health.boostActiveUntil.IsZero() {
		return health.snapshotLocked(now), false
	}
	health.requestedGear = gear
	return health.snapshotLocked(now), true
}

func (health *vehicleHealth) activateBoost(now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return defaultVehicleHealthSnapshot(now), false
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	if health.requestedGear != vehicleNormalGearMaximum || health.boost < vehicleBoostMaximum || health.fuel <= 0 ||
		health.pitPresent {
		return health.snapshotLocked(now), false
	}
	health.boost = vehicleBoostMaximum
	health.boostActiveUntil = now.Add(vehicleBoostDuration)
	return health.snapshotLocked(now), true
}

func (health *vehicleHealth) effectiveGearLocked(now time.Time) int {
	if !health.boostActiveUntil.IsZero() && now.Before(health.boostActiveUntil) && health.fuel > 0 {
		return vehicleBoostGear
	}
	return maxInt(1, minInt(health.requestedGear, vehicleNormalGearMaximum))
}

func (health *vehicleHealth) cancelBoostLocked() {
	health.boost = 0
	health.boostActiveUntil = time.Time{}
}

func (health *vehicleHealth) boostStateLocked(now time.Time) string {
	if !health.boostActiveUntil.IsZero() && now.Before(health.boostActiveUntil) {
		return "active"
	}
	if health.boost >= vehicleBoostMaximum {
		return "ready"
	}
	return "charging"
}

func (health *vehicleHealth) isActivelyDrivingLocked(now time.Time) bool {
	return health.driveEnabled && !health.pitPresent &&
		!health.lastForwardAt.IsZero() && now.Sub(health.lastForwardAt) <= vehicleHealthForwardCommandGrace
}

func (health *vehicleHealth) isActivelyDriving(now time.Time) bool {
	if health == nil {
		return false
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.isActivelyDrivingLocked(now)
}

func (health *vehicleHealth) raceGameplayActiveLocked() bool {
	// Race Control state is checkpoint-driven and can be quiet for several seconds.
	// WebSocket disconnect handling owns liveness; message age must not stop gameplay.
	return health.raceConnected && health.activeRaceRunID != "" && health.lastRacePhase == "green"
}

func (health *vehicleHealth) fuelRateLocked() float64 {
	return vehicleFuelMaximum / health.fuelDriveDuration.Seconds() * health.fuelRateMultiplierLocked()
}

func (health *vehicleHealth) observeThrottleVariationLocked(throttle float64, now time.Time) {
	throttle = clampFloat64(throttle, 0, 1)
	if !health.hasThrottleSample || !now.After(health.lastThrottleSampleAt) {
		health.lastThrottleSample = throttle
		health.lastThrottleSampleAt = now
		health.hasThrottleSample = true
		return
	}
	elapsed := now.Sub(health.lastThrottleSampleAt)
	decay := math.Exp(-elapsed.Seconds() / vehicleFuelVariationTimeConstant.Seconds())
	if elapsed > vehicleFuelVariationGrace {
		health.throttleVariation *= decay
	} else {
		instantVariation := math.Abs(throttle-health.lastThrottleSample) / elapsed.Seconds()
		health.throttleVariation = health.throttleVariation*decay + instantVariation*(1-decay)
	}
	health.lastThrottleSample = throttle
	health.lastThrottleSampleAt = now
}

func (health *vehicleHealth) fuelRateMultiplierLocked() float64 {
	excess := math.Max(0, health.throttleVariation-vehicleFuelVariationDeadzone)
	penaltyRange := vehicleFuelVariationFullPenalty - vehicleFuelVariationDeadzone
	if penaltyRange <= 0 {
		return 1
	}
	fraction := math.Min(1, excess/penaltyRange)
	return 1 + fraction*(vehicleFuelRoughMaxMultiplier-1)
}

func (health *vehicleHealth) resetThrottleVariationLocked() {
	health.throttleVariation = 0
	health.lastThrottleSample = 0
	health.lastThrottleSampleAt = time.Time{}
	health.hasThrottleSample = false
}

func (health *vehicleHealth) fuelConsumptionEnabledLocked() bool {
	return health.lastSessionType != "practice"
}

func normalizeRaceSessionType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "practice":
		return "practice"
	case "qualify":
		return "qualify"
	case "race":
		return "race"
	default:
		return ""
	}
}

func vehicleSessionTypeState(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (health *vehicleHealth) boostChargeDurationLocked() time.Duration {
	if !health.raceGameplayActiveLocked() {
		return vehicleBoostFallbackCharge
	}
	if health.position == 1 {
		return vehicleBoostLeaderCharge
	}
	if !health.raceGapKnown || health.position < 1 {
		return vehicleBoostFallbackCharge
	}
	if health.lapDeltaToAhead > 0 {
		duration := vehicleBoostOneLapDownCharge - time.Duration(health.lapDeltaToAhead-1)*2*time.Second
		if duration < vehicleBoostMinimumCharge {
			return vehicleBoostMinimumCharge
		}
		return duration
	}
	gap := time.Duration(health.gapToAheadMS) * time.Millisecond
	if gap >= vehicleBoostFullGapBenefit {
		return vehicleBoostFarGapCharge
	}
	fraction := float64(gap) / float64(vehicleBoostFullGapBenefit)
	return time.Duration(float64(vehicleBoostCloseGapCharge) - fraction*float64(vehicleBoostCloseGapCharge-vehicleBoostFarGapCharge))
}

func vehicleGearForwardMaximum(gear int) int {
	switch gear {
	case 1:
		return vehicleGearOneForwardMaximum
	case 2:
		return 1700
	case 3:
		return 1800
	case 4:
		return 1900
	default:
		return vehicleGearOneForwardMaximum
	}
}

func vehicleGearBrakeMinimum(gear int) int {
	switch gear {
	case 1, 2:
		return 1300
	case 3:
		return 1200
	case 4:
		return 1100
	default:
		return 1000
	}
}

func normalizeForwardThrottle(pwm int, gear int) float64 {
	maximum := vehicleGearForwardMaximum(gear)
	return math.Max(0, math.Min(1, float64(pwm-1500)/float64(maximum-1500)))
}

func clampFloat64(value float64, minimum float64, maximum float64) float64 {
	return math.Max(minimum, math.Min(maximum, value))
}

func vehicleFuelState(fuel float64) string {
	if fuel <= 0 {
		return "empty"
	}
	if fuel <= 20 {
		return "low"
	}
	return "normal"
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

func maxInt64(left int64, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func vehicleHealthSpeedCap(hp float64) float64 {
	hp = math.Max(0, math.Min(vehicleHealthMaximum, hp))
	switch {
	case hp >= 70:
		return 0.90 + ((hp - 70) / 30 * 0.10)
	case hp >= 35:
		return 0.60 + ((hp - 35) / 35 * 0.30)
	default:
		return 0.35 + (hp / 35 * 0.25)
	}
}

func vehicleHealthRecoveryRate(hp float64) float64 {
	switch {
	case hp < 40:
		return 1.5
	case hp < 70:
		return 0.8
	case hp < vehicleHealthMaximum:
		return 0.25
	default:
		return 0
	}
}

func vehicleHealthMode(hp float64) string {
	switch {
	case hp <= 0:
		return "limp"
	case hp < 35:
		return "critical"
	case hp < 70:
		return "damaged"
	default:
		return "healthy"
	}
}

func formatVehicleHealthTelemetry(snapshot vehicleHealthSnapshot) string {
	return fmt.Sprintf("VHS:1,%.1f,%.3f,%s", snapshot.HP, snapshot.SpeedCap, snapshot.Mode)
}

func formatVehicleGameplayTelemetry(snapshot vehicleHealthSnapshot) string {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	return "VGS:1," + string(payload)
}

func relayImpactDamage(impactClass string) float64 {
	switch impactClass {
	case "strong":
		return vehicleHealthStrongDamage
	case "severe":
		return vehicleHealthSevereDamage
	default:
		return 0
	}
}

type relayImpactCandidate struct {
	Boot      string
	Sequence  uint64
	Magnitude float64
	Jerk      float64
	Axis      [3]float64
}

func parseRelayImpactCandidate(raw string) (relayImpactCandidate, bool) {
	if !strings.HasPrefix(raw, "TEL:") {
		return relayImpactCandidate{}, false
	}
	var payload struct {
		Version  int    `json:"v"`
		Kind     string `json:"k"`
		Boot     string `json:"boot"`
		Sequence uint64 `json:"seq"`
		Event    struct {
			Name      string    `json:"n"`
			Magnitude float64   `json:"m"`
			Jerk      float64   `json:"j"`
			Axis      []float64 `json:"a"`
		} `json:"e"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "TEL:")), &payload); err != nil {
		return relayImpactCandidate{}, false
	}
	if payload.Version != 2 || payload.Kind != "e" || payload.Event.Name != "impact_candidate" ||
		strings.TrimSpace(payload.Boot) == "" || len(payload.Event.Axis) != 3 ||
		math.IsNaN(payload.Event.Magnitude) || math.IsInf(payload.Event.Magnitude, 0) ||
		math.IsNaN(payload.Event.Jerk) || math.IsInf(payload.Event.Jerk, 0) {
		return relayImpactCandidate{}, false
	}
	candidate := relayImpactCandidate{
		Boot:      payload.Boot,
		Sequence:  payload.Sequence,
		Magnitude: payload.Event.Magnitude,
		Jerk:      payload.Event.Jerk,
		Axis:      [3]float64{payload.Event.Axis[0], payload.Event.Axis[1], payload.Event.Axis[2]},
	}
	for _, value := range candidate.Axis {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return relayImpactCandidate{}, false
		}
	}
	return candidate, true
}

func classifyRelayImpactCandidate(candidate relayImpactCandidate) string {
	if candidate.Magnitude < relayImpactWeakMagnitudeMPS2 {
		return ""
	}
	if candidate.Magnitude >= relayImpactSevereMagnitudeMPS2 && candidate.Jerk >= relayImpactSevereJerkMPS3 {
		return "severe"
	}
	if candidate.Magnitude >= relayImpactStrongMagnitudeMPS2 && candidate.Jerk >= relayImpactStrongJerkMPS3 {
		return "strong"
	}
	return "weak"
}

func classifyRelayImpact(raw string) string {
	candidate, ok := parseRelayImpactCandidate(raw)
	if !ok {
		return ""
	}
	return classifyRelayImpactCandidate(candidate)
}

func isLegacyImpactEvent(raw string) bool {
	if !strings.HasPrefix(raw, "TEL:") || !strings.Contains(raw, `"evt"`) {
		return false
	}
	var payload struct {
		Version int    `json:"v"`
		Kind    string `json:"k"`
		Event   struct {
			Name string `json:"name"`
		} `json:"evt"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "TEL:")), &payload); err != nil {
		return false
	}
	return payload.Version == 1 && payload.Kind == "e" && payload.Event.Name == "impact"
}
