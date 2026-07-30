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
	vehicleHealthDamageCooldown      = 800 * time.Millisecond
	vehicleHealthRecoveryDelay       = 4 * time.Second
	vehicleHealthForwardCommandGrace = 350 * time.Millisecond
	vehicleHealthPublishInterval     = 100 * time.Millisecond
)

type vehicleHealthSnapshot struct {
	HP       float64
	SpeedCap float64
	Mode     string
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
}

func newVehicleHealth(now time.Time) *vehicleHealth {
	return &vehicleHealth{
		hp:            vehicleHealthMaximum,
		lastUpdatedAt: now,
	}
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
	return health.snapshotLocked(), true
}

// ingestTelemetry returns a new state only when it should be sent to Viewer
// and Observer. Raw state frames clock recovery; impact events change HP.
func (health *vehicleHealth) ingestTelemetry(raw string, now time.Time) (vehicleHealthSnapshot, bool) {
	if health == nil {
		return vehicleHealthSnapshot{HP: vehicleHealthMaximum, SpeedCap: 1, Mode: "healthy"}, false
	}
	health.mu.Lock()
	defer health.mu.Unlock()

	changed := health.advanceRecoveryLocked(now)
	if impactClass := classifyRelayImpact(raw); impactClass != "" {
		health.lastUnsafeAt = now
		damage := relayImpactDamage(impactClass)
		if damage > 0 && (health.lastDamageAt.IsZero() || now.Sub(health.lastDamageAt) >= vehicleHealthDamageCooldown) {
			health.hp = math.Max(0, health.hp-damage)
			health.lastDamageAt = now
			health.lastUpdatedAt = now
			changed = true
		}
	}

	snapshot := health.snapshotLocked()
	if changed || health.lastPublishedAt.IsZero() || now.Sub(health.lastPublishedAt) >= vehicleHealthPublishInterval {
		health.lastPublishedAt = now
		return snapshot, true
	}
	return snapshot, false
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
	if elapsed <= 0 || health.hp >= vehicleHealthMaximum ||
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
		return 0.85 + ((hp - 70) / 30 * 0.15)
	case hp >= 35:
		return 0.55 + ((hp - 35) / 35 * 0.30)
	default:
		return 0.35 + (hp / 35 * 0.20)
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

func classifyRelayImpact(raw string) string {
	if !strings.HasPrefix(raw, "TEL:") {
		return ""
	}
	var payload struct {
		Version int    `json:"v"`
		Kind    string `json:"k"`
		Event   struct {
			Name      string  `json:"n"`
			Magnitude float64 `json:"m"`
			Jerk      float64 `json:"j"`
		} `json:"e"`
		LegacyEvent struct {
			Name string `json:"name"`
			Data struct {
				Magnitude float64 `json:"mag_mps2"`
				Jerk      float64 `json:"jerk_mps3"`
			} `json:"data"`
		} `json:"evt"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "TEL:")), &payload); err != nil {
		return ""
	}

	name := payload.Event.Name
	magnitude := payload.Event.Magnitude
	jerk := payload.Event.Jerk
	if payload.Version != 2 {
		name = payload.LegacyEvent.Name
		magnitude = payload.LegacyEvent.Data.Magnitude
		jerk = payload.LegacyEvent.Data.Jerk
	}
	if payload.Kind != "e" || (name != "impact" && name != "impact_candidate") || magnitude < 10 {
		return ""
	}
	if magnitude >= 18 && jerk >= 250 {
		return "severe"
	}
	if magnitude >= 12 {
		return "strong"
	}
	return "weak"
}
