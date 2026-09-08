package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const screenStatsFixture = `{"version":2,"sequence":1,"clientSampledAtUnixMs":1788779596000,"windowMs":1000,
"visibility":"visible","visibilityChanged":false,"peerState":"connected","videoWidth":1280,"videoHeight":720,
"renderCallbackFps":30,"videoFrameAgeMs":10}`

func TestObserverScreenStatsSchema(t *testing.T) {
	if _, err := parseObserverScreenStats(screenStatsFixture); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"future schema":      strings.Replace(screenStatsFixture, `"version":2`, `"version":3`, 1),
		"negative fps":       strings.Replace(screenStatsFixture, `"renderCallbackFps":30`, `"renderCallbackFps":-1`, 1),
		"missing window":     strings.Replace(screenStatsFixture, `"windowMs":1000,`, ``, 1),
		"zero window":        strings.Replace(screenStatsFixture, `"windowMs":1000`, `"windowMs":0`, 1),
		"missing visibility": strings.Replace(screenStatsFixture, `"visibility":"visible",`, ``, 1),
		"spoofed car":        strings.Replace(screenStatsFixture, `"version":2`, `"version":2,"carId":"CP-other"`, 1),
		"old schema":         strings.Replace(screenStatsFixture, `"version":2`, `"version":1`, 1),
		"removed RTC":        strings.Replace(screenStatsFixture, `"version":2`, `"version":2,"rtc":{}`, 1),
		"removed telemetry":  strings.Replace(screenStatsFixture, `"version":2`, `"version":2,"telemetry":{}`, 1),
		"trailing object":    screenStatsFixture + `{}`,
		"oversized":          strings.Repeat(" ", 4097),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseObserverScreenStats(raw); err == nil {
				t.Fatal("invalid diagnostics accepted")
			}
		})
	}
	var missing map[string]any
	_ = json.Unmarshal([]byte(screenStatsFixture), &missing)
	missing["renderCallbackFps"] = nil
	missing["videoFrameAgeMs"] = nil
	raw, _ := json.Marshal(missing)
	sample, err := parseObserverScreenStats(string(raw))
	if err != nil || sample.RenderCallbackFPS != nil || sample.VideoFrameAgeMS != nil {
		t.Fatalf("unknown metrics must remain null: %v %#v", err, sample)
	}
}

func TestObserverScreenStatsRecorderIdentityLimitsAndContext(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	recorder.RecordRaceState(`{}`, telemetryRaceContext{Present: true, RaceRunID: "rr_one", Phase: "green", Sequence: 7})
	r := newStatusTestRelay("11.4", "CP-2")
	r.recorder = recorder
	client := &viewer{id: 42, role: "observer", clientKind: "web-observer"}
	now := time.Now()
	if err := r.recordObserverScreenStats(client, screenStatsFixture, now); err != nil {
		t.Fatal(err)
	}
	if err := r.recordObserverScreenStats(client, screenStatsFixture, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := r.recordObserverScreenStats(client, screenStatsFixture, now.Add(time.Second)); err == nil {
		t.Fatal("duplicate accepted")
	}
	recorder.RecordRaceState(`{}`, telemetryRaceContext{Present: true, RaceRunID: "rr_two", Phase: "ready", Sequence: 1})
	next := strings.Replace(screenStatsFixture, `"sequence":1`, `"sequence":2`, 1)
	if err := r.recordObserverScreenStats(client, next, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*viewer{{role: "pilot", clientKind: "web-observer"}, {role: "observer", clientKind: "recorder"}} {
		if err := r.recordObserverScreenStats(bad, screenStatsFixture, now); err == nil {
			t.Fatal("non-observer accepted")
		}
	}
	if r.viewerCommandAllowed(client) {
		t.Fatal("logging granted control authority")
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	var stats []telemetryLogRecord
	for _, row := range readTelemetryLogRecords(t, recorder.Path()) {
		if row.Type == "observer_stats" {
			stats = append(stats, row)
		}
	}
	if len(stats) != 2 {
		t.Fatalf("record count %d", len(stats))
	}
	if stats[0].SourceID != "11.4" || stats[0].CarID != "CP-2" || stats[0].ViewerID != 42 || stats[0].RelaySessionID == "" ||
		stats[0].RelayReceivedAt == nil || stats[0].RelayElapsedUs == nil || stats[0].RelayVideo == nil ||
		stats[0].RaceRunID != "rr_one" || stats[1].RaceRunID != "rr_two" {
		t.Fatalf("server context lost: %#v", stats)
	}
	if recorder.Stats().ObserverStatsRecords != 2 {
		t.Fatal("stats count incorrect")
	}
	r.recorder = nil
	if err := r.recordObserverScreenStats(client, next, now.Add(3*time.Second)); err == nil {
		t.Fatal("disabled logger accepted")
	}
}

func TestObserverScreenStatsWebSocketToNDJSON(t *testing.T) {
	r, err := newRelay("11.4", "ws://127.0.0.1:1/ws", "CP-2", false,
		defaultRTPStallTimeout, defaultUpstreamStartTimeout, vehicleHealthRecoveryDisabled)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	r.recorder = recorder
	server := httptest.NewServer(http.HandlerFunc(r.serveViewerWS))
	defer server.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/?role=observer&client=web-observer", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	api, err := newH264API()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var message signalMessage
		if err := ws.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message.Type == "error" {
			t.Fatal(message.Error)
		}
		if message.Type == "observer-stats-config" {
			if message.Data != "2" {
				t.Fatal("unexpected schema capability")
			}
			break
		}
	}
	if err := ws.WriteJSON(signalMessage{Type: "observer-stats", Data: screenStatsFixture}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for recorder.Stats().ObserverStatsRecords == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	for _, row := range readTelemetryLogRecords(t, recorder.Path()) {
		if row.Type == "observer_stats" {
			if row.SourceID != "11.4" || row.CarID != "CP-2" || row.ViewerID == 0 || *row.ObserverStats.RenderCallbackFPS != 30 {
				t.Fatalf("wrong persisted stats: %#v", row)
			}
			return
		}
	}
	t.Fatal("WebSocket observation did not reach NDJSON")
}
