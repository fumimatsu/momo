package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Observer reports are untrusted diagnostics, never race/control authority.
type observerScreenStats struct {
	Version               int      `json:"version"`
	Sequence              uint64   `json:"sequence"`
	ClientSampledAtUnixMS int64    `json:"clientSampledAtUnixMs"`
	WindowMS              float64  `json:"windowMs"`
	Visibility            string   `json:"visibility"`
	VisibilityChanged     *bool    `json:"visibilityChanged"`
	PeerState             string   `json:"peerState"`
	VideoWidth            *uint32  `json:"videoWidth"`
	VideoHeight           *uint32  `json:"videoHeight"`
	RenderCallbackFPS     *float64 `json:"renderCallbackFps"`
	VideoFrameAgeMS       *float64 `json:"videoFrameAgeMs"`
}

type observerRelayVideoStats struct {
	IngressAccessUnitFPS    float64 `json:"ingressAccessUnitFps"`
	RelayWriteAccessUnitFPS float64 `json:"relayWriteAccessUnitFps"`
}

func parseObserverScreenStats(raw string) (*observerScreenStats, error) {
	if len(raw) == 0 || len(raw) > 4096 {
		return nil, fmt.Errorf("invalid payload size")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var sample observerScreenStats
	if err := decoder.Decode(&sample); err != nil {
		return nil, fmt.Errorf("invalid schema")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	if sample.Version != 2 || sample.Sequence == 0 || sample.Sequence > 9007199254740991 ||
		sample.ClientSampledAtUnixMS <= 0 || sample.ClientSampledAtUnixMS > 9007199254740991 ||
		!boundedObserverNumber(sample.WindowMS, 1, 86400000) ||
		(sample.Visibility != "visible" && sample.Visibility != "hidden") || sample.VisibilityChanged == nil ||
		sample.VideoWidth == nil || sample.VideoHeight == nil || *sample.VideoWidth > 16384 || *sample.VideoHeight > 16384 {
		return nil, fmt.Errorf("missing or invalid sample fields")
	}
	switch sample.PeerState {
	case "new", "connecting", "connected", "disconnected", "failed", "closed", "unknown":
	default:
		return nil, fmt.Errorf("invalid peer state")
	}
	if !optionalObserverNumber(sample.RenderCallbackFPS, 1000) || !optionalObserverNumber(sample.VideoFrameAgeMS, 86400000) {
		return nil, fmt.Errorf("invalid video stats")
	}
	return &sample, nil
}

func boundedObserverNumber(value, min, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= min && value <= max
}

func optionalObserverNumber(value *float64, max float64) bool {
	return value == nil || boundedObserverNumber(*value, 0, max)
}

// Called only by this viewer's WebSocket read loop. No control/data-channel path.
func (r *relay) recordObserverScreenStats(client *viewer, raw string, now time.Time) error {
	if r.recorder == nil || client.role != "observer" || client.clientKind != "web-observer" ||
		r.name == "" || r.raceCarID == "" {
		return fmt.Errorf("screen logging unavailable for this connection")
	}
	if !client.lastScreenStatsAt.IsZero() && now.Sub(client.lastScreenStatsAt) < 500*time.Millisecond {
		return nil // Bound diagnostics without disrupting signaling or producing a log flood.
	}
	client.lastScreenStatsAt = now
	sample, err := parseObserverScreenStats(raw)
	if err != nil {
		return err
	}
	if sample.Sequence <= client.lastScreenStatsSequence {
		return fmt.Errorf("non-increasing screen stats sequence")
	}
	client.lastScreenStatsSequence = sample.Sequence
	ingress, writes := r.frameRate.snapshot(now)
	nowUTC := now.UTC()
	elapsed := now.Sub(r.recorder.startedAt).Microseconds()
	record := telemetryLogRecord{
		Type: "observer_stats", SchemaVersion: telemetryLogSchemaVersion,
		RelaySessionID: r.recorder.sessionID, RelayReceivedAt: &nowUTC, RelayElapsedUs: &elapsed,
		SourceID: r.name, CarID: r.raceCarID, ViewerID: client.id, ObserverStats: sample,
		RelayVideo: &observerRelayVideoStats{IngressAccessUnitFPS: ingress, RelayWriteAccessUnitFPS: writes},
	}
	r.recorder.appendRaceContext(&record, r.recorder.currentRaceContext())
	if r.recorder.enqueue(record) {
		r.recorder.observerStatsRecords.Add(1)
	}
	return nil
}
