package main

import (
	"net/http/httptest"
	"testing"
)

func TestStatusCountsConnectedClients(t *testing.T) {
	first := &loadClient{id: "first", role: "pilot"}
	second := &loadClient{id: "second", role: "observer"}
	first.connected.Store(true)
	first.rtpFrames.Store(10)
	runner := &loadRunner{clients: []*loadClient{first, second}}
	recorder := httptest.NewRecorder()
	runner.serveStatus(recorder, httptest.NewRequest("GET", "/api/v1/status", nil))
	if recorder.Code != 200 {
		t.Fatalf("status=%d", recorder.Code)
	}
	if body := recorder.Body.String(); body == "" {
		t.Fatal("empty response")
	}
}

func TestParsePilotSourcesCombinesCompatibilityAndListInputs(t *testing.T) {
	sources, err := parsePilotSources("virtual-01", " virtual-02,virtual-03 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"virtual-01", "virtual-02", "virtual-03"}
	if len(sources) != len(want) {
		t.Fatalf("sources=%v want=%v", sources, want)
	}
	for index := range want {
		if sources[index] != want[index] {
			t.Fatalf("sources=%v want=%v", sources, want)
		}
	}
}

func TestParsePilotSourcesRejectsDuplicates(t *testing.T) {
	if _, err := parsePilotSources("virtual-01", "virtual-02,virtual-01"); err == nil {
		t.Fatal("expected duplicate pilot source error")
	}
}
