package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

func TestMomoEventsDataChannelEndToEnd(t *testing.T) {
	source, err := newRelay(
		"11.3",
		"ws://127.0.0.1:1/ws",
		"CP-1",
		false,
		defaultRTPStallTimeout,
		defaultUpstreamStartTimeout,
		vehicleHealthRecoveryDisabled,
	)
	if err != nil {
		t.Fatalf("create relay source: %v", err)
	}

	now := time.Now()
	source.vehicleHealth.observeRaceState(true, "rr_events_e2e", "green", 1, 2, now)
	source.resetVehicleEvents("rr_events_e2e")
	if !source.vehicleEvents.add(vehicleImpactEvent{
		Type:          "vehicle_event",
		Version:       1,
		EventID:       "CP-1:boot-before-connect:9",
		RaceRunID:     "rr_events_e2e",
		CarID:         "CP-1",
		ImpactClass:   "weak",
		DamageApplied: false,
		HPBefore:      100,
		HPAfter:       100,
	}) {
		t.Fatal("seed snapshot event was not stored")
	}
	dispatchContext, stopDispatcher := context.WithCancel(context.Background())
	defer stopDispatcher()
	go source.runVehicleEventDispatcher(dispatchContext)

	server := &relayServer{sources: map[string]*relay{"11.3": source}}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serveViewerWS))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/?device=11.3&role=observer"
	signaling, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("connect Relay signaling: %v", err)
	}
	defer signaling.Close()

	clientAPI, err := newH264API()
	if err != nil {
		t.Fatalf("create client WebRTC API: %v", err)
	}
	peer, err := clientAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create client peer connection: %v", err)
	}
	defer peer.Close()
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("add video transceiver: %v", err)
	}

	ordered := true
	eventsChannel, err := peer.CreateDataChannel(eventsLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("create events DataChannel: %v", err)
	}
	unordered := false
	maxRetransmits := uint16(0)
	telemetryChannel, err := peer.CreateDataChannel(telemetryLabel, &webrtc.DataChannelInit{
		Ordered:        &unordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		t.Fatalf("create telemetry DataChannel: %v", err)
	}
	commandChannel, err := peer.CreateDataChannel(commandLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("create command DataChannel: %v", err)
	}

	eventsOpen := make(chan struct{})
	telemetryOpen := make(chan struct{})
	commandOpen := make(chan struct{})
	eventMessages := make(chan string, 8)
	eventsChannel.OnOpen(func() { close(eventsOpen) })
	eventsChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		eventMessages <- string(message.Data)
	})
	telemetryMessages := make(chan string, 16)
	telemetryChannel.OnOpen(func() { close(telemetryOpen) })
	telemetryChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		telemetryMessages <- string(message.Data)
	})
	commandChannel.OnOpen(func() { close(commandOpen) })

	signalingErrors := make(chan error, 8)
	answerSet := make(chan struct{})
	var writeMu sync.Mutex
	writeSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return signaling.WriteJSON(message)
	}
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := writeSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			reportE2EError(signalingErrors, fmt.Errorf("send client ICE candidate: %w", err))
		}
	})
	go readRelaySignaling(peer, signaling, answerSet, signalingErrors)

	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local offer: %v", err)
	}
	if err := writeSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		t.Fatalf("send offer: %v", err)
	}
	waitForE2ESignal(t, "Relay answer", answerSet, signalingErrors)
	waitForE2ESignal(t, "momo-events open", eventsOpen, signalingErrors)
	waitForE2ESignal(t, "momo-telemetry open", telemetryOpen, signalingErrors)
	waitForE2ESignal(t, "momo-command open", commandOpen, signalingErrors)

	if !eventsChannel.Ordered() || eventsChannel.MaxRetransmits() != nil || eventsChannel.MaxPacketLifeTime() != nil {
		t.Fatalf(
			"momo-events reliability = ordered:%t maxRetransmits:%v maxPacketLifeTime:%v",
			eventsChannel.Ordered(),
			eventsChannel.MaxRetransmits(),
			eventsChannel.MaxPacketLifeTime(),
		)
	}

	snapshotMessage := waitForE2EMessage(t, "initial vehicle event snapshot", eventMessages, signalingErrors, nil)
	var snapshot vehicleEventSnapshot
	if err := json.Unmarshal([]byte(snapshotMessage), &snapshot); err != nil {
		t.Fatalf("decode initial snapshot: %v", err)
	}
	if snapshot.Type != "vehicle_event_snapshot" || snapshot.Version != 1 || snapshot.RaceRunID != "rr_events_e2e" ||
		len(snapshot.Events) != 1 || snapshot.Events[0].EventID != "CP-1:boot-before-connect:9" {
		t.Fatalf("initial snapshot = %#v", snapshot)
	}

	impact := `TEL:{"v":2,"k":"e","boot":"boot-e2e","seq":1,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(impact), IsString: true}, 1)

	liveMessage := waitForE2EMessage(t, "live vehicle event", eventMessages, signalingErrors, func(message string) bool {
		return strings.Contains(message, `"type":"vehicle_event"`)
	})
	var event vehicleImpactEvent
	if err := json.Unmarshal([]byte(liveMessage), &event); err != nil {
		t.Fatalf("decode live event: %v", err)
	}
	if event.EventID != "CP-1:boot-e2e:1" || event.RaceRunID != "rr_events_e2e" || event.CarID != "CP-1" ||
		event.ImpactClass != "strong" || !event.DamageApplied || event.Damage != 12 || event.HPBefore != 100 || event.HPAfter != 88 {
		t.Fatalf("live event = %#v", event)
	}

	waitForE2EMessage(t, "original impact telemetry", telemetryMessages, signalingErrors, func(message string) bool {
		return message == impact
	})
	if commandChannel.ReadyState() != webrtc.DataChannelStateOpen {
		t.Fatalf("command DataChannel state after event = %s", commandChannel.ReadyState())
	}

	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(impact), IsString: true}, 1)
	select {
	case duplicate := <-eventMessages:
		t.Fatalf("duplicate impact produced another momo-events message: %s", duplicate)
	case err := <-signalingErrors:
		t.Fatalf("signaling failed while checking duplicate suppression: %v", err)
	case <-time.After(800 * time.Millisecond):
	}
	if hp := source.vehicleHealth.snapshot(time.Now()).HP; hp != 88 {
		t.Fatalf("HP after duplicate impact = %.1f, want 88", hp)
	}
}

func readRelaySignaling(
	peer *webrtc.PeerConnection,
	signaling *websocket.Conn,
	answerSet chan<- struct{},
	errors chan<- error,
) {
	var pendingCandidates []webrtc.ICECandidateInit
	remoteDescriptionSet := false
	for {
		var message signalMessage
		if err := signaling.ReadJSON(&message); err != nil {
			reportE2EError(errors, fmt.Errorf("read Relay signaling: %w", err))
			return
		}
		switch message.Type {
		case "answer":
			if remoteDescriptionSet {
				continue
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				reportE2EError(errors, fmt.Errorf("set Relay answer: %w", err))
				return
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := peer.AddICECandidate(candidate); err != nil {
					reportE2EError(errors, fmt.Errorf("apply pending Relay ICE candidate: %w", err))
					return
				}
			}
			pendingCandidates = nil
			close(answerSet)
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := peer.AddICECandidate(*message.ICE); err != nil {
				reportE2EError(errors, fmt.Errorf("apply Relay ICE candidate: %w", err))
				return
			}
		case "error":
			reportE2EError(errors, fmt.Errorf("Relay signaling error: %s", message.Error))
			return
		}
	}
}

func waitForE2ESignal(t *testing.T, name string, signal <-chan struct{}, errors <-chan error) {
	t.Helper()
	select {
	case <-signal:
	case err := <-errors:
		t.Fatalf("%s failed: %v", name, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForE2EMessage(
	t *testing.T,
	name string,
	messages <-chan string,
	errors <-chan error,
	accept func(string) bool,
) string {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-messages:
			if accept == nil || accept(message) {
				return message
			}
		case err := <-errors:
			t.Fatalf("%s failed: %v", name, err)
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", name)
		}
	}
}

func reportE2EError(errors chan<- error, err error) {
	select {
	case errors <- err:
	default:
	}
}
