package main

import (
	"strings"
	"testing"
	"time"
)

func TestVehicleHealthAppliesDamageAndClampsForwardThrottle(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)

	_, published := health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":13.0,"j":120}}`, base.Add(time.Second))
	if !published {
		t.Fatal("strong impact must publish health")
	}
	snapshot := health.snapshot(base.Add(time.Second))
	if snapshot.HP != 88 || snapshot.Mode != "healthy" {
		t.Fatalf("strong impact snapshot = %#v, want HP 88 healthy", snapshot)
	}
	if got := health.limitCommand("S:1500,T:2000\n", base.Add(1100*time.Millisecond)); got != "S:1500,T:1970\n" {
		t.Fatalf("limited command = %q, want 1970", got)
	}
	if got := health.limitCommand("S:1500,T:1300\n", base.Add(1200*time.Millisecond)); got != "S:1500,T:1300\n" {
		t.Fatalf("brake command must not be limited: %q", got)
	}
}

func TestVehicleHealthRecoveryRequiresForwardDrivingAndQuietPeriod(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, base)
	if got := health.snapshot(base).HP; got != 72 {
		t.Fatalf("severe impact HP = %.1f, want 72", got)
	}

	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, base.Add(5*time.Second))
	if got := health.snapshot(base.Add(5 * time.Second)).HP; got != 72 {
		t.Fatalf("health recovered without forward command: %.1f", got)
	}
	health.limitCommand("S:1500,T:2000", base.Add(5*time.Second))
	health.limitCommand("S:1500,T:2000", base.Add(5950*time.Millisecond))
	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, base.Add(6*time.Second))
	if got := health.snapshot(base.Add(6 * time.Second)).HP; got <= 72 {
		t.Fatalf("health did not recover during safe forward driving: %.1f", got)
	}
}

func TestVehicleHealthReadyTransitionResetsOnlyOnce(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, base)
	if _, reset := health.observeRacePhase("green", base.Add(time.Second)); reset {
		t.Fatal("green must not reset health")
	}
	snapshot, reset := health.observeRacePhase("ready", base.Add(2*time.Second))
	if !reset || snapshot.HP != 100 {
		t.Fatalf("ready reset = %#v reset=%t", snapshot, reset)
	}
	if _, reset := health.observeRacePhase("ready", base.Add(3*time.Second)); reset {
		t.Fatal("repeated ready must not reset or publish")
	}
}

func TestVehicleHealthClassifiesOnlyImpactEvents(t *testing.T) {
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":18.0,"j":250}}`); got != "severe" {
		t.Fatalf("V2 severe = %q", got)
	}
	if got := classifyRelayImpact(`TEL:{"v":1,"k":"e","evt":{"name":"impact","data":{"mag_mps2":12.0,"jerk_mps3":0}}}`); got != "strong" {
		t.Fatalf("V1 strong = %q", got)
	}
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"s","m":{"a":[20,0,0]}}`); got != "" {
		t.Fatalf("state frame classified as impact: %q", got)
	}
	if got := formatVehicleHealthTelemetry(vehicleHealthSnapshot{HP: 72, SpeedCap: 0.86, Mode: "healthy"}); !strings.HasPrefix(got, "VHS:1,72.0,0.860,healthy") {
		t.Fatalf("formatted health = %q", got)
	}
}
