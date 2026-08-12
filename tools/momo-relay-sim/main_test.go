package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateConfig(t *testing.T) {
	valid := config{listen: "127.0.0.1:18080", fps: 30, packetsPerFrame: 8, payloadBytes: 1200, telemetryHz: 15}
	if err := validateConfig(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	tests := []config{
		{listen: "", fps: 30, packetsPerFrame: 8, payloadBytes: 1200},
		{listen: "x", fps: 0, packetsPerFrame: 8, payloadBytes: 1200},
		{listen: "x", fps: 30, packetsPerFrame: 0, payloadBytes: 1200},
		{listen: "x", fps: 30, packetsPerFrame: 8, payloadBytes: 1500},
		{listen: "x", fps: 30, packetsPerFrame: 8, payloadBytes: 1200, telemetryHz: 121},
	}
	for i, test := range tests {
		if err := validateConfig(test); err == nil {
			t.Fatalf("case %d unexpectedly passed", i)
		}
	}
}

func TestDisconnectRejectsGet(t *testing.T) {
	sim := &simulator{sessions: make(map[*session]struct{})}
	recorder := httptest.NewRecorder()
	sim.serveDisconnect(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/disconnect", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", recorder.Code)
	}
}
