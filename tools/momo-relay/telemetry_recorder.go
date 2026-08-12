package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	telemetryLogSchemaVersion    = 1
	defaultTelemetryLogQueue     = 8192
	telemetryLogFlushInterval    = time.Second
	defaultTelemetryLogRetention = 24 * time.Hour
)

// telemetryRaceContext is the latest Race Control state observed by the Relay.
// It tags telemetry without making the logging lifecycle depend on race timing.
type telemetryRaceContext struct {
	RaceID    string
	RaceRunID string
	Phase     string
	Flag      string
	Sequence  uint64
	Present   bool
}

type telemetryRecorderStats struct {
	TelemetryRecords    uint64 `json:"telemetryRecords"`
	RaceStateRecords    uint64 `json:"raceStateRecords"`
	DriveStateRecords   uint64 `json:"driveStateRecords"`
	VehicleEventRecords uint64 `json:"vehicleEventRecords"`
	QueueDrops          uint64 `json:"queueDrops"`
	WriteErrors         uint64 `json:"writeErrors"`
}

type telemetryLogRecord struct {
	Type            string                  `json:"type"`
	SchemaVersion   int                     `json:"schemaVersion"`
	RelaySessionID  string                  `json:"relaySessionId"`
	RelayStartedAt  *time.Time              `json:"relayStartedAt,omitempty"`
	RelayReceivedAt *time.Time              `json:"relayReceivedAt,omitempty"`
	RelayElapsedUs  *int64                  `json:"relayElapsedUs,omitempty"`
	SourceID        string                  `json:"sourceId,omitempty"`
	CarID           string                  `json:"carId,omitempty"`
	UpstreamGen     uint64                  `json:"upstreamGeneration,omitempty"`
	Raw             string                  `json:"raw,omitempty"`
	RaceID          string                  `json:"raceId,omitempty"`
	RaceRunID       string                  `json:"raceRunId,omitempty"`
	RacePhase       string                  `json:"racePhase,omitempty"`
	RaceFlag        string                  `json:"raceFlag,omitempty"`
	RaceSequence    *uint64                 `json:"raceSequence,omitempty"`
	DriveEnabled    *bool                   `json:"driveEnabled,omitempty"`
	DriveReason     string                  `json:"driveReason,omitempty"`
	PilotID         uint64                  `json:"pilotId,omitempty"`
	VehicleEvent    *vehicleImpactEvent     `json:"vehicleEvent,omitempty"`
	Stats           *telemetryRecorderStats `json:"stats,omitempty"`
}

// telemetryRecorder writes Relay-local, interleaved records for every source.
// Producers only enqueue a compact record and never wait for filesystem I/O.
type telemetryRecorder struct {
	startedAt time.Time
	sessionID string
	path      string
	queue     chan telemetryLogRecord
	stop      chan struct{}
	done      chan struct{}

	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeErr  error

	raceContext atomic.Value // telemetryRaceContext

	telemetryRecords    atomic.Uint64
	raceStateRecords    atomic.Uint64
	driveStateRecords   atomic.Uint64
	vehicleEventRecords atomic.Uint64
	queueDrops          atomic.Uint64
	writeErrors         atomic.Uint64

	errorMu      sync.Mutex
	lastWriteErr error
}

func newTelemetryRecorder(directory string, retention time.Duration) (*telemetryRecorder, error) {
	if err := removeExpiredTelemetryLogs(directory, retention, time.Now()); err != nil {
		return nil, err
	}
	return newTelemetryRecorderWithQueue(directory, defaultTelemetryLogQueue)
}

func removeExpiredTelemetryLogs(directory string, retention time.Duration, now time.Time) error {
	if retention <= 0 {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read telemetry log directory: %w", err)
	}
	cutoff := now.Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matched, err := filepath.Match("telemetry-*.ndjson", entry.Name())
		if err != nil || !matched {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat telemetry log %q: %w", entry.Name(), err)
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove expired telemetry log %q: %w", path, err)
		}
	}
	return nil
}

func newTelemetryRecorderWithQueue(directory string, queueCapacity int) (*telemetryRecorder, error) {
	if queueCapacity <= 0 {
		return nil, fmt.Errorf("telemetry log queue capacity must be positive")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create telemetry log directory: %w", err)
	}

	startedAt := time.Now()
	sessionID, err := newRelaySessionID(startedAt)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "telemetry-"+sessionID+".ndjson")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create telemetry log %q: %w", path, err)
	}

	recorder := &telemetryRecorder{
		startedAt: startedAt,
		sessionID: sessionID,
		path:      path,
		queue:     make(chan telemetryLogRecord, queueCapacity),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	recorder.raceContext.Store(telemetryRaceContext{})

	writer := bufio.NewWriterSize(file, 64*1024)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	startedAtUTC := startedAt.UTC()
	if err := encoder.Encode(telemetryLogRecord{
		Type:           "relay_session",
		SchemaVersion:  telemetryLogSchemaVersion,
		RelaySessionID: sessionID,
		RelayStartedAt: &startedAtUTC,
	}); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write telemetry log header: %w", err)
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("flush telemetry log header: %w", err)
	}

	go recorder.run(file, writer, encoder)
	return recorder, nil
}

func newRelaySessionID(startedAt time.Time) (string, error) {
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate relay session ID: %w", err)
	}
	return startedAt.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(randomBytes), nil
}

func (r *telemetryRecorder) Path() string {
	return r.path
}

func (r *telemetryRecorder) RecordTelemetry(sourceID string, carID string, upstreamGeneration uint64, raw string) {
	if r == nil {
		return
	}
	now := time.Now()
	nowUTC := now.UTC()
	elapsedUs := time.Since(r.startedAt).Microseconds()
	record := telemetryLogRecord{
		Type:            "telemetry",
		SchemaVersion:   telemetryLogSchemaVersion,
		RelaySessionID:  r.sessionID,
		RelayReceivedAt: &nowUTC,
		RelayElapsedUs:  &elapsedUs,
		SourceID:        sourceID,
		CarID:           carID,
		UpstreamGen:     upstreamGeneration,
		Raw:             raw,
	}
	r.appendRaceContext(&record, r.currentRaceContext())
	if r.enqueue(record) {
		r.telemetryRecords.Add(1)
	}
}

func (r *telemetryRecorder) RecordRaceState(raw string, context telemetryRaceContext) {
	if r == nil {
		return
	}
	r.raceContext.Store(context)
	now := time.Now()
	nowUTC := now.UTC()
	elapsedUs := time.Since(r.startedAt).Microseconds()
	record := telemetryLogRecord{
		Type:            "race_state",
		SchemaVersion:   telemetryLogSchemaVersion,
		RelaySessionID:  r.sessionID,
		RelayReceivedAt: &nowUTC,
		RelayElapsedUs:  &elapsedUs,
		Raw:             raw,
	}
	r.appendRaceContext(&record, context)
	if r.enqueue(record) {
		r.raceStateRecords.Add(1)
	}
}

func (r *telemetryRecorder) RecordDriveState(sourceID string, carID string, pilotID uint64, enabled bool, reason string) {
	if r == nil {
		return
	}
	now := time.Now()
	nowUTC := now.UTC()
	elapsedUs := time.Since(r.startedAt).Microseconds()
	record := telemetryLogRecord{
		Type:            "drive_state",
		SchemaVersion:   telemetryLogSchemaVersion,
		RelaySessionID:  r.sessionID,
		RelayReceivedAt: &nowUTC,
		RelayElapsedUs:  &elapsedUs,
		SourceID:        sourceID,
		CarID:           carID,
		DriveEnabled:    boolPointer(enabled),
		DriveReason:     reason,
		PilotID:         pilotID,
	}
	r.appendRaceContext(&record, r.currentRaceContext())
	if r.enqueue(record) {
		r.driveStateRecords.Add(1)
	}
}

func (r *telemetryRecorder) RecordVehicleEvent(sourceID string, carID string, event vehicleImpactEvent) {
	if r == nil {
		return
	}
	now := time.Now()
	nowUTC := now.UTC()
	elapsedUs := time.Since(r.startedAt).Microseconds()
	eventCopy := event
	record := telemetryLogRecord{
		Type:            "vehicle_event",
		SchemaVersion:   telemetryLogSchemaVersion,
		RelaySessionID:  r.sessionID,
		RelayReceivedAt: &nowUTC,
		RelayElapsedUs:  &elapsedUs,
		SourceID:        sourceID,
		CarID:           carID,
		VehicleEvent:    &eventCopy,
	}
	r.appendRaceContext(&record, r.currentRaceContext())
	if r.enqueue(record) {
		r.vehicleEventRecords.Add(1)
	}
}

func (r *telemetryRecorder) currentRaceContext() telemetryRaceContext {
	return r.raceContext.Load().(telemetryRaceContext)
}

func (r *telemetryRecorder) appendRaceContext(record *telemetryLogRecord, context telemetryRaceContext) {
	if !context.Present {
		return
	}
	record.RaceID = context.RaceID
	record.RaceRunID = context.RaceRunID
	record.RacePhase = context.Phase
	record.RaceFlag = context.Flag
	sequence := context.Sequence
	record.RaceSequence = &sequence
}

func (r *telemetryRecorder) enqueue(record telemetryLogRecord) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false
	}
	select {
	case r.queue <- record:
		return true
	default:
		r.queueDrops.Add(1)
		return false
	}
}

func (r *telemetryRecorder) Stats() telemetryRecorderStats {
	return telemetryRecorderStats{
		TelemetryRecords:    r.telemetryRecords.Load(),
		RaceStateRecords:    r.raceStateRecords.Load(),
		DriveStateRecords:   r.driveStateRecords.Load(),
		VehicleEventRecords: r.vehicleEventRecords.Load(),
		QueueDrops:          r.queueDrops.Load(),
		WriteErrors:         r.writeErrors.Load(),
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func (r *telemetryRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.stop)
		r.mu.Unlock()
		<-r.done
		r.errorMu.Lock()
		r.closeErr = r.lastWriteErr
		r.errorMu.Unlock()
	})
	return r.closeErr
}

func (r *telemetryRecorder) run(file *os.File, writer *bufio.Writer, encoder *json.Encoder) {
	defer close(r.done)
	ticker := time.NewTicker(telemetryLogFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case record := <-r.queue:
			r.writeRecord(encoder, record)
		case <-ticker.C:
			r.flush(writer)
		case <-r.stop:
			r.drainQueue(encoder)
			endedAt := time.Now().UTC()
			r.writeRecord(encoder, telemetryLogRecord{
				Type:            "relay_session_end",
				SchemaVersion:   telemetryLogSchemaVersion,
				RelaySessionID:  r.sessionID,
				RelayReceivedAt: &endedAt,
				Stats:           telemetryRecorderStatsPointer(r.Stats()),
			})
			r.flush(writer)
			if err := file.Close(); err != nil {
				r.setWriteError(fmt.Errorf("close telemetry log: %w", err))
			}
			return
		}
	}
}

func telemetryRecorderStatsPointer(stats telemetryRecorderStats) *telemetryRecorderStats {
	return &stats
}

func (r *telemetryRecorder) drainQueue(encoder *json.Encoder) {
	for {
		select {
		case record := <-r.queue:
			r.writeRecord(encoder, record)
		default:
			return
		}
	}
}

func (r *telemetryRecorder) writeRecord(encoder *json.Encoder, record telemetryLogRecord) {
	if err := encoder.Encode(record); err != nil {
		r.setWriteError(fmt.Errorf("write telemetry log record: %w", err))
	}
}

func (r *telemetryRecorder) flush(writer *bufio.Writer) {
	if err := writer.Flush(); err != nil {
		r.setWriteError(fmt.Errorf("flush telemetry log: %w", err))
	}
}

func (r *telemetryRecorder) setWriteError(err error) {
	r.writeErrors.Add(1)
	r.errorMu.Lock()
	r.lastWriteErr = err
	r.errorMu.Unlock()
}
