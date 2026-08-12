package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const (
	relayConfigVersion       = 1
	maximumConfiguredSources = 32
)

type relayFileConfig struct {
	Version int               `json:"version"`
	Sources []relayFileSource `json:"sources"`
}

type relayFileSource struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	RaceCarID      string `json:"raceCarId,omitempty"`
	AyamePilotRoom string `json:"ayamePilotRoom,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

type relayConfigMappings struct {
	Sources         sourceFlag
	RaceCars        sourceFlag
	AyamePilotRooms sourceFlag
}

func loadRelayConfig(path string) (relayConfigMappings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return relayConfigMappings{}, fmt.Errorf("read Relay config: %w", err)
	}
	var config relayFileConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return relayConfigMappings{}, fmt.Errorf("decode Relay config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return relayConfigMappings{}, fmt.Errorf("decode Relay config: multiple JSON values are not allowed")
		}
		return relayConfigMappings{}, fmt.Errorf("decode Relay config trailing data: %w", err)
	}
	if config.Version != relayConfigVersion {
		return relayConfigMappings{}, fmt.Errorf("Relay config version must be %d", relayConfigVersion)
	}

	mappings := relayConfigMappings{}
	seenIDs := make(map[string]struct{}, len(config.Sources))
	seenRaceCars := make(map[string]string, len(config.Sources))
	for index, source := range config.Sources {
		enabled := source.Enabled == nil || *source.Enabled
		id := strings.TrimSpace(source.ID)
		sourceURL := strings.TrimSpace(source.URL)
		raceCarID := strings.TrimSpace(source.RaceCarID)
		roomID := strings.TrimSpace(source.AyamePilotRoom)
		if id == "" || strings.Contains(id, "=") {
			return relayConfigMappings{}, fmt.Errorf("sources[%d].id must be non-empty and must not contain '='", index)
		}
		if _, exists := seenIDs[id]; exists {
			return relayConfigMappings{}, fmt.Errorf("duplicate source id %q", id)
		}
		seenIDs[id] = struct{}{}
		if !enabled {
			continue
		}
		if err := validateRelaySourceURL(sourceURL); err != nil {
			return relayConfigMappings{}, fmt.Errorf("source %q: %w", id, err)
		}
		if raceCarID != "" {
			if existing, exists := seenRaceCars[raceCarID]; exists {
				return relayConfigMappings{}, fmt.Errorf("duplicate raceCarId %q for sources %q and %q", raceCarID, existing, id)
			}
			seenRaceCars[raceCarID] = id
			mappings.RaceCars = append(mappings.RaceCars, id+"="+raceCarID)
		}
		mappings.Sources = append(mappings.Sources, id+"="+sourceURL)
		if roomID != "" {
			mappings.AyamePilotRooms = append(mappings.AyamePilotRooms, id+"="+roomID)
		}
	}
	if len(mappings.Sources) == 0 {
		return relayConfigMappings{}, fmt.Errorf("Relay config must contain at least one enabled source")
	}
	if len(mappings.Sources) > maximumConfiguredSources {
		return relayConfigMappings{}, fmt.Errorf("Relay config has %d enabled sources; maximum is %d", len(mappings.Sources), maximumConfiguredSources)
	}
	return mappings, nil
}

func validateRelaySourceURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" {
		return fmt.Errorf("url must be an absolute ws:// or wss:// URL")
	}
	return nil
}
