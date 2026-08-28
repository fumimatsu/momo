package racerecorder

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maximumRequestBytes = 1 << 20
	defaultMaxTailMS    = int64(30_000)
	defaultMaxSources   = 64
)

type Config struct {
	RelayWebSocketURL string
	StorageRoot       string
	Token             string
	MinimumFreeBytes  int64
	MaximumSources    int
	MaximumTailMS     int64
	StartTimeout      time.Duration
	SegmentDuration   time.Duration
}

type commandReceipt struct {
	Kind       string `json:"kind"`
	Hash       string `json:"hash"`
	RaceRunID  string `json:"raceRunId"`
	RecordedAt string `json:"recordedAt"`
}

type receiptFile struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Commands      map[string]commandReceipt `json:"commands"`
}

type Server struct {
	mu          sync.Mutex
	config      Config
	ctx         context.Context
	cancel      context.CancelFunc
	state       string
	lastError   string
	active      *runSession
	starting    bool
	startedAt   time.Time
	updatedAt   time.Time
	receipts    map[string]commandReceipt
	receiptPath string
	closeOnce   sync.Once
}

func NewServer(config Config) (*Server, error) {
	config.RelayWebSocketURL = strings.TrimSpace(config.RelayWebSocketURL)
	config.StorageRoot = strings.TrimSpace(config.StorageRoot)
	config.Token = strings.TrimSpace(config.Token)
	if _, err := recorderSourceURL(config.RelayWebSocketURL, "source-check"); err != nil {
		return nil, err
	}
	if config.StorageRoot == "" {
		return nil, fmt.Errorf("storage root is required")
	}
	if len(config.Token) < 32 {
		return nil, fmt.Errorf("Recorder token must contain at least 32 characters")
	}
	if config.MinimumFreeBytes < 0 {
		return nil, fmt.Errorf("minimum free bytes must not be negative")
	}
	if config.MaximumSources == 0 {
		config.MaximumSources = defaultMaxSources
	}
	if config.MaximumSources < 1 || config.MaximumSources > 64 {
		return nil, fmt.Errorf("maximum sources must be between 1 and 64")
	}
	if config.MaximumTailMS == 0 {
		config.MaximumTailMS = defaultMaxTailMS
	}
	if config.MaximumTailMS < 0 {
		return nil, fmt.Errorf("maximum tail must not be negative")
	}
	if config.StartTimeout <= 0 {
		config.StartTimeout = 4 * time.Second
	}
	if config.SegmentDuration <= 0 {
		config.SegmentDuration = 2 * time.Minute
	}
	if err := os.MkdirAll(config.StorageRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create Recorder storage root: %w", err)
	}
	root, err := filepath.Abs(config.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve Recorder storage root: %w", err)
	}
	config.StorageRoot = root
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	server := &Server{
		config: config, ctx: ctx, cancel: cancel, state: StateReady, updatedAt: now,
		receipts: make(map[string]commandReceipt), receiptPath: filepath.Join(root, "command-receipts.json"),
	}
	if err := server.loadReceipts(); err != nil {
		cancel()
		return nil, err
	}
	return server, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", server.authorize(server.serveStatus))
	mux.HandleFunc("/api/v1/recordings/start", server.authorize(server.serveStart))
	mux.HandleFunc("/api/v1/recordings/stop", server.authorize(server.serveStop))
	return mux
}

func (server *Server) Close() error {
	var result error
	server.closeOnce.Do(func() {
		server.cancel()
		server.mu.Lock()
		active := server.active
		server.mu.Unlock()
		if active != nil {
			result = active.Stop("race_aborted")
		}
	})
	return result
}

func (server *Server) serveStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "GET is required")
		return
	}
	server.writeStatus(writer, http.StatusOK)
}

func (server *Server) serveStart(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "POST is required")
		return
	}
	var command StartRequest
	if err := decodeStrictJSON(request, &command); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := validateStartRequest(command, server.config.MaximumSources); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_start", err.Error())
		return
	}
	if command.Mode == ModeProgramOnly {
		writeError(writer, http.StatusUnprocessableEntity, "program_source_unavailable", "program_only requires a registered Program Director output and is not enabled")
		return
	}
	hash := requestHash(command)
	server.mu.Lock()
	if handled := server.handleDuplicateLocked(writer, command.CommandID, "start", hash, command.RaceRunID); handled {
		server.mu.Unlock()
		return
	}
	if server.starting || server.active != nil {
		server.mu.Unlock()
		writeError(writer, http.StatusConflict, "recorder_busy", "another recording run is active or starting")
		return
	}
	server.starting = true
	server.updatedAt = time.Now().UTC()
	server.mu.Unlock()

	free, err := availableBytes(server.config.StorageRoot)
	if err != nil || free < server.config.MinimumFreeBytes {
		message := fmt.Sprintf("storage reserve check failed: free=%d required=%d", free, server.config.MinimumFreeBytes)
		if err != nil {
			message = fmt.Sprintf("storage reserve check failed: %v", err)
		}
		server.failStart(message)
		writeError(writer, http.StatusInsufficientStorage, "insufficient_storage", message)
		return
	}
	session, err := newRunSession(server.ctx, command, server.config.StorageRoot, server.config.RelayWebSocketURL, server.config.SegmentDuration, server.sourceFailed)
	if err == nil {
		err = session.Start(server.config.StartTimeout)
	}
	if err != nil {
		if session != nil {
			if archiveErr := archiveFailedRunDirectory(server.config.StorageRoot, command.RaceRunID); archiveErr != nil {
				err = fmt.Errorf("%w; archive failed Recorder attempt: %v", err, archiveErr)
			}
		}
		server.failStart(err.Error())
		writeError(writer, http.StatusBadGateway, "source_start_failed", err.Error())
		return
	}
	if state, message := session.currentState(); state != "recording" {
		_ = session.Stop("race_aborted")
		if archiveErr := archiveFailedRunDirectory(server.config.StorageRoot, command.RaceRunID); archiveErr != nil {
			message = fmt.Sprintf("%s; archive failed Recorder attempt: %v", message, archiveErr)
		}
		if message == "" {
			message = "a Recorder source failed before the start response was committed"
		}
		server.failStart(message)
		writeError(writer, http.StatusBadGateway, "source_start_failed", message)
		return
	}

	server.mu.Lock()
	server.starting = false
	server.active = session
	server.state = StateRecording
	server.lastError = ""
	server.startedAt = session.startedAt
	server.updatedAt = time.Now().UTC()
	server.receipts[command.CommandID] = commandReceipt{Kind: "start", Hash: hash, RaceRunID: command.RaceRunID, RecordedAt: server.updatedAt.Format(time.RFC3339Nano)}
	if err := server.saveReceiptsLocked(); err != nil {
		delete(server.receipts, command.CommandID)
		server.state = StateDegraded
		server.lastError = err.Error()
		server.active = nil
		server.startedAt = time.Time{}
		server.mu.Unlock()
		_ = session.Stop("race_aborted")
		_ = archiveFailedRunDirectory(server.config.StorageRoot, command.RaceRunID)
		writeError(writer, http.StatusInternalServerError, "receipt_persistence_failed", err.Error())
		return
	}
	status := server.statusLocked()
	server.mu.Unlock()
	writeJSON(writer, http.StatusOK, status)
}

func (server *Server) serveStop(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "POST is required")
		return
	}
	var command StopRequest
	if err := decodeStrictJSON(request, &command); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := validateStopRequest(command, server.config.MaximumTailMS); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_stop", err.Error())
		return
	}
	hash := requestHash(command)
	server.mu.Lock()
	if handled := server.handleDuplicateLocked(writer, command.CommandID, "stop", hash, command.RaceRunID); handled {
		server.mu.Unlock()
		return
	}
	if server.active == nil || server.active.request.RaceRunID != command.RaceRunID {
		server.mu.Unlock()
		writeError(writer, http.StatusConflict, "race_run_mismatch", "raceRunId does not identify the active recording")
		return
	}
	server.state = StateStopping
	server.updatedAt = time.Now().UTC()
	server.receipts[command.CommandID] = commandReceipt{Kind: "stop", Hash: hash, RaceRunID: command.RaceRunID, RecordedAt: server.updatedAt.Format(time.RFC3339Nano)}
	if err := server.saveReceiptsLocked(); err != nil {
		delete(server.receipts, command.CommandID)
		server.state = StateDegraded
		server.lastError = err.Error()
		server.mu.Unlock()
		writeError(writer, http.StatusInternalServerError, "receipt_persistence_failed", err.Error())
		return
	}
	status := server.statusLocked()
	active := server.active
	server.mu.Unlock()
	go server.finishAfterTail(active, command)
	writeJSON(writer, http.StatusAccepted, status)
}

func (server *Server) finishAfterTail(session *runSession, command StopRequest) {
	timer := time.NewTimer(time.Duration(command.TailMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-server.ctx.Done():
	case <-timer.C:
	}
	err := session.Stop(command.Reason)
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.active != session {
		return
	}
	server.active = nil
	server.startedAt = time.Time{}
	server.updatedAt = time.Now().UTC()
	if err != nil {
		server.state = StateDegraded
		server.lastError = fmt.Sprintf("finalize recording: %v", err)
		return
	}
	server.state = StateReady
	server.lastError = ""
}

func (server *Server) failStart(message string) {
	server.mu.Lock()
	server.starting = false
	server.state = StateDegraded
	server.lastError = message
	server.updatedAt = time.Now().UTC()
	server.mu.Unlock()
}

func (server *Server) sourceFailed(err error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.active == nil {
		return
	}
	server.state = StateDegraded
	server.lastError = err.Error()
	server.updatedAt = time.Now().UTC()
}

func (server *Server) handleDuplicateLocked(writer http.ResponseWriter, commandID string, kind string, hash string, raceRunID string) bool {
	receipt, exists := server.receipts[commandID]
	if !exists {
		return false
	}
	if receipt.Kind != kind || receipt.Hash != hash || receipt.RaceRunID != raceRunID {
		writeError(writer, http.StatusConflict, "command_conflict", "commandId was already used with a different request")
		return true
	}
	if kind == "start" && (server.active == nil || server.active.request.RaceRunID != raceRunID) {
		writeError(writer, http.StatusConflict, "start_outcome_not_active", "the persisted start command no longer identifies an active recording")
		return true
	}
	writeJSON(writer, http.StatusOK, server.statusLocked())
	return true
}

func (server *Server) writeStatus(writer http.ResponseWriter, code int) {
	server.mu.Lock()
	status := server.statusLocked()
	server.mu.Unlock()
	writeJSON(writer, code, status)
}

func (server *Server) statusLocked() Status {
	free, err := availableBytes(server.config.StorageRoot)
	if err != nil {
		free = 0
	}
	status := Status{
		Type: "race_recorder_status", State: server.state, StorageRoot: server.config.StorageRoot,
		FreeBytes: free, UpdatedAtUnixMS: server.updatedAt.UnixMilli(), LastError: server.lastError,
	}
	if server.active != nil {
		status.ActiveRaceRunID = server.active.request.RaceRunID
		status.ActiveMode = server.active.request.Mode
		status.StartedAtUnixMS = server.startedAt.UnixMilli()
	}
	return status
}

func (server *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		value := request.Header.Get("Authorization")
		expected := "Bearer " + server.config.Token
		if len(value) != len(expected) || subtle.ConstantTimeCompare([]byte(value), []byte(expected)) != 1 {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "valid Bearer token is required")
			return
		}
		next(writer, request)
	}
}

func (server *Server) loadReceipts() error {
	payload, err := os.ReadFile(server.receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Recorder command receipts: %w", err)
	}
	var stored receiptFile
	if err := json.Unmarshal(payload, &stored); err != nil || stored.SchemaVersion != SchemaVersion || stored.Commands == nil {
		return fmt.Errorf("Recorder command receipt file is invalid")
	}
	server.receipts = stored.Commands
	return nil
}

func (server *Server) saveReceiptsLocked() error {
	payload, err := json.MarshalIndent(receiptFile{SchemaVersion: SchemaVersion, Commands: server.receipts}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Recorder command receipts: %w", err)
	}
	temporary := server.receiptPath + ".tmp"
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write Recorder command receipts: %w", err)
	}
	if err := os.Rename(temporary, server.receiptPath); err != nil {
		return fmt.Errorf("publish Recorder command receipts: %w", err)
	}
	return nil
}

func archiveFailedRunDirectory(storageRoot string, raceRunID string) error {
	source := filepath.Join(storageRoot, raceRunID)
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	failedRoot := filepath.Join(storageRoot, "_failed")
	if err := os.MkdirAll(failedRoot, 0o755); err != nil {
		return err
	}
	target := filepath.Join(failedRoot, fmt.Sprintf("%s-%s", raceRunID, time.Now().UTC().Format("20060102T150405.000000000Z")))
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return nil
}

func decodeStrictJSON(request *http.Request, target any) error {
	if request.Header.Get("Content-Type") != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maximumRequestBytes+1))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if len(payload) > maximumRequestBytes {
		return fmt.Errorf("request exceeds %d bytes", maximumRequestBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request contains a trailing JSON value")
	}
	return nil
}

func requestHash(value any) string {
	payload, _ := json.Marshal(value)
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func writeJSON(writer http.ResponseWriter, code int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, code int, errorCode string, message string) {
	writeJSON(writer, code, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{Error: errorCode, Message: message})
}
