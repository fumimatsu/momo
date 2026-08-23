package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type replayManifest struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Sources       []replayManifestSource `json:"sources"`
}

type replayManifestSource struct {
	SourceID          string  `json:"sourceId"`
	InputPath         string  `json:"inputPath"`
	FPS               int     `json:"fps"`
	StartOffsetMS     int64   `json:"startOffsetMs,omitempty"`
	TelemetryPath     string  `json:"telemetryPath,omitempty"`
	CaptureReplayRate float64 `json:"captureReplayRate,omitempty"`
}

type videoAsset struct {
	inputPath     string
	accessUnits   []h264AccessUnit
	frameDuration time.Duration
}

func (asset *videoAsset) duration() time.Duration {
	return time.Duration(len(asset.accessUnits)) * asset.frameDuration
}

type timedReplayMessage struct {
	offset        time.Duration
	data          string
	alternateData string
}

type captureReplayRecord struct {
	Kind         string  `json:"kind"`
	RunElapsedMS float64 `json:"run_elapsed_ms"`
	RawMessage   string  `json:"raw_message"`
}

func loadReplayManifest(path string) (map[string]playbackProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read replay manifest: %w", err)
	}
	var manifest replayManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode replay manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("replay manifest schemaVersion=%d want=1", manifest.SchemaVersion)
	}
	if len(manifest.Sources) == 0 || len(manifest.Sources) > maximumVirtualSources {
		return nil, fmt.Errorf("replay manifest must contain 1 to %d sources", maximumVirtualSources)
	}

	baseDirectory := filepath.Dir(path)
	assets := make(map[string]*videoAsset)
	telemetryLogs := make(map[string][]timedReplayMessage)
	profiles := make(map[string]playbackProfile, len(manifest.Sources))
	for _, source := range manifest.Sources {
		parsedSourceIDs, err := parseSourceIDs(source.SourceID)
		if err != nil {
			return nil, fmt.Errorf("sourceId %q: %w", source.SourceID, err)
		}
		if len(parsedSourceIDs) != 1 || parsedSourceIDs[0] != source.SourceID {
			return nil, fmt.Errorf("invalid sourceId %q", source.SourceID)
		}
		if _, exists := profiles[source.SourceID]; exists {
			return nil, fmt.Errorf("duplicate sourceId in replay manifest: %s", source.SourceID)
		}
		if source.FPS < 1 || source.FPS > 120 {
			return nil, fmt.Errorf("source %q fps must be between 1 and 120", source.SourceID)
		}
		if source.StartOffsetMS < 0 {
			return nil, fmt.Errorf("source %q startOffsetMs must not be negative", source.SourceID)
		}
		rate := source.CaptureReplayRate
		if rate == 0 {
			rate = 1
		}
		if rate < 0.25 || rate > 4 {
			return nil, fmt.Errorf("source %q captureReplayRate must be between 0.25 and 4", source.SourceID)
		}

		inputPath := resolveReplayPath(baseDirectory, source.InputPath)
		assetKey := fmt.Sprintf("%s|%d", strings.ToLower(inputPath), source.FPS)
		asset := assets[assetKey]
		if asset == nil {
			loaded, err := loadVideoAsset(inputPath, source.FPS)
			if err != nil {
				return nil, fmt.Errorf("source %q: %w", source.SourceID, err)
			}
			asset = loaded
			assets[assetKey] = asset
		}

		startIndex := keyframeAtOrAfter(asset.accessUnits, int(time.Duration(source.StartOffsetMS)*time.Millisecond/asset.frameDuration))
		profile := playbackProfile{
			asset:             asset,
			startIndex:        startIndex,
			captureReplayRate: rate,
		}
		if source.TelemetryPath != "" {
			telemetryPath := resolveReplayPath(baseDirectory, source.TelemetryPath)
			events, exists := telemetryLogs[telemetryPath]
			if !exists {
				loaded, err := loadCaptureTelemetry(telemetryPath)
				if err != nil {
					return nil, fmt.Errorf("source %q: %w", source.SourceID, err)
				}
				events = loaded
				telemetryLogs[telemetryPath] = events
			}
			startOffset := time.Duration(startIndex) * asset.frameDuration
			schedule := buildLoopSchedule(events, rate, startOffset, asset.duration())
			profile.telemetry, err = normalizeReplayTelemetrySchedule(source.SourceID, schedule)
			if err != nil {
				return nil, fmt.Errorf("source %q: %w", source.SourceID, err)
			}
			profile.telemetryPath = telemetryPath
		}
		profiles[source.SourceID] = profile
	}
	return profiles, nil
}

func normalizeReplayTelemetrySchedule(sourceID string, schedule []timedReplayMessage) ([]timedReplayMessage, error) {
	result := make([]timedReplayMessage, len(schedule))
	bootIDs := [2]string{replayBootID(sourceID, 0), replayBootID(sourceID, 1)}
	for index, message := range schedule {
		body := strings.TrimSpace(strings.TrimPrefix(message.data, "TEL:"))
		if body == message.data {
			return nil, fmt.Errorf("telemetry record %d does not start with TEL:", index)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return nil, fmt.Errorf("decode telemetry record %d: %w", index, err)
		}
		messageData := [2]string{}
		for variant := range bootIDs {
			payload["boot"] = bootIDs[variant]
			payload["seq"] = index + 1
			payload["t_us"] = message.offset.Microseconds()
			encoded, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("encode telemetry record %d: %w", index, err)
			}
			messageData[variant] = "TEL:" + string(encoded)
		}
		message.data = messageData[0]
		message.alternateData = messageData[1]
		result[index] = message
	}
	return result, nil
}

func replayBootID(sourceID string, variant int) string {
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "%s:%d", sourceID, variant)
	return fmt.Sprintf("%08x", hash.Sum32())
}

func resolveReplayPath(baseDirectory string, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(baseDirectory, value))
}

func loadVideoAsset(path string, fps int) (*videoAsset, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("inputPath is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read H.264 input %q: %w", path, err)
	}
	units, err := splitH264AccessUnits(data)
	if err != nil {
		return nil, fmt.Errorf("parse H.264 input %q: %w", path, err)
	}
	return &videoAsset{inputPath: path, accessUnits: units, frameDuration: time.Second / time.Duration(fps)}, nil
}

func loadCaptureTelemetry(path string) ([]timedReplayMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open capture log %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	events := make([]timedReplayMessage, 0)
	for scanner.Scan() {
		var record captureReplayRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || record.Kind != "telemetry" || record.RawMessage == "" || record.RunElapsedMS < 0 {
			continue
		}
		events = append(events, timedReplayMessage{
			offset: time.Duration(record.RunElapsedMS * float64(time.Millisecond)),
			data:   record.RawMessage,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan capture log %q: %w", path, err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("capture log %q has no telemetry records", path)
	}
	sort.SliceStable(events, func(left, right int) bool { return events[left].offset < events[right].offset })
	return events, nil
}

func buildLoopSchedule(events []timedReplayMessage, replayRate float64, startOffset time.Duration, loopDuration time.Duration) []timedReplayMessage {
	if len(events) == 0 || replayRate <= 0 || loopDuration <= 0 {
		return nil
	}
	schedule := make([]timedReplayMessage, 0, len(events))
	for _, event := range events {
		transformedOffset := time.Duration(float64(event.offset) / replayRate)
		if transformedOffset >= loopDuration {
			continue
		}
		relative := transformedOffset - startOffset
		for relative < 0 {
			relative += loopDuration
		}
		if relative >= loopDuration {
			relative %= loopDuration
		}
		schedule = append(schedule, timedReplayMessage{offset: relative, data: event.data})
	}
	sort.SliceStable(schedule, func(left, right int) bool { return schedule[left].offset < schedule[right].offset })
	return schedule
}

func keyframeAtOrAfter(units []h264AccessUnit, requested int) int {
	if len(units) == 0 {
		return 0
	}
	if requested < 0 {
		requested = 0
	}
	if requested >= len(units) {
		requested %= len(units)
	}
	return nextKeyframe(units, requested)
}
