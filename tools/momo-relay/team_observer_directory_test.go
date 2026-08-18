package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamObserverDirectoryProjectionExposesOnlyDisplayData(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)
	path := writeTeamObserverDirectoryCache(t, validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", now.Add(-2*time.Hour)))
	source, err := newTeamObserverDirectorySource(path, "madsystem", "tokorozawa-2026-08", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := source.projection(now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Version != 1 || !projection.Stale || projection.AgeMS != (2*time.Hour).Milliseconds() {
		t.Fatalf("projection freshness = %#v", projection)
	}
	if len(projection.Vehicles) != 1 || projection.Vehicles[0].VehicleID != "vehicle-01" || projection.Vehicles[0].SourceID != "11.3" {
		t.Fatalf("projection vehicles = %#v", projection.Vehicles)
	}
	if len(projection.Pilots) != 1 || projection.Pilots[0].PilotID != "pilot-01" {
		t.Fatalf("projection pilots = %#v", projection.Pilots)
	}
	if len(projection.Entries) != 1 || projection.Entries[0].EntryID != "entry-01" {
		t.Fatalf("projection entries = %#v", projection.Entries)
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"etag", "organizationId", "themeSong", "provider"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("projection exposes %q: %s", forbidden, encoded)
		}
	}
}

func TestTeamObserverDirectoryRejectsWrongScopeAndMalformedCache(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"wrong event":       validTeamObserverDirectoryCache("madsystem", "other-event", now),
		"unknown field":     strings.Replace(validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", now), `"schemaVersion": 1,`, `"schemaVersion": 1, "privateToken": "secret",`, 1),
		"revision mismatch": strings.Replace(validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", now), `"etag": "rd_0123456789abcdef0123456789abcdef"`, `"etag": "rd_ffffffffffffffffffffffffffffffff"`, 1),
		"duplicate source":  strings.Replace(validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", now), `"vehicles": [`, `"vehicles": [{"vehicleId":"vehicle-02","vehicleName":"Second","status":"active","sourceBindings":[{"sourceId":"11.3","active":true}]},`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTeamObserverDirectoryCache(t, body)
			source, err := newTeamObserverDirectorySource(path, "madsystem", "tokorozawa-2026-08", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.projection(now); err == nil {
				t.Fatal("projection accepted an invalid cache")
			}
		})
	}
}

func TestTeamObserverDirectoryRejectsMissingCollections(t *testing.T) {
	var cache teamObserverDirectoryCache
	if err := json.Unmarshal([]byte(validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", time.Now().UTC())), &cache); err != nil {
		t.Fatal(err)
	}
	cache.Directory.Pilots = nil
	if err := validateTeamObserverDirectoryCache(cache, "madsystem", "tokorozawa-2026-08"); err == nil {
		t.Fatal("accepted a directory with a missing pilots collection")
	}
}

func TestTeamObserverDirectoryHTTPContract(t *testing.T) {
	now := time.Now().UTC()
	path := writeTeamObserverDirectoryCache(t, validTeamObserverDirectoryCache("madsystem", "tokorozawa-2026-08", now))
	source, err := newTeamObserverDirectorySource(path, "madsystem", "tokorozawa-2026-08", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server := &relayServer{teamObserverDirectory: source}

	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/team-observer-directory", nil)
	recorder := httptest.NewRecorder()
	server.serveTeamObserverDirectory(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET directory = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://relay.test/api/v1/team-observer-directory", nil)
	recorder = httptest.NewRecorder()
	server.serveTeamObserverDirectory(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST directory = %d Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	recorder = httptest.NewRecorder()
	(&relayServer{}).serveTeamObserverDirectory(recorder, httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/team-observer-directory", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("disabled directory = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestTeamObserverDirectorySourceRequiresCompleteExplicitScope(t *testing.T) {
	if source, err := newTeamObserverDirectorySource("", "", "", time.Hour); err != nil || source != nil {
		t.Fatalf("disabled source = %#v, %v", source, err)
	}
	for _, values := range [][3]string{
		{"cache.json", "", "event"},
		{"cache.json", "organization", ""},
		{"", "organization", "event"},
	} {
		if _, err := newTeamObserverDirectorySource(values[0], values[1], values[2], time.Hour); err == nil {
			t.Fatalf("accepted incomplete source configuration: %#v", values)
		}
	}
	if _, err := newTeamObserverDirectorySource("cache.json", "organization", "event", 0); err == nil {
		t.Fatal("accepted a non-positive max age")
	}
}

func validTeamObserverDirectoryCache(organization, event string, fetchedAt time.Time) string {
	return `{
  "schemaVersion": 1,
  "fetchedAt": "` + fetchedAt.Format(time.RFC3339Nano) + `",
  "etag": "rd_0123456789abcdef0123456789abcdef",
  "directory": {
    "schemaVersion": 1,
    "generatedAt": "2026-08-18T12:00:00Z",
    "organization": {"organizationId":"org-01","slug":"` + organization + `","name":"MADSYSTEM"},
    "event": {"eventId":"event-01","slug":"` + event + `","name":"Tokorozawa","status":"open"},
    "pilots": [{
      "pilotId":"pilot-01","pilotNo":"1","callsign":"AYA","displayName":"Aya",
      "teamName":"SDK Racing","photoUrl":"https://example.test/aya.jpg","color":"#2F8FDA",
      "themeSong":{"url":"https://example.test/theme.mp3","provider":"Demo"}
    }],
    "vehicles": [{
      "vehicleId":"vehicle-01","vehicleName":"GR Supra","displayNumber":"1","status":"active",
      "sourceBindings":[{"sourceId":"11.3","active":true}]
    }],
    "entries": [{"entryId":"entry-01","pilotId":"pilot-01","classCode":"NORMAL","entryStatus":"confirmed"}],
    "rosterCandidates": [{"entryId":"entry-01","pilotId":"pilot-01","classCode":"NORMAL"}],
    "directoryRevision": "rd_0123456789abcdef0123456789abcdef"
  }
}`
}

func writeTeamObserverDirectoryCache(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "race-directory-cache.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
