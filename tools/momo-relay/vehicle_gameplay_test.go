package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func prepareGameplayHealth(base time.Time, fuelDuration time.Duration, position int, fieldSize int) *vehicleHealth {
	health := newVehicleHealthWithFuelDuration(base, fuelDuration)
	health.observeRaceState(true, "rr_gameplay", "green", position, fieldSize, base)
	health.setDriveEnabled(true, base)
	health.setRequestedGear(vehicleNormalGearMaximum, base)
	health.limitCommand("S:1500,T:2000", base)
	return health
}

func advanceGameplayDriving(health *vehicleHealth, base time.Time, seconds int, position int, fieldSize int) {
	for tick := 1; tick <= seconds*10; tick++ {
		now := base.Add(time.Duration(tick) * 100 * time.Millisecond)
		if tick%10 == 0 {
			health.observeRaceState(true, "rr_gameplay", "green", position, fieldSize, now)
		}
		health.limitCommand("S:1500,T:2000", now)
	}
}

func advanceGameplayDrivingInSession(health *vehicleHealth, base time.Time, seconds int, sessionType string) {
	for tick := 1; tick <= seconds*10; tick++ {
		now := base.Add(time.Duration(tick) * 100 * time.Millisecond)
		if tick%10 == 0 {
			health.observeRaceState(true, "rr_"+sessionType, "green", 1, 4, now, sessionType)
		}
		health.limitCommand("S:1500,T:1800", now)
	}
}

func TestVehicleGameplayFuelDrainAndEmptyLimit(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, 10*time.Second, 1, 4)
	advanceGameplayDriving(health, base, 10, 1, 4)

	snapshot := health.snapshot(base.Add(10 * time.Second))
	if math.Abs(snapshot.Fuel) > 0.001 || snapshot.FuelState != "empty" {
		t.Fatalf("fuel after full drive duration = %#v", snapshot)
	}
	if got := health.limitCommand("S:1500,T:2000", base.Add(10*time.Second)); got != "S:1500,T:1590" {
		t.Fatalf("empty-fuel command = %q, want limp PWM 1590", got)
	}
}

func TestVehicleGameplayPracticeDoesNotConsumeFuel(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealthWithFuelDuration(base, 10*time.Second)
	health.observeRaceState(true, "rr_practice", "green", 1, 4, base, "practice")
	health.setDriveEnabled(true, base)
	health.limitCommand("S:1500,T:1800", base)

	advanceGameplayDrivingInSession(health, base, 12, "practice")

	snapshot := health.snapshot(base.Add(12 * time.Second))
	if snapshot.Fuel != vehicleFuelMaximum || snapshot.FuelRatePerSec != 0 {
		t.Fatalf("practice fuel = %#v, want full tank with zero consumption", snapshot)
	}
	if snapshot.SessionType != "practice" {
		t.Fatalf("practice session type = %q", snapshot.SessionType)
	}
	if snapshot.Boost <= 0 {
		t.Fatalf("practice boost = %.2f, want boost charging to remain enabled", snapshot.Boost)
	}
}

func TestVehicleGameplayQualifyConsumesFuel(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealthWithFuelDuration(base, 10*time.Second)
	health.observeRaceState(true, "rr_qualify", "green", 1, 4, base, "qualify")
	health.setDriveEnabled(true, base)
	health.limitCommand("S:1500,T:1800", base)

	advanceGameplayDrivingInSession(health, base, 5, "qualify")
	now := base.Add(5 * time.Second)
	snapshot := health.snapshot(now)
	if math.Abs(snapshot.Fuel-50) > 0.001 {
		t.Fatalf("qualify fuel = %#v, want 50", snapshot)
	}
}

func TestVehicleGameplayBoostUsesRankAndExpiresToGearThree(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, 5*time.Minute, 4, 4)
	advanceGameplayDriving(health, base, 22, 4, 4)

	ready := health.snapshot(base.Add(22 * time.Second))
	if ready.BoostState != "ready" || math.Abs(ready.Boost-vehicleBoostMaximum) > 0.001 || ready.Gear != 3 {
		t.Fatalf("last-place boost state = %#v", ready)
	}
	active, ok := health.activateBoost(base.Add(22 * time.Second))
	if !ok || active.Gear != 4 || active.BoostState != "active" || active.BoostRemainingMS != vehicleBoostDuration.Milliseconds() {
		t.Fatalf("boost activation = %#v accepted=%t", active, ok)
	}
	if _, accepted := health.setRequestedGear(2, base.Add(23*time.Second)); accepted {
		t.Fatal("gear shift was accepted while boost was active")
	}
	if got := health.limitCommand("S:1500,T:2000", base.Add(23*time.Second)); got != "S:1500,T:1900" {
		t.Fatalf("boost command = %q, want G4 PWM 1900", got)
	}
	expired := health.snapshot(base.Add(24500 * time.Millisecond))
	if expired.Gear != 3 || expired.BoostState != "charging" || expired.Boost != 0 {
		t.Fatalf("expired boost state = %#v", expired)
	}
}

func TestVehicleGameplayBoostChargeDurationsByRank(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	for position, want := range map[int]time.Duration{
		1: 40 * time.Second,
		2: 34 * time.Second,
		3: 28 * time.Second,
		4: 22 * time.Second,
	} {
		health.mu.Lock()
		health.position = position
		health.fieldSize = 4
		got := health.boostChargeDurationLocked()
		health.mu.Unlock()
		if got != want {
			t.Errorf("position %d charge duration = %s, want %s", position, got, want)
		}
	}
}

func TestVehicleGameplayPausesFuelAndBoostInPit(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, 10*time.Second, 4, 4)
	health.setPitPresent(true, base)
	advanceGameplayDriving(health, base, 5, 4, 4)
	snapshot := health.snapshot(base.Add(5 * time.Second))
	if snapshot.Fuel != 100 || snapshot.Boost != 0 {
		t.Fatalf("PIT advanced gameplay resources = %#v", snapshot)
	}
}

func TestVehicleGameplayRejectsDirectGearFour(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, time.Minute, 1, 4)
	if snapshot, accepted := health.setRequestedGear(4, base); accepted || snapshot.Gear != 3 {
		t.Fatalf("direct G4 request accepted=%t snapshot=%#v", accepted, snapshot)
	}
}

func TestVehicleGameplayPitTickRecoversFuelAndHealthAtomically(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, 50*time.Second, 2, 4)
	advanceGameplayDriving(health, base, 20, 2, 4)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":300}}`, "CP-1", base.Add(20*time.Second))
	health.setPitPresent(true, base.Add(20*time.Second))

	command := pitRecoveryCommand{CommandID: "cmd-1", RaceRunID: "rr_gameplay", CarID: "CP-1", EntryID: "entry-1", Tick: 1}
	result, applyErr := health.applyPitRecovery(command, base.Add(22*time.Second))
	if applyErr != nil {
		t.Fatalf("pit recovery failed: %#v", applyErr)
	}
	if result.RecoveredAmount != 20 || result.FuelRecoveredAmount != 20 || result.Snapshot.HP != 100 || math.Abs(result.Snapshot.Fuel-80) > 0.001 {
		t.Fatalf("pit recovery result = %#v", result)
	}
	duplicate, applyErr := health.applyPitRecovery(command, base.Add(24*time.Second))
	if applyErr != nil || duplicate.Status != "duplicate" || duplicate.Snapshot != result.Snapshot {
		t.Fatalf("duplicate pit recovery = %#v error=%#v", duplicate, applyErr)
	}
}

func TestVehicleGameplayTelemetryIsVersionedJSON(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	message := formatVehicleGameplayTelemetry(defaultVehicleHealthSnapshot(now))
	if !strings.HasPrefix(message, "VGS:1,") {
		t.Fatalf("gameplay telemetry = %q", message)
	}
	var snapshot vehicleHealthSnapshot
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "VGS:1,")), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.HP != 100 || snapshot.Fuel != 100 || snapshot.NormalGearMax != 3 || snapshot.Gear != 1 {
		t.Fatalf("gameplay telemetry snapshot = %#v", snapshot)
	}
}

func TestRelayRaceContextUpdatesGameplayRank(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	server := &relayServer{sources: map[string]*relay{
		"11.5": {raceCarID: "CP-1", vehicleHealth: health, viewers: make(map[uint64]*viewer)},
	}}
	envelope := raceStateEnvelope{RaceRunID: "rr_gameplay", Phase: "green"}
	envelope.Standings = append(envelope.Standings,
		struct {
			CarID    string `json:"carId"`
			Position int    `json:"position"`
			Status   string `json:"status"`
		}{CarID: "CP-2", Position: 1, Status: "racing"},
		struct {
			CarID    string `json:"carId"`
			Position int    `json:"position"`
			Status   string `json:"status"`
		}{CarID: "CP-1", Position: 2, Status: "racing"},
	)
	server.observeRaceContext(envelope, base)
	snapshot := health.snapshot(base)
	if snapshot.Position != 2 || snapshot.FieldSize != 2 {
		t.Fatalf("race rank snapshot = %#v", snapshot)
	}
}
