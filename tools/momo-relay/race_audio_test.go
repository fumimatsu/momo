package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestRaceAudioDetectorIgnoresHistoryOnInitialState(t *testing.T) {
	detector := raceAudioDetector{}
	initial := raceAudioTestState("run-1", "green", 1, 13444)
	if events := detector.observe(initial, "CP-1"); len(events) != 0 {
		t.Fatalf("initial state emitted %d events", len(events))
	}
	if events := detector.observe(initial, "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate state emitted %d events", len(events))
	}
}

func TestRaceAudioDetectorEmitsNewLapAndFinishOnce(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(raceAudioTestState("run-1", "green", 1, 13444), "CP-1")
	events := detector.observe(raceAudioTestState("run-1", "green", 2, 13120), "CP-1")
	if len(events) != 1 || events[0].Kind != "lap_complete" || !strings.Contains(events[0].EnglishText, "Lap 2") {
		t.Fatalf("unexpected lap events: %#v", events)
	}
	events = detector.observe(raceAudioTestState("run-1", "finished", 2, 13120), "CP-1")
	if len(events) != 1 || events[0].Kind != "race_finish" {
		t.Fatalf("unexpected finish events: %#v", events)
	}
	if events := detector.observe(raceAudioTestState("run-1", "finished", 2, 13120), "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate finish emitted %d events", len(events))
	}
}

func TestRaceAudioEnglishTemplatesAreShortAndOmitUnknownPosition(t *testing.T) {
	if got, want := raceAudioEnglishLapText(4, 13715, 2), "Lap 4. 13.715. P 2."; got != want {
		t.Fatalf("lap text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishLapText(4, 13715, 0), "Lap 4. 13.715."; got != want {
		t.Fatalf("lap text without position = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishFinishText(2), "Race finished. P 2."; got != want {
		t.Fatalf("finish text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishFinishText(0), "Race finished."; got != want {
		t.Fatalf("finish text without position = %q, want %q", got, want)
	}
}

func TestRaceAudioServiceClientDefaultsToMichael(t *testing.T) {
	client, err := newRaceAudioServiceClient(
		"http://127.0.0.1:18090", "", "en-US", "", "", 1.04,
	)
	if err != nil {
		t.Fatal(err)
	}
	if client.englishVoice != "am_michael" {
		t.Fatalf("English voice = %q, want am_michael", client.englishVoice)
	}
}

func TestRaceAudioDetectorResetsForNewRunWithoutReplayingHistory(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(raceAudioTestState("run-1", "green", 1, 13444), "CP-1")
	if events := detector.observe(raceAudioTestState("run-2", "green", 1, 13000), "CP-1"); len(events) != 0 {
		t.Fatalf("new run baseline emitted %d events", len(events))
	}
}

func TestRaceAudioDetectorRejectsStateWithoutRunIdentity(t *testing.T) {
	detector := raceAudioDetector{}
	var state map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raceAudioTestState("run-1", "green", 1, 13444), "RACE:")), &state); err != nil {
		t.Fatal(err)
	}
	state["raceId"] = ""
	state["raceRunId"] = ""
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	message := "RACE:" + string(payload)
	if events := detector.observe(message, "CP-1"); len(events) != 0 || detector.initialized {
		t.Fatalf("state without run identity changed detector: events=%#v initialized=%v", events, detector.initialized)
	}
}

func TestRaceAudioServiceClientUsesBearerTokenAndDecodesPackets(t *testing.T) {
	const token = "test-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("unexpected authorization: %q", request.Header.Get("Authorization"))
		}
		var payload raceAudioSynthesisRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Language != "ja-JP" || payload.Voice != "jf_alpha" {
			t.Fatalf("unexpected request: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(raceAudioSynthesisResponse{
			Version:         1,
			Codec:           "opus",
			ClockRate:       48000,
			Channels:        1,
			FrameDurationMS: 20,
			DurationMS:      20,
			Packets:         []string{base64.StdEncoding.EncodeToString([]byte{0xf8, 0xff, 0xfe})},
		})
	}))
	defer server.Close()
	client, err := newRaceAudioServiceClient(server.URL, token, "en-US", "af_heart", "jf_alpha", 1.04)
	if err != nil {
		t.Fatal(err)
	}
	clip, duration, err := client.synthesize(context.Background(), raceAudioEvent{
		EventID:      "run-1:CP-1:lap:1:13444",
		Kind:         "lap_complete",
		EnglishText:  "Lap one complete.",
		JapaneseText: "1 周目。",
	}, "ja-JP")
	if err != nil {
		t.Fatal(err)
	}
	if duration != 20 || len(clip.packets) != 1 {
		t.Fatalf("unexpected clip duration=%d packets=%d", duration, len(clip.packets))
	}
}

func TestRaceAudioServiceClientRequiresTokenForNonLoopbackURL(t *testing.T) {
	if _, err := newRaceAudioServiceClient(
		"http://192.168.11.105:18090", "", "en-US", "af_heart", "jf_alpha", 1.04,
	); err == nil || !strings.Contains(err.Error(), "TOKEN is required") {
		t.Fatalf("non-loopback URL without token returned %v", err)
	}
	if _, err := newRaceAudioServiceClient(
		"http://127.0.0.1:18090", "", "en-US", "af_heart", "jf_alpha", 1.04,
	); err != nil {
		t.Fatalf("loopback fixture URL was rejected: %v", err)
	}
}

func TestValidateRaceAudioResponseRejectsDurationMismatch(t *testing.T) {
	_, err := validateRaceAudioResponse(raceAudioSynthesisResponse{
		Version:         1,
		Codec:           "opus",
		ClockRate:       48000,
		Channels:        1,
		FrameDurationMS: 20,
		DurationMS:      41,
		Packets: []string{
			base64.StdEncoding.EncodeToString([]byte{1}),
			base64.StdEncoding.EncodeToString([]byte{2}),
		},
	})
	if err == nil {
		t.Fatal("duration mismatch was accepted")
	}
}

func TestRaceAudioTrackDeliversOpusWithoutRenegotiation(t *testing.T) {
	latency := measureRaceAudioTrackDelivery(t, raceAudioClip{
		event:   raceAudioEvent{EventID: "test", Kind: "lap_complete"},
		packets: [][]byte{{0x18, 0x01, 0x02, 0x03}},
	})
	if latency > time.Second {
		t.Fatalf("race audio track delivery took %s", latency)
	}
}

func TestRaceAudioExternalServiceDeliversEnglishAndJapanese(t *testing.T) {
	serviceURL := strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_TEST_URL"))
	if serviceURL == "" {
		t.Skip("MOMO_RACE_AUDIO_TEST_URL is not set")
	}
	englishVoice := strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_TEST_ENGLISH_VOICE"))
	if englishVoice == "" {
		englishVoice = raceAudioDefaultEnglishVoice
	}
	japaneseVoice := strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_TEST_JAPANESE_VOICE"))
	if japaneseVoice == "" {
		japaneseVoice = raceAudioDefaultJapaneseVoice
	}
	client, err := newRaceAudioServiceClient(
		serviceURL,
		strings.TrimSpace(os.Getenv("MOMO_RACE_AUDIO_TEST_TOKEN")),
		"en-US",
		englishVoice,
		japaneseVoice,
		1.04,
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		language string
		event    raceAudioEvent
	}{
		{
			language: "en-US",
			event: raceAudioEvent{
				EventID:      "benchmark-run:CP-1:lap:4",
				Kind:         "lap_complete",
				EnglishText:  "Lap 4. 13.715. P 2.",
				JapaneseText: "4 周目。13.715 秒。現在 2 位。",
			},
		},
		{
			language: "ja-JP",
			event: raceAudioEvent{
				EventID:      "benchmark-run:CP-1:lap:5",
				Kind:         "lap_complete",
				EnglishText:  "Lap 5 complete. 13.620 seconds. Position 2.",
				JapaneseText: "5 周目。13.620 秒。現在 2 位。",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.language, func(t *testing.T) {
			started := time.Now()
			clip, durationMS, err := client.synthesize(context.Background(), testCase.event, testCase.language)
			if err != nil {
				t.Fatal(err)
			}
			synthesisLatency := time.Since(started)
			trackLatency := measureRaceAudioTrackDelivery(t, clip)
			totalLatency := synthesisLatency + trackLatency
			t.Logf(
				"language=%s synthesis=%s track=%s total=%s audio=%dms packets=%d",
				testCase.language,
				synthesisLatency.Round(time.Millisecond),
				trackLatency.Round(time.Millisecond),
				totalLatency.Round(time.Millisecond),
				durationMS,
				len(clip.packets),
			)
			if totalLatency > 2500*time.Millisecond {
				t.Fatalf("race audio exceeded the 2.5 second delivery target: %s", totalLatency)
			}

			cacheStarted := time.Now()
			if _, _, err := client.synthesize(context.Background(), testCase.event, testCase.language); err != nil {
				t.Fatal(err)
			}
			t.Logf("language=%s cache=%s", testCase.language, time.Since(cacheStarted).Round(time.Millisecond))
		})
	}
}

func measureRaceAudioTrackDelivery(t *testing.T, clip raceAudioClip) time.Duration {
	t.Helper()
	source, err := newRelay("11.3", "ws://127.0.0.1:1/ws", "CP-1", false,
		defaultRTPStallTimeout, defaultUpstreamStartTimeout, vehicleHealthRecoveryDisabled)
	if err != nil {
		t.Fatal(err)
	}
	source.raceAudio = &raceAudioSource{relay: source, service: &raceAudioServiceClient{defaultLanguage: "en-US"}}
	serverPeer, err := source.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer serverPeer.Close()
	clientPeer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPeer.Close()
	clientConnected := make(chan struct{})
	var clientConnectedOnce sync.Once
	clientPeer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			clientConnectedOnce.Do(func() { close(clientConnected) })
		}
	})
	if _, err := clientPeer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	payloads := make(chan []byte, 8)
	clientPeer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, readErr := track.ReadRTP()
				if readErr != nil {
					return
				}
				payloads <- append([]byte(nil), packet.Payload...)
			}
		}()
	})
	viewer := &viewer{id: 1, role: "pilot"}
	if err := source.configureRaceAudioPeer(viewer, serverPeer); err != nil {
		t.Fatal(err)
	}
	defer viewer.raceAudioStopOnce.Do(func() { close(viewer.raceAudioStop) })
	offer, err := clientPeer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientGathering := webrtc.GatheringCompletePromise(clientPeer)
	if err := clientPeer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-clientGathering
	if err := serverPeer.SetRemoteDescription(*clientPeer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := serverPeer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverGathering := webrtc.GatheringCompletePromise(serverPeer)
	if err := serverPeer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-serverGathering
	if err := clientPeer.SetRemoteDescription(*serverPeer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientConnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the audio peer connection")
	}
	if len(clip.packets) == 0 {
		t.Fatal("race audio clip has no packets")
	}
	testPayload := clip.packets[0]
	started := time.Now()
	viewer.raceAudioQueue <- clip
	deadline := time.After(5 * time.Second)
	for {
		select {
		case payload := <-payloads:
			if string(payload) == string(testPayload) {
				return time.Since(started)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the race audio Opus payload")
		}
	}
}

func raceAudioTestState(runID string, phase string, newestLap int, newestLapMS int) string {
	history := make([]map[string]any, 0, newestLap)
	for lap := 1; lap <= newestLap; lap++ {
		lapTimeMS := 14000 + lap
		if lap == newestLap {
			lapTimeMS = newestLapMS
		}
		history = append(history, map[string]any{
			"carId": "CP-1", "lap": lap, "lapTimeMs": lapTimeMS,
		})
	}
	payload := map[string]any{
		"type": "race_state", "version": 2, "raceId": "race-test", "raceRunId": runID,
		"phase": phase, "viewerCarId": "CP-1",
		"standings":  []map[string]any{{"carId": "CP-1", "position": 2, "status": "racing", "lap": newestLap}},
		"lapHistory": history,
	}
	encoded, _ := json.Marshal(payload)
	return "RACE:" + string(encoded)
}
