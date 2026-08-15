package main

import (
	"testing"
	"time"
)

func TestTelemetryLogCleanupIntervalIsTwoHours(t *testing.T) {
	if telemetryLogCleanupInterval != 2*time.Hour {
		t.Fatalf("telemetryLogCleanupInterval = %v, want 2h", telemetryLogCleanupInterval)
	}
}

func TestTelemetryLogCleanupSafeRequiresNonGreenAndNoDrivingVehicle(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(now)
	server := &relayServer{
		sources: map[string]*relay{
			"11.4": {vehicleHealth: health},
		},
	}

	if safe, reason := server.telemetryLogCleanupSafe(now); !safe || reason != "idle" {
		t.Fatalf("idle cleanup = safe %t reason %q, want true/idle", safe, reason)
	}

	server.raceContext = relayRaceContext{Connected: true, Phase: "green"}
	if safe, reason := server.telemetryLogCleanupSafe(now); safe || reason != "race_green" {
		t.Fatalf("green cleanup = safe %t reason %q, want false/race_green", safe, reason)
	}

	server.raceContext = relayRaceContext{Connected: true, Phase: "final"}
	health.setDriveEnabled(true, now)
	health.limitCommand("S:1500,T:1800", now)
	if safe, reason := server.telemetryLogCleanupSafe(now); safe || reason != "vehicle_driving:11.4" {
		t.Fatalf("driving cleanup = safe %t reason %q, want false/vehicle_driving:11.4", safe, reason)
	}

	afterGrace := now.Add(vehicleHealthForwardCommandGrace + time.Millisecond)
	if safe, reason := server.telemetryLogCleanupSafe(afterGrace); !safe || reason != "idle" {
		t.Fatalf("stopped cleanup = safe %t reason %q, want true/idle", safe, reason)
	}
}

func TestTelemetryLogCleanupTreatsDisconnectedStaleGreenAsIdle(t *testing.T) {
	server := &relayServer{
		sources:     map[string]*relay{},
		raceContext: relayRaceContext{Connected: false, Phase: "green"},
	}
	if safe, reason := server.telemetryLogCleanupSafe(time.Now()); !safe || reason != "idle" {
		t.Fatalf("disconnected stale green cleanup = safe %t reason %q, want true/idle", safe, reason)
	}
}
