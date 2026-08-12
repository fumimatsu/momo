package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestRaceMessageForCarPreservesTimingStateV2(t *testing.T) {
	state := []byte(`{"type":"race_state","version":2,"raceRunId":"rr_123","sequence":17,"standings":[{"carId":"CP-1","position":1,"lap":4,"status":"racing"},{"carId":"CP-2","position":2,"lap":4,"status":"racing","intervalToAheadMs":1200}]}`)
	message, err := raceMessageForCar(state, "CP-2")
	if err != nil {
		t.Fatalf("raceMessageForCar returned an error: %v", err)
	}
	var payload struct {
		RaceRunID string `json:"raceRunId"`
		Sequence  int    `json:"sequence"`
		ViewerID  string `json:"viewerCarId"`
		Standings []struct {
			CarID             string `json:"carId"`
			IntervalToAheadMs *int   `json:"intervalToAheadMs"`
		} `json:"standings"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "RACE:")), &payload); err != nil {
		t.Fatalf("decode race message: %v", err)
	}
	if payload.RaceRunID != "rr_123" || payload.Sequence != 17 || payload.ViewerID != "CP-2" {
		t.Fatalf("race identity = %#v, want run rr_123 sequence 17 viewer CP-2", payload)
	}
	if len(payload.Standings) != 2 || payload.Standings[0].CarID != "CP-1" || payload.Standings[1].IntervalToAheadMs == nil || *payload.Standings[1].IntervalToAheadMs != 1200 {
		t.Fatalf("standings = %#v, want preserved timing values", payload.Standings)
	}
}

func TestRaceMessageForCarPreservesCanonicalFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("contracts", "sector-progress.race-state-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	message, err := raceMessageForCar(fixture, "CP-1")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		ViewerCarID string `json:"viewerCarId"`
		Standings   []struct {
			CarID       string `json:"carId"`
			SectorTimes []struct {
				Sector int  `json:"sector"`
				LastMS *int `json:"lastMs"`
				BestMS *int `json:"bestMs"`
			} `json:"sectorTimes"`
		} `json:"standings"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "RACE:")), &state); err != nil {
		t.Fatal(err)
	}
	if state.ViewerCarID != "CP-1" || len(state.Standings) == 0 || len(state.Standings[0].SectorTimes) < 2 {
		t.Fatalf("canonical fixture identity was not preserved: %#v", state)
	}
	sector2 := state.Standings[0].SectorTimes[1]
	if sector2.Sector != 2 || sector2.LastMS == nil || *sector2.LastMS != 4700 || sector2.BestMS != nil {
		t.Fatalf("canonical in-progress sector = %#v", sector2)
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

func TestRaceStateAPIProvidesLatestStateWithoutCaching(t *testing.T) {
	server := &relayServer{}
	server.publishGlobalRaceState(`RACE:{"type":"race_state","version":2,"raceId":"race-test","sequence":7,"standings":[]}`)

	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/race-state", nil)
	recorder := httptest.NewRecorder()
	server.serveRaceState(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET race state code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(recorder.Body.String(), `"sequence":7`) {
		t.Fatalf("race state response = %s", recorder.Body.String())
	}
	var state raceStateEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("race state response is not JSON: %v", err)
	}
	if state.Sequence != 7 {
		t.Fatalf("race state sequence = %d, want 7", state.Sequence)
	}

	request = httptest.NewRequest(http.MethodPost, "http://relay.test/api/v1/race-state", nil)
	recorder = httptest.NewRecorder()
	server.serveRaceState(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST race state = %d Allow=%q, want 405 GET", recorder.Code, recorder.Header().Get("Allow"))
	}

	emptyServer := &relayServer{}
	request = httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/race-state", nil)
	recorder = httptest.NewRecorder()
	emptyServer.serveRaceState(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("empty race state = %d, want %d", recorder.Code, http.StatusNoContent)
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

func TestPilotDevicesAPIExposesOnlyPilotSelectionState(t *testing.T) {
	ready := newStatusTestRelay("11.3", "CP-1")
	ready.lifecycle.Store(int32(sourceConnected))
	ready.videoHealth.Store(int32(videoReceiving))
	inUse := newStatusTestRelay("11.4", "CP-2")
	inUse.lifecycle.Store(int32(sourceConnected))
	inUse.videoHealth.Store(int32(videoReceiving))
	inUse.pilotID = 42
	server := &relayServer{
		sources:     map[string]*relay{"11.3": ready, "11.4": inUse},
		sourceOrder: []string{"11.3", "11.4"},
	}
	policy, err := parseOperationsAccessPolicy([]string{"192.168.11.0/24"})
	if err != nil {
		t.Fatalf("parseOperationsAccessPolicy() error = %v", err)
	}
	handler := policy.wrap(server.servePilotDevices)

	request := httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/pilot-devices", nil)
	request.RemoteAddr = "192.168.11.20:40000"
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET pilot devices = %d, want %d", recorder.Code, http.StatusOK)
	}
	if strings.Contains(recorder.Body.String(), "lastErrorCode") || strings.Contains(recorder.Body.String(), "upstream") {
		t.Fatalf("pilot device response exposes operations diagnostics: %s", recorder.Body.String())
	}
	var response pilotDevicesStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pilot device response: %v", err)
	}
	if len(response.Devices) != 2 {
		t.Fatalf("devices = %#v, want two devices", response.Devices)
	}
	if got := response.Devices[0]; got.Device != "11.3" || got.CarID != "CP-1" || got.Availability != "ready" || got.PilotInUse {
		t.Fatalf("ready device = %#v", got)
	}
	if got := response.Devices[1]; got.Availability != "in_use" || !got.PilotInUse {
		t.Fatalf("in-use device = %#v", got)
	}

	request = httptest.NewRequest(http.MethodGet, "http://relay.test/api/v1/pilot-devices", nil)
	request.RemoteAddr = "192.168.12.20:40000"
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-LAN pilot devices = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestTelemetryDiagnosticsClassifiesTextAndBinaryFrames(t *testing.T) {
	source := newStatusTestRelay("11.5", "CP-3")
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte("TEL:{\"v\":1}"), IsString: true}, 1)
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte("TEL:{\"v\":1}")}, 1)
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte("AUD:1,boot,1,0,payload")}, 1)
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte{0x01, 0x02}}, 1)

	telemetry := source.statusSnapshot(time.Now()).Telemetry
	if telemetry.TextTEL != 1 || telemetry.BinaryTEL != 1 || telemetry.BinaryAudio != 1 || telemetry.Other != 1 {
		t.Fatalf("telemetry diagnostics = %#v", telemetry)
	}
}

func TestObserverTelemetrySamplingKeepsEventsAndLimitsState(t *testing.T) {
	client := &viewer{id: 1, role: "observer", clientKind: "web-observer"}
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	state := webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":2,"k":"s","seq":1}`), IsString: true}
	if !shouldDeliverObserverTelemetry(client, state, base) {
		t.Fatal("first observer telemetry state must be delivered")
	}
	if shouldDeliverObserverTelemetry(client, state, base.Add(observerTelemetryInterval-time.Millisecond)) {
		t.Fatal("observer telemetry state inside the sample interval must be dropped")
	}
	if !shouldDeliverObserverTelemetry(client, state, base.Add(observerTelemetryInterval)) {
		t.Fatal("observer telemetry state at the sample interval must be delivered")
	}
	event := webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":2,"k":"e","seq":2}`), IsString: true}
	if !shouldDeliverObserverTelemetry(client, event, base.Add(observerTelemetryInterval+time.Millisecond)) {
		t.Fatal("observer telemetry events must bypass state sampling")
	}
	if !shouldDeliverObserverTelemetry(client, webrtc.DataChannelMessage{Data: []byte(`PIT:1,{}`), IsString: true}, base) {
		t.Fatal("gameplay messages must bypass telemetry sampling")
	}
}

func TestM5AudioMessageClassification(t *testing.T) {
	if !isM5AudioMessage(webrtc.DataChannelMessage{Data: []byte("AUD:1,deadbeef,1,8,ima,AAAA"), IsString: false}) {
		t.Fatal("AUD frame was not classified as M5 audio")
	}
	if isM5AudioMessage(webrtc.DataChannelMessage{Data: []byte("TEL:1,2,3"), IsString: true}) {
		t.Fatal("TEL frame was classified as M5 audio")
	}
}

func TestPilotM5AudioUsesWebSocketOnlyWhileSubscribed(t *testing.T) {
	client := &viewer{id: 1, role: "pilot", audioWS: make(chan string, 1)}
	relay := &relay{viewers: map[uint64]*viewer{client.id: client}}
	message := webrtc.DataChannelMessage{Data: []byte("AUD:1,deadbeef,1,8,ima,AAAA")}

	relay.broadcastTelemetry(message)
	if got := len(client.audioWS); got != 0 {
		t.Fatalf("audio queue length while unsubscribed = %d, want 0", got)
	}

	client.audioSubscribed.Store(true)
	relay.broadcastTelemetry(message)
	if got := <-client.audioWS; got != string(message.Data) {
		t.Fatalf("queued audio = %q, want %q", got, message.Data)
	}
}

func TestRaceStateUsesViewerWebSocketQueue(t *testing.T) {
	client := &viewer{id: 1, role: "pilot", raceWS: make(chan string, 1)}
	relay := &relay{viewers: map[uint64]*viewer{client.id: client}}

	relay.broadcastRaceState(`RACE:{"sequence":7}`)
	if got := <-client.raceWS; got != `RACE:{"sequence":7}` {
		t.Fatalf("queued race state = %q", got)
	}
}

func TestGlobalRaceStateStreamKeepsLatestStateForOneObserverSubscription(t *testing.T) {
	server := &relayServer{}
	id, queue, current := server.subscribeRaceState()
	if current != "" {
		t.Fatalf("initial race state = %q, want empty", current)
	}
	if got := server.raceStreamStatusSnapshot().Subscribers; got != 1 {
		t.Fatalf("race subscribers = %d, want 1", got)
	}

	server.publishGlobalRaceState(`RACE:{"sequence":7}`)
	server.publishGlobalRaceState(`RACE:{"sequence":8}`)
	if got := <-queue; got != `RACE:{"sequence":8}` {
		t.Fatalf("queued race state = %q, want latest sequence", got)
	}
	if got := server.currentGlobalRaceState(); got != `RACE:{"sequence":8}` {
		t.Fatalf("current race state = %q", got)
	}
	status := server.raceStreamStatusSnapshot()
	if status.PublishedMessages != 2 || status.PublishedBytes == 0 || status.QueueReplacements != 1 {
		t.Fatalf("race publish diagnostics = %#v", status)
	}
	if status.LastPublishedAt == nil || status.DeliveredMessages != 0 || status.LastDeliveredAt != nil {
		t.Fatalf("race publish timestamps = %#v", status)
	}

	server.unsubscribeRaceState(id)
	if got := server.raceStreamStatusSnapshot().Subscribers; got != 0 {
		t.Fatalf("race subscribers = %d, want 0", got)
	}
}

func TestRaceStateWebSocketSendsLatestStateAndUnsubscribes(t *testing.T) {
	server := &relayServer{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serveRaceStateWS))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.publishGlobalRaceState(`RACE:{"sequence":9}`)
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var message signalMessage
	if err := connection.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "race-state" || message.Data != `RACE:{"sequence":9}` {
		t.Fatalf("race stream message = %#v", message)
	}
	deadline := time.Now().Add(time.Second)
	var status raceStreamOperationsState
	for {
		status = server.raceStreamStatusSnapshot()
		if status.DeliveredMessages == 1 && status.DeliveredBytes == uint64(len(message.Data)) && status.LastDeliveredAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("race delivery diagnostics = %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	if status.WriteErrors != 0 {
		t.Fatalf("race delivery write errors = %#v", status)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(time.Second)
	for {
		server.raceStreamMu.RLock()
		remaining := len(server.raceSubscribers)
		server.raceStreamMu.RUnlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("race subscribers after close = %d, want 0", remaining)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInitialWebDownlinkStateUsesSignalingMessages(t *testing.T) {
	now := time.Now()
	health := newVehicleHealth(now)
	relay := &relay{
		name:          "11.4",
		raceCarID:     "CP-2",
		vehicleHealth: health,
		pitPresence:   newPitPresenceState("CP-2", health.snapshot(now).HP),
		vehicleEvents: newVehicleEventStore(),
	}
	var messages []signalMessage
	if err := relay.sendInitialWebDownlinkState(func(message signalMessage) error {
		messages = append(messages, message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("initial messages = %d, want 4", len(messages))
	}
	for index := 0; index < 3; index++ {
		if messages[index].Type != "telemetry" || messages[index].Data == "" {
			t.Fatalf("initial gameplay message %d = %#v", index, messages[index])
		}
	}
	if messages[3].Type != "vehicle-event" || messages[3].Data == "" {
		t.Fatalf("initial event message = %#v", messages[3])
	}
}

func TestBinaryTELIsNormalizedForViewerDelivery(t *testing.T) {
	normalized, raw, isTEL, wasBinaryTEL := normalizeTelemetryMessage(
		webrtc.DataChannelMessage{Data: []byte("TEL:{\"v\":1}")},
	)
	if !normalized.IsString || raw != "TEL:{\"v\":1}" || !isTEL || !wasBinaryTEL {
		t.Fatalf("normalized binary TEL = %#v raw=%q isTEL=%t wasBinaryTEL=%t", normalized, raw, isTEL, wasBinaryTEL)
	}
}

func TestDownstreamStatusSeparatesLeaseNegotiationConnectionAndChannels(t *testing.T) {
	source := newStatusTestRelay("11.3", "CP-1")
	pilot := &viewer{id: 1, role: "pilot"}
	pilot.state.Store(int32(viewerConnected))
	pilot.telemetry.Store(new(webrtc.DataChannel))
	pilot.race.Store(new(webrtc.DataChannel))
	pilot.events.Store(new(webrtc.DataChannel))
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
	if status.TelemetryOpen != 1 || status.RaceOpen != 1 || status.EventsOpen != 1 {
		t.Fatalf("unexpected channel state: %#v", status)
	}

	pilot.telemetry.Store(nil)
	pilot.race.Store(nil)
	pilot.events.Store(nil)
	status = source.downstreamStatusSnapshot()
	if status.TelemetryOpen != 0 || status.RaceOpen != 0 || status.EventsOpen != 0 {
		t.Fatalf("closed channels still counted: %#v", status)
	}
}

func TestCommandDropLogIsRateLimitedPerViewer(t *testing.T) {
	client := &viewer{id: 7}
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if !shouldLogCommandDrop(client, base) {
		t.Fatal("first unavailable command must be logged")
	}
	if shouldLogCommandDrop(client, base.Add(999*time.Millisecond)) {
		t.Fatal("repeated unavailable command inside one second must be suppressed")
	}
	if !shouldLogCommandDrop(client, base.Add(time.Second)) {
		t.Fatal("unavailable command at the one-second boundary must be logged")
	}
	if !shouldLogCommandDrop(&viewer{id: 8}, base.Add(time.Millisecond)) {
		t.Fatal("a different viewer must have an independent log interval")
	}
}

func TestDriveStateTracksCurrentPilotGear(t *testing.T) {
	source := newStatusTestRelay("11.5", "CP-3")
	pilot := &viewer{id: 7, role: "pilot"}
	source.viewers = map[uint64]*viewer{pilot.id: pilot}
	source.pilotID = pilot.id

	source.handleDriveState(pilot, webrtc.DataChannelMessage{Data: []byte("GEAR:3"), IsString: true})
	if got := source.driveGear.Load(); got != 3 {
		t.Fatalf("drive gear = %d, want 3", got)
	}
	source.handleDriveState(pilot, webrtc.DataChannelMessage{Data: []byte("GEAR:6"), IsString: true})
	if got := source.driveGear.Load(); got != 3 {
		t.Fatalf("invalid gear changed state to %d", got)
	}
	source.handleDriveState(&viewer{id: 8, role: "observer"}, webrtc.DataChannelMessage{Data: []byte("GEAR:2"), IsString: true})
	if got := source.driveGear.Load(); got != 3 {
		t.Fatalf("observer gear changed state to %d", got)
	}
}

func TestCommandAuditAddsGearWithoutChangingUpstreamMessage(t *testing.T) {
	message := webrtc.DataChannelMessage{Data: []byte("S:1500,T:1800\n"), IsString: true}
	audit := commandAuditWithGear(message, 3)
	if got := string(audit.Data); got != "S:1500,T:1800,G:3" {
		t.Fatalf("command audit = %q", got)
	}
	if got := string(message.Data); got != "S:1500,T:1800\n" {
		t.Fatalf("upstream command was changed to %q", got)
	}
	for _, item := range []struct {
		message webrtc.DataChannelMessage
		gear    int32
	}{
		{webrtc.DataChannelMessage{Data: []byte("S:1500,T:1800\n"), IsString: true}, 0},
		{webrtc.DataChannelMessage{Data: []byte("PING:1"), IsString: true}, 3},
		{webrtc.DataChannelMessage{Data: []byte("S:1500,T:1800")}, 3},
	} {
		got := commandAuditWithGear(item.message, item.gear)
		if string(got.Data) != string(item.message.Data) || got.IsString != item.message.IsString {
			t.Fatalf("unexpected audit mutation: %#v -> %#v", item.message, got)
		}
	}
}

func TestTelemetryDeliveryLogIsRateLimitedPerViewer(t *testing.T) {
	client := &viewer{id: 7, role: "pilot"}
	base := time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
	if !shouldLogTelemetryDelivery(client, base) {
		t.Fatal("first telemetry delivery must be logged")
	}
	if shouldLogTelemetryDelivery(client, base.Add(telemetryDeliveryLogInterval-time.Millisecond)) {
		t.Fatal("telemetry delivery inside the interval must be suppressed")
	}
	if !shouldLogTelemetryDelivery(client, base.Add(telemetryDeliveryLogInterval)) {
		t.Fatal("telemetry delivery at the interval boundary must be logged")
	}
	if shouldLogTelemetryDelivery(nil, base) {
		t.Fatal("nil viewer must not request a telemetry delivery log")
	}
}

func TestEnqueueLatestTelemetryReplacesStaleValue(t *testing.T) {
	queue := make(chan string, 1)
	enqueueLatestTelemetry(queue, "TEL:old")
	enqueueLatestTelemetry(queue, "TEL:new")

	select {
	case got := <-queue:
		if got != "TEL:new" {
			t.Fatalf("latest telemetry = %q, want TEL:new", got)
		}
	default:
		t.Fatal("latest telemetry was not queued")
	}
}

func TestGameplayTelemetryUsesOrderedQueueDuringTelemetryFlood(t *testing.T) {
	client := &viewer{
		id:          1,
		role:        "pilot",
		clientKind:  "web-pilot",
		telemetryWS: make(chan string, 1),
		gameplayWS:  make(chan string, 8),
	}
	relay := &relay{viewers: map[uint64]*viewer{client.id: client}}
	gameplay := []string{
		"VHS:1,80.0,1.000,healthy",
		`VGS:1,{"hp":80,"fuel":40}`,
		`PIT:1,{"present":true,"serviceState":"servicing"}`,
	}
	for _, payload := range gameplay {
		relay.broadcastTelemetry(webrtc.DataChannelMessage{Data: []byte(payload), IsString: true})
	}
	for index := 0; index < 100; index++ {
		payload := fmt.Sprintf(`TEL:{"v":2,"k":"s","seq":%d}`, index)
		relay.broadcastTelemetry(webrtc.DataChannelMessage{Data: []byte(payload), IsString: true})
	}

	for index, want := range gameplay {
		select {
		case got := <-client.gameplayWS:
			if got != want {
				t.Fatalf("gameplay message %d = %q, want %q", index, got, want)
			}
		default:
			t.Fatalf("gameplay message %d was dropped", index)
		}
	}
	select {
	case got := <-client.telemetryWS:
		if got != `TEL:{"v":2,"k":"s","seq":99}` {
			t.Fatalf("latest ordinary telemetry = %q", got)
		}
	default:
		t.Fatal("ordinary telemetry queue is empty")
	}
}

func TestGameplayTelemetryClassification(t *testing.T) {
	for _, payload := range []string{"VHS:1,100,1,healthy", "VGS:1,{}", "PIT:1,{}"} {
		if !isVehicleGameplayTelemetry(webrtc.DataChannelMessage{Data: []byte(payload), IsString: true}) {
			t.Fatalf("gameplay payload was not classified: %q", payload)
		}
	}
	for _, message := range []webrtc.DataChannelMessage{
		{Data: []byte(`TEL:{"v":2}`), IsString: true},
		{Data: []byte("VGS:1,{}"), IsString: false},
	} {
		if isVehicleGameplayTelemetry(message) {
			t.Fatalf("non-gameplay payload was classified: %#v", message)
		}
	}
}

func TestTelemetryDataChannelSaturationUsesHighWatermark(t *testing.T) {
	if telemetryDataChannelSaturated(telemetryDataHighWatermark - 1) {
		t.Fatal("buffer below the high watermark must remain writable")
	}
	if !telemetryDataChannelSaturated(telemetryDataHighWatermark) {
		t.Fatal("buffer at the high watermark must drop telemetry")
	}
}

func TestEnqueueVehicleEventPreservesOrderAndReportsFullQueue(t *testing.T) {
	queue := make(chan string, 2)
	if !enqueueVehicleEvent(queue, "event-1") || !enqueueVehicleEvent(queue, "event-2") {
		t.Fatal("vehicle events must be queued while capacity remains")
	}
	if enqueueVehicleEvent(queue, "event-3") {
		t.Fatal("a full vehicle event queue must reject the new event")
	}
	if first, second := <-queue, <-queue; first != "event-1" || second != "event-2" {
		t.Fatalf("vehicle event order = %q, %q", first, second)
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

func TestEmbeddedWebObserverAssetsAreComplete(t *testing.T) {
	paths := []string{
		"web/observer.html",
		"web/observer.css",
		"web/observer.js",
		"web/observer-core.js",
		"web/observer-config.json",
		"web/telemetry.js",
	}
	for _, path := range paths {
		contents, err := webAssets.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded asset %s: %v", path, err)
		}
		if len(contents) == 0 {
			t.Fatalf("embedded asset %s is empty", path)
		}
	}

	html, err := webAssets.ReadFile("web/observer.html")
	if err != nil {
		t.Fatalf("read embedded observer HTML: %v", err)
	}
	for _, reference := range []string{"observer.css", "/telemetry.js", "observer.js"} {
		if !strings.Contains(string(html), reference) {
			t.Fatalf("observer HTML does not reference %q", reference)
		}
	}
}

func TestEmbeddedWebPilotAssetsAreComplete(t *testing.T) {
	paths := []string{
		"web/pilot.html",
		"web/pilot.js",
		"web/race-battle.js",
		"web/telemetry.js",
		"web/m5-audio.js",
		"web/ffb-bridge.js",
	}
	for _, path := range paths {
		contents, err := webAssets.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded asset %s: %v", path, err)
		}
		if len(contents) == 0 {
			t.Fatalf("embedded asset %s is empty", path)
		}
	}

	html, err := webAssets.ReadFile("web/pilot.html")
	if err != nil {
		t.Fatalf("read embedded Pilot HTML: %v", err)
	}
	for _, reference := range []string{"telemetry.js", "m5-audio.js", "ffb-bridge.js", "race-battle.js", "pilot.js"} {
		if !strings.Contains(string(html), reference) {
			t.Fatalf("Pilot HTML does not reference %q", reference)
		}
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
