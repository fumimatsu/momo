package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newPitPresenceTestServer(t *testing.T) (*relayServer, *relay) {
	t.Helper()
	now := time.Now()
	health := newVehicleHealth(now)
	health.setRecoveryMode(vehicleHealthRecoveryHybrid)
	health.observeRaceRun("rr_123", now)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","e":{"n":"impact_candidate","m":20.0,"j":300}}`, now)
	source := &relay{
		name:          "11.5",
		raceCarID:     "CP-1",
		viewers:       make(map[uint64]*viewer),
		vehicleHealth: health,
		pitPresence:   newPitPresenceState("CP-1", health.snapshot(now).HP),
	}
	server := &relayServer{
		sources: map[string]*relay{"11.5": source},
		raceContext: relayRaceContext{
			Connected: true,
			RaceRunID: "rr_123",
			Phase:     "green",
		},
		pitEvents: make(map[string]pitPresenceReceipt),
	}
	return server, source
}

func performGameplayRequest(handler http.HandlerFunc, path string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func TestPitPresenceHTTPContractAndIdempotency(t *testing.T) {
	server, source := newPitPresenceTestServer(t)
	handler := bearerTokenHandler("test-token", server.servePitPresenceEvent)
	entered := `{"schemaVersion":1,"event":"pit_presence","eventId":"event-entered","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-7","transition":"entered","occurredAtUnixMs":1786348800123,"reason":"marker_confirmed"}`

	response := performGameplayRequest(handler, "/api/v1/gameplay/pit-presence-events", entered)
	if response.Code != http.StatusOK {
		t.Fatalf("entered status = %d body=%s", response.Code, response.Body.String())
	}
	var enteredResponse pitPresenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &enteredResponse); err != nil {
		t.Fatal(err)
	}
	if enteredResponse.Status != "applied" || !enteredResponse.Present || enteredResponse.ServerTimeMs <= 0 {
		t.Fatalf("entered response = %#v", enteredResponse)
	}
	snapshot := source.pitPresence.snapshot(source.vehicleHealth.snapshot(time.Now()))
	if !snapshot.Present || snapshot.EntryID != "entry-7" || snapshot.ServiceState != "servicing" || snapshot.HP != 72 {
		t.Fatalf("entered snapshot = %#v", snapshot)
	}
	if snapshot.EnteredAtUnixMs == 1786348800123 {
		t.Fatal("enteredAtUnixMs used the MADSYSTEM clock instead of Relay server time")
	}

	response = performGameplayRequest(handler, "/api/v1/gameplay/pit-presence-events", entered)
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d body=%s", response.Code, response.Body.String())
	}
	var duplicate pitPresenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.Status != "duplicate" || duplicate.ServerTimeMs != enteredResponse.ServerTimeMs {
		t.Fatalf("duplicate response = %#v, first = %#v", duplicate, enteredResponse)
	}

	conflict := strings.Replace(entered, `"entryId":"entry-7"`, `"entryId":"entry-8"`, 1)
	response = performGameplayRequest(handler, "/api/v1/gameplay/pit-presence-events", conflict)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "event_conflict") {
		t.Fatalf("event conflict status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestPitPresenceExitAndRecoveryTickSynchronization(t *testing.T) {
	server, source := newPitPresenceTestServer(t)
	presenceHandler := bearerTokenHandler("test-token", server.servePitPresenceEvent)
	recoveryHandler := bearerTokenHandler("test-token", server.servePitRecoveryTick)
	entered := `{"schemaVersion":1,"event":"pit_presence","eventId":"event-entered","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-7","transition":"entered","occurredAtUnixMs":1786348800123,"reason":"marker_confirmed"}`
	if response := performGameplayRequest(presenceHandler, "/api/v1/gameplay/pit-presence-events", entered); response.Code != http.StatusOK {
		t.Fatalf("entered status = %d body=%s", response.Code, response.Body.String())
	}

	tick := `{"schemaVersion":1,"command":"pit_recovery_tick","commandId":"cmd-1","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-7","tick":1}`
	if response := performGameplayRequest(recoveryHandler, "/api/v1/gameplay/pit-recovery-ticks", tick); response.Code != http.StatusOK {
		t.Fatalf("tick status = %d body=%s", response.Code, response.Body.String())
	}
	snapshot := source.pitPresence.snapshot(source.vehicleHealth.snapshot(time.Now()))
	if snapshot.LastAcceptedTick != 1 || snapshot.HP != 92 || snapshot.ServiceState != "servicing" {
		t.Fatalf("recovery snapshot = %#v", snapshot)
	}

	wrongExit := `{"schemaVersion":1,"event":"pit_presence","eventId":"event-wrong-exit","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-8","transition":"exited","occurredAtUnixMs":1786348802123,"reason":"marker_lost"}`
	response := performGameplayRequest(presenceHandler, "/api/v1/gameplay/pit-presence-events", wrongExit)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "entry_mismatch") {
		t.Fatalf("wrong exit status = %d body=%s", response.Code, response.Body.String())
	}

	exited := strings.Replace(wrongExit, `"eventId":"event-wrong-exit"`, `"eventId":"event-exited"`, 1)
	exited = strings.Replace(exited, `"entryId":"entry-8"`, `"entryId":"entry-7"`, 1)
	response = performGameplayRequest(presenceHandler, "/api/v1/gameplay/pit-presence-events", exited)
	if response.Code != http.StatusOK {
		t.Fatalf("exit status = %d body=%s", response.Code, response.Body.String())
	}
	snapshot = source.pitPresence.snapshot(source.vehicleHealth.snapshot(time.Now()))
	if snapshot.Present || snapshot.ServiceState != "outside" || snapshot.ExitReason != "marker_lost" || snapshot.ExitedAtUnixMs <= 0 {
		t.Fatalf("exit snapshot = %#v", snapshot)
	}
	if formatted := formatPitPresenceTelemetry(snapshot); !strings.HasPrefix(formatted, `PIT:1,{"raceRunId":"rr_123","carId":"CP-1","present":false`) {
		t.Fatalf("formatted PIT telemetry = %q", formatted)
	}
}

func TestPitPresenceResetsOnPhaseRunAndDisconnect(t *testing.T) {
	server, source := newPitPresenceTestServer(t)
	base := time.Now()
	if _, err := source.pitPresence.apply(pitPresenceEvent{
		RaceRunID: "rr_123", CarID: "CP-1", EntryID: "entry-7", Transition: "entered",
	}, base, 72); err != nil {
		t.Fatalf("seed entry: %#v", err)
	}

	server.observeRaceContext(raceStateEnvelope{RaceRunID: "rr_123", Phase: "finished"}, base.Add(time.Second))
	snapshot := source.pitPresence.snapshot(source.vehicleHealth.snapshot(base.Add(time.Second)))
	if snapshot.Present || snapshot.ExitReason != "race_phase_changed" {
		t.Fatalf("phase reset snapshot = %#v", snapshot)
	}

	if _, err := source.pitPresence.apply(pitPresenceEvent{
		RaceRunID: "rr_123", CarID: "CP-1", EntryID: "entry-8", Transition: "entered",
	}, base.Add(2*time.Second), 72); err != nil {
		t.Fatalf("second entry: %#v", err)
	}
	server.observeRaceContext(raceStateEnvelope{RaceRunID: "rr_456", Phase: "green"}, base.Add(3*time.Second))
	snapshot = source.pitPresence.snapshot(source.vehicleHealth.snapshot(base.Add(3 * time.Second)))
	if snapshot.Present || snapshot.RaceRunID != "rr_456" || snapshot.EntryID != "" {
		t.Fatalf("run reset snapshot = %#v", snapshot)
	}

	if _, err := source.pitPresence.apply(pitPresenceEvent{
		RaceRunID: "rr_456", CarID: "CP-1", EntryID: "entry-9", Transition: "entered",
	}, base.Add(4*time.Second), 72); err != nil {
		t.Fatalf("third entry: %#v", err)
	}
	server.markRaceControlDisconnected()
	snapshot = source.pitPresence.snapshot(source.vehicleHealth.snapshot(base.Add(4 * time.Second)))
	if snapshot.Present || snapshot.ExitReason != "race_control_disconnected" {
		t.Fatalf("disconnect reset snapshot = %#v", snapshot)
	}
}

func TestPitPresenceRejectsUnknownFieldsAndOversizedBodies(t *testing.T) {
	server, _ := newPitPresenceTestServer(t)
	handler := bearerTokenHandler("test-token", server.servePitPresenceEvent)
	unknown := `{"schemaVersion":1,"event":"pit_presence","eventId":"event-entered","sourceId":"madsystem","raceRunId":"rr_123","carId":"CP-1","entryId":"entry-7","transition":"entered","occurredAtUnixMs":1786348800123,"reason":"marker_confirmed","extra":true}`
	response := performGameplayRequest(handler, "/api/v1/gameplay/pit-presence-events", unknown)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unknown field") {
		t.Fatalf("unknown field status = %d body=%s", response.Code, response.Body.String())
	}

	oversized := bytes.Repeat([]byte(" "), pitRecoveryMaxBodyBytes+1)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/gameplay/pit-presence-events", bytes.NewReader(oversized))
	request.Header.Set("Authorization", "Bearer test-token")
	response = httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "request body too large") {
		t.Fatalf("oversized status = %d body=%s", response.Code, response.Body.String())
	}
}
