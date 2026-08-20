package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	teamObserverDirectoryCacheVersion = 1
	teamObserverDirectoryVersion      = 1
	maxTeamObserverDirectoryBytes     = 4 << 20
	maxTeamObserverDirectoryItems     = 256
)

var teamObserverDirectoryRevisionPattern = regexp.MustCompile(`^rd_[a-f0-9]{32}$`)

type teamObserverDirectorySource struct {
	path             string
	organizationSlug string
	eventSlug        string
	maxAge           time.Duration
}

type teamObserverDirectoryCache struct {
	SchemaVersion int                           `json:"schemaVersion"`
	FetchedAt     string                        `json:"fetchedAt"`
	ETag          string                        `json:"etag"`
	Directory     teamObserverDirectoryDocument `json:"directory"`
}

type teamObserverDirectoryDocument struct {
	SchemaVersion     int                                    `json:"schemaVersion"`
	GeneratedAt       string                                 `json:"generatedAt"`
	Organization      teamObserverDirectoryOrganization      `json:"organization"`
	Event             teamObserverDirectoryEvent             `json:"event"`
	Pilots            []teamObserverDirectoryPilot           `json:"pilots"`
	Vehicles          []teamObserverDirectoryVehicle         `json:"vehicles"`
	Entries           []teamObserverDirectoryEntry           `json:"entries"`
	RosterCandidates  []teamObserverDirectoryRosterCandidate `json:"rosterCandidates"`
	DirectoryRevision string                                 `json:"directoryRevision"`
}

type teamObserverDirectoryOrganization struct {
	OrganizationID string `json:"organizationId"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
}

type teamObserverDirectoryEvent struct {
	EventID string `json:"eventId"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Status  string `json:"status"`
}

type teamObserverDirectoryPilot struct {
	PilotID     string                          `json:"pilotId"`
	PilotNo     string                          `json:"pilotNo,omitempty"`
	Callsign    string                          `json:"callsign"`
	DisplayName string                          `json:"displayName,omitempty"`
	TeamName    string                          `json:"teamName,omitempty"`
	PhotoURL    string                          `json:"photoUrl,omitempty"`
	Color       string                          `json:"color,omitempty"`
	ThemeSong   *teamObserverDirectoryThemeSong `json:"themeSong,omitempty"`
}

type teamObserverDirectoryThemeSong struct {
	URL        string `json:"url"`
	Provider   string `json:"provider"`
	Title      string `json:"title,omitempty"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
}

type teamObserverDirectoryVehicle struct {
	VehicleID      string                               `json:"vehicleId"`
	VehicleName    string                               `json:"vehicleName"`
	DisplayNumber  string                               `json:"displayNumber,omitempty"`
	Status         string                               `json:"status"`
	SourceBindings []teamObserverDirectorySourceBinding `json:"sourceBindings"`
}

type teamObserverDirectorySourceBinding struct {
	SourceID string `json:"sourceId"`
	Active   bool   `json:"active"`
}

type teamObserverDirectoryEntry struct {
	EntryID     string `json:"entryId"`
	PilotID     string `json:"pilotId"`
	ClassCode   string `json:"classCode,omitempty"`
	EntryStatus string `json:"entryStatus"`
}

type teamObserverDirectoryRosterCandidate struct {
	EntryID   string `json:"entryId"`
	PilotID   string `json:"pilotId"`
	ClassCode string `json:"classCode,omitempty"`
}

type teamObserverDirectoryProjection struct {
	Version           int                                `json:"version"`
	ServerTime        time.Time                          `json:"serverTime"`
	FetchedAt         time.Time                          `json:"fetchedAt"`
	AgeMS             int64                              `json:"ageMs"`
	Stale             bool                               `json:"stale"`
	DirectoryRevision string                             `json:"directoryRevision"`
	Event             teamObserverDirectoryEvent         `json:"event"`
	Pilots            []teamObserverDirectoryPilotView   `json:"pilots"`
	Vehicles          []teamObserverDirectoryVehicleView `json:"vehicles"`
	Entries           []teamObserverDirectoryEntryView   `json:"entries"`
}

type teamObserverDirectoryPilotView struct {
	PilotID     string `json:"pilotId"`
	PilotNo     string `json:"pilotNo,omitempty"`
	Callsign    string `json:"callsign"`
	DisplayName string `json:"displayName,omitempty"`
	TeamName    string `json:"teamName,omitempty"`
	PhotoURL    string `json:"photoUrl,omitempty"`
	Color       string `json:"color,omitempty"`
}

type teamObserverDirectoryVehicleView struct {
	VehicleID     string `json:"vehicleId"`
	VehicleName   string `json:"vehicleName"`
	DisplayNumber string `json:"displayNumber,omitempty"`
	Status        string `json:"status"`
	SourceID      string `json:"sourceId,omitempty"`
}

type teamObserverDirectoryEntryView struct {
	EntryID   string `json:"entryId"`
	PilotID   string `json:"pilotId"`
	ClassCode string `json:"classCode,omitempty"`
}

func newTeamObserverDirectorySource(path, organizationSlug, eventSlug string, maxAge time.Duration) (*teamObserverDirectorySource, error) {
	path = strings.TrimSpace(path)
	organizationSlug = strings.TrimSpace(organizationSlug)
	eventSlug = strings.TrimSpace(eventSlug)
	if path == "" {
		if organizationSlug != "" || eventSlug != "" {
			return nil, fmt.Errorf("Team Observer directory organization/event require a cache path")
		}
		return nil, nil
	}
	if organizationSlug == "" || eventSlug == "" {
		return nil, fmt.Errorf("Team Observer directory cache requires organization and event slugs")
	}
	if maxAge <= 0 {
		return nil, fmt.Errorf("Team Observer directory max age must be positive")
	}
	return &teamObserverDirectorySource{
		path: path, organizationSlug: organizationSlug, eventSlug: eventSlug, maxAge: maxAge,
	}, nil
}

func (source *teamObserverDirectorySource) projection(now time.Time) (teamObserverDirectoryProjection, error) {
	cache, err := source.cache()
	if err != nil {
		return teamObserverDirectoryProjection{}, err
	}
	fetchedAt, _ := time.Parse(time.RFC3339Nano, cache.FetchedAt)
	age := now.Sub(fetchedAt)
	if age < 0 {
		age = 0
	}
	projection := teamObserverDirectoryProjection{
		Version: teamObserverDirectoryVersion, ServerTime: now.UTC(), FetchedAt: fetchedAt.UTC(),
		AgeMS: age.Milliseconds(), Stale: age > source.maxAge,
		DirectoryRevision: cache.Directory.DirectoryRevision, Event: cache.Directory.Event,
		Pilots:   make([]teamObserverDirectoryPilotView, 0, len(cache.Directory.Pilots)),
		Vehicles: make([]teamObserverDirectoryVehicleView, 0, len(cache.Directory.Vehicles)),
		Entries:  make([]teamObserverDirectoryEntryView, 0, len(cache.Directory.RosterCandidates)),
	}
	for _, pilot := range cache.Directory.Pilots {
		projection.Pilots = append(projection.Pilots, teamObserverDirectoryPilotView{
			PilotID: pilot.PilotID, PilotNo: pilot.PilotNo, Callsign: pilot.Callsign,
			DisplayName: pilot.DisplayName, TeamName: pilot.TeamName, PhotoURL: pilot.PhotoURL, Color: pilot.Color,
		})
	}
	for _, vehicle := range cache.Directory.Vehicles {
		view := teamObserverDirectoryVehicleView{
			VehicleID: vehicle.VehicleID, VehicleName: vehicle.VehicleName,
			DisplayNumber: vehicle.DisplayNumber, Status: vehicle.Status,
		}
		if len(vehicle.SourceBindings) == 1 {
			view.SourceID = vehicle.SourceBindings[0].SourceID
		}
		projection.Vehicles = append(projection.Vehicles, view)
	}
	entryByID := make(map[string]teamObserverDirectoryEntry, len(cache.Directory.Entries))
	for _, entry := range cache.Directory.Entries {
		entryByID[entry.EntryID] = entry
	}
	for _, candidate := range cache.Directory.RosterCandidates {
		entry := entryByID[candidate.EntryID]
		projection.Entries = append(projection.Entries, teamObserverDirectoryEntryView{
			EntryID: entry.EntryID, PilotID: entry.PilotID, ClassCode: entry.ClassCode,
		})
	}
	return projection, nil
}

func (source *teamObserverDirectorySource) cache() (teamObserverDirectoryCache, error) {
	file, err := os.Open(source.path)
	if err != nil {
		return teamObserverDirectoryCache{}, fmt.Errorf("open cache: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTeamObserverDirectoryBytes+1))
	if err != nil {
		return teamObserverDirectoryCache{}, fmt.Errorf("read cache: %w", err)
	}
	if len(data) > maxTeamObserverDirectoryBytes {
		return teamObserverDirectoryCache{}, fmt.Errorf("cache exceeds %d bytes", maxTeamObserverDirectoryBytes)
	}
	var cache teamObserverDirectoryCache
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cache); err != nil {
		return teamObserverDirectoryCache{}, fmt.Errorf("decode cache: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return teamObserverDirectoryCache{}, fmt.Errorf("decode cache: trailing JSON value")
	}
	if err := validateTeamObserverDirectoryCache(cache, source.organizationSlug, source.eventSlug); err != nil {
		return teamObserverDirectoryCache{}, err
	}
	return cache, nil
}

func validateTeamObserverDirectoryCache(cache teamObserverDirectoryCache, organizationSlug, eventSlug string) error {
	if cache.SchemaVersion != teamObserverDirectoryCacheVersion || cache.Directory.SchemaVersion != teamObserverDirectoryVersion {
		return fmt.Errorf("unsupported cache or directory schemaVersion")
	}
	if _, err := time.Parse(time.RFC3339Nano, cache.FetchedAt); err != nil {
		return fmt.Errorf("invalid fetchedAt: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, cache.Directory.GeneratedAt); err != nil {
		return fmt.Errorf("invalid generatedAt: %w", err)
	}
	if cache.Directory.Organization.Slug != organizationSlug || cache.Directory.Event.Slug != eventSlug {
		return fmt.Errorf("cache scope does not match configured organization/event")
	}
	if cache.Directory.Pilots == nil || cache.Directory.Vehicles == nil || cache.Directory.Entries == nil ||
		cache.Directory.RosterCandidates == nil {
		return fmt.Errorf("directory pilots, vehicles, entries, and rosterCandidates arrays are required")
	}
	if !teamObserverDirectoryRevisionPattern.MatchString(cache.Directory.DirectoryRevision) ||
		normalizeTeamObserverDirectoryETag(cache.ETag) != cache.Directory.DirectoryRevision {
		return fmt.Errorf("cache revision or ETag is invalid")
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Organization.OrganizationID, "organizationId", 128); err != nil {
		return err
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Organization.Slug, "organization slug", 128); err != nil {
		return err
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Organization.Name, "organization name", 256); err != nil {
		return err
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Event.EventID, "eventId", 128); err != nil {
		return err
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Event.Slug, "event slug", 128); err != nil {
		return err
	}
	if err := requiredTeamObserverDirectoryText(cache.Directory.Event.Name, "event name", 256); err != nil {
		return err
	}
	if !isTeamObserverDirectoryEventStatus(cache.Directory.Event.Status) {
		return fmt.Errorf("event status is invalid")
	}
	if len(cache.Directory.Pilots) > maxTeamObserverDirectoryItems || len(cache.Directory.Vehicles) > maxTeamObserverDirectoryItems ||
		len(cache.Directory.Entries) > maxTeamObserverDirectoryItems || len(cache.Directory.RosterCandidates) > maxTeamObserverDirectoryItems {
		return fmt.Errorf("directory collection exceeds %d items", maxTeamObserverDirectoryItems)
	}
	pilotIDs := make(map[string]struct{}, len(cache.Directory.Pilots))
	for _, pilot := range cache.Directory.Pilots {
		if err := requiredTeamObserverDirectoryText(pilot.PilotID, "pilotId", 128); err != nil {
			return err
		}
		if err := requiredTeamObserverDirectoryText(pilot.Callsign, "callsign", 128); err != nil {
			return err
		}
		for name, value := range map[string]string{
			"pilotNo": pilot.PilotNo, "displayName": pilot.DisplayName, "teamName": pilot.TeamName,
		} {
			if err := optionalTeamObserverDirectoryText(value, name, 512); err != nil {
				return err
			}
		}
		if _, exists := pilotIDs[pilot.PilotID]; exists {
			return fmt.Errorf("duplicate pilotId %q", pilot.PilotID)
		}
		pilotIDs[pilot.PilotID] = struct{}{}
		if pilot.PhotoURL != "" && !isTeamObserverDirectoryHTTPURL(pilot.PhotoURL) {
			return fmt.Errorf("pilot photoUrl is invalid")
		}
		if pilot.Color != "" && !isTeamObserverDirectoryColor(pilot.Color) {
			return fmt.Errorf("pilot color is invalid")
		}
		if pilot.ThemeSong != nil {
			if err := validateTeamObserverDirectoryThemeSong(*pilot.ThemeSong); err != nil {
				return err
			}
		}
	}
	vehicleIDs := make(map[string]struct{}, len(cache.Directory.Vehicles))
	sourceIDs := make(map[string]struct{}, len(cache.Directory.Vehicles))
	for _, vehicle := range cache.Directory.Vehicles {
		if err := requiredTeamObserverDirectoryText(vehicle.VehicleID, "vehicleId", 128); err != nil {
			return err
		}
		if err := requiredTeamObserverDirectoryText(vehicle.VehicleName, "vehicleName", 128); err != nil {
			return err
		}
		if err := optionalTeamObserverDirectoryText(vehicle.DisplayNumber, "vehicle displayNumber", 32); err != nil {
			return err
		}
		if _, exists := vehicleIDs[vehicle.VehicleID]; exists {
			return fmt.Errorf("duplicate vehicleId %q", vehicle.VehicleID)
		}
		vehicleIDs[vehicle.VehicleID] = struct{}{}
		if vehicle.Status != "active" && vehicle.Status != "maintenance" {
			return fmt.Errorf("vehicle status is invalid")
		}
		if len(vehicle.SourceBindings) > 1 {
			return fmt.Errorf("vehicle %q has multiple active sources", vehicle.VehicleID)
		}
		for _, binding := range vehicle.SourceBindings {
			if !binding.Active {
				return fmt.Errorf("vehicle %q contains an inactive source binding", vehicle.VehicleID)
			}
			if err := requiredTeamObserverDirectoryText(binding.SourceID, "sourceId", 128); err != nil {
				return err
			}
			if _, exists := sourceIDs[binding.SourceID]; exists {
				return fmt.Errorf("duplicate active sourceId %q", binding.SourceID)
			}
			sourceIDs[binding.SourceID] = struct{}{}
		}
	}
	entries := make(map[string]teamObserverDirectoryEntry, len(cache.Directory.Entries))
	confirmed := make(map[string]struct{})
	for _, entry := range cache.Directory.Entries {
		if err := requiredTeamObserverDirectoryText(entry.EntryID, "entryId", 128); err != nil {
			return err
		}
		if _, exists := entries[entry.EntryID]; exists {
			return fmt.Errorf("duplicate entryId %q", entry.EntryID)
		}
		if _, exists := pilotIDs[entry.PilotID]; !exists {
			return fmt.Errorf("entry %q references an unknown pilot", entry.EntryID)
		}
		if !isTeamObserverDirectoryEntryStatus(entry.EntryStatus) {
			return fmt.Errorf("entry status is invalid")
		}
		if err := optionalTeamObserverDirectoryText(entry.ClassCode, "entry classCode", 64); err != nil {
			return err
		}
		entries[entry.EntryID] = entry
		if entry.EntryStatus == "confirmed" {
			confirmed[entry.EntryID] = struct{}{}
		}
	}
	candidates := make(map[string]struct{}, len(cache.Directory.RosterCandidates))
	for _, candidate := range cache.Directory.RosterCandidates {
		entry, exists := entries[candidate.EntryID]
		if !exists || entry.EntryStatus != "confirmed" || entry.PilotID != candidate.PilotID || entry.ClassCode != candidate.ClassCode {
			return fmt.Errorf("roster candidate %q does not match a confirmed entry", candidate.EntryID)
		}
		if _, exists := candidates[candidate.EntryID]; exists {
			return fmt.Errorf("duplicate roster candidate %q", candidate.EntryID)
		}
		candidates[candidate.EntryID] = struct{}{}
	}
	if len(candidates) != len(confirmed) {
		return fmt.Errorf("roster candidates do not match confirmed entries")
	}
	return nil
}

func (server *relayServer) serveTeamObserverDirectory(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if server.teamObserverDirectory == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	projection, err := server.teamObserverDirectory.projection(time.Now())
	if err != nil {
		logTeamObserverDirectoryError(err)
		http.Error(w, "team observer directory unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(projection)
}

func (server *relayServer) serveCoordinatorDirectoryCache(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if server.teamObserverDirectory == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cache, err := server.teamObserverDirectory.cache()
	if err != nil {
		logTeamObserverDirectoryError(err)
		http.Error(w, "coordinator directory cache unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", `"`+normalizeTeamObserverDirectoryETag(cache.ETag)+`"`)
	_ = json.NewEncoder(w).Encode(cache)
}

func logTeamObserverDirectoryError(err error) {
	// Keep the response generic while retaining a local operational diagnostic.
	fmt.Fprintf(os.Stderr, "Team Observer directory: %v\n", err)
}

func normalizeTeamObserverDirectoryETag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "W/"))
	}
	return strings.Trim(value, `"`)
}

func requiredTeamObserverDirectoryText(value, name string, maximum int) error {
	if strings.TrimSpace(value) == "" || len([]byte(value)) > maximum || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func optionalTeamObserverDirectoryText(value, name string, maximum int) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" || len([]byte(value)) > maximum || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateTeamObserverDirectoryThemeSong(song teamObserverDirectoryThemeSong) error {
	if err := requiredTeamObserverDirectoryText(song.Provider, "themeSong provider", 128); err != nil {
		return err
	}
	if len(song.URL) > 2048 || !isTeamObserverDirectoryHTTPURL(song.URL) {
		return fmt.Errorf("themeSong URL is invalid")
	}
	if err := optionalTeamObserverDirectoryText(song.Title, "themeSong title", 512); err != nil {
		return err
	}
	if song.ArtworkURL != "" && (len(song.ArtworkURL) > 2048 || !isTeamObserverDirectoryHTTPURL(song.ArtworkURL)) {
		return fmt.Errorf("themeSong artworkUrl is invalid")
	}
	if song.UpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, song.UpdatedAt); err != nil {
			return fmt.Errorf("themeSong updatedAt is invalid")
		}
	}
	return nil
}

func isTeamObserverDirectoryHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func isTeamObserverDirectoryColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, character := range value[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func isTeamObserverDirectoryEventStatus(value string) bool {
	switch value {
	case "draft", "open", "live", "closed", "archived":
		return true
	default:
		return false
	}
}

func isTeamObserverDirectoryEntryStatus(value string) bool {
	switch value {
	case "pending", "confirmed", "waitlist", "cancelled":
		return true
	default:
		return false
	}
}
