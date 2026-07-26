package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestTelemetryRecorderWritesInterleavedRelayTimeline(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}

	recorder.RecordRaceState(`{"type":"race_state","version":2,"raceId":"race-test","raceRunId":"rr_123","sequence":0,"phase":"countdown","flag":"none"}`, telemetryRaceContext{
		RaceID:    "race-test",
		RaceRunID: "rr_123",
		Phase:     "countdown",
		Flag:      "none",
		Sequence:  0,
		Present:   true,
	})
	recorder.RecordTelemetry("11.3", "CP-1", 7, `TEL:{"v":1,"src":"imu0","seq":4}`)
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records := readTelemetryLogRecords(t, recorder.Path())
	if len(records) != 4 {
		t.Fatalf("record count = %d, want 4", len(records))
	}
	if records[0].Type != "relay_session" || records[0].RelayStartedAt == nil {
		t.Fatalf("header = %#v, want relay_session with start time", records[0])
	}
	if records[1].Type != "race_state" || records[1].RaceRunID != "rr_123" || records[1].RaceSequence == nil || *records[1].RaceSequence != 0 {
		t.Fatalf("race record = %#v, want race run rr_123 sequence 0", records[1])
	}
	if records[2].Type != "telemetry" || records[2].SourceID != "11.3" || records[2].CarID != "CP-1" || records[2].UpstreamGen != 7 {
		t.Fatalf("telemetry identity = %#v", records[2])
	}
	if records[2].Raw != `TEL:{"v":1,"src":"imu0","seq":4}` || records[2].RelayReceivedAt == nil || records[2].RelayElapsedUs == nil {
		t.Fatalf("telemetry payload/timestamp = %#v", records[2])
	}
	if records[2].RaceRunID != "rr_123" || records[2].RacePhase != "countdown" {
		t.Fatalf("telemetry race context = %#v", records[2])
	}
	if records[3].Type != "relay_session_end" || records[3].Stats == nil || records[3].Stats.TelemetryRecords != 1 || records[3].Stats.RaceStateRecords != 1 {
		t.Fatalf("footer = %#v, want final stats", records[3])
	}
}

func TestRelayRecordsOnlyTELTextMessages(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	source := newStatusTestRelay("11.3", "CP-1")
	source.recorder = recorder

	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":1,"seq":1}`), IsString: true}, 2)
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte("PONG:1"), IsString: true}, 2)
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte{0x01, 0x02}, IsString: false}, 2)
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var telemetry []telemetryLogRecord
	for _, record := range readTelemetryLogRecords(t, recorder.Path()) {
		if record.Type == "telemetry" {
			telemetry = append(telemetry, record)
		}
	}
	if len(telemetry) != 1 {
		t.Fatalf("telemetry records = %#v, want exactly one TEL record", telemetry)
	}
	if telemetry[0].Raw != `TEL:{"v":1,"seq":1}` || telemetry[0].SourceID != "11.3" || telemetry[0].CarID != "CP-1" {
		t.Fatalf("TEL record = %#v", telemetry[0])
	}
}

func readTelemetryLogRecords(t *testing.T, path string) []telemetryLogRecord {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("open telemetry log: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var records []telemetryLogRecord
	for scanner.Scan() {
		var record telemetryLogRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode telemetry log line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read telemetry log: %v", err)
	}
	return records
}
