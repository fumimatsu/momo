package racerecorder

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	SchemaVersion = 1

	ModeProgramOnly = "program_only"
	ModeFullArchive = "full_archive"

	StateReady     = "ready"
	StateRecording = "recording"
	StateStopping  = "stopping"
	StateDegraded  = "degraded"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var commandIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type Source struct {
	SourceID  string `json:"sourceId"`
	VehicleID string `json:"vehicleId"`
	CarID     string `json:"carId"`
}

type StartRequest struct {
	SchemaVersion     int      `json:"schemaVersion"`
	CommandID         string   `json:"commandId"`
	RaceID            string   `json:"raceId"`
	RaceRunID         string   `json:"raceRunId"`
	HeatID            string   `json:"heatId,omitempty"`
	Mode              string   `json:"mode"`
	Sources           []Source `json:"sources"`
	RequestedAtUnixMS int64    `json:"requestedAtUnixMs"`
}

type StopRequest struct {
	SchemaVersion     int    `json:"schemaVersion"`
	CommandID         string `json:"commandId"`
	RaceRunID         string `json:"raceRunId"`
	Reason            string `json:"reason"`
	TailMS            int64  `json:"tailMs"`
	RequestedAtUnixMS int64  `json:"requestedAtUnixMs"`
}

type Status struct {
	Type            string `json:"type"`
	State           string `json:"state"`
	ActiveRaceRunID string `json:"activeRaceRunId,omitempty"`
	ActiveMode      string `json:"activeMode,omitempty"`
	StorageRoot     string `json:"storageRoot,omitempty"`
	FreeBytes       int64  `json:"freeBytes,omitempty"`
	StartedAtUnixMS int64  `json:"startedAtUnixMs,omitempty"`
	UpdatedAtUnixMS int64  `json:"updatedAtUnixMs,omitempty"`
	LastError       string `json:"lastError,omitempty"`
}

func validateStartRequest(request StartRequest, maximumSources int) error {
	if request.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	if !commandIDPattern.MatchString(strings.TrimSpace(request.CommandID)) {
		return fmt.Errorf("commandId must match %s", commandIDPattern.String())
	}
	if err := validateIdentity("raceId", request.RaceID); err != nil {
		return err
	}
	if err := validateIdentity("raceRunId", request.RaceRunID); err != nil {
		return err
	}
	if request.HeatID != "" {
		if err := validateIdentity("heatId", request.HeatID); err != nil {
			return err
		}
	}
	if request.Mode != ModeFullArchive && request.Mode != ModeProgramOnly {
		return fmt.Errorf("mode must be %q or %q", ModeFullArchive, ModeProgramOnly)
	}
	if request.RequestedAtUnixMS <= 0 {
		return fmt.Errorf("requestedAtUnixMs must be positive")
	}
	if request.Sources == nil || len(request.Sources) == 0 {
		return fmt.Errorf("sources must contain at least one entry")
	}
	if len(request.Sources) > maximumSources {
		return fmt.Errorf("sources exceeds the configured maximum of %d", maximumSources)
	}
	seenSource := make(map[string]struct{}, len(request.Sources))
	seenVehicle := make(map[string]struct{}, len(request.Sources))
	seenCar := make(map[string]struct{}, len(request.Sources))
	for index, source := range request.Sources {
		for field, value := range map[string]string{
			"sourceId": source.SourceID, "vehicleId": source.VehicleID, "carId": source.CarID,
		} {
			if err := validateIdentity(fmt.Sprintf("sources[%d].%s", index, field), value); err != nil {
				return err
			}
		}
		if _, exists := seenSource[source.SourceID]; exists {
			return fmt.Errorf("sourceId %q is duplicated", source.SourceID)
		}
		if _, exists := seenVehicle[source.VehicleID]; exists {
			return fmt.Errorf("vehicleId %q is duplicated", source.VehicleID)
		}
		if _, exists := seenCar[source.CarID]; exists {
			return fmt.Errorf("carId %q is duplicated", source.CarID)
		}
		seenSource[source.SourceID] = struct{}{}
		seenVehicle[source.VehicleID] = struct{}{}
		seenCar[source.CarID] = struct{}{}
	}
	return nil
}

func validateStopRequest(request StopRequest, maximumTailMS int64) error {
	if request.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	if !commandIDPattern.MatchString(strings.TrimSpace(request.CommandID)) {
		return fmt.Errorf("commandId must match %s", commandIDPattern.String())
	}
	if err := validateIdentity("raceRunId", request.RaceRunID); err != nil {
		return err
	}
	if request.Reason != "race_finished" && request.Reason != "race_aborted" {
		return fmt.Errorf("reason must be race_finished or race_aborted")
	}
	if request.TailMS < 0 || request.TailMS > maximumTailMS {
		return fmt.Errorf("tailMs must be between 0 and %d", maximumTailMS)
	}
	if request.RequestedAtUnixMS <= 0 {
		return fmt.Errorf("requestedAtUnixMs must be positive")
	}
	return nil
}

func validateIdentity(field string, value string) error {
	value = strings.TrimSpace(value)
	if !identityPattern.MatchString(value) {
		return fmt.Errorf("%s must match %s", field, identityPattern.String())
	}
	return nil
}
