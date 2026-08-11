package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	pitRecoveryCommandName     = "pit_recovery_tick"
	pitRecoverySourceID        = "madsystem"
	pitRecoveryAmount          = 10.0
	pitRecoveryMinimumInterval = time.Second
	pitRecoveryMaxBodyBytes    = 16 * 1024
)

type vehicleHealthRecoveryMode string

const (
	vehicleHealthRecoveryLegacy    vehicleHealthRecoveryMode = "legacy"
	vehicleHealthRecoveryPitMarker vehicleHealthRecoveryMode = "pit-marker"
	vehicleHealthRecoveryHybrid    vehicleHealthRecoveryMode = "hybrid"
	vehicleHealthRecoveryDisabled  vehicleHealthRecoveryMode = "disabled"
	vehicleHealthRecoveryDefault                             = vehicleHealthRecoveryHybrid
)

func parseVehicleHealthRecoveryMode(value string) (vehicleHealthRecoveryMode, error) {
	mode := vehicleHealthRecoveryMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case vehicleHealthRecoveryLegacy, vehicleHealthRecoveryPitMarker, vehicleHealthRecoveryHybrid, vehicleHealthRecoveryDisabled:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid health recovery mode %q; want legacy, pit-marker, hybrid, or disabled", value)
	}
}

func (mode vehicleHealthRecoveryMode) allowsDrivingRecovery() bool {
	return mode == vehicleHealthRecoveryLegacy || mode == vehicleHealthRecoveryHybrid
}

func (mode vehicleHealthRecoveryMode) allowsPitRecovery() bool {
	return mode == vehicleHealthRecoveryPitMarker || mode == vehicleHealthRecoveryHybrid
}

type pitRecoveryCommand struct {
	SchemaVersion int    `json:"schemaVersion"`
	Command       string `json:"command"`
	CommandID     string `json:"commandId"`
	SourceID      string `json:"sourceId"`
	RaceRunID     string `json:"raceRunId"`
	CarID         string `json:"carId"`
	EntryID       string `json:"entryId"`
	Tick          int    `json:"tick"`
}

func (command pitRecoveryCommand) validate() error {
	values := map[string]string{
		"commandId": command.CommandID,
		"raceRunId": command.RaceRunID,
		"carId":     command.CarID,
		"entryId":   command.EntryID,
	}
	if command.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1")
	}
	if command.Command != pitRecoveryCommandName {
		return fmt.Errorf("command must be %q", pitRecoveryCommandName)
	}
	if command.SourceID != pitRecoverySourceID {
		return fmt.Errorf("sourceId must be %q", pitRecoverySourceID)
	}
	for name, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return fmt.Errorf("%s is required", name)
		}
		if trimmed != value {
			return fmt.Errorf("%s must not have surrounding whitespace", name)
		}
		if len(value) > 128 {
			return fmt.Errorf("%s must be at most 128 bytes", name)
		}
	}
	if command.Tick < 1 {
		return fmt.Errorf("tick must be at least 1")
	}
	return nil
}

func (command pitRecoveryCommand) fingerprint() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", command.RaceRunID, command.CarID, command.EntryID, command.Tick)
}

type pitRecoveryResponse struct {
	SchemaVersion       int     `json:"schemaVersion"`
	Status              string  `json:"status"`
	CommandID           string  `json:"commandId"`
	RaceRunID           string  `json:"raceRunId"`
	CarID               string  `json:"carId"`
	EntryID             string  `json:"entryId"`
	Tick                int     `json:"tick"`
	RecoveredAmount     float64 `json:"recoveredAmount"`
	FuelRecoveredAmount float64 `json:"fuelRecoveredAmount"`
	HP                  float64 `json:"hp"`
	Fuel                float64 `json:"fuel"`
	SpeedCap            float64 `json:"speedCap"`
	Mode                string  `json:"mode"`
}

type pitRecoveryErrorResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Error         string `json:"error"`
	Message       string `json:"message"`
	RetryAfterMs  int64  `json:"retryAfterMs,omitempty"`
}

type relayRaceContext struct {
	Connected bool
	RaceRunID string
	Phase     string
}

func (server *relayServer) observeRaceContext(envelope raceStateEnvelope, now time.Time) {
	server.raceMu.Lock()
	previous := server.raceContext
	server.raceContext = relayRaceContext{
		Connected: true,
		RaceRunID: strings.TrimSpace(envelope.RaceRunID),
		Phase:     strings.ToLower(strings.TrimSpace(envelope.Phase)),
	}
	currentRunID := server.raceContext.RaceRunID
	currentPhase := server.raceContext.Phase
	server.raceMu.Unlock()

	for _, source := range server.sources {
		position := 0
		for _, standing := range envelope.Standings {
			if standing.CarID == source.raceCarID {
				position = standing.Position
				break
			}
		}
		health, changed := source.vehicleHealth.observeRaceState(
			true,
			currentRunID,
			currentPhase,
			position,
			len(envelope.Standings),
			now,
			envelope.RaceInfo.SessionType,
		)
		if changed {
			source.driveGear.Store(int32(health.Gear))
			source.broadcastVehicleGameplay(health)
		}
		if previous.RaceRunID != currentRunID {
			source.resetVehicleEvents(currentRunID)
			if source.pitPresence == nil {
				continue
			}
			if pit, changed := source.pitPresence.resetForRunGameplay(currentRunID, health); changed {
				source.broadcastPitPresence(pit)
			}
			continue
		}
		if currentPhase != "green" {
			if source.pitPresence == nil {
				continue
			}
			if pit, changed := source.pitPresence.resetActiveGameplay("race_phase_changed", now, health); changed {
				source.vehicleHealth.setPitPresent(false, now)
				source.broadcastPitPresence(pit)
			}
		}
	}
}

func (server *relayServer) markRaceControlDisconnected() {
	server.raceMu.Lock()
	server.raceContext.Connected = false
	server.raceMu.Unlock()
	now := time.Now()
	for _, source := range server.sources {
		health, changed := source.vehicleHealth.markRaceDisconnected(now)
		if changed {
			source.driveGear.Store(int32(health.Gear))
			source.broadcastVehicleGameplay(health)
		}
		if source.pitPresence == nil {
			continue
		}
		if pit, changed := source.pitPresence.resetActiveGameplay("race_control_disconnected", now, health); changed {
			source.vehicleHealth.setPitPresent(false, now)
			source.broadcastPitPresence(pit)
		}
	}
}

func (server *relayServer) raceContextSnapshot() relayRaceContext {
	server.raceMu.RLock()
	defer server.raceMu.RUnlock()
	return server.raceContext
}

func (server *relayServer) sourceForCarID(carID string) (*relay, bool) {
	var matched *relay
	for _, source := range server.sources {
		if source.raceCarID != carID {
			continue
		}
		if matched != nil {
			return nil, false
		}
		matched = source
	}
	return matched, matched != nil
}

func (server *relayServer) servePitRecoveryTick(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePitRecoveryError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method must be POST", 0)
		return
	}
	defer req.Body.Close()
	var command pitRecoveryCommand
	if err := decodeGameplayJSON(w, req, &command); err != nil {
		writePitRecoveryError(w, http.StatusBadRequest, "invalid_json", err.Error(), 0)
		return
	}
	if err := command.validate(); err != nil {
		writePitRecoveryError(w, http.StatusBadRequest, "invalid_command", err.Error(), 0)
		return
	}

	context := server.raceContextSnapshot()
	if !context.Connected {
		writePitRecoveryError(w, http.StatusConflict, "race_control_unavailable", "Race Control is not connected", 0)
		return
	}
	if context.RaceRunID == "" || command.RaceRunID != context.RaceRunID {
		writePitRecoveryError(w, http.StatusConflict, "race_run_mismatch", "raceRunId does not match the active run", 0)
		return
	}
	if context.Phase != "green" {
		writePitRecoveryError(w, http.StatusConflict, "phase_not_allowed", "pit recovery is allowed only during green", 0)
		return
	}
	source, ok := server.sourceForCarID(command.CarID)
	if !ok {
		writePitRecoveryError(w, http.StatusNotFound, "unknown_car", "carId is not mapped to exactly one Relay source", 0)
		return
	}

	result, applyErr := source.vehicleHealth.applyPitRecovery(command, time.Now())
	if applyErr != nil {
		writePitRecoveryError(w, applyErr.StatusCode, applyErr.Code, applyErr.Message, applyErr.RetryAfter.Milliseconds())
		return
	}
	if result.Status == "applied" {
		source.broadcastVehicleGameplay(result.Snapshot)
		if source.pitPresence != nil {
			if pit, changed := source.pitPresence.observeRecovery(command, result.Snapshot); changed {
				source.broadcastPitPresence(pit)
			}
		}
		log.Printf("source %q car %q: applied pit recovery entry=%q tick=%d hpRecovered=%.1f fuelRecovered=%.1f hp=%.1f fuel=%.1f",
			source.name, command.CarID, command.EntryID, command.Tick,
			result.RecoveredAmount, result.FuelRecoveredAmount, result.Snapshot.HP, result.Snapshot.Fuel)
	}
	writePitRecoveryJSON(w, http.StatusOK, pitRecoveryResponse{
		SchemaVersion:       1,
		Status:              result.Status,
		CommandID:           command.CommandID,
		RaceRunID:           command.RaceRunID,
		CarID:               command.CarID,
		EntryID:             command.EntryID,
		Tick:                command.Tick,
		RecoveredAmount:     result.RecoveredAmount,
		FuelRecoveredAmount: result.FuelRecoveredAmount,
		HP:                  result.Snapshot.HP,
		Fuel:                result.Snapshot.Fuel,
		SpeedCap:            result.Snapshot.SpeedCap,
		Mode:                result.Snapshot.Mode,
	})
}

func decodeGameplayJSON(w http.ResponseWriter, req *http.Request, destination any) error {
	req.Body = http.MaxBytesReader(w, req.Body, pitRecoveryMaxBodyBytes)
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return ensureJSONBodyEnded(decoder)
}

func ensureJSONBodyEnded(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

func bearerTokenHandler(token string, next http.HandlerFunc) http.HandlerFunc {
	token = strings.TrimSpace(token)
	return func(w http.ResponseWriter, req *http.Request) {
		if token == "" {
			writePitRecoveryError(w, http.StatusServiceUnavailable, "gameplay_api_disabled", "MOMO_RELAY_GAMEPLAY_TOKEN is not configured", 0)
			return
		}
		authorization := req.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writePitRecoveryError(w, http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required", 0)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writePitRecoveryError(w, http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required", 0)
			return
		}
		next(w, req)
	}
}

func writePitRecoveryError(w http.ResponseWriter, status int, code string, message string, retryAfterMs int64) {
	writePitRecoveryJSON(w, status, pitRecoveryErrorResponse{
		SchemaVersion: 1,
		Error:         code,
		Message:       message,
		RetryAfterMs:  retryAfterMs,
	})
}

func writePitRecoveryJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
