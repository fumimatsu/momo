package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
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
	recorder.RecordCourseMarker("11.3", "CP-1", courseMarkerLogSample{
		EventID: "rr_123:CP-1:course_marker:2:1", Lap: 2, MarkerIndex: intPointer(1),
		MarkerRaceMS: int64Pointer(30000), CurrentSector: 2, SectorCount: 3,
	}, telemetryRaceContext{RaceID: "race-test", RaceRunID: "rr_123", Phase: "countdown", Flag: "none", Sequence: 0, Present: true})
	recorder.RecordTelemetry("11.3", "CP-1", 7, `TEL:{"v":1,"src":"imu0","seq":4}`)
	gapToAheadMS := int64(3200)
	lastMarkerIndex := 1
	routeGateIndex := 4
	routeRaceMS := int64(31200)
	recorder.RecordDriveInput("11.3", "CP-1", 9, driveInputLogSample{
		SteeringPWM: 1420, Steering: -0.16, RequestedPowerPWM: 1800, EffectivePowerPWM: 1700,
		Throttle: 1, EffectiveThrottle: 2.0 / 3.0, Gear: 3, DriveEnabled: true,
		HP: 80, SpeedCap: 0.8, Fuel: 45, Boost: 12, BoostState: "charging",
		BoostChargeEligible: true, BoostChargeMS: 24000, BoostPassiveScale: vehicleBoostPassiveChargeScale, Position: 2, FieldSize: 4,
		RaceGapKnown: true, GapToAheadMS: &gapToAheadMS, OutputLimited: true,
		OutputLimitReasons: []string{"damage_cap"}, Lap: 2, LastMarkerIndex: &lastMarkerIndex,
		RouteGateIndex: &routeGateIndex, RouteRaceMS: &routeRaceMS,
		FuelRatePerSecond: 0.5, FuelRateMultiplier: 1.3, FuelPowerScale: 0.8,
		FuelRoughMultiplier: 1.2, FuelBoostMultiplier: 1, ThrottleVariation: 1.2,
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
	if len(records) != 7 {
		t.Fatalf("record count = %d, want 7", len(records))
	}
	if records[0].Type != "relay_session" || records[0].RelayStartedAt == nil {
		t.Fatalf("header = %#v, want relay_session with start time", records[0])
	}
	if records[1].Type != "race_state" || records[1].RaceRunID != "rr_123" || records[1].RaceSequence == nil || *records[1].RaceSequence != 0 {
		t.Fatalf("race record = %#v, want race run rr_123 sequence 0", records[1])
	}
	if records[2].Type != "course_marker" || records[2].CourseMarker == nil || records[2].CourseMarker.EventID != "rr_123:CP-1:course_marker:2:1" || records[2].CourseMarker.MarkerRaceMS == nil || *records[2].CourseMarker.MarkerRaceMS != 30000 {
		t.Fatalf("course marker = %#v", records[2])
	}
	if records[3].Type != "telemetry" || records[3].SourceID != "11.3" || records[3].TelemetrySource != "imu0" || records[3].CarID != "CP-1" || records[3].UpstreamGen != 7 {
		t.Fatalf("telemetry identity = %#v", records[3])
	}
	if records[3].Raw != `TEL:{"v":1,"src":"imu0","seq":4}` || records[3].RelayReceivedAt == nil || records[3].RelayElapsedUs == nil {
		t.Fatalf("telemetry payload/timestamp = %#v", records[3])
	}
	if records[3].RaceRunID != "rr_123" || records[3].RacePhase != "countdown" {
		t.Fatalf("telemetry race context = %#v", records[3])
	}
	if records[4].Type != "drive_input" || records[4].DriveInput == nil || records[4].DriveInput.SteeringPWM != 1420 || records[4].DriveInput.EffectivePowerPWM != 1700 || records[4].DriveInput.FuelRateMultiplier != 1.3 || records[4].DriveInput.FuelPowerScale != 0.8 || records[4].DriveInput.FuelRoughMultiplier != 1.2 || records[4].DriveInput.FuelBoostMultiplier != 1 || records[4].DriveInput.ThrottleVariation != 1.2 || records[4].DriveInput.BoostState != "charging" || !records[4].DriveInput.BoostChargeEligible || records[4].DriveInput.BoostChargeMS != 24000 || records[4].DriveInput.BoostPassiveScale != vehicleBoostPassiveChargeScale || records[4].DriveInput.GapToAheadMS == nil || *records[4].DriveInput.GapToAheadMS != 3200 || !records[4].DriveInput.OutputLimited || len(records[4].DriveInput.OutputLimitReasons) != 1 || records[4].DriveInput.OutputLimitReasons[0] != "damage_cap" || records[4].DriveInput.Lap != 2 || records[4].DriveInput.LastMarkerIndex == nil || *records[4].DriveInput.LastMarkerIndex != 1 || records[4].DriveInput.RouteGateIndex == nil || *records[4].DriveInput.RouteGateIndex != 4 || records[4].DriveInput.RouteRaceMS == nil || *records[4].DriveInput.RouteRaceMS != 31200 || records[4].PilotID != 9 {
		t.Fatalf("drive input = %#v", records[4])
	}
	if records[5].Type != "vehicle_event" || records[5].VehicleEvent == nil || records[5].VehicleEvent.EventID != "impact-1" || records[5].VehicleEvent.SuppressionReason != "boost_active" {
		t.Fatalf("vehicle event = %#v", records[5])
	}
	if records[5].SourceID != "11.3" || records[5].CarID != "CP-1" || records[5].RaceRunID != "rr_123" {
		t.Fatalf("vehicle event context = %#v", records[5])
	}
	if records[6].Type != "relay_session_end" || records[6].Stats == nil || records[6].Stats.TelemetryRecords != 1 || records[6].Stats.RaceStateRecords != 1 || records[6].Stats.DriveInputRecords != 1 || records[6].Stats.CourseMarkerRecords != 1 || records[6].Stats.VehicleEventRecords != 1 {
		t.Fatalf("footer = %#v, want final stats", records[6])
	}
}

func TestTelemetryPayloadSourceSeparatesVehicleAndESCStream(t *testing.T) {
	if got := telemetryPayloadSource(`TEL:{"v":2,"k":"s","src":"esc0","esc":{"rpm":1200}}`); got != "esc0" {
		t.Fatalf("telemetryPayloadSource() = %q, want esc0", got)
	}
	for _, raw := range []string{
		`TEL:{"v":2,"src":"bad/source"}`,
		`TEL:{invalid}`,
		`PONG:1`,
	} {
		if got := telemetryPayloadSource(raw); got != "" {
			t.Fatalf("telemetryPayloadSource(%q) = %q, want empty", raw, got)
		}
	}
}

func TestTelemetryRecorderWritesBoostRegenProbe(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 8)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	recorder.RecordRaceState(`{"type":"race_state","raceRunId":"rr_regen","phase":"green"}`, telemetryRaceContext{
		RaceRunID: "rr_regen",
		Phase:     "green",
		Present:   true,
	})
	recorder.RecordBoostRegenProbe("11.4", "CP-2", boostRegenLogSample{
		EventID:            "11.4:CP-2:regen_live:boot:8",
		Mode:               "live",
		AlgorithmVersion:   boostRegenAlgorithmVersion,
		Trigger:            "partial_lift",
		EndReason:          "rpm_recovery",
		StartRPM:           6000,
		MinimumRPM:         2000,
		StartThrottle:      1,
		MinimumThrottle:    0.4,
		EndThrottle:        0.8,
		ThrottleDrop:       0.6,
		LongestLiftSamples: 4,
		MinimumLiftSamples: boostRegenMinimumLiftSamples,
		EnergyFraction:     0.5,
		GapMultiplier:      1.2,
		TargetPassiveScale: boostRegenTargetPassiveScale,
		PointsPerEnergy:    boostRegenPointsPerEnergy,
		EventChargeCap:     boostRegenMaximumEventPoints,
		ChargePreview:      6,
		ChargeApplied:      6,
		Eligible:           true,
		BoostAfter:         26,
		ActualBoostDelta:   1.5,
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records := readTelemetryLogRecords(t, recorder.Path())
	if len(records) != 4 {
		t.Fatalf("record count = %d, want 4", len(records))
	}
	record := records[2]
	if record.Type != "boost_regen_probe" || record.SourceID != "11.4" || record.CarID != "CP-2" || record.RaceRunID != "rr_regen" || record.RacePhase != "green" || record.BoostRegenProbe == nil {
		t.Fatalf("regen record = %#v", record)
	}
	if record.BoostRegenProbe.EventID != "11.4:CP-2:regen_live:boot:8" || record.BoostRegenProbe.Mode != "live" || record.BoostRegenProbe.AlgorithmVersion != boostRegenAlgorithmVersion || record.BoostRegenProbe.Trigger != "partial_lift" || record.BoostRegenProbe.EndReason != "rpm_recovery" || record.BoostRegenProbe.ThrottleDrop != 0.6 || record.BoostRegenProbe.LongestLiftSamples != 4 || record.BoostRegenProbe.MinimumLiftSamples != boostRegenMinimumLiftSamples || !record.BoostRegenProbe.Eligible || record.BoostRegenProbe.TargetPassiveScale != boostRegenTargetPassiveScale || record.BoostRegenProbe.PointsPerEnergy != boostRegenPointsPerEnergy || record.BoostRegenProbe.EventChargeCap != boostRegenMaximumEventPoints || record.BoostRegenProbe.ChargePreview != 6 || record.BoostRegenProbe.ChargeApplied != 6 || record.BoostRegenProbe.BoostAfter != 26 {
		t.Fatalf("regen payload = %#v", record.BoostRegenProbe)
	}
	footer := records[3]
	if footer.Stats == nil || footer.Stats.BoostRegenProbeRecords != 1 {
		t.Fatalf("footer stats = %#v", footer.Stats)
	}
}

func TestTelemetryRecorderWritesImpactShadow(t *testing.T) {
	recorder, err := newTelemetryRecorderWithQueue(t.TempDir(), 8)
	if err != nil {
		t.Fatalf("newTelemetryRecorderWithQueue() error = %v", err)
	}
	recorder.RecordRaceState(`{"type":"race_state","raceRunId":"rr_shadow","phase":"green"}`, telemetryRaceContext{
		RaceRunID: "rr_shadow",
		Phase:     "green",
		Present:   true,
	})
	recorder.RecordImpactShadow("11.5", "CP-3", impactShadowLogSample{
		EventID:                "CP-3:boot-shadow:7",
		AlgorithmVersion:       impactShadowAlgorithmVersion,
		CurrentImpactClass:     "strong",
		AxisProposalKind:       "road_impact",
		ProposedKind:           "ambiguous",
		RuntimeBehaviorChanged: false,
		WindowComplete:         true,
		WindowBeforeMS:         300,
		WindowAfterMS:          300,
		MotionSamples:          19,
		Reasons:                []string{"mixed_axis_candidate"},
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records := readTelemetryLogRecords(t, recorder.Path())
	if len(records) != 4 {
		t.Fatalf("record count = %d, want 4", len(records))
	}
	record := records[2]
	if record.Type != "impact_shadow" || record.SourceID != "11.5" || record.CarID != "CP-3" || record.RaceRunID != "rr_shadow" || record.ImpactShadow == nil {
		t.Fatalf("impact shadow record = %#v", record)
	}
	if record.ImpactShadow.EventID != "CP-3:boot-shadow:7" || record.ImpactShadow.AlgorithmVersion != impactShadowAlgorithmVersion || record.ImpactShadow.ProposedKind != "ambiguous" || record.ImpactShadow.RuntimeBehaviorChanged {
		t.Fatalf("impact shadow payload = %#v", record.ImpactShadow)
	}
	footer := records[3]
	if footer.Stats == nil || footer.Stats.ImpactShadowRecords != 1 {
		t.Fatalf("footer stats = %#v", footer.Stats)
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

func TestDriveOutputLimitReasons(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		effective int
		health    vehicleHealthSnapshot
		want      []string
	}{
		{name: "unlimited", requested: 1800, effective: 1800, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 1}},
		{name: "gear", requested: 2000, effective: 1800, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 1}, want: []string{"gear_cap"}},
		{name: "damage", requested: 1800, effective: 1740, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 0.8}, want: []string{"damage_cap"}},
		{name: "empty fuel forward", requested: 1800, effective: vehicleFuelEmptyForwardPWM, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 1}, want: []string{"fuel_empty"}},
		{name: "empty fuel reverse", requested: 1000, effective: vehicleFuelEmptyReversePWM, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 1}, want: []string{"fuel_empty"}},
		{name: "gear damage and fuel", requested: 2000, effective: vehicleFuelEmptyForwardPWM, health: vehicleHealthSnapshot{Gear: 3, SpeedCap: 0.8}, want: []string{"gear_cap", "damage_cap", "fuel_empty"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := driveOutputLimitReasons(test.requested, test.effective, test.health)
			if !slices.Equal(got, test.want) {
				t.Fatalf("driveOutputLimitReasons() = %v, want %v", got, test.want)
			}
		})
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

func TestRemoveExpiredTelemetryLogsExceptPreservesActiveFile(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	closedPath := filepath.Join(directory, "telemetry-closed.ndjson")
	activePath := filepath.Join(directory, "telemetry-active.ndjson")
	for _, path := range []string{closedPath, activePath} {
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
		oldTime := now.Add(-25 * time.Hour)
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", path, err)
		}
	}

	removed, err := removeExpiredTelemetryLogsExcept(directory, 24*time.Hour, now, activePath)
	if err != nil {
		t.Fatalf("removeExpiredTelemetryLogsExcept() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(closedPath); !os.IsNotExist(err) {
		t.Fatalf("closed telemetry stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active telemetry stat error = %v", err)
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
