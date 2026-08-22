package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestRaceAudioAnnouncementSynthesizesOnceAndQueuesEveryPilot(t *testing.T) {
	var synthesisCalls atomic.Int32
	synthesisService := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		synthesisCalls.Add(1)
		var payload raceAudioSynthesisRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Language != "ja-JP" || !strings.HasSuffix(payload.EventKey, ":global:pre_race_formation") ||
			payload.Text != preRaceFormationAnnouncement("rr-formation").JapaneseText {
			t.Fatalf("unexpected synthesis request: %+v", payload)
		}
		_ = json.NewEncoder(writer).Encode(raceAudioSynthesisResponse{
			Version: 1, Codec: "opus", ClockRate: 48000, Channels: 1, FrameDurationMS: 20,
			DurationMS: 20, SHA256: "test", Packets: []string{base64.StdEncoding.EncodeToString([]byte{0x18, 0x01})},
		})
	}))
	defer synthesisService.Close()
	service, err := newRaceAudioServiceClient(synthesisService.URL, "", "ja-JP", "am_michael", "jf_alpha", 1.0, false)
	if err != nil {
		t.Fatal(err)
	}
	server := &relayServer{
		sources: map[string]*relay{}, sourceRuntime: relaySourceRuntime{raceAudioService: service},
	}
	for index, carID := range []string{"CP-1", "CP-2", "CP-3"} {
		source := raceAudioAnnouncementTestSource(t, string(rune('1'+index)), carID, service)
		server.sources[source.name] = source
	}

	body := []byte(`{"schemaVersion":1,"command":"pre_race_formation","commandId":"formation-1","raceRunId":"rr-formation","carIds":["CP-3","CP-1","CP-2"]}`)
	first := callRaceAudioAnnouncement(t, server, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var response raceAudioAnnouncementResponse
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "queued" || response.TargetCount != 3 || response.DurationMS != 20 {
		t.Fatalf("unexpected response: %+v", response)
	}
	for _, source := range server.sources {
		client := source.activeRaceAudioPilot()
		select {
		case clip := <-client.raceAudioQueue:
			if clip.event.EventID != response.EventID || len(clip.packets) != 1 {
				t.Fatalf("unexpected queued clip: %+v", clip)
			}
		default:
			t.Fatalf("source %q has no queued announcement", source.name)
		}
	}
	if synthesisCalls.Load() != 1 {
		t.Fatalf("synthesis calls=%d", synthesisCalls.Load())
	}

	duplicateBody := bytes.Replace(body, []byte("formation-1"), []byte("formation-2"), 1)
	duplicate := callRaceAudioAnnouncement(t, server, duplicateBody)
	if duplicate.Code != http.StatusOK || synthesisCalls.Load() != 1 {
		t.Fatalf("duplicate status=%d calls=%d body=%s", duplicate.Code, synthesisCalls.Load(), duplicate.Body.String())
	}
	if err := json.Unmarshal(duplicate.Body.Bytes(), &response); err != nil || response.Status != "duplicate" || response.CommandID != "formation-2" {
		t.Fatalf("unexpected duplicate response: %+v err=%v", response, err)
	}

	for _, source := range server.sources {
		source.activeRaceAudioPilot().raceAudio.Store(&webrtc.DataChannel{})
		if source.raceCarID == "CP-2" {
			for len(source.activeRaceAudioPilot().raceAudioQueue) < cap(source.activeRaceAudioPilot().raceAudioQueue) {
				source.activeRaceAudioPilot().raceAudioQueue <- raceAudioClip{}
			}
		}
	}
	queueFullBody := bytes.ReplaceAll(body, []byte("rr-formation"), []byte("rr-queue-full"))
	queueFullBody = bytes.Replace(queueFullBody, []byte("formation-1"), []byte("formation-full"), 1)
	queueFull := callRaceAudioAnnouncement(t, server, queueFullBody)
	if queueFull.Code != http.StatusServiceUnavailable || !bytes.Contains(queueFull.Body.Bytes(), []byte("playback_queue_full")) {
		t.Fatalf("queue full status=%d body=%s", queueFull.Code, queueFull.Body.String())
	}
	for _, source := range server.sources {
		queued := len(source.activeRaceAudioPilot().raceAudioQueue)
		if source.raceCarID == "CP-2" {
			if queued != cap(source.activeRaceAudioPilot().raceAudioQueue) {
				t.Fatalf("full queue changed for %s: %d", source.raceCarID, queued)
			}
		} else if queued != 0 {
			t.Fatalf("partial announcement queued for %s", source.raceCarID)
		}
	}
}

func TestRaceAudioAnnouncementRequiresEveryRequestedPilot(t *testing.T) {
	service := &raceAudioServiceClient{defaultLanguage: "ja-JP"}
	source := raceAudioAnnouncementTestSource(t, "1", "CP-1", service)
	server := &relayServer{
		sources: map[string]*relay{source.name: source}, sourceRuntime: relaySourceRuntime{raceAudioService: service},
	}
	body := []byte(`{"schemaVersion":1,"command":"pre_race_formation","commandId":"formation-1","raceRunId":"rr-formation","carIds":["CP-1","CP-2"]}`)
	response := callRaceAudioAnnouncement(t, server, body)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte("pilot_audio_not_ready")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func raceAudioAnnouncementTestSource(t *testing.T, name, carID string, service *raceAudioServiceClient) *relay {
	t.Helper()
	source, err := newRelay(name, "ws://127.0.0.1:1/ws", carID, false,
		defaultRTPStallTimeout, defaultUpstreamStartTimeout, vehicleHealthRecoveryDisabled)
	if err != nil {
		t.Fatal(err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(opusCodec, "race-audio-test", "momo-race-audio")
	if err != nil {
		t.Fatal(err)
	}
	client := &viewer{id: 1, role: "pilot", raceAudioTrack: track, raceAudioQueue: make(chan raceAudioClip, 4)}
	client.raceAudioLanguage.Store("ja-JP")
	client.raceAudio.Store(&webrtc.DataChannel{})
	source.viewers = map[uint64]*viewer{client.id: client}
	source.pilotID = client.id
	source.raceAudio = &raceAudioSource{relay: source, service: service, jobs: newRaceAudioJobQueue()}
	return source
}

func callRaceAudioAnnouncement(t *testing.T, server *relayServer, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/race-audio/announcements", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.serveRaceAudioAnnouncement(response, request)
	return response
}
