package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	raceAudioLabel                = "momo-race-audio"
	raceAudioProtocolVersion      = 1
	raceAudioPacketDuration       = 20 * time.Millisecond
	raceAudioQueueSize            = 4
	raceAudioJobQueueSize         = 16
	raceAudioMaximumResponse      = 4 * 1024 * 1024
	raceAudioSynthesisTimeout     = 12 * time.Second
	raceAudioDefaultLanguage      = "en-US"
	raceAudioDefaultEnglishVoice  = "am_michael"
	raceAudioDefaultJapaneseVoice = "jf_alpha"
)

var opusCodec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

// A 20 ms Opus DTX packet keeps the negotiated track alive without audible output.
var opusSilencePacket = []byte{0xf8, 0xff, 0xfe}

type raceAudioServiceClient struct {
	baseURL         string
	token           string
	defaultLanguage string
	englishVoice    string
	japaneseVoice   string
	speed           float64
	httpClient      *http.Client
}

type raceAudioSynthesisRequest struct {
	EventKey        string  `json:"eventKey"`
	Language        string  `json:"language"`
	Voice           string  `json:"voice"`
	Text            string  `json:"text"`
	Speed           float64 `json:"speed"`
	Codec           string  `json:"codec"`
	FrameDurationMS int     `json:"frameDurationMs"`
}

type raceAudioSynthesisResponse struct {
	Version          int      `json:"version"`
	Codec            string   `json:"codec"`
	ClockRate        int      `json:"clockRate"`
	Channels         int      `json:"channels"`
	FrameDurationMS  int      `json:"frameDurationMs"`
	DurationMS       int      `json:"durationMs"`
	SHA256           string   `json:"sha256"`
	Packets          []string `json:"packets"`
	CacheHit         bool     `json:"cacheHit"`
	ServiceElapsedMS int      `json:"serviceElapsedMs"`
}

type raceAudioClip struct {
	event   raceAudioEvent
	packets [][]byte
}

type raceAudioEvent struct {
	EventID      string
	Kind         string
	Priority     int
	EnglishText  string
	JapaneseText string
}

type raceAudioMetadata struct {
	Type         string            `json:"type"`
	Version      int               `json:"version"`
	State        string            `json:"state"`
	EventID      string            `json:"eventId,omitempty"`
	Kind         string            `json:"kind,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	Language     string            `json:"language,omitempty"`
	DurationMS   int               `json:"durationMs,omitempty"`
	FallbackText map[string]string `json:"fallbackText,omitempty"`
	Ducking      *raceAudioDucking `json:"ducking,omitempty"`
	Error        string            `json:"error,omitempty"`
}

type raceAudioDucking struct {
	M5AudioGain float64 `json:"m5AudioGain"`
	AttackMS    int     `json:"attackMs"`
	ReleaseMS   int     `json:"releaseMs"`
}

type raceAudioPreference struct {
	Type     string `json:"type"`
	Version  int    `json:"version"`
	Language string `json:"language"`
}

type raceAudioState struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	RaceID      string `json:"raceId"`
	RaceRunID   string `json:"raceRunId"`
	Phase       string `json:"phase"`
	ViewerCarID string `json:"viewerCarId"`
	Standings   []struct {
		CarID    string `json:"carId"`
		Position int    `json:"position"`
		Status   string `json:"status"`
		Lap      int    `json:"lap"`
	} `json:"standings"`
	LapHistory []struct {
		CarID     string `json:"carId"`
		Lap       int    `json:"lap"`
		LapTimeMS int    `json:"lapTimeMs"`
	} `json:"lapHistory"`
}

type raceAudioDetector struct {
	mu          sync.Mutex
	initialized bool
	runID       string
	phase       string
	seenLaps    map[string]struct{}
}

type raceAudioSource struct {
	relay    *relay
	service  *raceAudioServiceClient
	detector raceAudioDetector
	jobs     chan raceAudioEvent
}

func newRaceAudioServiceClient(baseURL string, token string, defaultLanguage string,
	englishVoice string, japaneseVoice string, speed float64) (*raceAudioServiceClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid race audio service URL %q", baseURL)
	}
	hostname := strings.ToLower(parsed.Hostname())
	address := net.ParseIP(hostname)
	if strings.TrimSpace(token) == "" && hostname != "localhost" && (address == nil || !address.IsLoopback()) {
		return nil, errors.New("MOMO_RACE_AUDIO_SERVICE_TOKEN is required for a non-loopback race audio service URL")
	}
	defaultLanguage, err = normalizeRaceAudioLanguage(defaultLanguage)
	if err != nil || defaultLanguage == "off" {
		return nil, fmt.Errorf("invalid race audio default language %q", defaultLanguage)
	}
	if strings.TrimSpace(englishVoice) == "" {
		englishVoice = raceAudioDefaultEnglishVoice
	}
	if strings.TrimSpace(japaneseVoice) == "" {
		japaneseVoice = raceAudioDefaultJapaneseVoice
	}
	if speed < 0.5 || speed > 2.0 {
		return nil, errors.New("race audio speed must be between 0.5 and 2.0")
	}
	return &raceAudioServiceClient{
		baseURL:         baseURL,
		token:           strings.TrimSpace(token),
		defaultLanguage: defaultLanguage,
		englishVoice:    strings.TrimSpace(englishVoice),
		japaneseVoice:   strings.TrimSpace(japaneseVoice),
		speed:           speed,
		httpClient:      &http.Client{Timeout: raceAudioSynthesisTimeout},
	}, nil
}

func normalizeRaceAudioLanguage(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "en", "en-us":
		return "en-US", nil
	case "ja", "ja-jp":
		return "ja-JP", nil
	case "off", "none", "disabled":
		return "off", nil
	default:
		return "", fmt.Errorf("unsupported race audio language %q", value)
	}
}

func (client *raceAudioServiceClient) voiceForLanguage(language string) string {
	if language == "ja-JP" {
		return client.japaneseVoice
	}
	return client.englishVoice
}

func (client *raceAudioServiceClient) synthesize(ctx context.Context, event raceAudioEvent,
	language string) (raceAudioClip, int, error) {
	if client == nil {
		return raceAudioClip{}, 0, errors.New("race audio service is disabled")
	}
	text := event.EnglishText
	if language == "ja-JP" {
		text = event.JapaneseText
	}
	payload, err := json.Marshal(raceAudioSynthesisRequest{
		EventKey:        event.EventID,
		Language:        language,
		Voice:           client.voiceForLanguage(language),
		Text:            text,
		Speed:           client.speed,
		Codec:           "opus",
		FrameDurationMS: int(raceAudioPacketDuration / time.Millisecond),
	})
	if err != nil {
		return raceAudioClip{}, 0, fmt.Errorf("encode race audio request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/synthesize", bytes.NewReader(payload))
	if err != nil {
		return raceAudioClip{}, 0, fmt.Errorf("create race audio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if client.token != "" {
		req.Header.Set("Authorization", "Bearer "+client.token)
	}
	response, err := client.httpClient.Do(req)
	if err != nil {
		return raceAudioClip{}, 0, fmt.Errorf("request race audio: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, raceAudioMaximumResponse+1))
	if err != nil {
		return raceAudioClip{}, 0, fmt.Errorf("read race audio response: %w", err)
	}
	if len(body) > raceAudioMaximumResponse {
		return raceAudioClip{}, 0, errors.New("race audio response exceeds 4 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return raceAudioClip{}, 0, fmt.Errorf("race audio service returned HTTP %d", response.StatusCode)
	}
	var decoded raceAudioSynthesisResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return raceAudioClip{}, 0, fmt.Errorf("decode race audio response: %w", err)
	}
	packets, err := validateRaceAudioResponse(decoded)
	if err != nil {
		return raceAudioClip{}, 0, err
	}
	return raceAudioClip{event: event, packets: packets}, decoded.DurationMS, nil
}

func validateRaceAudioResponse(response raceAudioSynthesisResponse) ([][]byte, error) {
	if response.Version != raceAudioProtocolVersion || !strings.EqualFold(response.Codec, "opus") ||
		response.ClockRate != 48000 || response.Channels != 1 ||
		response.FrameDurationMS != int(raceAudioPacketDuration/time.Millisecond) {
		return nil, errors.New("race audio response has an unsupported media contract")
	}
	if len(response.Packets) == 0 || response.DurationMS <= 0 {
		return nil, errors.New("race audio response is empty")
	}
	packets := make([][]byte, 0, len(response.Packets))
	for _, encoded := range response.Packets {
		packet, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(packet) == 0 || len(packet) > 1500 {
			return nil, errors.New("race audio response contains an invalid Opus packet")
		}
		packets = append(packets, packet)
	}
	expectedDuration := len(packets) * int(raceAudioPacketDuration/time.Millisecond)
	if response.DurationMS > expectedDuration || expectedDuration-response.DurationMS >= int(raceAudioPacketDuration/time.Millisecond) {
		return nil, errors.New("race audio response duration does not match packet count")
	}
	return packets, nil
}

func (detector *raceAudioDetector) observe(message string, configuredCarID string) []raceAudioEvent {
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(message), "RACE:"))
	if payload == "" {
		return nil
	}
	var state raceAudioState
	if err := json.Unmarshal([]byte(payload), &state); err != nil || state.Type != "race_state" || state.Version != 2 {
		return nil
	}
	carID := strings.TrimSpace(state.ViewerCarID)
	if carID == "" {
		carID = strings.TrimSpace(configuredCarID)
	}
	if carID == "" {
		return nil
	}
	runID := strings.TrimSpace(state.RaceRunID)
	if runID == "" {
		runID = strings.TrimSpace(state.RaceID)
	}
	if runID == "" {
		return nil
	}
	detector.mu.Lock()
	defer detector.mu.Unlock()
	if detector.runID != runID {
		detector.initialized = false
		detector.runID = runID
		detector.phase = ""
		detector.seenLaps = make(map[string]struct{})
	}
	if detector.seenLaps == nil {
		detector.seenLaps = make(map[string]struct{})
	}
	histories := make([]struct {
		CarID     string `json:"carId"`
		Lap       int    `json:"lap"`
		LapTimeMS int    `json:"lapTimeMs"`
	}, 0, len(state.LapHistory))
	for _, history := range state.LapHistory {
		if history.CarID == carID && history.Lap > 0 && history.LapTimeMS > 0 {
			histories = append(histories, history)
		}
	}
	sort.Slice(histories, func(left, right int) bool { return histories[left].Lap < histories[right].Lap })
	if !detector.initialized {
		for _, history := range histories {
			detector.seenLaps[raceAudioLapKey(runID, carID, history.Lap, history.LapTimeMS)] = struct{}{}
		}
		detector.phase = state.Phase
		detector.initialized = true
		return nil
	}
	standingPosition := 0
	for _, standing := range state.Standings {
		if standing.CarID == carID {
			standingPosition = standing.Position
			break
		}
	}
	events := make([]raceAudioEvent, 0, 2)
	for _, history := range histories {
		key := raceAudioLapKey(runID, carID, history.Lap, history.LapTimeMS)
		if _, exists := detector.seenLaps[key]; exists {
			continue
		}
		detector.seenLaps[key] = struct{}{}
		events = append(events, raceAudioEvent{
			EventID:      key,
			Kind:         "lap_complete",
			Priority:     40,
			EnglishText:  raceAudioEnglishLapText(history.Lap, history.LapTimeMS, standingPosition),
			JapaneseText: raceAudioJapaneseLapText(history.Lap, history.LapTimeMS, standingPosition),
		})
	}
	if detector.phase != "finished" && state.Phase == "finished" {
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:race_finish", runID, carID),
			Kind:         "race_finish",
			Priority:     70,
			EnglishText:  raceAudioEnglishFinishText(standingPosition),
			JapaneseText: raceAudioJapaneseFinishText(standingPosition),
		})
	}
	detector.phase = state.Phase
	return events
}

func raceAudioEnglishLapText(lap int, lapTimeMS int, position int) string {
	text := fmt.Sprintf("Lap %d. %.3f.", lap, float64(lapTimeMS)/1000)
	if position > 0 {
		text += fmt.Sprintf(" P %d.", position)
	}
	return text
}

func raceAudioJapaneseLapText(lap int, lapTimeMS int, position int) string {
	text := fmt.Sprintf("%d 周目。%.3f 秒。", lap, float64(lapTimeMS)/1000)
	if position > 0 {
		text += fmt.Sprintf("現在 %d 位。", position)
	}
	return text
}

func raceAudioEnglishFinishText(position int) string {
	if position > 0 {
		return fmt.Sprintf("Race finished. P %d.", position)
	}
	return "Race finished."
}

func raceAudioJapaneseFinishText(position int) string {
	if position > 0 {
		return fmt.Sprintf("レース終了。%d 位。", position)
	}
	return "レース終了。"
}

func raceAudioLapKey(runID string, carID string, lap int, _ int) string {
	return fmt.Sprintf("%s:%s:lap:%d", runID, carID, lap)
}

func newRaceAudioSource(r *relay, service *raceAudioServiceClient) *raceAudioSource {
	if r == nil || service == nil {
		return nil
	}
	return &raceAudioSource{
		relay:   r,
		service: service,
		jobs:    make(chan raceAudioEvent, raceAudioJobQueueSize),
	}
}

func (source *raceAudioSource) start(ctx context.Context) {
	if source == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-source.jobs:
				source.dispatch(ctx, event)
			}
		}
	}()
}

func (source *raceAudioSource) observe(message string) {
	if source == nil {
		return
	}
	for _, event := range source.detector.observe(message, source.relay.raceCarID) {
		select {
		case source.jobs <- event:
		default:
			log.Printf("source %q: drop race audio event %q because the queue is full", source.relay.name, event.EventID)
		}
	}
}

func (source *raceAudioSource) dispatch(parent context.Context, event raceAudioEvent) {
	client := source.relay.activeRaceAudioPilot()
	if client == nil {
		return
	}
	language := client.raceAudioLanguageValue(source.service.defaultLanguage)
	if language == "off" || client.raceAudio.Load() == nil {
		return
	}
	source.relay.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("queued", language, event, 0, ""))
	ctx, cancel := context.WithTimeout(parent, raceAudioSynthesisTimeout)
	clip, durationMS, err := source.service.synthesize(ctx, event, language)
	cancel()
	if err != nil {
		log.Printf("source %q: synthesize race audio event %q: %v", source.relay.name, event.EventID, err)
		source.relay.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("failed", language, event, 0, "synthesis_failed"))
		return
	}
	if source.relay.activeRaceAudioPilotID() != client.id || client.raceAudio.Load() == nil ||
		client.raceAudioLanguageValue(source.service.defaultLanguage) != language {
		return
	}
	clip.event = event
	select {
	case client.raceAudioQueue <- clip:
		source.relay.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("ready", language, event, durationMS, ""))
	default:
		source.relay.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("failed", language, event, 0, "playback_queue_full"))
	}
}

func raceAudioMetadataForEvent(state string, language string, event raceAudioEvent, durationMS int, errorCode string) raceAudioMetadata {
	return raceAudioMetadata{
		Type:       "race_audio",
		Version:    raceAudioProtocolVersion,
		State:      state,
		EventID:    event.EventID,
		Kind:       event.Kind,
		Priority:   event.Priority,
		Language:   language,
		DurationMS: durationMS,
		FallbackText: map[string]string{
			"en-US": event.EnglishText,
			"ja-JP": event.JapaneseText,
		},
		Ducking: &raceAudioDucking{M5AudioGain: 0.4, AttackMS: 80, ReleaseMS: 250},
		Error:   errorCode,
	}
}

func (client *viewer) raceAudioLanguageValue(fallback string) string {
	if value := client.raceAudioLanguage.Load(); value != nil {
		if language, ok := value.(string); ok && language != "" {
			return language
		}
	}
	return fallback
}

func (r *relay) activeRaceAudioPilotID() uint64 {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	return r.pilotID
}

func (r *relay) activeRaceAudioPilot() *viewer {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	client := r.viewers[r.pilotID]
	if client == nil || client.raceAudioTrack == nil {
		return nil
	}
	return client
}

func (r *relay) configureRaceAudioPeer(client *viewer, pc *webrtc.PeerConnection) error {
	if r.raceAudio == nil || client == nil || client.role != "pilot" {
		return nil
	}
	track, err := webrtc.NewTrackLocalStaticSample(opusCodec, fmt.Sprintf("race-audio-%d", client.id), "momo-race-audio")
	if err != nil {
		return fmt.Errorf("create race audio track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return fmt.Errorf("add race audio track: %w", err)
	}
	client.raceAudioTrack = track
	client.raceAudioQueue = make(chan raceAudioClip, raceAudioQueueSize)
	client.raceAudioStop = make(chan struct{})
	client.raceAudioLanguage.Store(r.raceAudio.service.defaultLanguage)
	go drainRaceAudioRTCP(sender)
	go r.runRaceAudioTrack(client)
	return nil
}

func drainRaceAudioRTCP(sender *webrtc.RTPSender) {
	for {
		if _, _, err := sender.ReadRTCP(); err != nil {
			return
		}
	}
}

func (r *relay) runRaceAudioTrack(client *viewer) {
	ticker := time.NewTicker(raceAudioPacketDuration)
	defer ticker.Stop()
	var current *raceAudioClip
	packetIndex := 0
	for {
		select {
		case <-client.raceAudioStop:
			return
		case <-ticker.C:
			if current == nil {
				select {
				case clip := <-client.raceAudioQueue:
					current = &clip
					packetIndex = 0
					language := client.raceAudioLanguageValue(r.raceAudio.service.defaultLanguage)
					r.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("playing", language, clip.event,
						len(clip.packets)*int(raceAudioPacketDuration/time.Millisecond), ""))
				default:
				}
			}
			packet := opusSilencePacket
			if current != nil {
				packet = current.packets[packetIndex]
				packetIndex++
			}
			if err := client.raceAudioTrack.WriteSample(media.Sample{Data: packet, Duration: raceAudioPacketDuration}); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					log.Printf("source %q: write race audio for viewer %d: %v", r.name, client.id, err)
				}
				return
			}
			if current != nil && packetIndex >= len(current.packets) {
				language := client.raceAudioLanguageValue(r.raceAudio.service.defaultLanguage)
				r.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("ended", language, current.event, 0, ""))
				current = nil
				packetIndex = 0
			}
		}
	}
}

func (r *relay) handleRaceAudioChannel(client *viewer, channel *webrtc.DataChannel) {
	if r.raceAudio == nil || r.raceAudio.service == nil {
		_ = channel.Close()
		return
	}
	channel.OnOpen(func() {
		client.raceAudio.Store(channel)
		language := client.raceAudioLanguageValue(r.raceAudio.service.defaultLanguage)
		r.sendRaceAudioMetadata(client, raceAudioMetadata{
			Type:     "race_audio_capabilities",
			Version:  raceAudioProtocolVersion,
			State:    "enabled",
			Language: language,
		})
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			return
		}
		var preference raceAudioPreference
		if err := json.Unmarshal(message.Data, &preference); err != nil ||
			preference.Type != "race_audio_preference" || preference.Version != raceAudioProtocolVersion {
			return
		}
		language, err := normalizeRaceAudioLanguage(preference.Language)
		if err != nil {
			return
		}
		client.raceAudioLanguage.Store(language)
		log.Printf("source %q: viewer %d race audio language=%s", r.name, client.id, language)
	})
	channel.OnClose(func() { client.raceAudio.CompareAndSwap(channel, nil) })
}

func (r *relay) sendRaceAudioMetadata(client *viewer, metadata raceAudioMetadata) bool {
	if client == nil {
		return false
	}
	channel := client.raceAudio.Load()
	if channel == nil {
		return false
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return false
	}
	client.raceAudioSendMu.Lock()
	defer client.raceAudioSendMu.Unlock()
	if client.raceAudio.Load() != channel {
		return false
	}
	if err := channel.SendText("RACE_AUDIO:" + string(payload)); err != nil {
		client.raceAudio.CompareAndSwap(channel, nil)
		return false
	}
	return true
}
