package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedAyameRoomIDIsStableAndSafe(t *testing.T) {
	roomID, err := generatedAyameRoomID("momo-relay", "11.3")
	if err != nil {
		t.Fatalf("generatedAyameRoomID() error = %v", err)
	}
	if roomID != "momo-relay-11-3-ext" {
		t.Fatalf("room id = %q", roomID)
	}
	roomID, err = generatedAyameRoomID("tokorozawa-relay", "MOMO_FPV.17")
	if err != nil {
		t.Fatalf("generatedAyameRoomID() error = %v", err)
	}
	if roomID != "tokorozawa-relay-momo-fpv-17-ext" {
		t.Fatalf("normalized room id = %q", roomID)
	}
}

func TestNormalizeSourceDefinitionDefaultsAyameAndAllowsOptOut(t *testing.T) {
	runtime := relaySourceRuntime{
		ayameSignalingURL: "wss://ayame.example/signaling",
		ayameRoomPrefix:   "momo-relay",
	}
	normalized, err := normalizeSourceDefinition(relayFileSource{
		ID:  "11.4",
		URL: "ws://192.168.11.4:8080/ws",
	}, runtime, true)
	if err != nil {
		t.Fatalf("normalizeSourceDefinition() error = %v", err)
	}
	if normalized.AyamePilotEnabled == nil || !*normalized.AyamePilotEnabled {
		t.Fatal("Ayame Pilot was not enabled by the room prefix")
	}
	if normalized.AyamePilotRoom != "momo-relay-11-4-ext" {
		t.Fatalf("Ayame room = %q", normalized.AyamePilotRoom)
	}

	normalized, err = normalizeSourceDefinition(relayFileSource{
		ID:                "11.5",
		URL:               "ws://192.168.11.5:8080/ws",
		AyamePilotEnabled: relayBoolPointer(false),
	}, runtime, true)
	if err != nil {
		t.Fatalf("normalize opt-out error = %v", err)
	}
	if normalized.AyamePilotEnabled == nil || *normalized.AyamePilotEnabled || normalized.AyamePilotRoom != "" {
		t.Fatalf("Ayame opt-out = %#v", normalized)
	}
}

func TestDynamicSourceRegistryRoundTripAndBackupRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay-sources.json")
	registry := &dynamicSourceRegistry{path: path}
	definitions := []relayFileSource{{
		ID:                "11.7",
		URL:               "ws://192.168.11.7:8080/ws",
		RaceCarID:         "CP-5",
		AyamePilotEnabled: relayBoolPointer(true),
		AyamePilotRoom:    "momo-relay-11-7-ext",
	}}
	if err := registry.save(definitions); err != nil {
		t.Fatalf("registry.save() error = %v", err)
	}
	loadedRegistry, loaded, err := loadDynamicSourceRegistry(path)
	if err != nil {
		t.Fatalf("loadDynamicSourceRegistry() error = %v", err)
	}
	if loadedRegistry == nil || len(loaded) != 1 || loaded[0].ID != "11.7" || loaded[0].AyamePilotRoom != "momo-relay-11-7-ext" {
		t.Fatalf("loaded registry = %#v", loaded)
	}
	if err := os.Rename(path, path+".bak"); err != nil {
		t.Fatalf("simulate interrupted registry replace: %v", err)
	}
	_, recovered, err := loadDynamicSourceRegistry(path)
	if err != nil {
		t.Fatalf("recover registry backup: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != "11.7" {
		t.Fatalf("recovered registry = %#v", recovered)
	}
}

func TestDynamicSourceAddPersistsAndRejectsConflicts(t *testing.T) {
	server := newDynamicSourceTestServer(t)
	created, err := server.addDynamicSource(relayFileSource{
		ID:        "11.7",
		URL:       "ws://192.168.11.7:8080/ws",
		RaceCarID: "CP-5",
	})
	if err != nil {
		t.Fatalf("addDynamicSource() error = %v", err)
	}
	if !created.Dynamic || created.AyamePilotRoom != "momo-relay-11-7-ext" {
		t.Fatalf("created source = %#v", created)
	}
	_, loaded, err := loadDynamicSourceRegistry(server.dynamicSourceRegistry.path)
	if err != nil || len(loaded) != 1 || loaded[0].ID != "11.7" {
		t.Fatalf("persisted sources = %#v error=%v", loaded, err)
	}
	if _, err := server.addDynamicSource(relayFileSource{
		ID:        "11.8",
		URL:       "ws://192.168.11.8:8080/ws",
		RaceCarID: "CP-5",
	}); sourceManagementErrorCode(err) != "duplicate_race_car_id" {
		t.Fatalf("duplicate race car error = %v", err)
	}
	if _, err := server.addDynamicSource(relayFileSource{
		ID:                "11.8",
		URL:               "ws://192.168.11.8:8080/ws",
		RaceCarID:         "CP-6",
		AyamePilotEnabled: relayBoolPointer(true),
		AyamePilotRoom:    "momo-relay-11-7-ext",
	}); sourceManagementErrorCode(err) != "duplicate_ayame_room" {
		t.Fatalf("duplicate Ayame room error = %v", err)
	}
}

func TestDynamicSourceUpdateChangesEndpointAndIsIdempotent(t *testing.T) {
	server := newDynamicSourceTestServer(t)
	definition := relayFileSource{
		ID:                "momo-fpv-17",
		URL:               "ws://192.168.11.17:8080/ws",
		RaceCarID:         "CP-17",
		AyamePilotEnabled: relayBoolPointer(true),
	}
	if _, err := server.addDynamicSource(definition); err != nil {
		t.Fatalf("addDynamicSource() error = %v", err)
	}
	definition.URL = "ws://192.168.11.117:8080/ws"
	updated, err := server.replaceDynamicSource(definition)
	if err != nil {
		t.Fatalf("replaceDynamicSource() error = %v", err)
	}
	if updated.URL != definition.URL || updated.AyamePilotRoom != "momo-relay-momo-fpv-17-ext" {
		t.Fatalf("updated source = %#v", updated)
	}
	unchanged, err := server.replaceDynamicSource(definition)
	if err != nil {
		t.Fatalf("idempotent replace error = %v", err)
	}
	if unchanged != updated {
		t.Fatalf("idempotent replace = %#v, want %#v", unchanged, updated)
	}
	_, loaded, err := loadDynamicSourceRegistry(server.dynamicSourceRegistry.path)
	if err != nil || len(loaded) != 1 || loaded[0].URL != definition.URL {
		t.Fatalf("updated registry = %#v error=%v", loaded, err)
	}
}

func TestDynamicSourceRemovalIsIdleOnlyAndStaticSourcesRemain(t *testing.T) {
	server := newDynamicSourceTestServer(t)
	if err := server.addInitialSource(relayFileSource{
		ID:                "11.3",
		URL:               "ws://192.168.11.3:8080/ws",
		AyamePilotEnabled: relayBoolPointer(false),
	}, false); err != nil {
		t.Fatalf("addInitialSource() error = %v", err)
	}
	if err := server.removeDynamicSource("11.3"); sourceManagementErrorCode(err) != "static_source" {
		t.Fatalf("static removal error = %v", err)
	}
	if _, err := server.addDynamicSource(relayFileSource{
		ID:                "11.7",
		URL:               "ws://192.168.11.7:8080/ws",
		AyamePilotEnabled: relayBoolPointer(false),
	}); err != nil {
		t.Fatalf("addDynamicSource() error = %v", err)
	}
	source, _ := server.lookupSource("11.7")
	source.viewersMu.Lock()
	source.viewers[1] = &viewer{id: 1, role: "observer"}
	source.viewersMu.Unlock()
	if err := server.removeDynamicSource("11.7"); sourceManagementErrorCode(err) != "source_in_use" {
		t.Fatalf("in-use removal error = %v", err)
	}
	source.viewersMu.Lock()
	delete(source.viewers, 1)
	source.viewersMu.Unlock()
	acquired, ok := server.acquireSourceSession("11.7")
	if !ok || acquired != source {
		t.Fatal("acquireSourceSession() did not reserve the dynamic source")
	}
	if err := server.removeDynamicSource("11.7"); sourceManagementErrorCode(err) != "source_in_use" {
		t.Fatalf("negotiating session removal error = %v", err)
	}
	acquired.activeSessions.Add(-1)
	if err := server.removeDynamicSource("11.7"); err != nil {
		t.Fatalf("removeDynamicSource() error = %v", err)
	}
	if _, exists := server.lookupSource("11.7"); exists {
		t.Fatal("removed source is still registered")
	}
}

func TestDynamicSourceMutationIsRejectedDuringGreenPhase(t *testing.T) {
	server := newDynamicSourceTestServer(t)
	server.raceContext = relayRaceContext{Connected: true, RaceRunID: "run-1", Phase: "green"}
	if _, err := server.addDynamicSource(relayFileSource{
		ID:                "11.7",
		URL:               "ws://192.168.11.7:8080/ws",
		AyamePilotEnabled: relayBoolPointer(false),
	}); sourceManagementErrorCode(err) != "race_active" {
		t.Fatalf("green phase add error = %v", err)
	}
}

func TestSourceManagementHTTPRequiresTokenAndExplicitURL(t *testing.T) {
	server := newDynamicSourceTestServer(t)
	handler := sourceAdminTokenHandler("admin-token", server.serveSources)

	unauthorized := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/sources", nil)
	unauthorizedResult := httptest.NewRecorder()
	handler(unauthorizedResult, unauthorized)
	if unauthorizedResult.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResult.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "http://relay.test/api/v1/sources", strings.NewReader(`{
		"id":"11.9",
		"url":"ws://192.168.11.9:8080/ws",
		"raceCarId":"CP-7"
	}`))
	request.Header.Set("Authorization", "Bearer admin-token")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	var created sourceDefinitionView
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.URL != "ws://192.168.11.9:8080/ws" {
		t.Fatalf("source URL = %q", created.URL)
	}
}

func newDynamicSourceTestServer(t *testing.T) *relayServer {
	t.Helper()
	rootContext, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "dynamic-sources.json")
	return &relayServer{
		sources:               make(map[string]*relay),
		managedSources:        make(map[string]*managedRelaySource),
		dynamicSourceRegistry: &dynamicSourceRegistry{path: path},
		pitEvents:             make(map[string]pitPresenceReceipt),
		sourceRuntime: relaySourceRuntime{
			rootContext:          rootContext,
			rtpStallTimeout:      defaultRTPStallTimeout,
			upstreamStartTimeout: defaultUpstreamStartTimeout,
			healthRecoveryMode:   vehicleHealthRecoveryDefault,
			fuelDriveDuration:    vehicleFuelDefaultDriveDuration,
			ayameSignalingURL:    "wss://ayame.example/signaling",
			ayameClientIDPrefix:  "momo-relay",
			ayameRoomPrefix:      "momo-relay",
		},
	}
}

func sourceManagementErrorCode(err error) string {
	if typed, ok := err.(*sourceManagementError); ok {
		return typed.code
	}
	return ""
}
