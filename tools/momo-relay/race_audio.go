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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	raceAudioLabel                 = "momo-race-audio"
	raceAudioProtocolVersion       = 1
	raceAudioPacketDuration        = 20 * time.Millisecond
	raceAudioQueueSize             = 4
	raceAudioJobQueueSize          = 16
	raceAudioMaximumResponse       = 4 * 1024 * 1024
	raceAudioSynthesisTimeout      = 12 * time.Second
	raceAudioDefaultLanguage       = "en-US"
	raceAudioDefaultEnglishVoice   = "am_michael"
	raceAudioDefaultJapaneseVoice  = "jf_alpha"
	raceAudioModeRemote            = "remote"
	raceAudioModeBrowserKokoro     = "browser-kokoro"
	raceAudioBrowserModelID        = "onnx-community/Kokoro-82M-v1.0-ONNX"
	raceAudioCalloutMinimumGap     = 2 * time.Second
	raceAudioCalloutSeenTTL        = 2 * time.Minute
	raceAudioCalloutMaximumMessage = 512
	raceAudioBlueFlagWarningGapMS  = 3000
	raceAudioBlueFlagReleaseGapMS  = 4000
	raceAudioFuelLowThreshold      = 20.0
	raceAudioFuelCriticalThreshold = 8.0
)

var raceAudioCalloutRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

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
	Type         string                  `json:"type"`
	Version      int                     `json:"version"`
	State        string                  `json:"state"`
	EventID      string                  `json:"eventId,omitempty"`
	Kind         string                  `json:"kind,omitempty"`
	Priority     int                     `json:"priority,omitempty"`
	Language     string                  `json:"language,omitempty"`
	DurationMS   int                     `json:"durationMs,omitempty"`
	FallbackText map[string]string       `json:"fallbackText,omitempty"`
	Ducking      *raceAudioDucking       `json:"ducking,omitempty"`
	Modes        []string                `json:"modes,omitempty"`
	Prompt       *raceAudioBrowserPrompt `json:"prompt,omitempty"`
	Error        string                  `json:"error,omitempty"`
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
	Mode     string `json:"mode,omitempty"`
}

type raceAudioCalloutRequest struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	RequestID string `json:"requestId"`
	Kind      string `json:"kind"`
	CarNumber int    `json:"carNumber"`
	GapMS     int    `json:"gapMs"`
}

type raceAudioLapHistory struct {
	CarID       string `json:"carId"`
	Lap         int    `json:"lap"`
	LapTimeMS   int    `json:"lapTimeMs"`
	Achievement string `json:"achievement"`
}

type raceAudioStanding struct {
	CarID              string `json:"carId"`
	Position           int    `json:"position"`
	Status             string `json:"status"`
	Lap                int    `json:"lap"`
	LappingCarBehindID string `json:"lappingCarBehindId"`
	LappingGapMS       int    `json:"lappingGapMs"`
}

type raceAudioState struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	RaceID      string `json:"raceId"`
	RaceRunID   string `json:"raceRunId"`
	Phase       string `json:"phase"`
	ViewerCarID string `json:"viewerCarId"`
	RaceInfo    struct {
		TotalLaps   int    `json:"totalLaps"`
		SessionType string `json:"sessionType"`
	} `json:"raceInfo"`
	Standings  []raceAudioStanding   `json:"standings"`
	LapHistory []raceAudioLapHistory `json:"lapHistory"`
}

type raceAudioDetector struct {
	mu            sync.Mutex
	initialized   bool
	runID         string
	carID         string
	phase         string
	sessionType   string
	position      int
	blueFlagCarID string
	finished      bool
	seenLaps      map[string]struct{}
}

type raceAudioRaceContext struct {
	RunID       string
	CarID       string
	Phase       string
	SessionType string
}

type raceAudioGameplayDetector struct {
	mu          sync.Mutex
	initialized bool
	runID       string
	fuelBand    int
	damageMode  string
}

type raceAudioPitDetector struct {
	mu               sync.Mutex
	initialized      bool
	runID            string
	entryID          string
	present          bool
	serviceState     string
	completedEntryID string
}

type raceAudioSource struct {
	relay            *relay
	service          *raceAudioServiceClient
	detector         raceAudioDetector
	gameplayDetector raceAudioGameplayDetector
	pitDetector      raceAudioPitDetector
	jobs             chan raceAudioJob
}

type raceAudioJob struct {
	event          raceAudioEvent
	targetClientID uint64
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

func normalizeRaceAudioMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", raceAudioModeRemote:
		return raceAudioModeRemote, nil
	case raceAudioModeBrowserKokoro:
		return raceAudioModeBrowserKokoro, nil
	default:
		return "", fmt.Errorf("unsupported race audio mode %q", value)
	}
}

func (client *raceAudioServiceClient) voiceForLanguage(language string) string {
	if language == "ja-JP" {
		return client.japaneseVoice
	}
	return client.englishVoice
}

func raceAudioTextForLanguage(event raceAudioEvent, language string) string {
	if language == "ja-JP" {
		return event.JapaneseText
	}
	return event.EnglishText
}

func (client *raceAudioServiceClient) prepare(ctx context.Context, event raceAudioEvent,
	language string) (*raceAudioBrowserPrompt, error) {
	if client == nil {
		return nil, errors.New("race audio service is disabled")
	}
	voice := client.voiceForLanguage(language)
	payload, err := json.Marshal(raceAudioPromptRequest{
		EventKey: event.EventID,
		Language: language,
		Voice:    voice,
		Text:     raceAudioTextForLanguage(event, language),
		Speed:    client.speed,
	})
	if err != nil {
		return nil, fmt.Errorf("encode race audio prompt request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/prepare", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create race audio prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if client.token != "" {
		req.Header.Set("Authorization", "Bearer "+client.token)
	}
	response, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request race audio prompt: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, raceAudioMaximumResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read race audio prompt response: %w", err)
	}
	if len(body) > raceAudioMaximumResponse {
		return nil, errors.New("race audio prompt response exceeds 4 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("race audio service returned HTTP %d for prompt", response.StatusCode)
	}
	var decoded raceAudioBrowserPrompt
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode race audio prompt response: %w", err)
	}
	if err := validateRaceAudioBrowserPrompt(decoded, language, voice, client.speed); err != nil {
		return nil, err
	}
	return &decoded, nil
}

func validateRaceAudioBrowserPrompt(prompt raceAudioBrowserPrompt, language string,
	voice string, speed float64) error {
	if prompt.Version != raceAudioProtocolVersion || prompt.Engine != "kokoro" ||
		prompt.ModelID != raceAudioBrowserModelID || prompt.Language != language ||
		prompt.Voice != voice || prompt.Speed != speed {
		return errors.New("race audio prompt has an unsupported synthesis contract")
	}
	if len(prompt.Phonemes) == 0 || len(prompt.Phonemes) > 4096 ||
		len(prompt.ModelInputIDs) < 3 || len(prompt.ModelInputIDs) > 1024 {
		return errors.New("race audio prompt is empty or exceeds browser limits")
	}
	last := len(prompt.ModelInputIDs) - 1
	if prompt.ModelInputIDs[0] != 0 || prompt.ModelInputIDs[last] != 0 {
		return errors.New("race audio prompt is missing model boundary tokens")
	}
	for _, token := range prompt.ModelInputIDs {
		if token < 0 {
			return errors.New("race audio prompt contains a negative model token")
		}
	}
	return nil
}

func (client *raceAudioServiceClient) synthesize(ctx context.Context, event raceAudioEvent,
	language string) (raceAudioClip, int, error) {
	if client == nil {
		return raceAudioClip{}, 0, errors.New("race audio service is disabled")
	}
	payload, err := json.Marshal(raceAudioSynthesisRequest{
		EventKey:        event.EventID,
		Language:        language,
		Voice:           client.voiceForLanguage(language),
		Text:            raceAudioTextForLanguage(event, language),
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

func (detector *raceAudioDetector) context() raceAudioRaceContext {
	detector.mu.Lock()
	defer detector.mu.Unlock()
	return raceAudioRaceContext{
		RunID:       detector.runID,
		CarID:       detector.carID,
		Phase:       detector.phase,
		SessionType: detector.sessionType,
	}
}

func raceAudioStandingForCar(state raceAudioState, carID string) *raceAudioStanding {
	for index := range state.Standings {
		if state.Standings[index].CarID == carID {
			return &state.Standings[index]
		}
	}
	return nil
}

func raceAudioCarNumber(carID string) int {
	carID = strings.TrimSpace(carID)
	start := len(carID)
	for start > 0 && carID[start-1] >= '0' && carID[start-1] <= '9' {
		start--
	}
	if start == len(carID) {
		return 0
	}
	value, err := strconv.Atoi(carID[start:])
	if err != nil || value < 1 || value > 999 {
		return 0
	}
	return value
}

func raceAudioBlueFlagCarID(state raceAudioState, self *raceAudioStanding, previousCarID string) string {
	if self == nil || !strings.EqualFold(strings.TrimSpace(state.Phase), "green") ||
		!strings.EqualFold(strings.TrimSpace(self.Status), "racing") {
		return ""
	}
	carID := strings.TrimSpace(self.LappingCarBehindID)
	if carID == "" || self.LappingGapMS <= 0 {
		return ""
	}
	lapping := raceAudioStandingForCar(state, carID)
	if lapping == nil || !strings.EqualFold(strings.TrimSpace(lapping.Status), "racing") ||
		lapping.Lap <= self.Lap || lapping.Position >= self.Position {
		return ""
	}
	maximumGapMS := raceAudioBlueFlagWarningGapMS
	if carID == previousCarID {
		maximumGapMS = raceAudioBlueFlagReleaseGapMS
	}
	if self.LappingGapMS > maximumGapMS {
		return ""
	}
	return carID
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
	phase := strings.ToLower(strings.TrimSpace(state.Phase))
	sessionType := strings.ToLower(strings.TrimSpace(state.RaceInfo.SessionType))
	detector.mu.Lock()
	defer detector.mu.Unlock()
	if detector.runID != runID {
		detector.initialized = false
		detector.runID = runID
		detector.carID = carID
		detector.phase = ""
		detector.sessionType = ""
		detector.position = 0
		detector.blueFlagCarID = ""
		detector.finished = false
		detector.seenLaps = make(map[string]struct{})
	}
	if detector.seenLaps == nil {
		detector.seenLaps = make(map[string]struct{})
	}
	histories := make([]raceAudioLapHistory, 0, len(state.LapHistory))
	for _, history := range state.LapHistory {
		if history.CarID == carID && history.Lap > 0 && history.LapTimeMS > 0 {
			histories = append(histories, history)
		}
	}
	sort.Slice(histories, func(left, right int) bool { return histories[left].Lap < histories[right].Lap })
	standing := raceAudioStandingForCar(state, carID)
	standingPosition := 0
	standingStatus := ""
	if standing != nil {
		standingPosition = standing.Position
		standingStatus = strings.ToLower(strings.TrimSpace(standing.Status))
	}
	isFinished := strings.EqualFold(strings.TrimSpace(state.Phase), "finished") || standingStatus == "finished"
	blueFlagCarID := raceAudioBlueFlagCarID(state, standing, detector.blueFlagCarID)
	if !detector.initialized {
		for _, history := range histories {
			detector.seenLaps[raceAudioLapKey(runID, carID, history.Lap, history.LapTimeMS)] = struct{}{}
		}
		detector.finished = isFinished
		detector.carID = carID
		detector.phase = phase
		detector.sessionType = sessionType
		detector.position = standingPosition
		detector.blueFlagCarID = blueFlagCarID
		detector.initialized = true
		return nil
	}
	previousPhase := detector.phase
	previousPosition := detector.position
	previousBlueFlagCarID := detector.blueFlagCarID
	events := make([]raceAudioEvent, 0, 3)
	for _, history := range histories {
		key := raceAudioLapKey(runID, carID, history.Lap, history.LapTimeMS)
		if _, exists := detector.seenLaps[key]; exists {
			continue
		}
		detector.seenLaps[key] = struct{}{}
		if isFinished || (state.RaceInfo.TotalLaps > 0 && history.Lap >= state.RaceInfo.TotalLaps) {
			continue
		}
		isFinalLap := sessionType == "race" && state.RaceInfo.TotalLaps > 1 &&
			history.Lap == state.RaceInfo.TotalLaps-1
		events = append(events, raceAudioEvent{
			EventID:      key,
			Kind:         "lap_complete",
			Priority:     40,
			EnglishText:  raceAudioEnglishLapText(history.Lap, history.LapTimeMS, history.Achievement, isFinalLap),
			JapaneseText: raceAudioJapaneseLapText(history.Lap, history.LapTimeMS, history.Achievement, isFinalLap),
		})
	}
	if sessionType == "race" && phase == "green" && previousPhase != "green" && previousPhase != "paused" {
		englishText := raceAudioEnglishStartPositionText(standingPosition, state.RaceInfo.TotalLaps == 1)
		japaneseText := raceAudioJapaneseStartPositionText(standingPosition, state.RaceInfo.TotalLaps == 1)
		if englishText != "" || japaneseText != "" {
			events = append(events, raceAudioEvent{
				EventID:      fmt.Sprintf("%s:%s:race_start", runID, carID),
				Kind:         "race_start",
				Priority:     55,
				EnglishText:  englishText,
				JapaneseText: japaneseText,
			})
		}
	} else if sessionType == "race" && phase == "green" && previousPhase == "paused" {
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:race_resumed", runID, carID),
			Kind:         "race_resumed",
			Priority:     90,
			EnglishText:  "Green flag. Race resumed.",
			JapaneseText: "グリーン。レース再開。",
		})
	} else if sessionType == "race" && phase == "paused" && previousPhase == "green" {
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:race_paused", runID, carID),
			Kind:         "race_paused",
			Priority:     95,
			EnglishText:  "Race stopped. Hold position.",
			JapaneseText: "レース停止。現在位置を維持してください。",
		})
	}
	if sessionType == "race" && phase == "green" && blueFlagCarID != "" &&
		blueFlagCarID != previousBlueFlagCarID {
		carNumber := raceAudioCarNumber(blueFlagCarID)
		englishText := "Blue flag. Faster car behind."
		japaneseText := "ブルーフラッグ。後方から速い車両が接近しています。"
		if carNumber > 0 {
			englishText = fmt.Sprintf("Blue flag. Car %d behind.", carNumber)
			japaneseText = fmt.Sprintf("ブルーフラッグ。後方、%d号車。", carNumber)
		}
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:blue_flag:%s", runID, carID, blueFlagCarID),
			Kind:         "blue_flag",
			Priority:     85,
			EnglishText:  englishText,
			JapaneseText: japaneseText,
		})
	}
	if sessionType == "race" && phase == "green" && previousPhase == "green" &&
		standingPosition > 0 && previousPosition > 0 && standingPosition != previousPosition && len(events) == 0 {
		events = append(events, raceAudioPositionEvent(runID, carID, previousPosition, standingPosition))
	}
	if !detector.finished && isFinished {
		finalLapTimeMS := 0
		if len(histories) > 0 {
			finalLapTimeMS = histories[len(histories)-1].LapTimeMS
		}
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:race_finish", runID, carID),
			Kind:         "race_finish",
			Priority:     70,
			EnglishText:  raceAudioEnglishFinishText(standingPosition, finalLapTimeMS),
			JapaneseText: raceAudioJapaneseFinishText(standingPosition, finalLapTimeMS),
		})
	}
	detector.carID = carID
	detector.phase = phase
	detector.sessionType = sessionType
	detector.position = standingPosition
	detector.blueFlagCarID = blueFlagCarID
	detector.finished = detector.finished || isFinished
	sort.SliceStable(events, func(left, right int) bool {
		return events[left].Priority > events[right].Priority
	})
	return events
}

func raceAudioFuelBand(fuel float64) int {
	switch {
	case fuel <= 0:
		return 3
	case fuel <= raceAudioFuelCriticalThreshold:
		return 2
	case fuel <= raceAudioFuelLowThreshold:
		return 1
	default:
		return 0
	}
}

func raceAudioDamageRank(mode string) int {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "limp":
		return 3
	case "critical":
		return 2
	case "damaged":
		return 1
	default:
		return 0
	}
}

func (detector *raceAudioGameplayDetector) observe(
	snapshot vehicleHealthSnapshot,
	context raceAudioRaceContext,
) []raceAudioEvent {
	detector.mu.Lock()
	defer detector.mu.Unlock()

	runID := strings.TrimSpace(context.RunID)
	carID := strings.TrimSpace(context.CarID)
	activeRace := runID != "" && strings.EqualFold(context.SessionType, "race") &&
		strings.EqualFold(context.Phase, "green")
	if !activeRace {
		detector.initialized = false
		detector.runID = runID
		return nil
	}
	if detector.runID != runID {
		detector.initialized = false
		detector.runID = runID
	}
	fuelBand := raceAudioFuelBand(snapshot.Fuel)
	damageMode := strings.ToLower(strings.TrimSpace(snapshot.Mode))
	if !detector.initialized {
		detector.initialized = true
		detector.fuelBand = fuelBand
		detector.damageMode = damageMode
		return nil
	}

	previousFuelBand := detector.fuelBand
	previousDamageRank := raceAudioDamageRank(detector.damageMode)
	detector.fuelBand = fuelBand
	detector.damageMode = damageMode
	events := make([]raceAudioEvent, 0, 2)
	if fuelBand > previousFuelBand {
		switch fuelBand {
		case 1:
			events = append(events, raceAudioEvent{
				EventID:      fmt.Sprintf("%s:%s:fuel_low", runID, carID),
				Kind:         "fuel_low",
				Priority:     45,
				EnglishText:  "Fuel low.",
				JapaneseText: "燃料残量低下。",
			})
		case 2:
			events = append(events, raceAudioEvent{
				EventID:      fmt.Sprintf("%s:%s:fuel_critical", runID, carID),
				Kind:         "fuel_critical",
				Priority:     75,
				EnglishText:  "Fuel critical.",
				JapaneseText: "燃料残量、危険域。",
			})
		case 3:
			events = append(events, raceAudioEvent{
				EventID:      fmt.Sprintf("%s:%s:fuel_empty", runID, carID),
				Kind:         "fuel_empty",
				Priority:     100,
				EnglishText:  "Fuel empty. Power limited.",
				JapaneseText: "燃料切れ。出力制限中。",
			})
		}
	}
	currentDamageRank := raceAudioDamageRank(damageMode)
	if previousDamageRank < 2 && currentDamageRank >= 2 {
		englishText := "Critical damage. Power limited."
		japaneseText := "重大ダメージ。出力制限中。"
		priority := 80
		if currentDamageRank >= 3 {
			englishText = "Critical damage. Power severely limited."
			japaneseText = "重大ダメージ。出力を大幅に制限しています。"
			priority = 95
		}
		events = append(events, raceAudioEvent{
			EventID:      fmt.Sprintf("%s:%s:damage_critical", runID, carID),
			Kind:         "damage_critical",
			Priority:     priority,
			EnglishText:  englishText,
			JapaneseText: japaneseText,
		})
	}
	sort.SliceStable(events, func(left, right int) bool {
		return events[left].Priority > events[right].Priority
	})
	return events
}

func (detector *raceAudioPitDetector) observe(snapshot pitPresenceSnapshot) *raceAudioEvent {
	runID := strings.TrimSpace(snapshot.RaceRunID)
	entryID := strings.TrimSpace(snapshot.EntryID)
	serviceState := strings.ToLower(strings.TrimSpace(snapshot.ServiceState))
	detector.mu.Lock()
	defer detector.mu.Unlock()

	if detector.runID != runID {
		detector.initialized = false
		detector.runID = runID
		detector.completedEntryID = ""
	}
	if !detector.initialized {
		detector.initialized = true
		detector.entryID = entryID
		detector.present = snapshot.Present
		detector.serviceState = serviceState
		return nil
	}

	previousEntryID := detector.entryID
	previousPresent := detector.present
	previousServiceState := detector.serviceState
	detector.entryID = entryID
	detector.present = snapshot.Present
	detector.serviceState = serviceState

	if runID == "" || entryID == "" || detector.completedEntryID == entryID ||
		!previousPresent || !snapshot.Present || previousEntryID != entryID ||
		previousServiceState != "servicing" || serviceState != "complete" {
		return nil
	}
	detector.completedEntryID = entryID
	return &raceAudioEvent{
		EventID:      fmt.Sprintf("%s:%s:pit_service_complete:%s", runID, snapshot.CarID, entryID),
		Kind:         "pit_service_complete",
		Priority:     65,
		EnglishText:  "Pit service complete.",
		JapaneseText: "ピットサービス完了。",
	}
}

func raceAudioEnglishStartPositionText(position int, finalLap bool) string {
	parts := make([]string, 0, 2)
	if position > 0 {
		parts = append(parts, fmt.Sprintf("Position %d.", position))
	}
	if finalLap {
		parts = append(parts, "Final lap.")
	}
	return strings.Join(parts, " ")
}

func raceAudioJapaneseStartPositionText(position int, finalLap bool) string {
	parts := make([]string, 0, 2)
	if position > 0 {
		parts = append(parts, fmt.Sprintf("現在%d位。", position))
	}
	if finalLap {
		parts = append(parts, "ファイナルラップ。")
	}
	return strings.Join(parts, "")
}

func raceAudioPositionEvent(runID string, carID string, previousPosition int, position int) raceAudioEvent {
	direction := "gained"
	englishText := fmt.Sprintf("Position gained. Position %d.", position)
	japaneseText := fmt.Sprintf("%d位に上がりました。", position)
	if position > previousPosition {
		direction = "lost"
		englishText = fmt.Sprintf("Position lost. Position %d.", position)
		japaneseText = fmt.Sprintf("現在%d位。", position)
	}
	return raceAudioEvent{
		EventID:      fmt.Sprintf("%s:%s:position:%d:%s", runID, carID, position, direction),
		Kind:         "position_change",
		Priority:     50,
		EnglishText:  englishText,
		JapaneseText: japaneseText,
	}
}

func raceAudioEnglishLapText(lap int, lapTimeMS int, achievement string, finalLap ...bool) string {
	text := fmt.Sprintf("Lap %d. %s seconds.", lap, raceAudioEnglishLapTime(lapTimeMS))
	switch achievement {
	case "overall_best":
		text += " New overall best."
	case "personal_best":
		text += " New personal best."
	default:
		text = strings.TrimSuffix(text, ".")
	}
	if len(finalLap) > 0 && finalLap[0] {
		text = strings.TrimSuffix(text, ".") + ". Final lap."
	}
	return text
}

func raceAudioEnglishLapTime(lapTimeMS int) string {
	digitWords := [...]string{
		"zero", "one", "two", "three", "four",
		"five", "six", "seven", "eight", "nine",
	}
	seconds := lapTimeMS / 1000
	milliseconds := lapTimeMS % 1000
	return fmt.Sprintf(
		"%d point %s %s %s",
		seconds,
		digitWords[milliseconds/100],
		digitWords[(milliseconds/10)%10],
		digitWords[milliseconds%10],
	)
}

func raceAudioJapaneseLapText(lap int, lapTimeMS int, achievement string, finalLap ...bool) string {
	text := fmt.Sprintf("%d周目、%d.%03d", lap, lapTimeMS/1000, lapTimeMS%1000)
	switch achievement {
	case "overall_best":
		text += "。全体ベスト更新。"
	case "personal_best":
		text += "。自己ベスト更新。"
	}
	if len(finalLap) > 0 && finalLap[0] {
		text = strings.TrimSuffix(text, "。") + "。ファイナルラップ。"
	}
	return text
}

func raceAudioEventFromCallout(clientID uint64, request raceAudioCalloutRequest) (raceAudioEvent, error) {
	requestID := strings.TrimSpace(request.RequestID)
	kind := strings.ToLower(strings.TrimSpace(request.Kind))
	if request.Type != "race_audio_callout_request" || request.Version != raceAudioProtocolVersion ||
		!raceAudioCalloutRequestIDPattern.MatchString(requestID) {
		return raceAudioEvent{}, errors.New("invalid race audio callout envelope")
	}
	if kind != "gap_ahead" && kind != "gap_behind" {
		return raceAudioEvent{}, errors.New("unsupported race audio callout kind")
	}
	if request.CarNumber < 1 || request.CarNumber > 999 || request.GapMS < 100 ||
		request.GapMS > 5000 || request.GapMS%100 != 0 {
		return raceAudioEvent{}, errors.New("invalid race audio callout value")
	}
	directionEN := "ahead"
	directionJA := "前"
	priority := 30
	if kind == "gap_behind" {
		directionEN = "behind"
		directionJA = "後ろ"
		priority = 60
		if request.GapMS <= 1000 {
			priority = 80
		}
	}
	return raceAudioEvent{
		EventID:      fmt.Sprintf("pilot:%d:%s", clientID, requestID),
		Kind:         kind,
		Priority:     priority,
		EnglishText:  fmt.Sprintf("Car %d %s. Gap %s seconds", request.CarNumber, directionEN, raceAudioEnglishGap(request.GapMS)),
		JapaneseText: fmt.Sprintf("%s、%d号車、差%d.%d", directionJA, request.CarNumber, request.GapMS/1000, (request.GapMS%1000)/100),
	}, nil
}

func raceAudioEnglishGap(gapMS int) string {
	digitWords := [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	seconds := gapMS / 1000
	tenths := (gapMS % 1000) / 100
	if tenths == 0 {
		return digitWords[seconds]
	}
	return fmt.Sprintf("%s point %s", digitWords[seconds], digitWords[tenths])
}

type raceAudioPromptRequest struct {
	EventKey string  `json:"eventKey"`
	Language string  `json:"language"`
	Voice    string  `json:"voice"`
	Text     string  `json:"text"`
	Speed    float64 `json:"speed"`
}

type raceAudioBrowserPrompt struct {
	Version       int     `json:"version"`
	Engine        string  `json:"engine"`
	ModelID       string  `json:"modelId"`
	Language      string  `json:"language"`
	Voice         string  `json:"voice"`
	Speed         float64 `json:"speed"`
	Phonemes      string  `json:"phonemes"`
	ModelInputIDs []int   `json:"modelInputIds"`
	PhonemePolicy string  `json:"phonemePolicy"`
}

func raceAudioEnglishFinishText(position int, finalLapTimeMS int) string {
	parts := []string{"Checkered flag."}
	if position > 0 {
		parts = append(parts, fmt.Sprintf("P %d.", position))
	}
	if finalLapTimeMS > 0 {
		parts = append(parts, fmt.Sprintf("Final lap, %s seconds.", raceAudioEnglishLapTime(finalLapTimeMS)))
	}
	return strings.Join(parts, " ")
}

func raceAudioJapaneseFinishText(position int, finalLapTimeMS int) string {
	parts := []string{"ゴール。"}
	if position > 0 {
		parts = append(parts, fmt.Sprintf("%d位。", position))
	}
	if finalLapTimeMS > 0 {
		parts = append(parts, fmt.Sprintf("最終ラップ、%d.%03d秒。", finalLapTimeMS/1000, finalLapTimeMS%1000))
	}
	return strings.Join(parts, " ")
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
		jobs:    make(chan raceAudioJob, raceAudioJobQueueSize),
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
			case job := <-source.jobs:
				source.dispatch(ctx, job)
			}
		}
	}()
}

func (source *raceAudioSource) observe(message string) {
	if source == nil {
		return
	}
	source.enqueueEvents(source.detector.observe(message, source.relay.raceCarID))
}

func (source *raceAudioSource) observeVehicleGameplay(snapshot vehicleHealthSnapshot) {
	if source == nil {
		return
	}
	source.enqueueEvents(source.gameplayDetector.observe(snapshot, source.detector.context()))
}

func (source *raceAudioSource) enqueueEvents(events []raceAudioEvent) {
	for _, event := range events {
		select {
		case source.jobs <- raceAudioJob{event: event}:
		default:
			log.Printf("source %q: drop race audio event %q because the queue is full", source.relay.name, event.EventID)
		}
	}
}

func (source *raceAudioSource) observePit(snapshot pitPresenceSnapshot) {
	if source == nil {
		return
	}
	event := source.pitDetector.observe(snapshot)
	if event == nil {
		return
	}
	select {
	case source.jobs <- raceAudioJob{event: *event}:
	default:
		log.Printf("source %q: drop race audio event %q because the queue is full", source.relay.name, event.EventID)
	}
}

func (source *raceAudioSource) enqueueCallout(client *viewer, event raceAudioEvent) bool {
	if source == nil || client == nil {
		return false
	}
	select {
	case source.jobs <- raceAudioJob{event: event, targetClientID: client.id}:
		return true
	default:
		return false
	}
}

func raceAudioBrowserLocalEvent(kind string) bool {
	switch kind {
	case "lap_complete", "pit_service_complete", "gap_ahead", "gap_behind",
		"race_start", "race_paused", "race_resumed", "position_change", "blue_flag",
		"fuel_low", "fuel_critical", "fuel_empty", "damage_critical":
		return true
	default:
		return false
	}
}

func (source *raceAudioSource) dispatch(parent context.Context, job raceAudioJob) {
	client := source.relay.activeRaceAudioPilot()
	if client == nil || (job.targetClientID != 0 && job.targetClientID != client.id) {
		return
	}
	event := job.event
	language := client.raceAudioLanguageValue(source.service.defaultLanguage)
	if language == "off" || client.raceAudio.Load() == nil {
		return
	}
	mode := client.raceAudioModeValue()
	if mode == raceAudioModeBrowserKokoro && raceAudioBrowserLocalEvent(event.Kind) {
		source.relay.sendRaceAudioMetadata(client, raceAudioMetadataForEvent("queued", language, event, 0, ""))
		ctx, cancel := context.WithTimeout(parent, raceAudioSynthesisTimeout)
		prompt, err := source.service.prepare(ctx, event, language)
		cancel()
		if err != nil {
			log.Printf("source %q: prepare browser race audio event %q: %v", source.relay.name, event.EventID, err)
			source.relay.sendRaceAudioMetadata(client,
				raceAudioMetadataForEvent("failed", language, event, 0, "prompt_prepare_failed"))
			return
		}
		if source.relay.activeRaceAudioPilotID() != client.id || client.raceAudio.Load() == nil ||
			client.raceAudioLanguageValue(source.service.defaultLanguage) != language ||
			client.raceAudioModeValue() != mode {
			return
		}
		metadata := raceAudioMetadataForEvent("prompt", language, event, 0, "")
		metadata.Prompt = prompt
		source.relay.sendRaceAudioMetadata(client, metadata)
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

func (client *viewer) raceAudioModeValue() string {
	if value := client.raceAudioMode.Load(); value != nil {
		if mode, ok := value.(string); ok && mode != "" {
			return mode
		}
	}
	return raceAudioModeRemote
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
	client.raceAudioMode.Store(raceAudioModeRemote)
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
			Modes:    []string{raceAudioModeRemote, raceAudioModeBrowserKokoro},
		})
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString || len(message.Data) > raceAudioCalloutMaximumMessage {
			return
		}
		var envelope struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(message.Data, &envelope); err != nil || envelope.Version != raceAudioProtocolVersion {
			return
		}
		switch envelope.Type {
		case "race_audio_preference":
			var preference raceAudioPreference
			if err := json.Unmarshal(message.Data, &preference); err != nil {
				return
			}
			language, err := normalizeRaceAudioLanguage(preference.Language)
			if err != nil {
				return
			}
			mode, err := normalizeRaceAudioMode(preference.Mode)
			if err != nil {
				return
			}
			client.raceAudioLanguage.Store(language)
			client.raceAudioMode.Store(mode)
			log.Printf("source %q: viewer %d race audio language=%s mode=%s", r.name, client.id, language, mode)
		case "race_audio_callout_request":
			if client.raceAudioModeValue() != raceAudioModeBrowserKokoro || r.activeRaceAudioPilotID() != client.id {
				return
			}
			var request raceAudioCalloutRequest
			if err := json.Unmarshal(message.Data, &request); err != nil {
				return
			}
			event, err := raceAudioEventFromCallout(client.id, request)
			if err != nil || !client.acceptRaceAudioCallout(request.RequestID, time.Now()) {
				return
			}
			if !r.raceAudio.enqueueCallout(client, event) {
				r.sendRaceAudioMetadata(client,
					raceAudioMetadataForEvent("failed", client.raceAudioLanguageValue(r.raceAudio.service.defaultLanguage),
						event, 0, "prompt_queue_full"))
			}
		}
	})
	channel.OnClose(func() { client.raceAudio.CompareAndSwap(channel, nil) })
}

func (client *viewer) acceptRaceAudioCallout(requestID string, now time.Time) bool {
	requestID = strings.TrimSpace(requestID)
	client.raceAudioCalloutMu.Lock()
	defer client.raceAudioCalloutMu.Unlock()
	if client.raceAudioCalloutSeen == nil {
		client.raceAudioCalloutSeen = make(map[string]time.Time)
	}
	if _, duplicate := client.raceAudioCalloutSeen[requestID]; duplicate {
		return false
	}
	if !client.raceAudioCalloutAt.IsZero() && now.Sub(client.raceAudioCalloutAt) < raceAudioCalloutMinimumGap {
		return false
	}
	for key, seenAt := range client.raceAudioCalloutSeen {
		if now.Sub(seenAt) > raceAudioCalloutSeenTTL {
			delete(client.raceAudioCalloutSeen, key)
		}
	}
	client.raceAudioCalloutSeen[requestID] = now
	client.raceAudioCalloutAt = now
	return true
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
