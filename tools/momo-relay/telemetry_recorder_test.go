package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	recorder.RecordDriveInput("11.3", "CP-1", 9, driveInputLogSample{
		SteeringPWM: 1420, Steering: -0.16, RequestedPowerPWM: 1800, EffectivePowerPWM: 1700,
		Throttle: 1, EffectiveThrottle: 2.0 / 3.0, Gear: 3, DriveEnabled: true,
		HP: 80, Fuel: 45, Boost: 12, Position: 2, FieldSize: 4, FuelRatePerSecond: 0.5,
		SessionType: "race",
	})
	recorder.RecordVehicleEvent("11.3", "CP-1", vehicleImpactEvent{
		Type:              "vehicle_event",
		Version:           1,
		EventID:           "impact-1",
		RaceRunID:         "rr_123",
		CarID:             "CP-1",
		ImpactClass:       "strong",
		MagnitudeMPS2:     18.5,
		JerkMPS3:          92,
		Axis:              [3]float64{1, -2, 3},
		DamageApplied:     false,
		SuppressionReason: "boost_active",
		HPBefore:          82,
		HPAfter:           82,
		ServerTimeMS:      123456,
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records := readTelemetryLogRecords(t, recorder.Path())
	if len(records) != 6 {
		t.Fatalf("record count = %d, want 6", len(records))
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
	if records[3].Type != "drive_input" || records[3].DriveInput == nil || records[3].DriveInput.SteeringPWM != 1420 || records[3].DriveInput.EffectivePowerPWM != 1700 || records[3].PilotID != 9 {
		t.Fatalf("drive input = %#v", records[3])
	}
	if records[4].Type != "vehicle_event" || records[4].VehicleEvent == nil || records[4].VehicleEvent.EventID != "impact-1" || records[4].VehicleEvent.SuppressionReason != "boost_active" {
		t.Fatalf("vehicle event = %#v", records[4])
	}
	if records[4].SourceID != "11.3" || records[4].CarID != "CP-1" || records[4].RaceRunID != "rr_123" {
		t.Fatalf("vehicle event context = %#v", records[4])
	}
	if records[5].Type != "relay_session_end" || records[5].Stats == nil || records[5].Stats.TelemetryRecords != 1 || records[5].Stats.RaceStateRecords != 1 || records[5].Stats.DriveInputRecords != 1 || records[5].Stats.VehicleEventRecords != 1 {
		t.Fatalf("footer = %#v, want final stats", records[5])
	}
}

func TestParseAndNormalizeDriveCommand(t *testing.T) {
	steering, power, ok := parseDriveCommand("S:1250,T:1800\n")
	if !ok || steering != 1250 || power != 1800 {
		t.Fatalf("parsed drive command = steering %d power %d ok=%t", steering, power, ok)
	}
	throttle, brake := normalizeDrivePower(power, 3)
	if math.Abs(throttle-1) > 0.000001 || brake != 0 {
		t.Fatalf("normalized throttle = %.3f brake = %.3f", throttle, brake)
	}
	throttle, brake = normalizeDrivePower(1200, 3)
	if throttle != 0 || math.Abs(brake-1) > 0.000001 {
		t.Fatalf("normalized brake = throttle %.3f brake %.3f", throttle, brake)
	}
}

func TestRelayRecordsAcceptedVehicleEventOnce(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	source := newStatusTestRelay("11.4", "CP-2")
	source.recorder = recorder
	source.vehicleEvents = newVehicleEventStore()
	event := vehicleImpactEvent{Type: "vehicle_event", Version: 1, EventID: "impact-2", RaceRunID: "rr_456", CarID: "CP-2"}

	source.publishVehicleEvent(event)
	source.publishVehicleEvent(event)
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var events []telemetryLogRecord
	for _, record := range readTelemetryLogRecords(t, recorder.Path()) {
		if record.Type == "vehicle_event" {
			events = append(events, record)
		}
	}
	if len(events) != 1 || events[0].VehicleEvent == nil || events[0].VehicleEvent.EventID != "impact-2" {
		t.Fatalf("vehicle event records = %#v, want one accepted event", events)
	}
}

func TestRemoveExpiredTelemetryLogsOnlyDeletesMatchingOldFiles(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	oldTelemetry := filepath.Join(directory, "telemetry-old.ndjson")
	recentTelemetry := filepath.Join(directory, "telemetry-recent.ndjson")
	oldOther := filepath.Join(directory, "keep-old.txt")
	for _, path := range []string{oldTelemetry, recentTelemetry, oldOther} {
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}
	oldTime := now.Add(-25 * time.Hour)
	for _, path := range []string{oldTelemetry, oldOther} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", path, err)
		}
	}

	if err := removeExpiredTelemetryLogs(directory, 24*time.Hour, now); err != nil {
		t.Fatalf("removeExpiredTelemetryLogs() error = %v", err)
	}
	if _, err := os.Stat(oldTelemetry); !os.IsNotExist(err) {
		t.Fatalf("old telemetry stat error = %v, want not exist", err)
	}
	for _, path := range []string{recentTelemetry, oldOther} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved file %q stat error = %v", path, err)
		}
	}
}

func TestRelayRecordsOnlyTELTextMessages(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	source := newStatusTestRelay("11.3", "CP-1")
	source.recorder = recorder

	source.driveLoggingEnabled.Store(true)
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

func TestRelayDriveStateGatesTelemetryAndRejectsObservers(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 16)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	source := newStatusTestRelay("11.3", "CP-1")
	source.viewers = make(map[uint64]*viewer)
	source.recorder = recorder
	pilot := &viewer{id: 7, role: "pilot"}
	source.addViewer(pilot)
	if !source.reservePilot(pilot.id) {
		t.Fatal("reservePilot() returned false")
	}

	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":1,"seq":1}`), IsString: true}, 3)
	source.handleDriveState(pilot, webrtc.DataChannelMessage{Data: []byte("DRIVE:1"), IsString: true})
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":1,"seq":2}`), IsString: true}, 3)
	source.handleDriveState(&viewer{id: 8, role: "observer"}, webrtc.DataChannelMessage{Data: []byte("DRIVE:0"), IsString: true})
	if !source.driveLoggingEnabled.Load() {
		t.Fatal("observer must not disable pilot drive state")
	}
	source.handleDriveState(pilot, webrtc.DataChannelMessage{Data: []byte("DRIVE:0"), IsString: true})
	source.handleUpstreamTelemetry(webrtc.DataChannelMessage{Data: []byte(`TEL:{"v":1,"seq":3}`), IsString: true}, 3)
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var telemetry []telemetryLogRecord
	var driveStates []telemetryLogRecord
	for _, record := range readTelemetryLogRecords(t, recorder.Path()) {
		switch record.Type {
		case "telemetry":
			telemetry = append(telemetry, record)
		case "drive_state":
			driveStates = append(driveStates, record)
		}
	}
	if len(telemetry) != 1 || telemetry[0].Raw != `TEL:{"v":1,"seq":2}` {
		t.Fatalf("gated telemetry = %#v, want only seq 2", telemetry)
	}
	if len(driveStates) != 2 || driveStates[0].DriveEnabled == nil || !*driveStates[0].DriveEnabled || driveStates[1].DriveEnabled == nil || *driveStates[1].DriveEnabled {
		t.Fatalf("drive states = %#v, want on then off", driveStates)
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
