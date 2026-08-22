package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const markerSourceManifestVersion = 1

type markerSourceManifest struct {
	Version          int                      `json:"version"`
	Revision         string                   `json:"revision"`
	SelectionMode    string                   `json:"selectionMode"`
	RaceID           string                   `json:"raceId,omitempty"`
	RaceRunID        string                   `json:"raceRunId,omitempty"`
	Phase            string                   `json:"phase,omitempty"`
	RaceSequence     uint64                   `json:"raceSequence,omitempty"`
	Sources          []markerSourceDescriptor `json:"sources"`
	MissingSourceIDs []string                 `json:"missingSourceIds,omitempty"`
}

type markerSourceDescriptor struct {
	SourceID     string `json:"sourceId"`
	CarID        string `json:"carId,omitempty"`
	ObserverPath string `json:"observerPath"`
	State        string `json:"state"`
	VideoHealth  string `json:"videoHealth"`
}

type markerRaceStateEnvelope struct {
	RaceID    string `json:"raceId"`
	RaceRunID string `json:"raceRunId"`
	Phase     string `json:"phase"`
	Sequence  uint64 `json:"sequence"`
	Roster    *struct {
		Participants []struct {
			SourceID string `json:"sourceId"`
			CarID    string `json:"carId"`
		} `json:"participants"`
	} `json:"roster"`
}

func (server *relayServer) markerSourceManifestSnapshot(now time.Time, requestedSelection string) markerSourceManifest {
	race := markerRaceStateEnvelope{}
	rawRace := strings.TrimPrefix(strings.TrimSpace(server.currentGlobalRaceState()), "RACE:")
	if rawRace != "" {
		_ = json.Unmarshal([]byte(rawRace), &race)
	}

	selectedBySource := make(map[string]string)
	selectionMode := "drive"
	if requestedSelection == "all" {
		selectionMode = "configured"
	} else if race.RaceRunID != "" && race.Roster != nil && len(race.Roster.Participants) > 0 {
		selectionMode = "locked_roster"
		for _, participant := range race.Roster.Participants {
			sourceID := strings.TrimSpace(participant.SourceID)
			if sourceID != "" {
				selectedBySource[sourceID] = strings.TrimSpace(participant.CarID)
			}
		}
	}

	manifest := markerSourceManifest{
		Version:       markerSourceManifestVersion,
		SelectionMode: selectionMode,
		RaceID:        strings.TrimSpace(race.RaceID),
		RaceRunID:     strings.TrimSpace(race.RaceRunID),
		Phase:         strings.TrimSpace(race.Phase),
		RaceSequence:  race.Sequence,
		Sources:       make([]markerSourceDescriptor, 0),
	}
	found := make(map[string]struct{}, len(selectedBySource))

	server.sourcesMu.RLock()
	for _, sourceID := range server.sourceOrder {
		managed := server.managedSources[sourceID]
		if managed == nil || managed.relay == nil || effectiveRelaySourceKind(managed.definition.SourceKind) != relaySourceKindVehicle {
			continue
		}
		carID, rosterSelected := selectedBySource[sourceID]
		if selectionMode == "locked_roster" && !rosterSelected {
			continue
		}
		if selectionMode == "drive" && !managed.relay.driveStatusSnapshot().Enabled {
			continue
		}
		if carID == "" {
			carID = strings.TrimSpace(managed.definition.RaceCarID)
		}
		status := managed.relay.statusSnapshot(now)
		manifest.Sources = append(manifest.Sources, markerSourceDescriptor{
			SourceID:     sourceID,
			CarID:        carID,
			ObserverPath: "/ws?role=observer&device=" + url.QueryEscape(sourceID),
			State:        status.State,
			VideoHealth:  status.VideoHealth,
		})
		found[sourceID] = struct{}{}
	}
	server.sourcesMu.RUnlock()

	if selectionMode == "locked_roster" {
		for sourceID := range selectedBySource {
			if _, exists := found[sourceID]; !exists {
				manifest.MissingSourceIDs = append(manifest.MissingSourceIDs, sourceID)
			}
		}
		sort.Strings(manifest.MissingSourceIDs)
	}
	sort.Slice(manifest.Sources, func(left, right int) bool {
		return manifest.Sources[left].SourceID < manifest.Sources[right].SourceID
	})
	manifest.Revision = markerSourceManifestRevision(manifest)
	return manifest
}

func markerSourceManifestRevision(manifest markerSourceManifest) string {
	revisionInput := struct {
		SelectionMode    string                   `json:"selectionMode"`
		RaceRunID        string                   `json:"raceRunId,omitempty"`
		Sources          []markerSourceDescriptor `json:"sources"`
		MissingSourceIDs []string                 `json:"missingSourceIds,omitempty"`
	}{
		SelectionMode:    manifest.SelectionMode,
		RaceRunID:        manifest.RaceRunID,
		Sources:          append([]markerSourceDescriptor(nil), manifest.Sources...),
		MissingSourceIDs: append([]string(nil), manifest.MissingSourceIDs...),
	}
	for index := range revisionInput.Sources {
		revisionInput.Sources[index].State = ""
		revisionInput.Sources[index].VideoHealth = ""
	}
	body, _ := json.Marshal(revisionInput)
	digest := sha256.Sum256(body)
	return "ms_" + hex.EncodeToString(digest[:16])
}

func (server *relayServer) serveMarkerSources(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	selection := strings.TrimSpace(req.URL.Query().Get("selection"))
	if selection != "" && selection != "auto" && selection != "all" {
		http.Error(w, "selection must be auto or all", http.StatusBadRequest)
		return
	}
	manifest := server.markerSourceManifestSnapshot(time.Now(), selection)
	etag := `"` + manifest.Revision + `"`
	w.Header().Set("ETag", etag)
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeSourceJSON(w, http.StatusOK, manifest)
}
