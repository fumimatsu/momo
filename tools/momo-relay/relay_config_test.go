package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRelayConfigBuildsMappingsAndSkipsDisabledSources(t *testing.T) {
	path := writeRelayConfigTestFile(t, `{
  "version": 1,
  "sources": [
    {"id":"11.3","url":"ws://192.168.11.3:8080/ws","raceCarId":"CP-1","ayamePilotRoom":"room-3"},
    {"id":"11.4","url":"wss://relay.example/ws","raceCarId":"CP-2"},
    {"id":"spare","url":"ws://192.168.11.9:8080/ws","enabled":false}
  ]
}`)
	mappings, err := loadRelayConfig(path)
	if err != nil {
		t.Fatalf("loadRelayConfig() error = %v", err)
	}
	if got := strings.Join(mappings.Sources, ","); got != "11.3=ws://192.168.11.3:8080/ws,11.4=wss://relay.example/ws" {
		t.Fatalf("sources = %q", got)
	}
	if got := strings.Join(mappings.RaceCars, ","); got != "11.3=CP-1,11.4=CP-2" {
		t.Fatalf("race cars = %q", got)
	}
	if got := strings.Join(mappings.AyamePilotRooms, ","); got != "11.3=room-3" {
		t.Fatalf("Ayame rooms = %q", got)
	}
	if mappings.Definitions[0].SourceKind != relaySourceKindVehicle {
		t.Fatalf("default source kind = %q", mappings.Definitions[0].SourceKind)
	}
}

func TestLoadRelayConfigAcceptsVenueWithoutRaceOrPilotMapping(t *testing.T) {
	path := writeRelayConfigTestFile(t, `{
  "version": 1,
  "sources": [
    {"id":"venue-main","url":"ws://192.168.11.20:8080/ws","sourceKind":"venue","displayName":"TRACK CAM","ayamePilotEnabled":false}
  ]
}`)
	mappings, err := loadRelayConfig(path)
	if err != nil {
		t.Fatalf("loadRelayConfig() error = %v", err)
	}
	if len(mappings.Definitions) != 1 {
		t.Fatalf("definitions = %#v", mappings.Definitions)
	}
	definition := mappings.Definitions[0]
	if definition.SourceKind != relaySourceKindVenue || definition.DisplayName != "TRACK CAM" || definition.RaceCarID != "" {
		t.Fatalf("venue definition = %#v", definition)
	}
	if len(mappings.RaceCars) != 0 || len(mappings.AyamePilotRooms) != 0 {
		t.Fatalf("venue mappings include race or pilot routes: %#v", mappings)
	}
}

func TestRelayConfigExampleDefinesFourUniqueAyameSources(t *testing.T) {
	mappings, err := loadRelayConfig("relay-config.example.json")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if len(mappings.Definitions) != 4 {
		t.Fatalf("example source count = %d", len(mappings.Definitions))
	}
	for index, definition := range mappings.Definitions {
		if definition.AyamePilotEnabled == nil || !*definition.AyamePilotEnabled {
			t.Fatalf("source %q Ayame Pilot is not enabled", definition.ID)
		}
		wantRoom := fmt.Sprintf("momo-relay-11-%d-ext", index+3)
		if definition.AyamePilotRoom != wantRoom {
			t.Fatalf("source %q room = %q, want %q", definition.ID, definition.AyamePilotRoom, wantRoom)
		}
	}
}

func TestLoadRelayConfigRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	tests := map[string]string{
		"unknown field":            `{"version":1,"extra":true,"sources":[{"id":"a","url":"ws://a/ws"}]}`,
		"wrong version":            `{"version":2,"sources":[{"id":"a","url":"ws://a/ws"}]}`,
		"duplicate id":             `{"version":1,"sources":[{"id":"a","url":"ws://a/ws"},{"id":"a","url":"ws://b/ws"}]}`,
		"duplicate car":            `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","raceCarId":"CP-1"},{"id":"b","url":"ws://b/ws","raceCarId":"CP-1"}]}`,
		"duplicate room":           `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","ayamePilotRoom":"room-a"},{"id":"b","url":"ws://b/ws","ayamePilotRoom":"room-a"}]}`,
		"disabled Ayame with room": `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","ayamePilotEnabled":false,"ayamePilotRoom":"room-a"}]}`,
		"unknown source kind":      `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","sourceKind":"camera"}]}`,
		"venue race car":           `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","sourceKind":"venue","raceCarId":"CP-1"}]}`,
		"venue Ayame pilot":        `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","sourceKind":"venue","ayamePilotEnabled":true}]}`,
		"http source":              `{"version":1,"sources":[{"id":"a","url":"http://a/ws"}]}`,
		"all disabled":             `{"version":1,"sources":[{"id":"a","url":"ws://a/ws","enabled":false}]}`,
		"trailing json":            `{"version":1,"sources":[{"id":"a","url":"ws://a/ws"}]} {}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadRelayConfig(writeRelayConfigTestFile(t, body)); err == nil {
				t.Fatal("loadRelayConfig() succeeded")
			}
		})
	}
}

func TestLoadRelayConfigEnforcesSourceLimit(t *testing.T) {
	entries := make([]string, 0, maximumConfiguredSources+1)
	for index := 0; index <= maximumConfiguredSources; index++ {
		entries = append(entries, fmt.Sprintf(`{"id":"source-%d","url":"ws://source-%d/ws"}`, index, index))
	}
	body := fmt.Sprintf(`{"version":1,"sources":[%s]}`, strings.Join(entries, ","))
	if _, err := loadRelayConfig(writeRelayConfigTestFile(t, body)); err == nil {
		t.Fatal("loadRelayConfig() accepted too many sources")
	}
}

func writeRelayConfigTestFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay-config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
