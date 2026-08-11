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
	vehicleHealthSevereDamage        = 28.0
	vehicleHealthDamageCooldown      = 600 * time.Millisecond
	vehicleHealthRecoveryDelay       = 4 * time.Second
	vehicleHealthForwardCommandGrace = 350 * time.Millisecond
	vehicleHealthPublishInterval     = 100 * time.Millisecond
)

type vehicleHealthSnapshot struct {
	HP       float64
	SpeedCap float64
	Mode     string
}

type pitRecoveryApplyResult struct {
	Status          string
	RecoveredAmount float64
	Snapshot        vehicleHealthSnapshot
}

type pitRecoveryApplyError struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
}

type pitRecoveryReceipt struct {
	Fingerprint     string
	RecoveredAmount float64
	Snapshot        vehicleHealthSnapshot
}

// vehicleHealth は Relay ごとの車体状態である。操縦上限を Viewer に預けると
// URL を直接開いた別の Pilot が制限を回避できるため、Relay 境界で適用する。
type vehicleHealth struct {
	mu sync.Mutex

	hp              float64
	lastUpdatedAt   time.Time
	lastUnsafeAt    time.Time
	lastDamageAt    time.Time
	lastForwardAt   time.Time
	lastPublishedAt time.Time
	lastRacePhase   string
	recoveryMode    vehicleHealthRecoveryMode
	activeRaceRunID string
	pitEntryID      string
	lastPitTick     int
	lastPitAt       time.Time
	pitReceipts     map[string]pitRecoveryReceipt
	pitSeenEntries  map[string]struct{}
	impactSeen      map[string]struct{}
}

func newVehicleHealth(now time.Time) *vehicleHealth {
	return &vehicleHealth{
		hp:             vehicleHealthMaximum,
		lastUpdatedAt:  now,
		recoveryMode:   vehicleHealthRecoveryDefault,
		pitReceipts:    make(map[string]pitRecoveryReceipt),
		pitSeenEntries: make(map[string]struct{}),
		impactSeen:     make(map[string]struct{}),
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
	health.lastUpdatedAt = now
	health.resetPitRecoveryLocked()
	health.resetImpactDedupeLocked()
}

func (health *vehicleHealth) snapshot(now time.Time) vehicleHealthSnapshot {
	if health == nil {
		return vehicleHealthSnapshot{HP: vehicleHealthMaximum, SpeedCap: 1, Mode: "healthy"}
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	health.advanceRecoveryLocked(now)
	return health.snapshotLocked()
}

func (health *vehicleHealth) observeRacePhase(phase string, now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return vehicleHealthSnapshot{HP: vehicleHealthMaximum, SpeedCap: 1, Mode: "healthy"}, false
	}
	phase = strings.ToLower(strings.TrimSpace(phase))
	health.mu.Lock()
	defer health.mu.Unlock()

	if phase == health.lastRacePhase {
		return health.snapshotLocked(), false
	}
	health.lastRacePhase = phase
	if phase != "ready" {
		return health.snapshotLocked(), false
	}

	health.hp = vehicleHealthMaximum
	health.lastUpdatedAt = now
	health.lastUnsafeAt = time.Time{}
	health.lastDamageAt = time.Time{}
	health.lastForwardAt = time.Time{}
	health.lastPublishedAt = now
	health.resetPitRecoveryLocked()
	health.resetImpactDedupeLocked()
	return health.snapshotLocked(), true
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
			Status:          "duplicate",
			RecoveredAmount: receipt.RecoveredAmount,
			Snapshot:        receipt.Snapshot,
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
				Message:    "the previous recovery tick was accepted less than 2 seconds ago",
				RetryAfter: pitRecoveryMinimumInterval - elapsed,
			}
		}
	}

	previousHP := health.hp
	health.hp = math.Min(vehicleHealthMaximum, health.hp+pitRecoveryAmount)
	recoveredAmount := health.hp - previousHP
	health.lastUpdatedAt = now
	health.lastPublishedAt = now
	health.pitEntryID = command.EntryID
	health.pitSeenEntries[command.EntryID] = struct{}{}
	health.lastPitTick = command.Tick
	health.lastPitAt = now
	snapshot := health.snapshotLocked()
	health.recordPitReceiptLocked(command.CommandID, pitRecoveryReceipt{
		Fingerprint:     fingerprint,
		RecoveredAmount: recoveredAmount,
		Snapshot:        snapshot,
	})
	return pitRecoveryApplyResult{
		Status:          "applied",
		RecoveredAmount: recoveredAmount,
		Snapshot:        snapshot,
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
		return vehicleHealthSnapshot{HP: vehicleHealthMaximum, SpeedCap: 1, Mode: "healthy"}, false, nil
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
			health.lastUnsafeAt = now
			damage := relayImpactDamage(impactClass)
			damageApplied := false
			suppressionReason := ""
			if damage > 0 && (health.lastDamageAt.IsZero() || now.Sub(health.lastDamageAt) >= vehicleHealthDamageCooldown) {
				health.hp = math.Max(0, health.hp-damage)
				health.lastDamageAt = now
				health.lastUpdatedAt = now
				changed = true
				damageApplied = true
			} else if damage > 0 {
				damage = 0
				suppressionReason = "cooldown"
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

	snapshot := health.snapshotLocked()
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
		limited := throttle
		if throttle > 1500 {
			limited = 1500 + int(math.Round(float64(throttle-1500)*vehicleHealthSpeedCap(health.hp)))
		}
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
	if !health.recoveryMode.allowsDrivingRecovery() || elapsed <= 0 || health.hp >= vehicleHealthMaximum ||
		(!health.lastUnsafeAt.IsZero() && now.Sub(health.lastUnsafeAt) < vehicleHealthRecoveryDelay) ||
		health.lastForwardAt.IsZero() || now.Sub(health.lastForwardAt) > vehicleHealthForwardCommandGrace {
		return false
	}

	rate := vehicleHealthRecoveryRate(health.hp)
	if rate <= 0 {
		return false
	}
	previous := health.hp
	health.hp = math.Min(vehicleHealthMaximum, health.hp+(rate*elapsed))
	return health.hp != previous
}

func (health *vehicleHealth) snapshotLocked() vehicleHealthSnapshot {
	return vehicleHealthSnapshot{
		HP:       health.hp,
		SpeedCap: vehicleHealthSpeedCap(health.hp),
		Mode:     vehicleHealthMode(health.hp),
	}
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
	if candidate.Magnitude < 10 {
		return ""
	}
	if candidate.Magnitude >= 18 && candidate.Jerk >= 250 {
		return "severe"
	}
	if candidate.Magnitude >= 12 && candidate.Jerk >= 250 {
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
