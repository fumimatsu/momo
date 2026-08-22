package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMarkerSourceManifestUsesDriveSelectionBeforeRun(t *testing.T) {
	server := markerManifestTestServer(t, 5)
	server.managedSources["source-1"].relay.setDriveLogging(1, true, "test")
	server.managedSources["source-5"].relay.setDriveLogging(5, true, "test")

	manifest := server.markerSourceManifestSnapshot(time.Now(), "")
	if manifest.SelectionMode != "drive" || len(manifest.Sources) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Sources[0].SourceID != "source-1" || manifest.Sources[1].SourceID != "source-5" {
		t.Fatalf("sources = %+v", manifest.Sources)
	}
	if manifest.Sources[1].ObserverPath != "/ws?role=observer&device=source-5" {
		t.Fatalf("observer path = %q", manifest.Sources[1].ObserverPath)
	}
}

func TestMarkerSourceManifestLocksToRaceRoster(t *testing.T) {
	server := markerManifestTestServer(t, 5)
	server.managedSources["source-2"].relay.setDriveLogging(2, true, "test")
	server.publishGlobalRaceState(`RACE:{"type":"race_state","version":2,"raceId":"race-1","raceRunId":"run-1","phase":"green","sequence":7,"roster":{"participants":[{"sourceId":"source-1","carId":"CAR-1"},{"sourceId":"source-5","carId":"CAR-5"},{"sourceId":"source-missing","carId":"CAR-X"}]}}`)

	manifest := server.markerSourceManifestSnapshot(time.Now(), "")
	if manifest.SelectionMode != "locked_roster" || manifest.RaceRunID != "run-1" || len(manifest.Sources) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Sources[0].CarID != "CAR-1" || manifest.Sources[1].CarID != "CAR-5" {
		t.Fatalf("sources = %+v", manifest.Sources)
	}
	if len(manifest.MissingSourceIDs) != 1 || manifest.MissingSourceIDs[0] != "source-missing" {
		t.Fatalf("missing = %+v", manifest.MissingSourceIDs)
	}
}

func TestServeMarkerSourcesIsReadOnly(t *testing.T) {
	server := markerManifestTestServer(t, 1)
	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/marker-sources", nil)
	response := httptest.NewRecorder()
	server.serveMarkerSources(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var manifest markerSourceManifest
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil || manifest.Version != markerSourceManifestVersion {
		t.Fatalf("manifest = %+v err=%v", manifest, err)
	}
	etag := response.Header().Get("ETag")
	request = httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/marker-sources", nil)
	request.Header.Set("If-None-Match", etag)
	response = httptest.NewRecorder()
	server.serveMarkerSources(response, request)
	if response.Code != http.StatusNotModified {
		t.Fatalf("conditional GET status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://relay.test/api/v1/marker-sources", nil)
	response = httptest.NewRecorder()
	server.serveMarkerSources(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", response.Code)
	}
}

func TestMarkerSourceManifestCanSelectAllConfiguredVehiclesForShadow(t *testing.T) {
	server := markerManifestTestServer(t, 5)
	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/marker-sources?selection=all", nil)
	response := httptest.NewRecorder()
	server.serveMarkerSources(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var manifest markerSourceManifest
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SelectionMode != "configured" || len(manifest.Sources) != 5 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestMarkerSourceManifestRevisionIgnoresTransientStatus(t *testing.T) {
	base := markerSourceManifest{
		SelectionMode: "locked_roster",
		RaceRunID:     "run-1",
		Phase:         "countdown",
		RaceSequence:  10,
		Sources: []markerSourceDescriptor{{
			SourceID:     "source-1",
			CarID:        "CAR-1",
			ObserverPath: "/ws?role=observer&device=source-1",
			State:        "STREAMING",
			VideoHealth:  "healthy",
		}},
	}
	changedStatus := base
	changedStatus.Phase = "green"
	changedStatus.RaceSequence = 11
	changedStatus.Sources = append([]markerSourceDescriptor(nil), base.Sources...)
	changedStatus.Sources[0].State = "RECONNECTING"
	changedStatus.Sources[0].VideoHealth = "stalled"

	if got, want := markerSourceManifestRevision(changedStatus), markerSourceManifestRevision(base); got != want {
		t.Fatalf("transient status changed revision: got %q want %q", got, want)
	}
}

func TestMarkerSourceManifestRevisionChangesWithRunOrTopology(t *testing.T) {
	base := markerSourceManifest{
		SelectionMode: "locked_roster",
		RaceRunID:     "run-1",
		Sources: []markerSourceDescriptor{{
			SourceID:     "source-1",
			CarID:        "CAR-1",
			ObserverPath: "/ws?role=observer&device=source-1",
		}},
	}
	changedRun := base
	changedRun.RaceRunID = "run-2"
	changedTopology := base
	changedTopology.Sources = append([]markerSourceDescriptor(nil), base.Sources...)
	changedTopology.Sources = append(changedTopology.Sources, markerSourceDescriptor{
		SourceID:     "source-2",
		CarID:        "CAR-2",
		ObserverPath: "/ws?role=observer&device=source-2",
	})

	baseRevision := markerSourceManifestRevision(base)
	if markerSourceManifestRevision(changedRun) == baseRevision {
		t.Fatal("run change did not change revision")
	}
	if markerSourceManifestRevision(changedTopology) == baseRevision {
		t.Fatal("topology change did not change revision")
	}
}

func markerManifestTestServer(t *testing.T, count int) *relayServer {
	t.Helper()
	server := &relayServer{
		sources:        make(map[string]*relay),
		managedSources: make(map[string]*managedRelaySource),
	}
	for index := 1; index <= count; index++ {
		sourceID := fmt.Sprintf("source-%d", index)
		carID := fmt.Sprintf("CAR-%d", index)
		source, err := newRelay(sourceID, "ws://source.invalid/ws", carID, false, 0, 0, vehicleHealthRecoveryDisabled)
		if err != nil {
			t.Fatal(err)
		}
		definition := relayFileSource{ID: sourceID, URL: "ws://source.invalid/ws", SourceKind: relaySourceKindVehicle, RaceCarID: carID}
		server.sourceOrder = append(server.sourceOrder, sourceID)
		server.sources[sourceID] = source
		server.managedSources[sourceID] = &managedRelaySource{relay: source, definition: definition}
	}
	return server
}
