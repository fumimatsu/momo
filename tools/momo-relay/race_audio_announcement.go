package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	raceAudioAnnouncementSchemaVersion = 1
	raceAudioAnnouncementReceiptLimit  = 128
)

var raceAudioAnnouncementKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type raceAudioAnnouncementRequest struct {
	SchemaVersion int      `json:"schemaVersion"`
	Command       string   `json:"command"`
	CommandID     string   `json:"commandId"`
	RaceRunID     string   `json:"raceRunId"`
	CarIDs        []string `json:"carIds"`
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
	carIDs, err := validateRaceAudioAnnouncementRequest(command)
	if err != nil {
		writeRaceAudioAnnouncementError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	command.CarIDs = carIDs
	event := preRaceFormationAnnouncement(command.RaceRunID)
	fingerprint := command.RaceRunID + "\n" + strings.Join(carIDs, "\n")

	server.raceAudioAnnouncementMu.Lock()
	defer server.raceAudioAnnouncementMu.Unlock()
	if receipt, exists := server.raceAudioAnnouncements[event.EventID]; exists {
		if receipt.fingerprint != fingerprint {
			writeRaceAudioAnnouncementError(writer, http.StatusConflict, "announcement_conflict", "the race announcement was already queued for a different car set")
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

func validateRaceAudioAnnouncementRequest(request raceAudioAnnouncementRequest) ([]string, error) {
	if request.SchemaVersion != raceAudioAnnouncementSchemaVersion || request.Command != "pre_race_formation" ||
		!raceAudioAnnouncementKeyPattern.MatchString(strings.TrimSpace(request.CommandID)) ||
		!raceAudioAnnouncementKeyPattern.MatchString(strings.TrimSpace(request.RaceRunID)) {
		return nil, fmt.Errorf("schemaVersion=1, command=pre_race_formation, commandId, and raceRunId are required")
	}
	if len(request.CarIDs) < 1 || len(request.CarIDs) > maximumConfiguredSources {
		return nil, fmt.Errorf("carIds must contain 1..%d cars", maximumConfiguredSources)
	}
	carIDs := make([]string, 0, len(request.CarIDs))
	seen := make(map[string]struct{}, len(request.CarIDs))
	for _, value := range request.CarIDs {
		carID := strings.TrimSpace(value)
		if !raceAudioAnnouncementKeyPattern.MatchString(carID) {
			return nil, fmt.Errorf("carIds contains an invalid car ID")
		}
		if _, duplicate := seen[carID]; duplicate {
			return nil, fmt.Errorf("carIds contains duplicate car ID %q", carID)
		}
		seen[carID] = struct{}{}
		carIDs = append(carIDs, carID)
	}
	sort.Strings(carIDs)
	return carIDs, nil
}

func preRaceFormationAnnouncement(raceRunID string) raceAudioEvent {
	return raceAudioEvent{
		EventID:      raceRunID + ":global:pre_race_formation",
		Kind:         "pre_race_formation",
		Priority:     70,
		EnglishText:  "Tsukuraji RC Park. D and D Night FPV RC Race. Drivers, begin the formation lap and take your grid positions. Red lights will follow. Get ready to race.",
		JapaneseText: "つくラジRCパーク、DアンドDナイト。FPVRCレース、まもなくスタート。ドライバーはフォーメーションラップへ。マシンをグリッドへ導け。全車がそろえば、レッドシグナル点灯。静寂のあと、勝負が始まる。さあ、準備はいいか。",
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
