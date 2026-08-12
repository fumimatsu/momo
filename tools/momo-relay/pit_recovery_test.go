package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseVehicleHealthRecoveryMode(t *testing.T) {
	t.Parallel()
	for _, value := range []vehicleHealthRecoveryMode{
		vehicleHealthRecoveryLegacy,
		vehicleHealthRecoveryPitMarker,
		vehicleHealthRecoveryHybrid,
		vehicleHealthRecoveryDisabled,
	} {
		value := value
		t.Run(string(value), func(t *testing.T) {
			got, err := parseVehicleHealthRecoveryMode(string(value))
			if err != nil || got != value {
				t.Fatalf("parseVehicleHealthRecoveryMode(%q) = %q, %v", value, got, err)
			}
		})
	}
	if _, err := parseVehicleHealthRecoveryMode("unknown"); err == nil {
		t.Fatal("unknown recovery mode was accepted")
	}
}

func TestVehicleHealthRecoveryModeCapabilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode        vehicleHealthRecoveryMode
		wantDriving bool
		wantPit     bool
	}{
		{mode: vehicleHealthRecoveryLegacy, wantDriving: true},
		{mode: vehicleHealthRecoveryPitMarker, wantPit: true},
		{mode: vehicleHealthRecoveryHybrid, wantDriving: true, wantPit: true},
		{mode: vehicleHealthRecoveryDisabled},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.mode), func(t *testing.T) {
			if got := test.mode.allowsDrivingRecovery(); got != test.wantDriving {
				t.Fatalf("allowsDrivingRecovery() = %t, want %t", got, test.wantDriving)
			}
			if got := test.mode.allowsPitRecovery(); got != test.wantPit {
				t.Fatalf("allowsPitRecovery() = %t, want %t", got, test.wantPit)
			}
		})
	}
}

func TestVehicleHealthDefaultsToHybridRecovery(t *testing.T) {
	health := newVehicleHealth(time.Now())
	if got := health.recoveryModeSnapshot(); got != vehicleHealthRecoveryDefault {
		t.Fatalf("default recovery mode = %q, want %q", got, vehicleHealthRecoveryDefault)
	}
}

func TestVehicleHealthHybridModeAllowsDrivingAndPitRecovery(t *testing.T) {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryHybrid)
	health.observeRaceState(true, "rr_hybrid", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	health.setDriveEnabled(true, base)

	health.observeRaceState(true, "rr_hybrid", "green", 1, 4, base.Add(5*time.Second))
	health.limitCommand("S:1500,T:2000", base.Add(5*time.Second))
	health.limitCommand("S:1500,T:2000", base.Add(5200*time.Millisecond))
	drivingHP := health.snapshot(base.Add(5200 * time.Millisecond)).HP
	if drivingHP <= 80 {
		t.Fatalf("hybrid mode did not apply driving recovery: HP %.3f", drivingHP)
	}

	result, applyErr := health.applyPitRecovery(pitRecoveryCommand{
		CommandID: "rr_hybrid:CP-1:entry-1:tick-1",
		RaceRunID: "rr_hybrid",
		CarID:     "CP-1",
		EntryID:   "entry-1",
		Tick:      1,
	}, base.Add(7200*time.Millisecond))
	if applyErr != nil {
		t.Fatalf("hybrid pit recovery failed: %#v", applyErr)
	}
	wantHP := math.Min(vehicleHealthMaximum, drivingHP+pitRecoveryAmount)
	if math.Abs(result.RecoveredAmount-pitRecoveryAmount) > 0.0001 || math.Abs(result.Snapshot.HP-wantHP) > 0.0001 {
		t.Fatalf("hybrid pit recovery = %#v, driving HP was %.3f", result, drivingHP)
	}
}

func TestVehicleHealthAppliesPitRecoveryTicksIdempotently(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setRecoveryMode(vehicleHealthRecoveryPitMarker)
	health.observeRaceState(true, "rr_123", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)

	first := pitRecoveryCommand{
		CommandID: "rr_123:CP-1:entry-7:tick-1",
		RaceRunID: "rr_123",
		CarID:     "CP-1",
		EntryID:   "entry-7",
		Tick:      1,
	}
	result, applyErr := health.applyPitRecovery(first, base.Add(time.Second))
	if applyErr != nil {
		t.Fatalf("first recovery failed: %#v", applyErr)
	}
	if result.Status != "applied" || result.RecoveredAmount != 10 || result.Snapshot.HP != 90 {
		t.Fatalf("first recovery = %#v, want applied +10 to HP 90", result)
	}

	duplicate, applyErr := health.applyPitRecovery(first, base.Add(3*time.Second))
	if applyErr != nil {
		t.Fatalf("duplicate recovery failed: %#v", applyErr)
	}
	if duplicate.Status != "duplicate" || duplicate.Snapshot.HP != 90 {
		t.Fatalf("duplicate recovery = %#v, want original receipt", duplicate)
	}
	if got := health.snapshot(base.Add(3 * time.Second)).HP; got != 90 {
		t.Fatalf("duplicate changed HP to %.1f", got)
	}

	second := first
	second.CommandID = "rr_123:CP-1:entry-7:tick-2"
	second.Tick = 2
	if _, applyErr := health.applyPitRecovery(second, base.Add(1500*time.Millisecond)); applyErr == nil || applyErr.Code != "recovery_too_soon" {
		t.Fatalf("early second tick error = %#v, want recovery_too_soon", applyErr)
	}
	result, applyErr = health.applyPitRecovery(second, base.Add(2*time.Second))
	if applyErr != nil {
		t.Fatalf("second recovery failed: %#v", applyErr)
	}
	if result.RecoveredAmount != 10 || result.Snapshot.HP != 100 {
		t.Fatalf("second recovery = %#v, want applied +10 to HP 100", result)
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
	health.observeRaceState(true, "rr_123", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	health.limitCommand("S:1500,T:2000", base.Add(5*time.Second))
	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, "CP-1", base.Add(6*time.Second))
	if got := health.snapshot(base.Add(6 * time.Second)).HP; got != 80 {
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
	health.observeRaceState(true, "rr_123", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
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
	if payload.Status != "applied" || payload.RecoveredAmount != 10 || payload.HP != 90 || payload.Fuel != 100 {
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
