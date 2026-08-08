package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVehicleHealthAppliesPitRecoveryTicksIdempotently(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryPitMarker)
	health.observeRaceRun("rr_123", base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, base)

	first := pitRecoveryCommand{
		CommandID: "rr_123:CP-1:entry-7:tick-1",
		RaceRunID: "rr_123",
		CarID:     "CP-1",
		EntryID:   "entry-7",
		Tick:      1,
	}
	result, applyErr := health.applyPitRecovery(first, base.Add(2*time.Second))
	if applyErr != nil {
		t.Fatalf("first recovery failed: %#v", applyErr)
	}
	if result.Status != "applied" || result.RecoveredAmount != 20 || result.Snapshot.HP != 92 {
		t.Fatalf("first recovery = %#v, want applied +20 to HP 92", result)
	}

	duplicate, applyErr := health.applyPitRecovery(first, base.Add(3*time.Second))
	if applyErr != nil {
		t.Fatalf("duplicate recovery failed: %#v", applyErr)
	}
	if duplicate.Status != "duplicate" || duplicate.Snapshot.HP != 92 {
		t.Fatalf("duplicate recovery = %#v, want original receipt", duplicate)
	}
	if got := health.snapshot(base.Add(3 * time.Second)).HP; got != 92 {
		t.Fatalf("duplicate changed HP to %.1f", got)
	}

	second := first
	second.CommandID = "rr_123:CP-1:entry-7:tick-2"
	second.Tick = 2
	if _, applyErr := health.applyPitRecovery(second, base.Add(3*time.Second)); applyErr == nil || applyErr.Code != "recovery_too_soon" {
		t.Fatalf("early second tick error = %#v, want recovery_too_soon", applyErr)
	}
	result, applyErr = health.applyPitRecovery(second, base.Add(4*time.Second))
	if applyErr != nil {
		t.Fatalf("second recovery failed: %#v", applyErr)
	}
	if result.RecoveredAmount != 8 || result.Snapshot.HP != 100 {
		t.Fatalf("second recovery = %#v, want clamp +8 to HP 100", result)
	}

	newEntry := first
	newEntry.CommandID = "rr_123:CP-1:entry-8:tick-1"
	newEntry.EntryID = "entry-8"
	if _, applyErr := health.applyPitRecovery(newEntry, base.Add(6*time.Second)); applyErr != nil {
		t.Fatalf("new entry failed: %#v", applyErr)
	}
	replayedEntry := first
	replayedEntry.CommandID = "rr_123:CP-1:entry-7:tick-1-replay"
	if _, applyErr := health.applyPitRecovery(replayedEntry, base.Add(8*time.Second)); applyErr == nil || applyErr.Code != "entry_id_reused" {
		t.Fatalf("replayed entry error = %#v, want entry_id_reused", applyErr)
	}
}

func TestVehicleHealthPitModeDisablesLegacyRecoveryAndChecksSequence(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryPitMarker)
	health.observeRaceRun("rr_123", base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, base)
	health.limitCommand("S:1500,T:2000", base.Add(5*time.Second))
	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, base.Add(6*time.Second))
	if got := health.snapshot(base.Add(6 * time.Second)).HP; got != 72 {
		t.Fatalf("pit-marker mode used legacy recovery: HP %.1f", got)
	}

	outOfSequence := pitRecoveryCommand{
		CommandID: "rr_123:CP-1:entry-7:tick-2",
		RaceRunID: "rr_123",
		CarID:     "CP-1",
		EntryID:   "entry-7",
		Tick:      2,
	}
	if _, applyErr := health.applyPitRecovery(outOfSequence, base.Add(6*time.Second)); applyErr == nil || applyErr.Code != "tick_out_of_sequence" {
		t.Fatalf("out-of-sequence error = %#v", applyErr)
	}
}

func TestPitRecoveryTickHTTPContract(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryPitMarker)
	health.observeRaceRun("rr_123", base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, base)
	server := &relayServer{
		sources: map[string]*relay{
			"11.5": {
				name:          "11.5",
				raceCarID:     "CP-1",
				viewers:       make(map[uint64]*viewer),
				vehicleHealth: health,
			},
		},
		raceContext: relayRaceContext{Connected: true, RaceRunID: "rr_123", Phase: "green"},
	}
	handler := bearerTokenHandler("test-token", server.servePitRecoveryTick)
	body := []byte(`{"schemaVersion":1,"command":"pit_recovery_tick","commandId":"cmd-1","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-7","tick":1}`)

	unauthorized := httptest.NewRequest(http.MethodPost, "/api/v1/gameplay/pit-recovery-ticks", bytes.NewReader(body))
	unauthorizedResult := httptest.NewRecorder()
	handler(unauthorizedResult, unauthorized)
	if unauthorizedResult.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorizedResult.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/gameplay/pit-recovery-ticks", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("recovery status = %d body=%s", response.Code, response.Body.String())
	}
	var payload pitRecoveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "applied" || payload.RecoveredAmount != 20 || payload.HP != 92 {
		t.Fatalf("recovery response = %#v", payload)
	}

	server.raceMu.Lock()
	server.raceContext.Phase = "finished"
	server.raceMu.Unlock()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/gameplay/pit-recovery-ticks", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response = httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("finished phase status = %d, want 409", response.Code)
	}
}

func TestRelayRaceContextControlsVehicleHealthRun(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryPitMarker)
	server := &relayServer{
		sources: map[string]*relay{
			"11.5": {vehicleHealth: health},
		},
	}
	server.observeRaceContext(raceStateEnvelope{RaceRunID: "rr_123", Phase: "green"}, base)
	context := server.raceContextSnapshot()
	if !context.Connected || context.RaceRunID != "rr_123" || context.Phase != "green" {
		t.Fatalf("race context = %#v", context)
	}
	if health.activeRaceRunID != "rr_123" {
		t.Fatalf("vehicle health run = %q", health.activeRaceRunID)
	}
	server.markRaceControlDisconnected()
	if server.raceContextSnapshot().Connected {
		t.Fatal("Race Control context remained connected")
	}
}
