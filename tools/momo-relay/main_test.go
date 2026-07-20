package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRaceMessageForCarAddsViewerCarID(t *testing.T) {
	message, err := raceMessageForCar([]byte(`{"type":"race_state","version":2,"standings":[]}`), "CP-2")
	if err != nil {
		t.Fatalf("raceMessageForCar returned an error: %v", err)
	}
	if !strings.HasPrefix(message, "RACE:") {
		t.Fatalf("message prefix = %q, want RACE:", message)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "RACE:")), &payload); err != nil {
		t.Fatalf("decode race message: %v", err)
	}
	if got := payload["viewerCarId"]; got != "CP-2" {
		t.Fatalf("viewerCarId = %v, want CP-2", got)
	}
}

func TestRaceMessageForCarRejectsEmptyCarID(t *testing.T) {
	if _, err := raceMessageForCar([]byte(`{"type":"race_state","version":2}`), ""); err == nil {
		t.Fatal("raceMessageForCar accepted an empty car ID")
	}
}
