package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	pitPresenceEventName  = "pit_presence"
	pitPresenceMaxEntries = 256
)

type pitPresenceEvent struct {
	SchemaVersion    int    `json:"schemaVersion"`
	Event            string `json:"event"`
	EventID          string `json:"eventId"`
	SourceID         string `json:"sourceId"`
	RaceRunID        string `json:"raceRunId"`
	CarID            string `json:"carId"`
	EntryID          string `json:"entryId"`
	Transition       string `json:"transition"`
	OccurredAtUnixMs int64  `json:"occurredAtUnixMs"`
	Reason           string `json:"reason"`
}

func (event pitPresenceEvent) validate() error {
	if event.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1")
	}
	if event.Event != pitPresenceEventName {
		return fmt.Errorf("event must be %q", pitPresenceEventName)
	}
	if event.SourceID != pitRecoverySourceID {
		return fmt.Errorf("sourceId must be %q", pitRecoverySourceID)
	}
	for name, value := range map[string]string{
		"eventId": event.EventID, "raceRunId": event.RaceRunID,
		"carId": event.CarID, "entryId": event.EntryID,
	} {
		if err := validateGameplayID(name, value, 128); err != nil {
			return err
		}
	}
	if event.OccurredAtUnixMs <= 0 {
		return fmt.Errorf("occurredAtUnixMs must be a positive integer")
	}
	if err := validateGameplayID("reason", event.Reason, 64); err != nil {
		return err
	}
	switch event.Transition {
	case "entered":
		if event.Reason != "marker_confirmed" {
			return fmt.Errorf("entered reason must be %q", "marker_confirmed")
		}
	case "exited":
		switch event.Reason {
		case "marker_lost", "observation_stale", "video_invalid":
		default:
			return fmt.Errorf("unsupported exited reason %q", event.Reason)
		}
	default:
		return fmt.Errorf("transition must be entered or exited")
	}
	return nil
}

func validateGameplayID(name string, value string, maxBytes int) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is required", name)
	}
	if trimmed != value {
		return fmt.Errorf("%s must not have surrounding whitespace", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s must be at most %d bytes", name, maxBytes)
	}
	return nil
}

func (event pitPresenceEvent) fingerprint() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		event.SourceID, event.RaceRunID, event.CarID, event.EntryID,
		event.Transition, event.OccurredAtUnixMs, event.Reason)
}

type pitPresenceResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Status        string `json:"status"`
	EventID       string `json:"eventId"`
	RaceRunID     string `json:"raceRunId"`
	CarID         string `json:"carId"`
	EntryID       string `json:"entryId"`
	Transition    string `json:"transition"`
	Present       bool   `json:"present"`
	ServerTimeMs  int64  `json:"serverTimeMs"`
}

type pitPresenceReceipt struct {
	Fingerprint string
	Response    pitPresenceResponse
}

type pitPresenceSnapshot struct {
	RaceRunID        string  `json:"raceRunId,omitempty"`
	CarID            string  `json:"carId"`
	Present          bool    `json:"present"`
	EntryID          string  `json:"entryId,omitempty"`
	EnteredAtUnixMs  int64   `json:"enteredAtUnixMs,omitempty"`
	ExitedAtUnixMs   int64   `json:"exitedAtUnixMs,omitempty"`
	ExitReason       string  `json:"exitReason,omitempty"`
	LastAcceptedTick int     `json:"lastAcceptedTick"`
	ServiceState     string  `json:"serviceState"`
	HP               float64 `json:"hp"`
	Fuel             float64 `json:"fuel"`
	ServerTimeMs     int64   `json:"serverTimeMs,omitempty"`
}

type pitPresenceState struct {
	mu sync.Mutex

	runID            string
	carID            string
	present          bool
	entryID          string
	enteredAtUnixMs  int64
	exitedAtUnixMs   int64
	exitReason       string
	lastAcceptedTick int
	hp               float64
	fuel             float64
	seenEntryIDs     map[string]struct{}
}

func newPitPresenceState(carID string, hp float64, fuels ...float64) *pitPresenceState {
	fuel := vehicleFuelMaximum
	if len(fuels) > 0 {
		fuel = fuels[0]
	}
	return &pitPresenceState{
		carID:        carID,
		hp:           hp,
		fuel:         fuel,
		seenEntryIDs: make(map[string]struct{}),
	}
}

func (state *pitPresenceState) apply(event pitPresenceEvent, now time.Time, hp float64) (pitPresenceSnapshot, *pitRecoveryApplyError) {
	state.mu.Lock()
	fuel := state.fuel
	state.mu.Unlock()
	return state.applyGameplay(event, now, vehicleHealthSnapshot{HP: hp, Fuel: fuel})
}

func (state *pitPresenceState) applyGameplay(event pitPresenceEvent, now time.Time, health vehicleHealthSnapshot) (pitPresenceSnapshot, *pitRecoveryApplyError) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.runID == "" {
		state.runID = event.RaceRunID
	}
	if state.runID != event.RaceRunID {
		return pitPresenceSnapshot{}, &pitRecoveryApplyError{StatusCode: http.StatusConflict, Code: "race_run_mismatch", Message: "raceRunId does not match the PIT state run"}
	}

	serverTimeMs := now.UnixMilli()
	switch event.Transition {
	case "entered":
		if state.present {
			code := "entry_already_active"
			message := "another PIT entry is already active"
			if state.entryID != event.EntryID {
				code = "entry_conflict"
			}
			return pitPresenceSnapshot{}, &pitRecoveryApplyError{StatusCode: http.StatusConflict, Code: code, Message: message}
		}
		if _, seen := state.seenEntryIDs[event.EntryID]; seen {
			return pitPresenceSnapshot{}, &pitRecoveryApplyError{StatusCode: http.StatusConflict, Code: "entry_id_reused", Message: "entryId was already used in this race run"}
		}
		state.present = true
		state.entryID = event.EntryID
		state.enteredAtUnixMs = serverTimeMs
		state.exitedAtUnixMs = 0
		state.exitReason = ""
		state.lastAcceptedTick = 0
		state.seenEntryIDs[event.EntryID] = struct{}{}
	case "exited":
		if !state.present {
			return pitPresenceSnapshot{}, &pitRecoveryApplyError{StatusCode: http.StatusConflict, Code: "entry_not_active", Message: "there is no active PIT entry"}
		}
		if state.entryID != event.EntryID {
			return pitPresenceSnapshot{}, &pitRecoveryApplyError{StatusCode: http.StatusConflict, Code: "entry_mismatch", Message: "entryId does not match the active PIT entry"}
		}
		state.present = false
		state.exitedAtUnixMs = serverTimeMs
		state.exitReason = event.Reason
	}
	state.hp = health.HP
	state.fuel = health.Fuel
	return state.snapshotLocked(), nil
}

func (state *pitPresenceState) observeRecovery(command pitRecoveryCommand, health vehicleHealthSnapshot) (pitPresenceSnapshot, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.hp = health.HP
	state.fuel = health.Fuel
	if !state.present || state.runID != command.RaceRunID || state.entryID != command.EntryID {
		return state.snapshotLocked(), false
	}
	changed := command.Tick > state.lastAcceptedTick
	if changed {
		state.lastAcceptedTick = command.Tick
	}
	return state.snapshotLocked(), changed
}

func (state *pitPresenceState) observeHealth(health vehicleHealthSnapshot) (pitPresenceSnapshot, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	previousService := state.serviceStateLocked()
	changed := math.Abs(state.hp-health.HP) >= 0.05 || math.Abs(state.fuel-health.Fuel) >= 0.05
	state.hp = health.HP
	state.fuel = health.Fuel
	if state.present && previousService != state.serviceStateLocked() {
		changed = true
	}
	return state.snapshotLocked(), state.present && changed
}

func (state *pitPresenceState) resetForRun(runID string, _ time.Time, hp float64) (pitPresenceSnapshot, bool) {
	return state.resetForRunGameplay(runID, vehicleHealthSnapshot{HP: hp, Fuel: vehicleFuelMaximum})
}

func (state *pitPresenceState) resetForRunGameplay(runID string, health vehicleHealthSnapshot) (pitPresenceSnapshot, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.runID == runID {
		state.hp = health.HP
		state.fuel = health.Fuel
		return state.snapshotLocked(), false
	}
	state.runID = runID
	state.present = false
	state.entryID = ""
	state.enteredAtUnixMs = 0
	state.exitedAtUnixMs = 0
	state.exitReason = ""
	state.lastAcceptedTick = 0
	state.hp = health.HP
	state.fuel = health.Fuel
	state.seenEntryIDs = make(map[string]struct{})
	return state.snapshotLocked(), true
}

func (state *pitPresenceState) resetActive(reason string, now time.Time, hp float64) (pitPresenceSnapshot, bool) {
	state.mu.Lock()
	fuel := state.fuel
	state.mu.Unlock()
	return state.resetActiveGameplay(reason, now, vehicleHealthSnapshot{HP: hp, Fuel: fuel})
}

func (state *pitPresenceState) resetActiveGameplay(reason string, now time.Time, health vehicleHealthSnapshot) (pitPresenceSnapshot, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.hp = health.HP
	state.fuel = health.Fuel
	if !state.present {
		return state.snapshotLocked(), false
	}
	state.present = false
	state.exitedAtUnixMs = now.UnixMilli()
	state.exitReason = reason
	return state.snapshotLocked(), true
}

func (state *pitPresenceState) snapshot(health vehicleHealthSnapshot) pitPresenceSnapshot {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.hp = health.HP
	state.fuel = health.Fuel
	return state.snapshotLocked()
}

func (state *pitPresenceState) snapshotLocked() pitPresenceSnapshot {
	return pitPresenceSnapshot{
		RaceRunID:        state.runID,
		CarID:            state.carID,
		Present:          state.present,
		EntryID:          state.entryID,
		EnteredAtUnixMs:  state.enteredAtUnixMs,
		ExitedAtUnixMs:   state.exitedAtUnixMs,
		ExitReason:       state.exitReason,
		LastAcceptedTick: state.lastAcceptedTick,
		ServiceState:     state.serviceStateLocked(),
		HP:               state.hp,
		Fuel:             state.fuel,
	}
}

func (state *pitPresenceState) serviceStateLocked() string {
	if !state.present {
		return "outside"
	}
	if state.hp >= vehicleHealthMaximum && state.fuel >= vehicleFuelMaximum {
		return "complete"
	}
	return "servicing"
}

func formatPitPresenceTelemetry(snapshot pitPresenceSnapshot) string {
	snapshot.ServerTimeMs = time.Now().UnixMilli()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	return "PIT:1," + string(payload)
}

func (r *relay) broadcastPitPresence(snapshot pitPresenceSnapshot) {
	message := formatPitPresenceTelemetry(snapshot)
	if message == "" {
		return
	}
	r.broadcastTelemetry(webrtc.DataChannelMessage{Data: []byte(message), IsString: true})
	if r.raceAudio != nil {
		r.raceAudio.observePit(snapshot)
	}
}

func (r *relay) currentGameplayMessages(now time.Time) []string {
	health := r.vehicleHealth.snapshot(now)
	messages := []string{formatVehicleHealthTelemetry(health), formatVehicleGameplayTelemetry(health)}
	if r.pitPresence != nil {
		messages = append(messages, formatPitPresenceTelemetry(r.pitPresence.snapshot(health)))
	}
	return messages
}

func (r *relay) sendCurrentGameplayState(client *viewer, channel *webrtc.DataChannel) {
	for _, message := range r.currentGameplayMessages(time.Now()) {
		if message == "" {
			continue
		}
		if err := channel.SendText(message); err != nil {
			log.Printf("source %q: send gameplay state to viewer %d: %v", r.name, client.id, err)
		}
	}
}

func (server *relayServer) servePitPresenceEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePitRecoveryError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method must be POST", 0)
		return
	}
	defer req.Body.Close()
	var event pitPresenceEvent
	if err := decodeGameplayJSON(w, req, &event); err != nil {
		writePitRecoveryError(w, http.StatusBadRequest, "invalid_json", err.Error(), 0)
		return
	}
	if err := event.validate(); err != nil {
		writePitRecoveryError(w, http.StatusBadRequest, "invalid_event", err.Error(), 0)
		return
	}

	server.pitEventsMu.Lock()
	if server.pitEvents == nil {
		server.pitEvents = make(map[string]pitPresenceReceipt)
	}
	fingerprint := event.fingerprint()
	if receipt, ok := server.pitEvents[event.EventID]; ok {
		server.pitEventsMu.Unlock()
		if receipt.Fingerprint != fingerprint {
			writePitRecoveryError(w, http.StatusConflict, "event_conflict", "eventId was already used for a different PIT presence event", 0)
			return
		}
		response := receipt.Response
		response.Status = "duplicate"
		writePitRecoveryJSON(w, http.StatusOK, response)
		return
	}

	context := server.raceContextSnapshot()
	if !context.Connected {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, http.StatusConflict, "race_control_unavailable", "Race Control is not connected", 0)
		return
	}
	if context.RaceRunID == "" || event.RaceRunID != context.RaceRunID {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, http.StatusConflict, "race_run_mismatch", "raceRunId does not match the active run", 0)
		return
	}
	if context.Phase != "green" {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, http.StatusConflict, "phase_not_allowed", "PIT presence is allowed only during green", 0)
		return
	}
	source, ok := server.sourceForCarID(event.CarID)
	if !ok {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, http.StatusNotFound, "unknown_car", "carId is not mapped to exactly one Relay source", 0)
		return
	}
	if source.pitPresence == nil {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, http.StatusConflict, "pit_state_unavailable", "PIT presence state is unavailable", 0)
		return
	}
	now := time.Now()
	health := source.vehicleHealth.snapshot(now)
	snapshot, applyErr := source.pitPresence.applyGameplay(event, now, health)
	if applyErr != nil {
		server.pitEventsMu.Unlock()
		writePitRecoveryError(w, applyErr.StatusCode, applyErr.Code, applyErr.Message, 0)
		return
	}
	response := pitPresenceResponse{
		SchemaVersion: 1,
		Status:        "applied",
		EventID:       event.EventID,
		RaceRunID:     event.RaceRunID,
		CarID:         event.CarID,
		EntryID:       event.EntryID,
		Transition:    event.Transition,
		Present:       snapshot.Present,
		ServerTimeMs:  now.UnixMilli(),
	}
	server.pitEvents[event.EventID] = pitPresenceReceipt{Fingerprint: fingerprint, Response: response}
	server.pitEventIDs = append(server.pitEventIDs, event.EventID)
	if len(server.pitEventIDs) > pitPresenceMaxEntries {
		oldest := server.pitEventIDs[0]
		server.pitEventIDs = server.pitEventIDs[1:]
		delete(server.pitEvents, oldest)
	}
	server.pitEventsMu.Unlock()

	source.broadcastVehicleGameplay(source.vehicleHealth.setPitPresent(snapshot.Present, now))
	source.broadcastPitPresence(snapshot)
	log.Printf("source %q car %q: applied PIT presence entry=%q transition=%s",
		source.name, event.CarID, event.EntryID, event.Transition)
	writePitRecoveryJSON(w, http.StatusOK, response)
}
