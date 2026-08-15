package main

import (
	"encoding/json"
	"fmt"
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
	if got := health.limitCommand("S:1500,T:1000", base.Add(10*time.Second)); got != "S:1500,T:1410" {
		t.Fatalf("empty-fuel reverse command = %q, want symmetric limp PWM 1410", got)
	}
	if got := health.limitCommand("S:1500,T:1500", base.Add(10*time.Second)); got != "S:1500,T:1500" {
		t.Fatalf("empty-fuel neutral command changed: %q", got)
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

func TestVehicleGameplayRoughThrottleConsumesMoreFuelThanSteadyThrottle(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	steady := newVehicleHealthWithFuelDuration(base, 30*time.Second)
	rough := newVehicleHealthWithFuelDuration(base, 30*time.Second)
	for _, health := range []*vehicleHealth{steady, rough} {
		health.observeRaceState(true, "rr_fuel_style", "green", 2, 4, base, "race")
		health.setDriveEnabled(true, base)
		health.setRequestedGear(3, base)
		health.limitCommand("S:1500,T:1800", base)
	}

	for tick := 1; tick <= 200; tick++ {
		now := base.Add(time.Duration(tick) * 50 * time.Millisecond)
		steady.limitCommand("S:1500,T:1800", now)
		roughPWM := 1800
		if (tick/4)%2 == 0 {
			roughPWM = 1500
		}
		rough.limitCommand(fmt.Sprintf("S:1500,T:%d", roughPWM), now)
	}

	steadySnapshot := steady.snapshot(base.Add(10 * time.Second))
	roughSnapshot := rough.snapshot(base.Add(10 * time.Second))
	if math.Abs(steadySnapshot.Fuel-(200.0/3.0)) > 0.001 || steadySnapshot.FuelRateMultiplier != 1 {
		t.Fatalf("steady fuel snapshot = %#v", steadySnapshot)
	}
	if roughSnapshot.Fuel >= steadySnapshot.Fuel-5 || roughSnapshot.FuelRateMultiplier < 1.5 || roughSnapshot.ThrottleVariation <= vehicleFuelVariationFullPenalty {
		t.Fatalf("rough fuel snapshot = %#v, steady = %#v", roughSnapshot, steadySnapshot)
	}
}

func TestVehicleGameplayBoostUsesRaceGapAndExpiresToGearThree(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := prepareGameplayHealth(base, 5*time.Minute, 4, 4)
	health.observeRaceStateWithGap(true, "rr_gameplay", "green", 4, 4, vehicleRaceGap{Known: true, LapDeltaToAhead: 1}, base)
	for tick := 1; tick <= 16*10; tick++ {
		now := base.Add(time.Duration(tick) * 100 * time.Millisecond)
		if tick%10 == 0 {
			health.observeRaceStateWithGap(true, "rr_gameplay", "green", 4, 4, vehicleRaceGap{Known: true, LapDeltaToAhead: 1}, now)
		}
		health.limitCommand("S:1500,T:2000", now)
	}

	ready := health.snapshot(base.Add(16 * time.Second))
	if ready.BoostState != "ready" || math.Abs(ready.Boost-vehicleBoostMaximum) > 0.001 || ready.Gear != 3 {
		t.Fatalf("last-place boost state = %#v", ready)
	}
	active, ok := health.activateBoost(base.Add(16 * time.Second))
	if !ok || active.Gear != 4 || active.BoostState != "active" || active.BoostRemainingMS != vehicleBoostDuration.Milliseconds() {
		t.Fatalf("boost activation = %#v accepted=%t", active, ok)
	}
	if _, accepted := health.setRequestedGear(2, base.Add(17*time.Second)); accepted {
		t.Fatal("gear shift was accepted while boost was active")
	}
	if got := health.limitCommand("S:1500,T:2000", base.Add(17*time.Second)); got != "S:1500,T:1900" {
		t.Fatalf("boost command = %q, want G4 PWM 1900", got)
	}
	expired := health.snapshot(base.Add(18500 * time.Millisecond))
	if expired.Gear != 3 || expired.BoostState != "charging" || expired.Boost != 0 {
		t.Fatalf("expired boost state = %#v", expired)
	}
}

func TestVehicleGameplayBoostChargeDurationsByRaceGap(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	tests := []struct {
		name     string
		position int
		gap      vehicleRaceGap
		want     time.Duration
	}{
		{name: "leader", position: 1, gap: vehicleRaceGap{Known: true}, want: 45 * time.Second},
		{name: "close", position: 2, gap: vehicleRaceGap{Known: true, IntervalToAheadMS: 0}, want: 40 * time.Second},
		{name: "four_seconds", position: 3, gap: vehicleRaceGap{Known: true, IntervalToAheadMS: 4000}, want: 30 * time.Second},
		{name: "far", position: 4, gap: vehicleRaceGap{Known: true, IntervalToAheadMS: 8000}, want: 20 * time.Second},
		{name: "one_lap", position: 4, gap: vehicleRaceGap{Known: true, LapDeltaToAhead: 1}, want: 16 * time.Second},
		{name: "three_laps", position: 4, gap: vehicleRaceGap{Known: true, LapDeltaToAhead: 3}, want: 12 * time.Second},
		{name: "unknown", position: 3, gap: vehicleRaceGap{}, want: 30 * time.Second},
	}
	for _, test := range tests {
		health.observeRaceStateWithGap(true, "rr_gap", "green", test.position, 4, test.gap, base)
		health.mu.Lock()
		got := health.boostChargeDurationLocked()
		health.mu.Unlock()
		if got != test.want {
			t.Errorf("%s charge duration = %s, want %s", test.name, got, test.want)
		}
	}
}

func TestVehicleGameplayStandaloneBoostUsesFallbackAndKeepsFuel(t *testing.T) {
	base := time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC)
	health := newVehicleHealthWithFuelDuration(base, 10*time.Second)
	health.observeRaceState(true, "rr_finished", "finished", 1, 4, base, "race")
	health.setDriveEnabled(true, base)
	health.setRequestedGear(vehicleNormalGearMaximum, base)
	health.limitCommand("S:1500,T:1800", base)

	for tick := 1; tick <= 30*10; tick++ {
		now := base.Add(time.Duration(tick) * 100 * time.Millisecond)
		health.limitCommand("S:1500,T:1800", now)
	}

	ready := health.snapshot(base.Add(30 * time.Second))
	if ready.BoostState != "ready" || ready.Boost != vehicleBoostMaximum || ready.BoostChargeMS != vehicleBoostFallbackCharge.Milliseconds() {
		t.Fatalf("standalone boost = %#v", ready)
	}
	if ready.Fuel != vehicleFuelMaximum || ready.FuelRatePerSec != 0 {
		t.Fatalf("standalone fuel changed = %#v", ready)
	}
	active, accepted := health.activateBoost(base.Add(30 * time.Second))
	if !accepted || active.Gear != vehicleBoostGear || active.BoostState != "active" {
		t.Fatalf("standalone boost activation = %#v accepted=%t", active, accepted)
	}
	expired := health.snapshot(base.Add(30*time.Second + vehicleBoostDuration))
	if expired.Gear != vehicleNormalGearMaximum || expired.BoostState != "charging" || expired.Boost != 0 {
		t.Fatalf("standalone boost expiration = %#v", expired)
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
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base.Add(20*time.Second))
	health.setPitPresent(true, base.Add(20*time.Second))

	command := pitRecoveryCommand{CommandID: "cmd-1", RaceRunID: "rr_gameplay", CarID: "CP-1", EntryID: "entry-1", Tick: 1}
	result, applyErr := health.applyPitRecovery(command, base.Add(21*time.Second))
	if applyErr != nil {
		t.Fatalf("pit recovery failed: %#v", applyErr)
	}
	if result.RecoveredAmount != 10 || result.FuelRecoveredAmount != 10 || result.Snapshot.HP != 90 || math.Abs(result.Snapshot.Fuel-70) > 0.001 {
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
	envelope.Standings = make([]raceStateStanding, 2)
	envelope.Standings[0].CarID = "CP-2"
	envelope.Standings[0].Position = 1
	envelope.Standings[0].Status = "racing"
	envelope.Standings[1].CarID = "CP-1"
	envelope.Standings[1].Position = 2
	envelope.Standings[1].Status = "racing"
	gapMS := int64(4000)
	envelope.Standings[1].IntervalToAheadMS = &gapMS
	server.observeRaceContext(envelope, base)
	snapshot := health.snapshot(base)
	if snapshot.Position != 2 || snapshot.FieldSize != 2 || !snapshot.RaceGapKnown || snapshot.GapToAheadMS == nil || *snapshot.GapToAheadMS != gapMS || snapshot.BoostChargeMS != (30*time.Second).Milliseconds() {
		t.Fatalf("race rank snapshot = %#v", snapshot)
	}
}
