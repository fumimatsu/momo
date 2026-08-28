package racerecorder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestServerRequiresAuthorization(t *testing.T) {
	server := newTestServer(t, Config{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestServerReportsReadyStatus(t *testing.T) {
	server := newTestServer(t, Config{})
	request := authorizedRequest(http.MethodGet, "/api/v1/status", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var status Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != StateReady || status.Type != "race_recorder_status" || status.StorageRoot == "" {
		t.Fatalf("status = %#v", status)
	}
}

func TestServerRejectsProgramOnlyUntilProgramSourceExists(t *testing.T) {
	server := newTestServer(t, Config{})
	requestValue := validStartRequest()
	requestValue.Mode = ModeProgramOnly
	request := authorizedJSONRequest(http.MethodPost, "/api/v1/recordings/start", requestValue)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestServerFailsClosedWhenStorageReserveIsUnavailable(t *testing.T) {
	server := newTestServer(t, Config{MinimumFreeBytes: math.MaxInt64})
	request := authorizedJSONRequest(http.MethodPost, "/api/v1/recordings/start", validStartRequest())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	statusRequest := authorizedRequest(http.MethodGet, "/api/v1/status", nil)
	statusResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(statusResponse, statusRequest)
	var status Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != StateDegraded || status.LastError == "" || status.ActiveRaceRunID != "" {
		t.Fatalf("status after reserve failure = %#v", status)
	}
}

func TestFailedStartIsArchivedSoExplicitRetryCanUseSameRun(t *testing.T) {
	storage := t.TempDir()
	server := newTestServer(t, Config{StorageRoot: storage, StartTimeout: 100 * time.Millisecond})
	for attempt := 1; attempt <= 2; attempt++ {
		value := validStartRequest()
		value.CommandID = fmt.Sprintf("start-attempt-%d", attempt)
		request := authorizedJSONRequest(http.MethodPost, "/api/v1/recordings/start", value)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d status = %d body=%s", attempt, response.Code, response.Body.String())
		}
		if _, err := os.Stat(filepath.Join(storage, value.RaceRunID)); !os.IsNotExist(err) {
			t.Fatalf("active run directory remains after attempt %d: %v", attempt, err)
		}
	}
	failed, err := os.ReadDir(filepath.Join(storage, "_failed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 2 {
		t.Fatalf("failed attempt count = %d, want 2", len(failed))
	}
}

func TestExistingCompletedRunDirectoryIsNeverArchivedOrReused(t *testing.T) {
	storage := t.TempDir()
	runDirectory := filepath.Join(storage, "run-1")
	if err := os.Mkdir(runDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(runDirectory, "manifest.json")
	if err := os.WriteFile(marker, []byte("completed"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, Config{StorageRoot: storage, StartTimeout: 100 * time.Millisecond})
	request := authorizedJSONRequest(http.MethodPost, "/api/v1/recordings/start", validStartRequest())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if payload, err := os.ReadFile(marker); err != nil || string(payload) != "completed" {
		t.Fatalf("completed run was moved or changed: payload=%q err=%v", payload, err)
	}
}

func TestRecorderSourceURLPreservesBaseQueryAndAddsIdentity(t *testing.T) {
	value, err := recorderSourceURL("ws://127.0.0.1:8090/ws?existing=1", "11.5")
	if err != nil {
		t.Fatal(err)
	}
	if value != "ws://127.0.0.1:8090/ws?client=recorder&device=11.5&existing=1&role=observer" {
		t.Fatalf("URL = %q", value)
	}
}

func newTestServer(t *testing.T, overrides Config) *Server {
	t.Helper()
	config := Config{
		RelayWebSocketURL: "ws://127.0.0.1:1/ws", StorageRoot: t.TempDir(), Token: testToken,
		MinimumFreeBytes: 1, MaximumSources: 64,
	}
	if overrides.StorageRoot != "" {
		config.StorageRoot = overrides.StorageRoot
	}
	if overrides.MinimumFreeBytes != 0 {
		config.MinimumFreeBytes = overrides.MinimumFreeBytes
	}
	if overrides.StartTimeout != 0 {
		config.StartTimeout = overrides.StartTimeout
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func authorizedRequest(method string, path string, body *bytes.Reader) *http.Request {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, body)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	return request
}

func authorizedJSONRequest(method string, path string, value any) *http.Request {
	payload, _ := json.Marshal(value)
	request := authorizedRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	return request
}
