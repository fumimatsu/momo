package main

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestRelayAppliesCompletedBoostRegenToVehicleState(t *testing.T) {
	base := time.Date(2026, 8, 18, 1, 2, 3, 0, time.UTC)
	health := newVehicleHealth(base)
	health.setDriveEnabled(true, base)
	relay := &relay{name: "11.4", raceCarID: "CP-2", vehicleHealth: health}
	relay.driveLoggingEnabled.Store(true)

	for sequence := uint64(1); sequence <= 3; sequence++ {
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		snapshot := health.snapshot(at)
		relay.observeBoostRegenDriveCommand("S:1500,T:1600", snapshot, at)
		if _, applied := relay.observeBoostRegenTelemetry(boostRegenTestRaw(sequence, 6000), snapshot, nil, at); applied {
			t.Fatalf("arming sample %d applied regen", sequence)
		}
	}

	appliedCount := 0
	for index, rpm := range []int{6000, 4000, 2000, 0, 0} {
		sequence := uint64(index + 4)
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		snapshot := health.snapshot(at)
		relay.observeBoostRegenDriveCommand("S:1500,T:1500", snapshot, at)
		if _, applied := relay.observeBoostRegenTelemetry(boostRegenTestRaw(sequence, rpm), snapshot, nil, at); applied {
			appliedCount++
		}
	}

	result := health.snapshot(base.Add(800 * time.Millisecond))
	if appliedCount != 1 || result.Boost != boostRegenMaximumEventPoints || result.BoostState != "charging" {
		t.Fatalf("live regen result = %#v, applied count=%d", result, appliedCount)
	}
}

func TestParseBoostRegenESCSample(t *testing.T) {
	raw := `TEL:{"v":2,"k":"s","src":"esc0","boot":"esc-boot","seq":42,"t_us":4200000,"esc":{"rpm":5200,"max":8000,"out":73},"q":{"p":100000,"ok":true,"age":0}}`
	sample, ok := parseBoostRegenESCSample(raw)
	if !ok {
		t.Fatal("parseBoostRegenESCSample() did not recognize esc0 telemetry")
	}
	if !sample.QualityOK || sample.Boot != "esc-boot" || sample.Sequence != 42 || !sample.HasDeviceTime || sample.DeviceTimeUS != 4200000 || sample.RPM != 5200 || sample.MaximumRPM != 8000 || sample.PeriodUS != 100000 {
		t.Fatalf("sample = %#v", sample)
	}

	invalid, ok := parseBoostRegenESCSample(`TEL:{"v":2,"k":"s","src":"esc0","boot":"esc-boot","seq":43,"esc":{"rpm":5000,"max":8000},"q":{"p":100000,"ok":false,"age":0}}`)
	if !ok || invalid.QualityOK {
		t.Fatalf("invalid quality sample = %#v, recognized=%t", invalid, ok)
	}
	if _, ok := parseBoostRegenESCSample(`TEL:{"v":2,"k":"s","src":"imu0"}`); ok {
		t.Fatal("imu0 telemetry must not be recognized as an ESC sample")
	}
}

func TestBoostRegenProbeProducesLiveCandidateWithoutApplyingItDirectly(t *testing.T) {
	probe := &boostRegenProbe{}
	base := time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC)
	marker := 4
	context := boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			Boost:         20,
			BoostState:    "charging",
			Fuel:          75,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
		Course: courseProgressSnapshot{Lap: 3, LastMarkerIndex: &marker},
	}

	armBoostRegenProbe(t, probe, context, base)
	completed := runBoostRegenCoast(t, probe, context, base, 4)
	if completed == nil {
		t.Fatal("coast episode did not complete")
	}
	if completed.Mode != "live" || completed.AlgorithmVersion != boostRegenAlgorithmVersion || completed.EndReason != "rpm_zero" || !completed.Eligible || completed.SuppressionReason != "" {
		t.Fatalf("eligibility = %#v", completed)
	}
	if completed.Trigger != "full_lift" || completed.StartThrottle != 1 || completed.MinimumThrottle != 0 || completed.EndThrottle != 0 || completed.ThrottleDrop != 1 {
		t.Fatalf("lift summary = %#v", completed)
	}
	if completed.StartRPM != 6000 || completed.MinimumRPM != 0 || completed.EndRPM != 0 || completed.MaximumRPM != 8000 || completed.SampleCount != 5 {
		t.Fatalf("RPM summary = %#v", completed)
	}
	if math.Abs(completed.EnergyFraction-0.5625) > 0.00001 || completed.ChargePreview != boostRegenMaximumEventPoints || completed.Intensity != "strong" {
		t.Fatalf("energy preview = %#v", completed)
	}
	if completed.TargetPassiveScale != boostRegenTargetPassiveScale || completed.PointsPerEnergy != boostRegenPointsPerEnergy || completed.EventChargeCap != boostRegenMaximumEventPoints {
		t.Fatalf("live settings = %#v", completed)
	}
	if completed.BoostStart != 20 || completed.BoostEnd != 20 || completed.BoostAfter != 20 || completed.ChargeApplied != 0 || completed.ActualBoostDelta != 0 {
		t.Fatalf("probe directly changed or misreported BOOST = %#v", completed)
	}
	if completed.Lap != 3 || completed.LastMarkerIndex == nil || *completed.LastMarkerIndex != marker {
		t.Fatalf("course position = %#v", completed)
	}
	if completed.EventID != "11.4:CP-2:regen_live:esc-boot:4" {
		t.Fatalf("event ID = %q", completed.EventID)
	}
}

func TestBoostRegenProbeKeepsImpactCandidateButSuppressesEligibility(t *testing.T) {
	probe := &boostRegenProbe{}
	base := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	context := boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			Boost:         30,
			BoostState:    "charging",
			Fuel:          80,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}

	armBoostRegenProbe(t, probe, context, base)
	probe.observeImpact(base.Add(350*time.Millisecond), vehicleImpactEvent{ImpactClass: "strong"})
	completed := runBoostRegenCoast(t, probe, context, base, 4)
	if completed == nil || completed.Eligible || completed.SuppressionReason != "impact" {
		t.Fatalf("impact-suppressed episode = %#v", completed)
	}
	if completed.EnergyFraction <= 0 || completed.ChargePreview <= 0 {
		t.Fatalf("suppressed candidate must retain its measured preview = %#v", completed)
	}
}

func TestBoostRegenProbeCapturesDirectRPMDropSeenInESCLogs(t *testing.T) {
	probe := &boostRegenProbe{}
	base := time.Date(2026, 8, 17, 2, 30, 0, 0, time.UTC)
	context := boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			BoostState:    "charging",
			Fuel:          100,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at, Throttle: 1})
		if got := probe.observeESC(boostRegenTestSample(sequence, 5200), context, at); got != nil {
			t.Fatalf("arming sample %d completed an episode: %#v", sequence, got)
		}
	}

	var completed *boostRegenLogSample
	for sequence := uint64(4); sequence <= 6; sequence++ {
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at})
		completed = probe.observeESC(boostRegenTestSample(sequence, 0), context, at)
	}
	if completed == nil || !completed.Eligible || completed.Trigger != "full_lift" || completed.EndReason != "rpm_zero" || completed.StartRPM != 5200 || completed.MinimumRPM != 0 || completed.SampleCount != 4 {
		t.Fatalf("direct-drop candidate = %#v", completed)
	}
	if math.Abs(completed.EnergyFraction-0.4225) > 0.00001 || completed.ChargePreview != boostRegenMaximumEventPoints {
		t.Fatalf("direct-drop energy = %#v", completed)
	}
}

func TestBoostRegenProbeCapturesPartialLiftInSSection(t *testing.T) {
	probe := &boostRegenProbe{}
	base := time.Date(2026, 8, 17, 2, 45, 0, 0, time.UTC)
	context := boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			Boost:         10,
			BoostState:    "charging",
			Fuel:          100,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}

	throttles := []float64{1, 1, 1, 0.75, 0.5, 0.4, 0.6, 0.8, 0.9, 1}
	rpms := []int{5100, 5100, 5100, 4800, 4600, 4000, 4200, 4500, 4800, 5000}
	var completed *boostRegenLogSample
	for index := range rpms {
		sequence := uint64(index + 1)
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at, Throttle: throttles[index]})
		if result := probe.observeESC(boostRegenTestSample(sequence, rpms[index]), context, at); result != nil {
			completed = result
		}
	}
	if completed == nil || !completed.Eligible || completed.Trigger != "partial_lift" || completed.EndReason != "rpm_recovery" {
		t.Fatalf("partial-lift episode = %#v", completed)
	}
	if completed.StartRPM != 5100 || completed.MinimumRPM != 4200 || completed.StartThrottle != 1 || completed.MinimumThrottle != 0.4 || completed.ThrottleDrop != 0.6 {
		t.Fatalf("partial-lift summary = %#v", completed)
	}
	if completed.LongestLiftSamples < boostRegenMinimumLiftSamples || completed.MinimumLiftSamples != boostRegenMinimumLiftSamples {
		t.Fatalf("partial-lift confirmation = %#v", completed)
	}
	wantEnergy := (5100.0*5100.0 - 4200.0*4200.0) / (8000.0 * 8000.0)
	if math.Abs(completed.EnergyFraction-wantEnergy) > 0.0001 {
		t.Fatalf("energy fraction = %.4f, want %.4f", completed.EnergyFraction, wantEnergy)
	}
}

func TestBoostRegenProbeSuppressesShortLiftAtCompletion(t *testing.T) {
	base := time.Date(2026, 8, 18, 7, 0, 0, 0, time.UTC)
	probe := &boostRegenProbe{
		drive: boostRegenDriveObservation{ObservedAt: base.Add(300 * time.Millisecond), Throttle: 1},
		episode: &boostRegenEpisode{
			startedAt:          base,
			boot:               "esc-boot",
			startSequence:      10,
			endSequence:        13,
			startRPM:           6000,
			minimumRPM:         4000,
			endRPM:             4200,
			maximumRPM:         8000,
			samples:            4,
			gapMultiplier:      1,
			startThrottle:      1,
			minimumThrottle:    0.7,
			longestLiftSamples: 1,
		},
	}
	completed := probe.finishEpisodeLocked(boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			Boost:         20,
			BoostState:    "charging",
			Fuel:          100,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}, base.Add(300*time.Millisecond), "rpm_recovery")
	if completed == nil || completed.Eligible || completed.SuppressionReason != "short_lift" {
		t.Fatalf("short lift result = %#v", completed)
	}
	if completed.LongestLiftSamples != 1 || completed.MinimumLiftSamples != boostRegenMinimumLiftSamples || completed.ChargePreview <= 0 {
		t.Fatalf("short lift diagnostics = %#v", completed)
	}
}

func TestBoostRegenLiftSampleCountsTracksConsecutiveLift(t *testing.T) {
	history := []boostRegenHistorySample{
		{throttle: 1},
		{throttle: 0.75},
		{throttle: 1},
		{throttle: 0.70},
	}
	current, longest := boostRegenLiftSampleCounts(history, 0.65, 1)
	if current != 2 || longest != 2 {
		t.Fatalf("lift counts = current %d, longest %d; want 2, 2", current, longest)
	}

	current, longest = boostRegenLiftSampleCounts(history, 1, 1)
	if current != 0 || longest != 1 {
		t.Fatalf("reset lift counts = current %d, longest %d; want 0, 1", current, longest)
	}
}

func TestBoostRegenProbeRequiresBothThrottleLiftAndRPMDecay(t *testing.T) {
	context := boostRegenObservationContext{
		Health: vehicleHealthSnapshot{
			BoostState:    "charging",
			Fuel:          100,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}
	base := time.Date(2026, 8, 17, 2, 50, 0, 0, time.UTC)
	tests := []struct {
		name      string
		throttles []float64
		rpms      []int
	}{
		{name: "lift without deceleration", throttles: []float64{0.4, 0.4, 0.4}, rpms: []int{5100, 5100, 5100}},
		{name: "deceleration without lift", throttles: []float64{1, 1, 1}, rpms: []int{4800, 4000, 3000}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &boostRegenProbe{}
			for sequence := uint64(1); sequence <= 3; sequence++ {
				at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
				probe.observeDrive(boostRegenDriveObservation{ObservedAt: at, Throttle: 1})
				probe.observeESC(boostRegenTestSample(sequence, 5100), context, at)
			}
			for index, rpm := range test.rpms {
				sequence := uint64(index + 4)
				at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
				probe.observeDrive(boostRegenDriveObservation{ObservedAt: at, Throttle: test.throttles[index]})
				if got := probe.observeESC(boostRegenTestSample(sequence, rpm), context, at); got != nil {
					t.Fatalf("unexpected completed episode = %#v", got)
				}
			}
			if probe.episode != nil {
				t.Fatalf("unexpected active episode = %#v", probe.episode)
			}
		})
	}
}

func TestBoostRegenProbeClosesEpisodeOnTelemetryGap(t *testing.T) {
	probe := &boostRegenProbe{}
	base := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	context := boostRegenObservationContext{
		SourceID: "11.4",
		CarID:    "CP-2",
		Health: vehicleHealthSnapshot{
			BoostState:    "charging",
			Fuel:          100,
			BoostChargeMS: vehicleBoostFallbackCharge.Milliseconds(),
		},
	}

	armBoostRegenProbe(t, probe, context, base)
	for index, rpm := range []int{4000, 2000} {
		sequence := uint64(index + 4)
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at})
		if got := probe.observeESC(boostRegenTestSample(sequence, rpm), context, at); got != nil {
			t.Fatalf("unexpected coast result at sequence %d = %#v", sequence, got)
		}
	}
	gapAt := base.Add(700 * time.Millisecond)
	probe.observeDrive(boostRegenDriveObservation{ObservedAt: gapAt})
	completed := probe.observeESC(boostRegenTestSample(7, 1000), context, gapAt)
	if completed == nil || completed.Eligible || completed.EndReason != "telemetry_gap" || completed.SuppressionReason != "telemetry_gap" {
		t.Fatalf("gap result = %#v", completed)
	}
}

func armBoostRegenProbe(t *testing.T, probe *boostRegenProbe, context boostRegenObservationContext, base time.Time) {
	t.Helper()
	for sequence := uint64(1); sequence <= 3; sequence++ {
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at, Throttle: 1})
		if got := probe.observeESC(boostRegenTestSample(sequence, 6000), context, at); got != nil {
			t.Fatalf("arming sample %d completed an episode: %#v", sequence, got)
		}
	}
}

func runBoostRegenCoast(t *testing.T, probe *boostRegenProbe, context boostRegenObservationContext, base time.Time, firstSequence uint64) *boostRegenLogSample {
	t.Helper()
	rpms := []int{6000, 4000, 2000, 0, 0}
	var completed *boostRegenLogSample
	for index, rpm := range rpms {
		sequence := firstSequence + uint64(index)
		at := base.Add(time.Duration(sequence) * 100 * time.Millisecond)
		probe.observeDrive(boostRegenDriveObservation{ObservedAt: at})
		result := probe.observeESC(boostRegenTestSample(sequence, rpm), context, at)
		if result != nil {
			if completed != nil {
				t.Fatal("coast produced more than one completed episode")
			}
			completed = result
		}
	}
	return completed
}

func boostRegenTestSample(sequence uint64, rpm int) boostRegenESCSample {
	return boostRegenESCSample{
		Boot:       "esc-boot",
		Sequence:   sequence,
		RPM:        rpm,
		MaximumRPM: 8000,
		PeriodUS:   100000,
		QualityOK:  true,
	}
}

func boostRegenTestRaw(sequence uint64, rpm int) string {
	return fmt.Sprintf(`TEL:{"v":2,"k":"s","src":"esc0","boot":"esc-boot","seq":%d,"t_us":%d,"esc":{"rpm":%d,"max":8000},"q":{"p":100000,"ok":true,"age":0}}`, sequence, sequence*100000, rpm)
}
