package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
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

func TestDisplaySourceStatePriority(t *testing.T) {
	tests := []struct {
		name        string
		lifecycle   sourceLifecycle
		videoHealth sourceVideoHealth
		want        string
	}{
		{"recovering wins", sourceRecovering, videoReceiving, "RECOVERING"},
		{"retry wait is disconnected", sourceRetryWait, videoReceiving, "DISCONNECTED"},
		{"startup waits", sourceWaiting, videoNotStarted, "WAITING"},
		{"watchdog grace is stale", sourceConnected, videoStalled, "STALE"},
		{"fresh rtp streams", sourceConnected, videoReceiving, "STREAMING"},
		{"connected before video connects", sourceConnected, videoNotStarted, "CONNECTING"},
		{"ice negotiation connects", sourceConnecting, videoNotStarted, "CONNECTING"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := displaySourceState(test.lifecycle, test.videoHealth); got != test.want {
				t.Fatalf("displaySourceState() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFrameRateWindowCountsAccessUnitsInLastSecond(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	var window frameRateWindow
	window.recordIngress(base)
	window.recordIngress(base.Add(600 * time.Millisecond))
	window.recordRelayWrite(base.Add(600 * time.Millisecond))
	window.recordRelayWrite(base.Add(900 * time.Millisecond))

	ingress, writes := window.snapshot(base.Add(900 * time.Millisecond))
	if ingress != 2 || writes != 2 {
		t.Fatalf("snapshot at 900ms = ingress %.1f writes %.1f, want 2/2", ingress, writes)
	}
	ingress, writes = window.snapshot(base.Add(1400 * time.Millisecond))
	if ingress != 1 || writes != 2 {
		t.Fatalf("snapshot at 1400ms = ingress %.1f writes %.1f, want 1/2", ingress, writes)
	}
}

func TestOperationsAccessPolicyDefaultsToLoopback(t *testing.T) {
	policy, err := parseOperationsAccessPolicy(nil)
	if err != nil {
		t.Fatalf("parseOperationsAccessPolicy() error = %v", err)
	}
	if !policy.allows("127.0.0.1:8090") || !policy.allows("[::1]:8090") {
		t.Fatal("default policy must allow loopback")
	}
	if policy.allows("192.168.11.20:8090") {
		t.Fatal("default policy must deny LAN clients")
	}
	policy, err = parseOperationsAccessPolicy([]string{"192.168.11.0/24"})
	if err != nil {
		t.Fatalf("parseOperationsAccessPolicy(LAN) error = %v", err)
	}
	if !policy.allows("192.168.11.20:8090") || policy.allows("192.168.12.20:8090") {
		t.Fatal("explicit CIDR allow list did not apply")
	}
}

func TestOperationsStatusAPIIsReadOnlyAndDoesNotExposeRawErrors(t *testing.T) {
	source := newStatusTestRelay("11.3", "CP-1")
	source.lastErrorCode.Store("upstream_signaling_failed")
	server := &relayServer{
		sources:     map[string]*relay{"11.3": source},
		sourceOrder: []string{"11.3"},
	}
	policy, err := parseOperationsAccessPolicy(nil)
	if err != nil {
		t.Fatalf("parseOperationsAccessPolicy() error = %v", err)
	}
	handler := policy.wrap(server.serveOperationsStatus)

	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/status", nil)
	request.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if strings.Contains(recorder.Body.String(), "ws://") || strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("status response exposes sensitive text: %s", recorder.Body.String())
	}
	var response operationsStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if len(response.Sources) != 1 || response.Sources[0].State != "WAITING" {
		t.Fatalf("status sources = %#v, want one waiting source", response.Sources)
	}
	if response.Sources[0].Upstream.LastRtpAgeMs != nil {
		t.Fatal("lastRtpAgeMs must be null before the first RTP frame")
	}
	if got := response.Sources[0].Recovery.LastErrorCode; got == nil || *got != "upstream_signaling_failed" {
		t.Fatalf("lastErrorCode = %#v, want fixed signaling code", got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://relay.test/api/v1/status", nil)
	request.RemoteAddr = "127.0.0.1:40000"
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST status = %d Allow=%q, want 405 GET", recorder.Code, recorder.Header().Get("Allow"))
	}

	request = httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/status", nil)
	request.RemoteAddr = "192.168.11.20:40000"
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestOperationsStatusFollowsConfiguredSourceOrder(t *testing.T) {
	server := &relayServer{
		sources: map[string]*relay{
			"11.4": newStatusTestRelay("11.4", "CP-2"),
			"11.3": newStatusTestRelay("11.3", "CP-1"),
		},
		sourceOrder: []string{"11.3", "11.4"},
	}
	status := server.operationsStatusSnapshot(time.Now())
	if len(status.Sources) != 2 || status.Sources[0].ID != "11.3" || status.Sources[1].ID != "11.4" {
		t.Fatalf("source order = %#v, want 11.3 then 11.4", status.Sources)
	}
}

func TestDownstreamStatusSeparatesLeaseNegotiationConnectionAndChannels(t *testing.T) {
	source := newStatusTestRelay("11.3", "CP-1")
	pilot := &viewer{id: 1, role: "pilot"}
	pilot.state.Store(int32(viewerConnected))
	pilot.telemetry.Store(new(webrtc.DataChannel))
	pilot.race.Store(new(webrtc.DataChannel))
	observer := &viewer{id: 2, role: "observer"}
	observer.state.Store(int32(viewerConnected))
	negotiating := &viewer{id: 3, role: "observer"}
	negotiating.state.Store(int32(viewerNegotiating))
	source.viewers = map[uint64]*viewer{pilot.id: pilot, observer.id: observer, negotiating.id: negotiating}
	source.pilotID = pilot.id

	status := source.downstreamStatusSnapshot()
	if !status.PilotLeaseReserved || status.ConnectedPilots != 1 || status.ConnectedObservers != 1 || status.NegotiatingPeers != 1 {
		t.Fatalf("unexpected downstream state: %#v", status)
	}
	if status.TelemetryOpen != 1 || status.RaceOpen != 1 {
		t.Fatalf("unexpected channel state: %#v", status)
	}

	pilot.telemetry.Store(nil)
	pilot.race.Store(nil)
	status = source.downstreamStatusSnapshot()
	if status.TelemetryOpen != 0 || status.RaceOpen != 0 {
		t.Fatalf("closed channels still counted: %#v", status)
	}
}

func TestOperationsPageHandlerHonorsCIDRAndHTTPMethod(t *testing.T) {
	policy, err := parseOperationsAccessPolicy([]string{"192.168.11.0/24"})
	if err != nil {
		t.Fatalf("parseOperationsAccessPolicy() error = %v", err)
	}
	handler := policy.wrap(operationsPageHandler([]byte("<!doctype html><title>Operations</title>")))

	request := httptest.NewRequest(http.MethodGet, "http://relay.test/operations.html", nil)
	request.RemoteAddr = "192.168.11.20:40000"
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("allowed page response = code %d cache %q content-type %q", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Header().Get("Content-Type"))
	}

	request = httptest.NewRequest(http.MethodGet, "http://relay.test/operations.html", nil)
	request.RemoteAddr = "192.168.12.20:40000"
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-allowed page response = %d, want %d", recorder.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest(http.MethodPost, "http://relay.test/operations.html", nil)
	request.RemoteAddr = "192.168.11.20:40000"
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST page = %d Allow=%q, want 405 GET", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func newStatusTestRelay(name string, carID string) *relay {
	source := &relay{name: name, raceCarID: carID, rtpStallTimeout: 5 * time.Second, upstreamStartTimeout: 20 * time.Second}
	source.lifecycle.Store(int32(sourceWaiting))
	source.videoHealth.Store(int32(videoNotStarted))
	source.upstreamPeerState.Store("new")
	source.lastErrorCode.Store("")
	return source
}
