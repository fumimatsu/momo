package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	raceAudioAnnouncementSchemaVersion     = 1
	raceAudioAnnouncementReceiptLimit      = 128
	raceAudioAnnouncementMaximumDurationMS = 45_000
)

var raceAudioAnnouncementKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type raceAudioAnnouncementRequest struct {
	SchemaVersion int                              `json:"schemaVersion"`
	Command       string                           `json:"command"`
	CommandID     string                           `json:"commandId"`
	RaceRunID     string                           `json:"raceRunId"`
	Grid          []raceAudioAnnouncementGridEntry `json:"grid"`
}

type raceAudioAnnouncementGridEntry struct {
	CarID         string `json:"carId"`
	DisplayNumber string `json:"displayNumber,omitempty"`
	PilotName     string `json:"pilotName"`
}

type raceAudioAnnouncementResponse struct {
	SchemaVersion int      `json:"schemaVersion"`
	Status        string   `json:"status"`
	CommandID     string   `json:"commandId"`
	EventID       string   `json:"eventId"`
	RaceRunID     string   `json:"raceRunId"`
	Kind          string   `json:"kind"`
	Language      string   `json:"language"`
	DurationMS    int      `json:"durationMs"`
	TargetCount   int      `json:"targetCount"`
	CarIDs        []string `json:"carIds"`
}

type raceAudioAnnouncementReceipt struct {
	fingerprint string
	response    raceAudioAnnouncementResponse
	createdAt   time.Time
}

type raceAudioBroadcastTarget struct {
	source *relay
	client *viewer
	carID  string
}

func (server *relayServer) serveRaceAudioAnnouncement(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeRaceAudioAnnouncementError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "POST is required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeRaceAudioAnnouncementError(writer, http.StatusUnsupportedMediaType, "application_json_required", "Content-Type must be application/json")
		return
	}
	var command raceAudioAnnouncementRequest
	if err := decodeGameplayJSON(writer, request, &command); err != nil {
		writeRaceAudioAnnouncementError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	grid, err := validateRaceAudioAnnouncementRequest(command)
	if err != nil {
		writeRaceAudioAnnouncementError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	command.Grid = grid
	event := preRaceFormationAnnouncement(command.RaceRunID, grid)
	if utf8.RuneCountInString(event.JapaneseText) > raceAudioCalloutMaximumMessage ||
		utf8.RuneCountInString(event.EnglishText) > raceAudioCalloutMaximumMessage {
		writeRaceAudioAnnouncementError(writer, http.StatusBadRequest, "announcement_too_long", "the locked grid exceeds the single announcement text limit")
		return
	}
	gridJSON, err := json.Marshal(grid)
	if err != nil {
		writeRaceAudioAnnouncementError(writer, http.StatusInternalServerError, "grid_encoding_failed", "the locked grid could not be encoded")
		return
	}
	fingerprint := command.RaceRunID + "\n" + string(gridJSON)
	carIDs := make([]string, 0, len(grid))
	for _, participant := range grid {
		carIDs = append(carIDs, participant.CarID)
	}

	server.raceAudioAnnouncementMu.Lock()
	defer server.raceAudioAnnouncementMu.Unlock()
	if receipt, exists := server.raceAudioAnnouncements[event.EventID]; exists {
		if receipt.fingerprint != fingerprint {
			writeRaceAudioAnnouncementError(writer, http.StatusConflict, "announcement_conflict", "the race announcement was already queued for a different locked grid")
			return
		}
		response := receipt.response
		response.Status = "duplicate"
		response.CommandID = command.CommandID
		writeRaceAudioAnnouncementJSON(writer, http.StatusOK, response)
		return
	}
	if server.sourceRuntime.raceAudioService == nil {
		writeRaceAudioAnnouncementError(writer, http.StatusServiceUnavailable, "race_audio_disabled", "Relay race audio service is not configured")
		return
	}
	targets, missing := server.raceAudioBroadcastTargets(carIDs)
	if len(missing) != 0 {
		writeRaceAudioAnnouncementError(writer, http.StatusConflict, "pilot_audio_not_ready", "race audio is not ready for cars: "+strings.Join(missing, ", "))
		return
	}

	clip, durationMS, err := server.sourceRuntime.raceAudioService.synthesize(request.Context(), event, "ja-JP")
	if err != nil {
		writeRaceAudioAnnouncementError(writer, http.StatusBadGateway, "synthesis_failed", err.Error())
		return
	}
	if durationMS <= 0 || durationMS > raceAudioAnnouncementMaximumDurationMS {
		writeRaceAudioAnnouncementError(writer, http.StatusBadGateway, "invalid_synthesis_duration", "the synthesized formation announcement duration is invalid")
		return
	}
	clip.event = event
	for _, target := range targets {
		target.client.raceAudioQueueMu.Lock()
	}
	queuesLocked := true
	unlockQueues := func() {
		if !queuesLocked {
			return
		}
		for index := len(targets) - 1; index >= 0; index-- {
			targets[index].client.raceAudioQueueMu.Unlock()
		}
		queuesLocked = false
	}
	defer unlockQueues()
	for _, target := range targets {
		if target.source.activeRaceAudioPilotID() != target.client.id || target.client.raceAudioQueue == nil ||
			target.client.raceAudio.Load() == nil ||
			target.client.raceAudioLanguageValue(target.source.raceAudio.service.defaultLanguage) == "off" {
			writeRaceAudioAnnouncementError(writer, http.StatusConflict, "pilot_audio_changed", "Pilot audio changed while the announcement was prepared")
			return
		}
		if len(target.client.raceAudioQueue) >= cap(target.client.raceAudioQueue) {
			writeRaceAudioAnnouncementError(writer, http.StatusServiceUnavailable, "playback_queue_full", "a Pilot race audio queue is full")
			return
		}
	}
	for _, target := range targets {
		target.source.sendRaceAudioMetadata(target.client, raceAudioMetadataForEvent("queued", "ja-JP", event, 0, ""))
	}
	for _, target := range targets {
		target.client.raceAudioQueue <- clip
	}
	unlockQueues()
	for _, target := range targets {
		target.source.sendRaceAudioMetadata(target.client, raceAudioMetadataForEvent("ready", "ja-JP", event, durationMS, ""))
	}
	response := raceAudioAnnouncementResponse{
		SchemaVersion: raceAudioAnnouncementSchemaVersion,
		Status:        "queued",
		CommandID:     command.CommandID,
		EventID:       event.EventID,
		RaceRunID:     command.RaceRunID,
		Kind:          event.Kind,
		Language:      "ja-JP",
		DurationMS:    durationMS,
		TargetCount:   len(targets),
		CarIDs:        append([]string(nil), carIDs...),
	}
	server.storeRaceAudioAnnouncementReceipt(event.EventID, raceAudioAnnouncementReceipt{
		fingerprint: fingerprint, response: response, createdAt: time.Now(),
	})
	writeRaceAudioAnnouncementJSON(writer, http.StatusOK, response)
}

func validateRaceAudioAnnouncementRequest(request raceAudioAnnouncementRequest) ([]raceAudioAnnouncementGridEntry, error) {
	if request.SchemaVersion != raceAudioAnnouncementSchemaVersion || request.Command != "pre_race_formation" ||
		!raceAudioAnnouncementKeyPattern.MatchString(strings.TrimSpace(request.CommandID)) ||
		!raceAudioAnnouncementKeyPattern.MatchString(strings.TrimSpace(request.RaceRunID)) {
		return nil, fmt.Errorf("schemaVersion=1, command=pre_race_formation, commandId, and raceRunId are required")
	}
	if len(request.Grid) < 1 || len(request.Grid) > maximumConfiguredSources {
		return nil, fmt.Errorf("grid must contain 1..%d participants", maximumConfiguredSources)
	}
	grid := make([]raceAudioAnnouncementGridEntry, 0, len(request.Grid))
	seen := make(map[string]struct{}, len(request.Grid))
	for _, value := range request.Grid {
		participant := raceAudioAnnouncementGridEntry{
			CarID:         strings.TrimSpace(value.CarID),
			DisplayNumber: strings.TrimSpace(value.DisplayNumber),
			PilotName:     strings.TrimSpace(value.PilotName),
		}
		if !raceAudioAnnouncementKeyPattern.MatchString(participant.CarID) {
			return nil, fmt.Errorf("grid contains an invalid carId")
		}
		if _, duplicate := seen[participant.CarID]; duplicate {
			return nil, fmt.Errorf("grid contains duplicate carId %q", participant.CarID)
		}
		if !validRaceAudioAnnouncementText(participant.PilotName, 64, false) {
			return nil, fmt.Errorf("grid contains an invalid pilotName for carId %q", participant.CarID)
		}
		if !validRaceAudioAnnouncementText(participant.DisplayNumber, 16, true) {
			return nil, fmt.Errorf("grid contains an invalid displayNumber for carId %q", participant.CarID)
		}
		seen[participant.CarID] = struct{}{}
		grid = append(grid, participant)
	}
	return grid, nil
}

func validRaceAudioAnnouncementText(value string, maximumRunes int, optional bool) bool {
	if value == "" {
		return optional
	}
	if utf8.RuneCountInString(value) > maximumRunes {
		return false
	}
	for _, valueRune := range value {
		if unicode.IsControl(valueRune) {
			return false
		}
	}
	return true
}

func preRaceFormationAnnouncement(raceRunID string, grid []raceAudioAnnouncementGridEntry) raceAudioEvent {
	var english strings.Builder
	var japanese strings.Builder
	english.WriteString("Tsukuraji RC Park. D and D Night FPV RC Race. The formation lap is about to begin. Here is the starting grid. ")
	japanese.WriteString("つくラジRCパーク、DアンドDナイト。FPVRCレース。まもなくフォーメーションラップ。本日のスターティンググリッドを紹介する。")
	for _, participant := range grid {
		number := participant.DisplayNumber
		if number == "" {
			number = participant.CarID
		}
		fmt.Fprintf(&english, "Car number %s, %s. ", number, participant.PilotName)
		fmt.Fprintf(&japanese, "カーナンバー%s、%s。", number, participant.PilotName)
	}
	english.WriteString("Drivers, begin the formation lap, take your grid positions, and wait for the official start signal.")
	japanese.WriteString("ドライバーはフォーメーションラップへ。マシンをグリッドへ導き、オフィシャルのスタートシグナルを待て。")
	return raceAudioEvent{
		EventID:      raceRunID + ":global:pre_race_formation",
		Kind:         "pre_race_formation",
		Priority:     70,
		EnglishText:  english.String(),
		JapaneseText: japanese.String(),
	}
}

func (server *relayServer) raceAudioBroadcastTargets(carIDs []string) ([]raceAudioBroadcastTarget, []string) {
	byCarID := make(map[string]*relay, len(carIDs))
	for _, source := range server.sourceSnapshot() {
		if effectiveRelaySourceKind(source.sourceKind) == relaySourceKindVehicle && source.raceAudio != nil {
			byCarID[strings.TrimSpace(source.raceCarID)] = source
		}
	}
	targets := make([]raceAudioBroadcastTarget, 0, len(carIDs))
	missing := make([]string, 0)
	for _, carID := range carIDs {
		source := byCarID[carID]
		if source == nil {
			missing = append(missing, carID)
			continue
		}
		client := source.activeRaceAudioPilot()
		if client == nil || client.raceAudioQueue == nil || client.raceAudio.Load() == nil ||
			client.raceAudioLanguageValue(source.raceAudio.service.defaultLanguage) == "off" {
			missing = append(missing, carID)
			continue
		}
		targets = append(targets, raceAudioBroadcastTarget{source: source, client: client, carID: carID})
	}
	return targets, missing
}

func (server *relayServer) storeRaceAudioAnnouncementReceipt(eventID string, receipt raceAudioAnnouncementReceipt) {
	if server.raceAudioAnnouncements == nil {
		server.raceAudioAnnouncements = make(map[string]raceAudioAnnouncementReceipt)
	}
	if len(server.raceAudioAnnouncements) >= raceAudioAnnouncementReceiptLimit {
		oldestID := ""
		var oldest time.Time
		for candidateID, candidate := range server.raceAudioAnnouncements {
			if oldestID == "" || candidate.createdAt.Before(oldest) {
				oldestID = candidateID
				oldest = candidate.createdAt
			}
		}
		delete(server.raceAudioAnnouncements, oldestID)
	}
	server.raceAudioAnnouncements[eventID] = receipt
}

func writeRaceAudioAnnouncementError(writer http.ResponseWriter, status int, code, message string) {
	writeRaceAudioAnnouncementJSON(writer, status, map[string]any{
		"schemaVersion": raceAudioAnnouncementSchemaVersion,
		"error":         code,
		"message":       message,
	})
}

func writeRaceAudioAnnouncementJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
