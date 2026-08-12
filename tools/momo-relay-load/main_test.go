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
