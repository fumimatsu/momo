package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const sourceManagementMaxBodyBytes = 16 * 1024

var (
	dynamicSourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ayameRoomIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type relaySourceRuntime struct {
	rootContext          context.Context
	allowObserverCommand bool
	rtpStallTimeout      time.Duration
	upstreamStartTimeout time.Duration
	healthRecoveryMode   vehicleHealthRecoveryMode
	fuelDriveDuration    time.Duration
	raceAudioService     *raceAudioServiceClient
	ayameSignalingURL    string
	ayameClientIDPrefix  string
	ayameSignalingKey    string
	ayameRoomPrefix      string
}

type managedRelaySource struct {
	relay      *relay
	definition relayFileSource
	dynamic    bool
	cancel     context.CancelFunc
}

type relaySourceEntry struct {
	id    string
	relay *relay
}

type dynamicSourceRegistry struct {
	path string
}

type sourceManagementSnapshot struct {
	Version int                    `json:"version"`
	Sources []sourceDefinitionView `json:"sources"`
}

type sourceDefinitionView struct {
	ID                string `json:"id"`
	URL               string `json:"url"`
	SourceKind        string `json:"sourceKind"`
	DisplayName       string `json:"displayName,omitempty"`
	RaceCarID         string `json:"raceCarId,omitempty"`
	AyamePilotEnabled bool   `json:"ayamePilotEnabled"`
	AyamePilotRoom    string `json:"ayamePilotRoom,omitempty"`
	Dynamic           bool   `json:"dynamic"`
	LocalPilotPath    string `json:"localPilotPath"`
}

type sourceManagementError struct {
	status  int
	code    string
	message string
}

func (err *sourceManagementError) Error() string {
	return err.message
}

func sourceError(status int, code string, format string, values ...any) error {
	return &sourceManagementError{status: status, code: code, message: fmt.Sprintf(format, values...)}
}

func relayBoolPointer(value bool) *bool {
	return &value
}

func normalizeSourceDefinition(definition relayFileSource, runtime relaySourceRuntime, dynamic bool) (relayFileSource, error) {
	definition.ID = strings.TrimSpace(definition.ID)
	definition.URL = strings.TrimSpace(definition.URL)
	definition.DisplayName = strings.TrimSpace(definition.DisplayName)
	definition.RaceCarID = strings.TrimSpace(definition.RaceCarID)
	definition.AyamePilotRoom = strings.TrimSpace(definition.AyamePilotRoom)
	definition.Enabled = nil
	if definition.ID == "" || strings.Contains(definition.ID, "=") {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_source_id", "source id must be non-empty and must not contain '='")
	}
	if definition.DisplayName == "" {
		definition.DisplayName = definition.ID
	}
	if dynamic && !dynamicSourceIDPattern.MatchString(definition.ID) {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_source_id", "dynamic source id must match %s", dynamicSourceIDPattern.String())
	}
	if err := validateRelaySourceURL(definition.URL); err != nil {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_source_url", "source %q: %v", definition.ID, err)
	}
	sourceKind, err := normalizeRelaySourceKind(definition.SourceKind)
	if err != nil {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_source_kind", "source %q: %v", definition.ID, err)
	}
	definition.SourceKind = sourceKind
	if strings.Contains(definition.RaceCarID, "=") {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_race_car_id", "raceCarId must not contain '='")
	}
	if sourceKind == relaySourceKindVenue {
		if definition.RaceCarID != "" {
			return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_venue_race_car", "venue source cannot define raceCarId")
		}
		if definition.AyamePilotRoom != "" || definition.AyamePilotEnabled != nil && *definition.AyamePilotEnabled {
			return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_venue_pilot", "venue source cannot enable Ayame Pilot")
		}
		definition.AyamePilotEnabled = relayBoolPointer(false)
		definition.AyamePilotRoom = ""
		return definition, nil
	}

	ayameEnabled := definition.AyamePilotRoom != "" || strings.TrimSpace(runtime.ayameRoomPrefix) != ""
	if definition.AyamePilotEnabled != nil {
		ayameEnabled = *definition.AyamePilotEnabled
	}
	if !ayameEnabled {
		if definition.AyamePilotRoom != "" {
			return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_ayame_source", "ayamePilotRoom cannot be set when ayamePilotEnabled is false")
		}
		definition.AyamePilotEnabled = relayBoolPointer(false)
		return definition, nil
	}
	if strings.TrimSpace(runtime.ayameSignalingURL) == "" {
		return relayFileSource{}, sourceError(http.StatusConflict, "ayame_unavailable", "Ayame signaling is not configured on Relay")
	}
	if definition.AyamePilotRoom == "" {
		roomID, err := generatedAyameRoomID(runtime.ayameRoomPrefix, definition.ID)
		if err != nil {
			return relayFileSource{}, err
		}
		definition.AyamePilotRoom = roomID
	}
	if !ayameRoomIDPattern.MatchString(definition.AyamePilotRoom) {
		return relayFileSource{}, sourceError(http.StatusBadRequest, "invalid_ayame_room", "ayamePilotRoom must match %s", ayameRoomIDPattern.String())
	}
	definition.AyamePilotEnabled = relayBoolPointer(true)
	return definition, nil
}

func generatedAyameRoomID(prefix string, sourceID string) (string, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "-._")
	if prefix == "" {
		return "", sourceError(http.StatusBadRequest, "ayame_room_required", "ayamePilotRoom or -ayame-room-prefix is required")
	}
	var token strings.Builder
	previousSeparator := false
	for _, value := range strings.ToLower(sourceID) {
		isSafe := value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-'
		if isSafe {
			token.WriteRune(value)
			previousSeparator = value == '-'
			continue
		}
		if !previousSeparator {
			token.WriteByte('-')
			previousSeparator = true
		}
	}
	sourceToken := strings.Trim(token.String(), "-")
	if sourceToken == "" {
		return "", sourceError(http.StatusBadRequest, "invalid_source_id", "source id cannot be converted to an Ayame room id")
	}
	roomID := prefix + "-" + sourceToken + "-ext"
	if !ayameRoomIDPattern.MatchString(roomID) {
		return "", sourceError(http.StatusBadRequest, "invalid_ayame_room", "generated Ayame room id is invalid or too long")
	}
	return roomID, nil
}

func loadDynamicSourceRegistry(path string) (*dynamicSourceRegistry, []relayFileSource, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, nil
	}
	registry := &dynamicSourceRegistry{path: path}
	readPath := path
	if _, err := os.Stat(readPath); errors.Is(err, os.ErrNotExist) {
		backupPath := path + ".bak"
		if _, backupErr := os.Stat(backupPath); backupErr == nil {
			readPath = backupPath
		} else if errors.Is(backupErr, os.ErrNotExist) {
			return registry, nil, nil
		} else {
			return nil, nil, fmt.Errorf("stat Relay source registry backup: %w", backupErr)
		}
	} else if err != nil {
		return nil, nil, fmt.Errorf("stat Relay source registry: %w", err)
	}
	config, err := readRelayFileConfig(readPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load Relay source registry: %w", err)
	}
	mappings, err := relayConfigMappingsForFile(config, true)
	if err != nil {
		return nil, nil, fmt.Errorf("validate Relay source registry: %w", err)
	}
	return registry, mappings.Definitions, nil
}

func (registry *dynamicSourceRegistry) save(definitions []relayFileSource) error {
	if registry == nil || strings.TrimSpace(registry.path) == "" {
		return errors.New("dynamic source registry is disabled")
	}
	directory := filepath.Dir(registry.path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create Relay source registry directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(registry.path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create Relay source registry temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return fmt.Errorf("protect Relay source registry temporary file: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(relayFileConfig{Version: relayConfigVersion, Sources: definitions}); err != nil {
		return fmt.Errorf("encode Relay source registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush Relay source registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Relay source registry: %w", err)
	}

	backupPath := registry.path + ".bak"
	_ = os.Remove(backupPath)
	hadCurrent := false
	if _, err := os.Stat(registry.path); err == nil {
		if err := os.Rename(registry.path, backupPath); err != nil {
			return fmt.Errorf("backup Relay source registry: %w", err)
		}
		hadCurrent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat Relay source registry before replace: %w", err)
	}
	if err := os.Rename(temporaryPath, registry.path); err != nil {
		if hadCurrent {
			_ = os.Rename(backupPath, registry.path)
		}
		return fmt.Errorf("replace Relay source registry: %w", err)
	}
	removeTemporary = false
	if hadCurrent {
		_ = os.Remove(backupPath)
	}
	return nil
}

func (server *relayServer) orderedSourcesSnapshot() []*relay {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	if len(server.sourceOrder) == 0 && len(server.sources) > 0 {
		sourceIDs := make([]string, 0, len(server.sources))
		for sourceID := range server.sources {
			sourceIDs = append(sourceIDs, sourceID)
		}
		sort.Strings(sourceIDs)
		sources := make([]*relay, 0, len(sourceIDs))
		for _, sourceID := range sourceIDs {
			if source := server.sources[sourceID]; source != nil {
				sources = append(sources, source)
			}
		}
		return sources
	}
	sources := make([]*relay, 0, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		if source := server.sources[sourceID]; source != nil {
			sources = append(sources, source)
		}
	}
	return sources
}

func (server *relayServer) sourceSnapshot() []*relay {
	return server.orderedSourcesSnapshot()
}

func (server *relayServer) sourceEntriesSnapshot() []relaySourceEntry {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	entries := make([]relaySourceEntry, 0, len(server.sources))
	seen := make(map[string]struct{}, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		if source := server.sources[sourceID]; source != nil {
			entries = append(entries, relaySourceEntry{id: sourceID, relay: source})
			seen[sourceID] = struct{}{}
		}
	}
	remaining := make([]string, 0, len(server.sources)-len(entries))
	for sourceID := range server.sources {
		if _, exists := seen[sourceID]; !exists {
			remaining = append(remaining, sourceID)
		}
	}
	sort.Strings(remaining)
	for _, sourceID := range remaining {
		if source := server.sources[sourceID]; source != nil {
			entries = append(entries, relaySourceEntry{id: sourceID, relay: source})
		}
	}
	return entries
}

func (server *relayServer) lookupSource(sourceID string) (*relay, bool) {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	source, ok := server.sources[sourceID]
	return source, ok
}

func (server *relayServer) acquireSourceSession(sourceID string) (*relay, bool) {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	source, ok := server.sources[sourceID]
	if ok {
		source.activeSessions.Add(1)
	}
	return source, ok
}

func (server *relayServer) onlySource() (*relay, bool) {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	if len(server.sources) != 1 {
		return nil, false
	}
	for _, source := range server.sources {
		return source, true
	}
	return nil, false
}

func (server *relayServer) validateSourceDefinitionLocked(definition relayFileSource, replacingSourceID string) error {
	if replacingSourceID == "" && len(server.sources) >= maximumConfiguredSources {
		return sourceError(http.StatusConflict, "source_capacity_reached", "Relay already has the maximum of %d sources", maximumConfiguredSources)
	}
	if _, exists := server.sources[definition.ID]; exists && definition.ID != replacingSourceID {
		return sourceError(http.StatusConflict, "duplicate_source_id", "source %q already exists", definition.ID)
	}
	for sourceID, managed := range server.managedSources {
		if sourceID == replacingSourceID {
			continue
		}
		if managed == nil {
			continue
		}
		existing := managed.definition
		if definition.RaceCarID != "" && existing.RaceCarID == definition.RaceCarID {
			return sourceError(http.StatusConflict, "duplicate_race_car_id", "raceCarId %q is already assigned to source %q", definition.RaceCarID, sourceID)
		}
		if definition.AyamePilotRoom != "" && existing.AyamePilotRoom == definition.AyamePilotRoom {
			return sourceError(http.StatusConflict, "duplicate_ayame_room", "Ayame room %q is already assigned to source %q", definition.AyamePilotRoom, sourceID)
		}
	}
	return nil
}

func (server *relayServer) prepareManagedSource(definition relayFileSource, dynamic bool) (*managedRelaySource, error) {
	normalized, err := normalizeSourceDefinition(definition, server.sourceRuntime, dynamic)
	if err != nil {
		return nil, err
	}
	source, err := newRelay(normalized.ID, normalized.URL, normalized.RaceCarID,
		server.sourceRuntime.allowObserverCommand,
		server.sourceRuntime.rtpStallTimeout,
		server.sourceRuntime.upstreamStartTimeout,
		server.sourceRuntime.healthRecoveryMode,
		server.sourceRuntime.fuelDriveDuration)
	if err != nil {
		return nil, sourceError(http.StatusInternalServerError, "source_initialization_failed", "initialize source %q: %v", normalized.ID, err)
	}
	source.sourceKind = normalized.SourceKind
	source.displayName = normalized.DisplayName
	source.recorder = server.recorder
	if normalized.SourceKind == relaySourceKindVehicle {
		source.raceAudio = newRaceAudioSource(source, server.sourceRuntime.raceAudioService)
	}
	return &managedRelaySource{
		relay:      source,
		definition: normalized,
		dynamic:    dynamic,
		cancel:     func() {},
	}, nil
}

func (server *relayServer) activateManagedSource(managed *managedRelaySource) error {
	server.sourcesMu.Lock()
	if err := server.validateSourceDefinitionLocked(managed.definition, ""); err != nil {
		server.sourcesMu.Unlock()
		managed.cancel()
		return err
	}
	if server.managedSources == nil {
		server.managedSources = make(map[string]*managedRelaySource)
	}
	server.sources[managed.definition.ID] = managed.relay
	server.sourceOrder = append(server.sourceOrder, managed.definition.ID)
	server.managedSources[managed.definition.ID] = managed
	server.sourcesMu.Unlock()
	server.startManagedSourceLoops(managed)
	return nil
}

func (server *relayServer) startManagedSourceLoops(managed *managedRelaySource) {
	sourceContext := server.sourceRuntime.rootContext
	if sourceContext == nil {
		sourceContext = context.Background()
	}
	sourceContext, managed.cancel = context.WithCancel(sourceContext)
	managed.relay.start(sourceContext)
	if managed.definition.AyamePilotRoom != "" {
		clientID := strings.TrimSuffix(server.sourceRuntime.ayameClientIDPrefix, "-") + "-" + managed.definition.ID
		managed.relay.startAyamePilot(sourceContext,
			server.sourceRuntime.ayameSignalingURL,
			managed.definition.AyamePilotRoom,
			clientID,
			server.sourceRuntime.ayameSignalingKey)
	}
}

func (server *relayServer) addInitialSource(definition relayFileSource, dynamic bool) error {
	managed, err := server.prepareManagedSource(definition, dynamic)
	if err != nil {
		return err
	}
	return server.activateManagedSource(managed)
}

func (server *relayServer) dynamicDefinitionsLocked() []relayFileSource {
	definitions := make([]relayFileSource, 0)
	for _, sourceID := range server.sourceOrder {
		managed := server.managedSources[sourceID]
		if managed != nil && managed.dynamic {
			definitions = append(definitions, managed.definition)
		}
	}
	return definitions
}

func (server *relayServer) addDynamicSource(definition relayFileSource) (sourceDefinitionView, error) {
	server.sourceMutationMu.Lock()
	defer server.sourceMutationMu.Unlock()
	if server.dynamicSourceRegistry == nil {
		return sourceDefinitionView{}, sourceError(http.StatusServiceUnavailable, "source_registry_disabled", "Relay source registry is not configured")
	}
	race := server.raceContextSnapshot()
	if race.Connected && strings.EqualFold(race.Phase, "green") {
		return sourceDefinitionView{}, sourceError(http.StatusConflict, "race_active", "sources cannot be added while the race phase is green")
	}
	managed, err := server.prepareManagedSource(definition, true)
	if err != nil {
		return sourceDefinitionView{}, err
	}
	server.sourcesMu.Lock()
	if err := server.validateSourceDefinitionLocked(managed.definition, ""); err != nil {
		server.sourcesMu.Unlock()
		managed.cancel()
		return sourceDefinitionView{}, err
	}
	definitions := append(server.dynamicDefinitionsLocked(), managed.definition)
	server.sourcesMu.Unlock()
	if err := server.dynamicSourceRegistry.save(definitions); err != nil {
		managed.cancel()
		return sourceDefinitionView{}, sourceError(http.StatusInternalServerError, "source_registry_write_failed", "%v", err)
	}
	if err := server.activateManagedSource(managed); err != nil {
		return sourceDefinitionView{}, err
	}
	return managedSourceView(managed), nil
}

func (server *relayServer) replaceDynamicSource(definition relayFileSource) (sourceDefinitionView, error) {
	server.sourceMutationMu.Lock()
	defer server.sourceMutationMu.Unlock()
	if server.dynamicSourceRegistry == nil {
		return sourceDefinitionView{}, sourceError(http.StatusServiceUnavailable, "source_registry_disabled", "Relay source registry is not configured")
	}
	race := server.raceContextSnapshot()
	if race.Connected && strings.EqualFold(race.Phase, "green") {
		return sourceDefinitionView{}, sourceError(http.StatusConflict, "race_active", "sources cannot be updated while the race phase is green")
	}
	managed, err := server.prepareManagedSource(definition, true)
	if err != nil {
		return sourceDefinitionView{}, err
	}

	server.sourcesMu.Lock()
	existing := server.managedSources[managed.definition.ID]
	if existing == nil {
		server.sourcesMu.Unlock()
		managed.cancel()
		return sourceDefinitionView{}, sourceError(http.StatusNotFound, "source_not_found", "source %q was not found", managed.definition.ID)
	}
	if !existing.dynamic {
		server.sourcesMu.Unlock()
		managed.cancel()
		return sourceDefinitionView{}, sourceError(http.StatusConflict, "static_source", "source %q is owned by the static Relay configuration", managed.definition.ID)
	}
	if sourceDefinitionsEqual(existing.definition, managed.definition) {
		view := managedSourceView(existing)
		server.sourcesMu.Unlock()
		managed.cancel()
		return view, nil
	}
	if relaySourceInUse(existing.relay, time.Now()) {
		server.sourcesMu.Unlock()
		managed.cancel()
		return sourceDefinitionView{}, sourceError(http.StatusConflict, "source_in_use", "source %q still has a viewer or active drive session", managed.definition.ID)
	}
	if err := server.validateSourceDefinitionLocked(managed.definition, managed.definition.ID); err != nil {
		server.sourcesMu.Unlock()
		managed.cancel()
		return sourceDefinitionView{}, err
	}
	definitions := server.dynamicDefinitionsLocked()
	for index := range definitions {
		if definitions[index].ID == managed.definition.ID {
			definitions[index] = managed.definition
			break
		}
	}
	server.sourcesMu.Unlock()
	if err := server.dynamicSourceRegistry.save(definitions); err != nil {
		managed.cancel()
		return sourceDefinitionView{}, sourceError(http.StatusInternalServerError, "source_registry_write_failed", "%v", err)
	}

	server.sourcesMu.Lock()
	server.sources[managed.definition.ID] = managed.relay
	server.managedSources[managed.definition.ID] = managed
	server.sourcesMu.Unlock()
	existing.relay.shutdown("source updated")
	existing.cancel()
	server.startManagedSourceLoops(managed)
	return managedSourceView(managed), nil
}

func sourceDefinitionsEqual(left relayFileSource, right relayFileSource) bool {
	leftAyameEnabled := left.AyamePilotEnabled != nil && *left.AyamePilotEnabled
	rightAyameEnabled := right.AyamePilotEnabled != nil && *right.AyamePilotEnabled
	return left.ID == right.ID &&
		left.URL == right.URL &&
		left.RaceCarID == right.RaceCarID &&
		leftAyameEnabled == rightAyameEnabled &&
		left.AyamePilotRoom == right.AyamePilotRoom
}

func relaySourceInUse(source *relay, now time.Time) bool {
	if source == nil {
		return false
	}
	source.viewersMu.RLock()
	viewerCount := len(source.viewers)
	pilotID := source.pilotID
	source.viewersMu.RUnlock()
	return source.activeSessions.Load() > 0 || viewerCount > 0 || pilotID != 0 || source.vehicleHealth.isActivelyDriving(now)
}

func (server *relayServer) removeDynamicSource(sourceID string) error {
	server.sourceMutationMu.Lock()
	defer server.sourceMutationMu.Unlock()
	if server.dynamicSourceRegistry == nil {
		return sourceError(http.StatusServiceUnavailable, "source_registry_disabled", "Relay source registry is not configured")
	}
	race := server.raceContextSnapshot()
	if race.Connected && strings.EqualFold(race.Phase, "green") {
		return sourceError(http.StatusConflict, "race_active", "sources cannot be removed while the race phase is green")
	}

	server.sourcesMu.Lock()
	managed := server.managedSources[sourceID]
	if managed == nil {
		server.sourcesMu.Unlock()
		return sourceError(http.StatusNotFound, "source_not_found", "source %q was not found", sourceID)
	}
	if !managed.dynamic {
		server.sourcesMu.Unlock()
		return sourceError(http.StatusConflict, "static_source", "source %q is owned by the static Relay configuration", sourceID)
	}
	if relaySourceInUse(managed.relay, time.Now()) {
		server.sourcesMu.Unlock()
		return sourceError(http.StatusConflict, "source_in_use", "source %q still has a viewer or active drive session", sourceID)
	}
	definitions := server.dynamicDefinitionsLocked()
	filtered := definitions[:0]
	for _, definition := range definitions {
		if definition.ID != sourceID {
			filtered = append(filtered, definition)
		}
	}
	server.sourcesMu.Unlock()
	if err := server.dynamicSourceRegistry.save(filtered); err != nil {
		return sourceError(http.StatusInternalServerError, "source_registry_write_failed", "%v", err)
	}

	server.sourcesMu.Lock()
	delete(server.sources, sourceID)
	delete(server.managedSources, sourceID)
	for index, existingID := range server.sourceOrder {
		if existingID == sourceID {
			server.sourceOrder = append(server.sourceOrder[:index], server.sourceOrder[index+1:]...)
			break
		}
	}
	server.sourcesMu.Unlock()
	managed.relay.shutdown("source removed")
	managed.cancel()
	return nil
}

func (r *relay) shutdown(reason string) {
	r.sendNeutralToUpstream(reason)
	r.upstreamMu.RLock()
	upstreamPC := r.upstreamPC
	r.upstreamMu.RUnlock()
	if upstreamPC != nil {
		_ = upstreamPC.Close()
	}
	r.lifecycle.Store(int32(sourceWaiting))
	r.videoHealth.Store(int32(videoNotStarted))
}

func managedSourceView(managed *managedRelaySource) sourceDefinitionView {
	definition := managed.definition
	ayameEnabled := definition.AyamePilotEnabled != nil && *definition.AyamePilotEnabled
	localPilotPath := ""
	if definition.SourceKind == relaySourceKindVehicle {
		localPilotPath = "/pilot.html?device=" + url.QueryEscape(definition.ID) + "&carId=" + url.QueryEscape(definition.RaceCarID)
	}
	return sourceDefinitionView{
		ID:                definition.ID,
		URL:               definition.URL,
		SourceKind:        definition.SourceKind,
		DisplayName:       definition.DisplayName,
		RaceCarID:         definition.RaceCarID,
		AyamePilotEnabled: ayameEnabled,
		AyamePilotRoom:    definition.AyamePilotRoom,
		Dynamic:           managed.dynamic,
		LocalPilotPath:    localPilotPath,
	}
}

func (server *relayServer) sourceManagementSnapshot() sourceManagementSnapshot {
	server.sourcesMu.RLock()
	defer server.sourcesMu.RUnlock()
	sources := make([]sourceDefinitionView, 0, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		if managed := server.managedSources[sourceID]; managed != nil {
			sources = append(sources, managedSourceView(managed))
		}
	}
	return sourceManagementSnapshot{Version: 1, Sources: sources}
}

func (server *relayServer) serveSources(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		writeSourceJSON(w, http.StatusOK, server.sourceManagementSnapshot())
	case http.MethodPost:
		defer req.Body.Close()
		var definition relayFileSource
		if err := decodeSourceManagementJSON(req, &definition); err != nil {
			writeSourceManagementError(w, sourceError(http.StatusBadRequest, "invalid_json", "%v", err))
			return
		}
		created, err := server.addDynamicSource(definition)
		if err != nil {
			writeSourceManagementError(w, err)
			return
		}
		writeSourceJSON(w, http.StatusCreated, created)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeSourceManagementError(w, sourceError(http.StatusMethodNotAllowed, "method_not_allowed", "method must be GET or POST"))
	}
}

func (server *relayServer) serveSourceByID(w http.ResponseWriter, req *http.Request) {
	sourceID, err := url.PathUnescape(strings.TrimPrefix(req.URL.Path, "/api/v1/sources/"))
	if err != nil || strings.TrimSpace(sourceID) == "" || strings.Contains(sourceID, "/") {
		writeSourceManagementError(w, sourceError(http.StatusBadRequest, "invalid_source_id", "a valid source id is required"))
		return
	}
	switch req.Method {
	case http.MethodPut:
		defer req.Body.Close()
		var definition relayFileSource
		if err := decodeSourceManagementJSON(req, &definition); err != nil {
			writeSourceManagementError(w, sourceError(http.StatusBadRequest, "invalid_json", "%v", err))
			return
		}
		if definition.ID != "" && definition.ID != sourceID {
			writeSourceManagementError(w, sourceError(http.StatusBadRequest, "source_id_mismatch", "body source id must match the request path"))
			return
		}
		definition.ID = sourceID
		updated, err := server.replaceDynamicSource(definition)
		if err != nil {
			writeSourceManagementError(w, err)
			return
		}
		writeSourceJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := server.removeDynamicSource(sourceID); err != nil {
			writeSourceManagementError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", http.MethodPut+", "+http.MethodDelete)
		writeSourceManagementError(w, sourceError(http.StatusMethodNotAllowed, "method_not_allowed", "method must be PUT or DELETE"))
	}
}

func decodeSourceManagementJSON(req *http.Request, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(req.Body, sourceManagementMaxBodyBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func sourceAdminTokenHandler(token string, next http.HandlerFunc) http.HandlerFunc {
	token = strings.TrimSpace(token)
	return func(w http.ResponseWriter, req *http.Request) {
		if token == "" {
			writeSourceManagementError(w, sourceError(http.StatusServiceUnavailable, "source_admin_disabled", "MOMO_RELAY_ADMIN_TOKEN is not configured"))
			return
		}
		authorization := req.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeSourceManagementError(w, sourceError(http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required"))
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeSourceManagementError(w, sourceError(http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required"))
			return
		}
		next(w, req)
	}
}

func writeSourceManagementError(w http.ResponseWriter, err error) {
	managementError := &sourceManagementError{status: http.StatusInternalServerError, code: "internal_error", message: err.Error()}
	var typedError *sourceManagementError
	if errors.As(err, &typedError) {
		managementError = typedError
	}
	writeSourceJSON(w, managementError.status, map[string]any{
		"ok":      false,
		"error":   managementError.code,
		"message": managementError.message,
	})
}

func writeSourceJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
