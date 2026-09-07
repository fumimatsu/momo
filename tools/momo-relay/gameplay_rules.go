package main

import "time"

// A run's explicit rules override legacy process defaults until the next run.
type raceGameplayRules struct {
	Version       int  `json:"version"`
	DamageEnabled bool `json:"damageEnabled"`
	FuelEnabled   bool `json:"fuelEnabled"`
	PitEnabled    bool `json:"pitEnabled"`
}

func (rules *raceGameplayRules) clone() *raceGameplayRules {
	if rules == nil {
		return nil
	}
	copy := *rules
	return &copy
}

func (health *vehicleHealth) observeGameplayRules(runID string, rules *raceGameplayRules, now time.Time) (vehicleHealthSnapshot, bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	if runID == "" || runID != health.activeRaceRunID || rules == nil || rules.Version != 1 {
		return health.snapshotLocked(now), false
	}
	// Repeated timing snapshots must not reset damage, fuel, or PIT receipts.
	if health.gameplayRules != nil {
		return health.snapshotLocked(now), false
	}
	health.gameplayRules = rules.clone()
	if !rules.DamageEnabled {
		health.restoreDamageLocked(now)
	}
	if !rules.FuelEnabled {
		health.fuel = vehicleFuelMaximum
		health.fuelRatePerSec = 0
	}
	if !rules.PitEnabled {
		health.pitPresent = false
	}
	return health.snapshotLocked(now), true
}

func (health *vehicleHealth) damageEnabledLocked() bool {
	if health.gameplayRules != nil {
		return health.gameplayRules.DamageEnabled
	}
	return health.damageEnabled
}

func (health *vehicleHealth) pitEnabledLocked() bool {
	if health.gameplayRules != nil {
		return health.gameplayRules.PitEnabled
	}
	return health.recoveryMode.allowsPitRecovery()
}
