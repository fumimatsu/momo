package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestRaceAudioDetectorAnnouncesInitialPositionWithoutStartPhrase(t *testing.T) {
	detector := raceAudioDetector{}
	ready := raceAudioScenarioState("run-start", "ready", "race", 10, 3, 0, nil, nil)
	if events := detector.observe(ready, "CP-1"); len(events) != 0 {
		t.Fatalf("ready baseline emitted events: %#v", events)
	}

	green := raceAudioScenarioState("run-start", "green", "race", 10, 3, 0, nil, nil)
	events := detector.observe(green, "CP-1")
	if len(events) != 1 || events[0].Kind != "race_start" ||
		events[0].EnglishText != "Position 3." || events[0].JapaneseText != "現在3位。" {
		t.Fatalf("unexpected initial position event: %#v", events)
	}
	if events := detector.observe(green, "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate green state emitted events: %#v", events)
	}

	positionTwo := raceAudioScenarioState("run-start", "green", "race", 10, 2, 0, nil, nil)
	events = detector.observe(positionTwo, "CP-1")
	if len(events) != 1 || events[0].Kind != "position_change" ||
		events[0].JapaneseText != "2位に上がりました。" {
		t.Fatalf("unexpected position event: %#v", events)
	}

	paused := raceAudioScenarioState("run-start", "paused", "race", 10, 2, 0, nil, nil)
	events = detector.observe(paused, "CP-1")
	if len(events) != 1 || events[0].Kind != "race_paused" {
		t.Fatalf("unexpected pause event: %#v", events)
	}
	events = detector.observe(positionTwo, "CP-1")
	if len(events) != 1 || events[0].Kind != "race_resumed" {
		t.Fatalf("unexpected resume event: %#v", events)
	}
}

func TestRaceAudioDetectorAppendsFinalLapToPreviousLapAnnouncement(t *testing.T) {
	detector := raceAudioDetector{}
	historyEight := []raceAudioLapHistory{{CarID: "CP-1", Lap: 8, LapTimeMS: 14000}}
	detector.observe(
		raceAudioScenarioState("run-final", "green", "race", 10, 2, 8, historyEight, nil),
		"CP-1",
	)
	historyNine := append(historyEight, raceAudioLapHistory{
		CarID: "CP-1", Lap: 9, LapTimeMS: 13500, Achievement: "personal_best",
	})
	events := detector.observe(
		raceAudioScenarioState("run-final", "green", "race", 10, 2, 9, historyNine, nil),
		"CP-1",
	)
	if len(events) != 1 || events[0].Kind != "lap_complete" ||
		events[0].EnglishText != "Lap 9. 13 point five zero zero seconds. New personal best. Final lap." ||
		events[0].JapaneseText != "9周目、13.500。自己ベスト更新。ファイナルラップ。" {
		t.Fatalf("unexpected final lap event: %#v", events)
	}
}

func TestRaceAudioDetectorAnnouncesBlueFlagAfterRearm(t *testing.T) {
	detector := raceAudioDetector{}
	baseline := raceAudioScenarioState("run-blue", "green", "race", 10, 3, 3, nil, nil)
	detector.observe(baseline, "CP-1")
	blue := raceAudioScenarioState(
		"run-blue", "green", "race", 10, 3, 3, nil,
		map[string]any{"lappingCarBehindId": "CP-2", "lappingGapMs": 2500},
		map[string]any{"carId": "CP-2", "position": 1, "status": "racing", "lap": 4},
	)
	events := detector.observe(blue, "CP-1")
	if len(events) != 1 || events[0].Kind != "blue_flag" ||
		events[0].EnglishText != "Blue flag. Car 2 behind." {
		t.Fatalf("unexpected blue flag event: %#v", events)
	}
	if events := detector.observe(blue, "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate blue flag emitted events: %#v", events)
	}

	clear := raceAudioScenarioState(
		"run-blue", "green", "race", 10, 3, 3, nil,
		map[string]any{"lappingCarBehindId": "CP-2", "lappingGapMs": 4500},
		map[string]any{"carId": "CP-2", "position": 1, "status": "racing", "lap": 4},
	)
	if events := detector.observe(clear, "CP-1"); len(events) != 0 {
		t.Fatalf("blue flag release emitted events: %#v", events)
	}
	if events := detector.observe(blue, "CP-1"); len(events) != 1 || events[0].Kind != "blue_flag" {
		t.Fatalf("rearmed blue flag did not emit once: %#v", events)
	}
}

func TestRaceAudioGameplayDetectorAnnouncesThresholdCrossingsAndRearms(t *testing.T) {
	detector := raceAudioGameplayDetector{}
	context := raceAudioRaceContext{RunID: "run-resources", CarID: "CP-1", Phase: "green", SessionType: "race"}
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 100, Mode: "healthy"}, context); len(events) != 0 {
		t.Fatalf("gameplay baseline emitted events: %#v", events)
	}

	for _, test := range []struct {
		fuel float64
		mode string
		kind string
	}{
		{fuel: 19, mode: "healthy", kind: "fuel_low"},
		{fuel: 7, mode: "healthy", kind: "fuel_critical"},
		{fuel: 0, mode: "healthy", kind: "fuel_empty"},
	} {
		events := detector.observe(vehicleHealthSnapshot{Fuel: test.fuel, Mode: test.mode}, context)
		if len(events) != 1 || events[0].Kind != test.kind {
			t.Fatalf("fuel=%v emitted %#v, want %s", test.fuel, events, test.kind)
		}
	}
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 0, Mode: "healthy"}, context); len(events) != 0 {
		t.Fatalf("unchanged empty fuel emitted events: %#v", events)
	}

	// PIT回復などでしきい値を上回った後は、次の低下を新しい警告として扱う。
	detector.observe(vehicleHealthSnapshot{Fuel: 100, Mode: "healthy"}, context)
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 19, Mode: "healthy"}, context); len(events) != 1 || events[0].Kind != "fuel_low" {
		t.Fatalf("rearmed fuel warning emitted %#v", events)
	}

	detector.observe(vehicleHealthSnapshot{Fuel: 19, Mode: "healthy"}, context)
	events := detector.observe(vehicleHealthSnapshot{Fuel: 19, Mode: "critical"}, context)
	if len(events) != 1 || events[0].Kind != "damage_critical" ||
		events[0].JapaneseText != "重大ダメージ。出力制限中。" {
		t.Fatalf("unexpected critical damage event: %#v", events)
	}
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 19, Mode: "critical"}, context); len(events) != 0 {
		t.Fatalf("unchanged critical damage emitted events: %#v", events)
	}
}

func TestRaceAudioGameplayDetectorSeedsSilentlyAndRunsOnlyDuringGreenRace(t *testing.T) {
	detector := raceAudioGameplayDetector{}
	greenRace := raceAudioRaceContext{
		RunID: "run-seed", CarID: "CP-1", Phase: "green", SessionType: "race",
	}
	critical := vehicleHealthSnapshot{Fuel: 7, Mode: "critical"}
	if events := detector.observe(critical, greenRace); len(events) != 0 {
		t.Fatalf("initial critical snapshot replayed events: %#v", events)
	}
	practice := raceAudioRaceContext{
		RunID: "run-practice", CarID: "CP-1", Phase: "green", SessionType: "practice",
	}
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 0, Mode: "limp"}, practice); len(events) != 0 {
		t.Fatalf("practice snapshot emitted events: %#v", events)
	}
	pausedRace := raceAudioRaceContext{
		RunID: "run-seed", CarID: "CP-1", Phase: "paused", SessionType: "race",
	}
	if events := detector.observe(vehicleHealthSnapshot{Fuel: 0, Mode: "limp"}, pausedRace); len(events) != 0 {
		t.Fatalf("paused race snapshot emitted events: %#v", events)
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
	if len(events) != 1 || events[0].Kind != "race_finish" ||
		events[0].EnglishText != "Checkered flag. P 2. Final lap, 13 point one two zero seconds." {
		t.Fatalf("unexpected finish events: %#v", events)
	}
	if events := detector.observe(raceAudioTestState("run-1", "finished", 2, 13120), "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate finish emitted %d events", len(events))
	}
}

func TestRaceAudioDetectorEmitsFinalLapBeforeFinishFromStandingStatus(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(raceAudioTestStateWithStatus("run-1", "green", "racing", 9, 18586), "CP-1")

	events := detector.observe(raceAudioTestStateWithStatus("run-1", "green", "finished", 10, 27760), "CP-1")
	if len(events) != 1 {
		t.Fatalf("final snapshot emitted %d events, want 1: %#v", len(events), events)
	}
	if events[0].Kind != "race_finish" ||
		events[0].EnglishText != "Checkered flag. P 2. Final lap, 27 point seven six zero seconds." {
		t.Fatalf("unexpected combined finish event: %#v", events[0])
	}
	if events := detector.observe(raceAudioTestStateWithStatus("run-1", "green", "finished", 10, 27760), "CP-1"); len(events) != 0 {
		t.Fatalf("duplicate final snapshot emitted %d events", len(events))
	}
}

func TestRaceAudioDetectorDefersConfiguredFinalLapUntilFinish(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(raceAudioTestStateWithStatus("run-1", "green", "racing", 9, 18586), "CP-1")

	if events := detector.observe(raceAudioTestStateWithStatus("run-1", "green", "racing", 10, 27760), "CP-1"); len(events) != 0 {
		t.Fatalf("configured final lap emitted before finish: %#v", events)
	}
	events := detector.observe(raceAudioTestStateWithStatus("run-1", "green", "finished", 10, 27760), "CP-1")
	if len(events) != 1 || events[0].Kind != "race_finish" ||
		events[0].JapaneseText != "ゴール。 2位。 最終ラップ、27.760秒。" {
		t.Fatalf("unexpected deferred finish event: %#v", events)
	}
}

func TestRaceAudioDetectorUsesAuthoritativeLapAchievement(t *testing.T) {
	detector := raceAudioDetector{}
	detector.observe(raceAudioTestState("run-best", "green", 1, 14000), "CP-1")
	events := detector.observe(
		raceAudioTestStateWithAchievement("run-best", "green", "racing", 2, 13000, "personal_best"),
		"CP-1",
	)
	if len(events) != 1 || events[0].EnglishText != "Lap 2. 13 point zero zero zero seconds. New personal best." ||
		events[0].JapaneseText != "2周目、13.000。自己ベスト更新。" {
		t.Fatalf("unexpected personal best event: %#v", events)
	}

	detector.observe(raceAudioTestState("run-overall", "green", 1, 14000), "CP-1")
	events = detector.observe(
		raceAudioTestStateWithAchievement("run-overall", "green", "racing", 2, 12000, "overall_best"),
		"CP-1",
	)
	if len(events) != 1 || events[0].EnglishText != "Lap 2. 12 point zero zero zero seconds. New overall best." ||
		events[0].JapaneseText != "2周目、12.000。全体ベスト更新。" {
		t.Fatalf("unexpected overall best event: %#v", events)
	}
}

func TestRaceAudioPitDetectorEmitsServiceCompleteOnce(t *testing.T) {
	detector := raceAudioPitDetector{}
	servicing := pitPresenceSnapshot{
		RaceRunID: "run-1", CarID: "CP-1", Present: true,
		EntryID: "entry-1", ServiceState: "servicing",
	}
	complete := servicing
	complete.ServiceState = "complete"

	if event := detector.observe(servicing); event != nil {
		t.Fatalf("initial servicing snapshot emitted event: %#v", event)
	}
	event := detector.observe(complete)
	if event == nil || event.Kind != "pit_service_complete" || event.Priority != 65 ||
		event.EnglishText != "Pit service complete." || event.JapaneseText != "ピットサービス完了。" {
		t.Fatalf("unexpected PIT complete event: %#v", event)
	}
	if event := detector.observe(complete); event != nil {
		t.Fatalf("duplicate complete snapshot emitted event: %#v", event)
	}
	if event := detector.observe(servicing); event != nil {
		t.Fatalf("same entry returning to servicing emitted event: %#v", event)
	}
	if event := detector.observe(complete); event != nil {
		t.Fatalf("same entry completed twice: %#v", event)
	}
}

func TestRaceAudioPitDetectorDoesNotReplayExistingCompleteState(t *testing.T) {
	detector := raceAudioPitDetector{}
	complete := pitPresenceSnapshot{
		RaceRunID: "run-1", CarID: "CP-1", Present: true,
		EntryID: "entry-1", ServiceState: "complete",
	}
	if event := detector.observe(complete); event != nil {
		t.Fatalf("initial complete snapshot emitted event: %#v", event)
	}
	complete.RaceRunID = "run-2"
	complete.EntryID = "entry-2"
	if event := detector.observe(complete); event != nil {
		t.Fatalf("new run complete baseline emitted event: %#v", event)
	}
}

func TestRaceAudioPitCompleteUsesBrowserKokoroPath(t *testing.T) {
	if !raceAudioBrowserLocalEvent("pit_service_complete") {
		t.Fatal("PIT complete is not routed through Browser Kokoro")
	}
}

func TestRaceAudioEnglishTemplatesAreShortAndOmitUnknownPosition(t *testing.T) {
	if got, want := raceAudioEnglishLapText(4, 13715, ""), "Lap 4. 13 point seven one five seconds"; got != want {
		t.Fatalf("lap text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishLapText(4, 13715, "personal_best"), "Lap 4. 13 point seven one five seconds. New personal best."; got != want {
		t.Fatalf("personal best lap text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishLapText(4, 13715, "overall_best"), "Lap 4. 13 point seven one five seconds. New overall best."; got != want {
		t.Fatalf("overall best lap text = %q, want %q", got, want)
	}
	if got, want := raceAudioJapaneseLapText(4, 13715, ""), "4周目、13.715"; got != want {
		t.Fatalf("Japanese lap text = %q, want %q", got, want)
	}
	if got, want := raceAudioJapaneseLapText(5, 13005, "personal_best"), "5周目、13.005。自己ベスト更新。"; got != want {
		t.Fatalf("Japanese lap text with zero digits = %q, want %q", got, want)
	}
	if got, want := raceAudioJapaneseFinishText(2, 13715), "ゴール。 2位。 最終ラップ、13.715秒。"; got != want {
		t.Fatalf("Japanese finish text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishLapTime(13005), "13 point zero zero five"; got != want {
		t.Fatalf("lap time = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishFinishText(2, 13715), "Checkered flag. P 2. Final lap, 13 point seven one five seconds."; got != want {
		t.Fatalf("finish text = %q, want %q", got, want)
	}
	if got, want := raceAudioEnglishFinishText(0, 0), "Checkered flag."; got != want {
		t.Fatalf("finish text without position = %q, want %q", got, want)
	}
}

func TestRaceAudioCalloutUsesFixedTemplates(t *testing.T) {
	event, err := raceAudioEventFromCallout(12, raceAudioCalloutRequest{
		Type: "race_audio_callout_request", Version: 1, RequestID: "gap_behind-4",
		Kind: "gap_behind", CarNumber: 7, GapMS: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != "gap_behind" || event.Priority != 80 || event.EnglishText != "Car 7 behind. Gap zero point six seconds" ||
		event.JapaneseText != "後ろ、7号車、差0.6" {
		t.Fatalf("unexpected callout event: %#v", event)
	}
	ahead, err := raceAudioEventFromCallout(12, raceAudioCalloutRequest{
		Type: "race_audio_callout_request", Version: 1, RequestID: "gap_ahead-5",
		Kind: "gap_ahead", CarNumber: 11, GapMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ahead.Priority != 30 || ahead.EnglishText != "Car 11 ahead. Gap two seconds" || ahead.JapaneseText != "前、11号車、差2.0" {
		t.Fatalf("unexpected ahead event: %#v", ahead)
	}
}

func TestRaceAudioCalloutRejectsInvalidOrArbitraryPayload(t *testing.T) {
	cases := []raceAudioCalloutRequest{
		{Type: "race_audio_callout_request", Version: 1, RequestID: "bad request", Kind: "gap_ahead", CarNumber: 1, GapMS: 500},
		{Type: "race_audio_callout_request", Version: 1, RequestID: "request-1", Kind: "speak_text", CarNumber: 1, GapMS: 500},
		{Type: "race_audio_callout_request", Version: 1, RequestID: "request-1", Kind: "gap_ahead", CarNumber: 1, GapMS: 550},
	}
	for _, request := range cases {
		if _, err := raceAudioEventFromCallout(1, request); err == nil {
			t.Fatalf("invalid request was accepted: %#v", request)
		}
	}
}

func TestRaceAudioCalloutRateLimitAndDeduplication(t *testing.T) {
	client := &viewer{}
	started := time.Unix(100, 0)
	if !client.acceptRaceAudioCallout("request-1", started) {
		t.Fatal("first callout was rejected")
	}
	if client.acceptRaceAudioCallout("request-1", started.Add(3*time.Second)) {
		t.Fatal("duplicate callout was accepted")
	}
	if client.acceptRaceAudioCallout("request-2", started.Add(time.Second)) {
		t.Fatal("rapid callout was accepted")
	}
	if !client.acceptRaceAudioCallout("request-2", started.Add(2*time.Second)) {
		t.Fatal("callout after hard interval was rejected")
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

func TestRaceAudioServiceClientPreparesBrowserKokoroPrompt(t *testing.T) {
	const token = "test-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/prepare" {
			t.Fatalf("unexpected request path: %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("unexpected authorization: %q", request.Header.Get("Authorization"))
		}
		var payload raceAudioPromptRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Language != "ja-JP" || payload.Voice != "jf_alpha" || payload.Text != "4周目、13.715" {
			t.Fatalf("unexpected prompt request: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(raceAudioBrowserPrompt{
			Version:       1,
			Engine:        "kokoro",
			ModelID:       raceAudioBrowserModelID,
			Language:      payload.Language,
			Voice:         payload.Voice,
			Speed:         payload.Speed,
			Phonemes:      "joɴɕuːme, ʥuː saɴteɴ nanaiʨi goː.",
			ModelInputIDs: []int{0, 10, 11, 0},
			PhonemePolicy: "strip-misaki-terminal-prosody-and-append-period-v1",
		})
	}))
	defer server.Close()
	client, err := newRaceAudioServiceClient(server.URL, token, "en-US", "am_michael", "jf_alpha", 1.04)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := client.prepare(context.Background(), raceAudioEvent{
		EventID:      "run-1:CP-1:lap:4:13715",
		Kind:         "lap_complete",
		EnglishText:  "Lap 4. 13.715 seconds",
		JapaneseText: "4周目、13.715",
	}, "ja-JP")
	if err != nil {
		t.Fatal(err)
	}
	if prompt.ModelID != raceAudioBrowserModelID || len(prompt.ModelInputIDs) != 4 {
		t.Fatalf("unexpected browser prompt: %#v", prompt)
	}
}

func TestValidateRaceAudioBrowserPromptRejectsMissingBoundaryTokens(t *testing.T) {
	prompt := raceAudioBrowserPrompt{
		Version:       1,
		Engine:        "kokoro",
		ModelID:       raceAudioBrowserModelID,
		Language:      "en-US",
		Voice:         "am_michael",
		Speed:         1.04,
		Phonemes:      "hello.",
		ModelInputIDs: []int{1, 2, 3},
	}
	if err := validateRaceAudioBrowserPrompt(prompt, "en-US", "am_michael", 1.04); err == nil {
		t.Fatal("browser prompt without boundary tokens was accepted")
	}
}

func TestNormalizeRaceAudioModeKeepsLegacyPreferenceRemote(t *testing.T) {
	if mode, err := normalizeRaceAudioMode(""); err != nil || mode != raceAudioModeRemote {
		t.Fatalf("empty mode = %q, %v", mode, err)
	}
	if mode, err := normalizeRaceAudioMode(raceAudioModeBrowserKokoro); err != nil || mode != raceAudioModeBrowserKokoro {
		t.Fatalf("browser mode = %q, %v", mode, err)
	}
	if _, err := normalizeRaceAudioMode("unknown"); err == nil {
		t.Fatal("unknown race audio mode was accepted")
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

func TestRelayPilotRaceAudioTrackEndToEnd(t *testing.T) {
	source, err := newRelay("11.4", "ws://127.0.0.1:1/ws", "CP-2", false,
		defaultRTPStallTimeout, defaultUpstreamStartTimeout, vehicleHealthRecoveryDisabled)
	if err != nil {
		t.Fatal(err)
	}
	source.raceAudio = &raceAudioSource{
		relay:   source,
		service: &raceAudioServiceClient{defaultLanguage: "en-US"},
	}
	server := &relayServer{sources: map[string]*relay{"11.4": source}}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serveViewerWS))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/?device=11.4&role=pilot&client=web-pilot"
	signaling, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer signaling.Close()

	clientAPI, err := newH264API()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := clientAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	unordered := false
	maxRetransmits := uint16(0)
	if _, err := peer.CreateDataChannel(commandLabel, &webrtc.DataChannelInit{
		Ordered:        &unordered,
		MaxRetransmits: &maxRetransmits,
	}); err != nil {
		t.Fatal(err)
	}
	ordered := true
	raceAudioChannel, err := peer.CreateDataChannel(raceAudioLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.CreateDataChannel(driveLabel, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}

	raceAudioOpen := make(chan struct{})
	raceAudioChannel.OnOpen(func() { close(raceAudioOpen) })
	audioPayloads := make(chan []byte, 8)
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go func() {
			packet, _, readErr := track.ReadRTP()
			if readErr == nil {
				audioPayloads <- append([]byte(nil), packet.Payload...)
			}
		}()
	})

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
		t.Fatal(err)
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := writeSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		t.Fatal(err)
	}
	waitForE2ESignal(t, "Relay answer", answerSet, signalingErrors)
	waitForE2ESignal(t, "momo-race-audio open", raceAudioOpen, signalingErrors)

	select {
	case payload := <-audioPayloads:
		if len(payload) == 0 {
			t.Fatal("race audio RTP payload is empty")
		}
	case err := <-signalingErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the Relay Pilot race audio track")
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
	return raceAudioTestStateWithAchievement(runID, phase, "racing", newestLap, newestLapMS, "")
}

func raceAudioTestStateWithStatus(runID string, phase string, status string, newestLap int, newestLapMS int) string {
	return raceAudioTestStateWithAchievement(runID, phase, status, newestLap, newestLapMS, "")
}

func raceAudioTestStateWithAchievement(
	runID string,
	phase string,
	status string,
	newestLap int,
	newestLapMS int,
	achievement string,
) string {
	history := make([]map[string]any, 0, newestLap)
	for lap := 1; lap <= newestLap; lap++ {
		lapTimeMS := 14000 + lap
		if lap == newestLap {
			lapTimeMS = newestLapMS
		}
		entry := map[string]any{
			"carId": "CP-1", "lap": lap, "lapTimeMs": lapTimeMS,
		}
		if lap == newestLap && achievement != "" {
			entry["achievement"] = achievement
		}
		history = append(history, entry)
	}
	payload := map[string]any{
		"type": "race_state", "version": 2, "raceId": "race-test", "raceRunId": runID,
		"phase": phase, "viewerCarId": "CP-1",
		"raceInfo":   map[string]any{"totalLaps": 10},
		"standings":  []map[string]any{{"carId": "CP-1", "position": 2, "status": status, "lap": newestLap}},
		"lapHistory": history,
	}
	encoded, _ := json.Marshal(payload)
	return "RACE:" + string(encoded)
}

func raceAudioScenarioState(
	runID string,
	phase string,
	sessionType string,
	totalLaps int,
	position int,
	lap int,
	history []raceAudioLapHistory,
	selfOverrides map[string]any,
	rivals ...map[string]any,
) string {
	self := map[string]any{
		"carId": "CP-1", "position": position, "status": "racing", "lap": lap,
	}
	for key, value := range selfOverrides {
		self[key] = value
	}
	standings := []map[string]any{self}
	standings = append(standings, rivals...)
	payload := map[string]any{
		"type": "race_state", "version": 2, "raceId": "race-test", "raceRunId": runID,
		"phase": phase, "viewerCarId": "CP-1",
		"raceInfo":  map[string]any{"totalLaps": totalLaps, "sessionType": sessionType},
		"standings": standings, "lapHistory": history,
	}
	encoded, _ := json.Marshal(payload)
	return "RACE:" + string(encoded)
}
