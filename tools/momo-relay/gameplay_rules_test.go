package main

import (
	"testing"
	"time"
)

func TestRunRulesOverrideDefaultsWithoutRestart(t *testing.T) {
	now := time.Now()
	health := prepareGameplayHealth(now, 10*time.Second, 1, 1)
	off := &raceGameplayRules{Version: 1}
	snapshot, changed := health.observeGameplayRules("rr_gameplay", off, now)
	if !changed || snapshot.DamageEnabled || snapshot.FuelConsumptionEnabled || snapshot.PitEnabled {
		t.Fatalf("off: %+v", snapshot)
	}
	advanceGameplayDriving(health, now, 10, 1, 1)
	if health.snapshot(now.Add(10*time.Second)).Fuel != 100 {
		t.Fatal("fuel drained while disabled")
	}
	if _, err := health.applyPitRecovery(pitRecoveryCommand{RaceRunID: "rr_gameplay", CommandID: "off", EntryID: "entry", Tick: 1}, now.Add(10*time.Second)); err == nil {
		t.Fatal("PIT accepted while disabled")
	}
	health.setDamageEnabled(false)
	health.setFuelConsumptionEnabled(false)
	health.setRecoveryMode(vehicleHealthRecoveryDisabled)
	now = now.Add(11 * time.Second)
	health.observeRaceState(true, "rr_next", "green", 1, 1, now, "practice")
	on := &raceGameplayRules{Version: 1, DamageEnabled: true, FuelEnabled: true, PitEnabled: true}
	snapshot, changed = health.observeGameplayRules("rr_next", on, now)
	if !changed || !snapshot.DamageEnabled || !snapshot.FuelConsumptionEnabled || !snapshot.PitEnabled {
		t.Fatalf("on: %+v", snapshot)
	}
	health.mu.Lock()
	health.hp = 80
	health.fuel = 50
	health.mu.Unlock()
	snapshot, changed = health.observeGameplayRules("rr_next", on, now)
	if changed || snapshot.HP != 80 || snapshot.Fuel != 50 {
		t.Fatal("repeated snapshot reset gameplay")
	}
	snapshot, changed = health.observeGameplayRules("rr_next", off, now)
	if changed || !snapshot.DamageEnabled {
		t.Fatal("rules changed in the same run")
	}
	health.setPitPresent(true, now)
	result, err := health.applyPitRecovery(pitRecoveryCommand{RaceRunID: "rr_next", CommandID: "on", EntryID: "entry", Tick: 1}, now.Add(time.Second))
	if err != nil || result.Snapshot.HP != 90 || result.Snapshot.Fuel != 60 {
		t.Fatalf("PIT %+v %v", result, err)
	}
	health.observeRaceState(true, "rr_legacy", "ready", 1, 1, now.Add(2*time.Second))
	snapshot = health.snapshot(now.Add(2 * time.Second))
	if snapshot.GameplayRules != nil || snapshot.DamageEnabled || snapshot.PitEnabled || snapshot.FuelConsumptionEnabled {
		t.Fatal("legacy defaults not restored")
	}
}

func TestRulesDamageOffPreservesImpactEventForFeedback(t *testing.T) {
	now := time.Now()
	health := prepareGameplayHealth(now, time.Minute, 1, 1)
	health.observeGameplayRules("rr_gameplay", &raceGameplayRules{Version: 1}, now)
	decision := impactShadowLogSample{EventID: "collision", CurrentImpactClass: "severe", ProposedKind: "collision", ProposedDamageAllowed: true, ProposedFFBAllowed: true, WindowComplete: true, observedAt: now, context: health.impactDecisionContext(now)}
	snapshot, _, event := health.applyImpactDecision(decision, "CAR-1", now)
	if snapshot.HP != 100 || event == nil || event.DamageApplied || event.SuppressionReason != "damage_disabled" {
		t.Fatalf("health=%+v event=%+v", snapshot, event)
	}
}

func TestRelayRaceEnvelopeAppliesRules(t *testing.T) {
	now := time.Now()
	source := &relay{name: "11.4", raceCarID: "CAR-2", vehicleHealth: newVehicleHealth(now)}
	server := &relayServer{sources: map[string]*relay{"11.4": source}}
	envelope := raceStateEnvelope{RaceRunID: "rr-rules", Phase: "ready"}
	envelope.RaceInfo.GameplayRules = &raceGameplayRules{Version: 1, PitEnabled: true}
	server.observeRaceContext(envelope, now)
	snapshot := source.vehicleHealth.snapshot(now)
	if snapshot.RaceRunID != "rr-rules" || snapshot.GameplayRules == nil || snapshot.DamageEnabled || !snapshot.PitEnabled {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
