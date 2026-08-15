package main

import (
	"context"
	"log"
	"strings"
	"time"
)

const telemetryLogCleanupInterval = 2 * time.Hour

func (server *relayServer) telemetryLogCleanupSafe(now time.Time) (bool, string) {
	if server == nil {
		return false, "relay_unavailable"
	}
	race := server.raceContextSnapshot()
	if race.Connected && strings.EqualFold(strings.TrimSpace(race.Phase), "green") {
		return false, "race_green"
	}
	for sourceID, source := range server.sources {
		if source != nil && source.vehicleHealth.isActivelyDriving(now) {
			return false, "vehicle_driving:" + sourceID
		}
	}
	return true, "idle"
}

func (server *relayServer) cleanupExpiredTelemetryLogs(now time.Time, directory string, retention time.Duration) (int, string, error) {
	safe, reason := server.telemetryLogCleanupSafe(now)
	if !safe {
		return 0, reason, nil
	}
	if server.recorder == nil {
		return 0, "recorder_unavailable", nil
	}
	removed, err := removeExpiredTelemetryLogsExcept(directory, retention, now, server.recorder.Path())
	return removed, "", err
}

func (server *relayServer) startTelemetryLogRetention(ctx context.Context, directory string, retention time.Duration) {
	if server == nil || server.recorder == nil || strings.TrimSpace(directory) == "" || retention <= 0 {
		return
	}
	go server.runTelemetryLogRetention(ctx, directory, retention, telemetryLogCleanupInterval)
}

func (server *relayServer) runTelemetryLogRetention(ctx context.Context, directory string, retention time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			removed, deferredReason, err := server.cleanupExpiredTelemetryLogs(now, directory, retention)
			switch {
			case err != nil:
				log.Printf("telemetry log cleanup failed: %v", err)
			case deferredReason != "":
				log.Printf("telemetry log cleanup deferred: reason=%s", deferredReason)
			case removed > 0:
				log.Printf("telemetry log cleanup completed: removed=%d", removed)
			}
		case <-ctx.Done():
			return
		}
	}
}
