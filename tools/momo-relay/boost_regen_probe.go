package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	boostRegenDriveMaximumAge    = 300 * time.Millisecond
	boostRegenSampleMinimum      = 40 * time.Millisecond
	boostRegenSampleMaximum      = 300 * time.Millisecond
	boostRegenImpactSuppression  = 500 * time.Millisecond
	boostRegenEpisodeMaximum     = 3 * time.Second
	boostRegenLookback           = 600 * time.Millisecond
	boostRegenMinimumRPM         = 1200
	boostRegenFullLiftMaximum    = 0.05
	boostRegenBrakeMinimum       = 0.05
	boostRegenThrottleArmMinimum = 0.30
	boostRegenThrottleDrop       = 0.20
	boostRegenMinimumEnergy      = 0.02
	boostRegenRPMRecovery        = 200
	boostRegenRecoverySamples    = 2
	boostRegenTargetPassiveScale = 0.35
	boostRegenPointsPerEnergy    = 30.0
	boostRegenMaximumEventPoints = 8.0
	boostRegenAlgorithmVersion   = 2
)

// boostRegenProbe evaluates regenerative BOOST candidates without changing vehicle state.
// The first production phase is deliberately log-only so track data can set the final
// thresholds before passive BOOST charging is reduced.
type boostRegenProbe struct {
	mu sync.Mutex

	drive        boostRegenDriveObservation
	lastImpactAt time.Time

	hasESC        bool
	lastBoot      string
	lastSequence  uint64
	lastSampleAt  time.Time
	deviceTimeUS  uint64
	hasDeviceTime bool
	rpmWindow     []int
	filteredRPM   int
	history       []boostRegenHistorySample
	episode       *boostRegenEpisode
}

type boostRegenDriveObservation struct {
	ObservedAt time.Time
	Throttle   float64
	Brake      float64
}

type boostRegenESCSample struct {
	Boot          string
	Sequence      uint64
	RPM           int
	MaximumRPM    int
	PeriodUS      int64
	AgeMS         int64
	QualityOK     bool
	DeviceTimeUS  uint64
	HasDeviceTime bool
}

type boostRegenObservationContext struct {
	SourceID   string
	CarID      string
	Health     vehicleHealthSnapshot
	PitPresent bool
	Course     courseProgressSnapshot
}

type boostRegenHistorySample struct {
	observedAt  time.Time
	sequence    uint64
	filteredRPM int
	rawRPM      int
	maximumRPM  int
	throttle    float64
}

type boostRegenEpisode struct {
	startedAt         time.Time
	boot              string
	startSequence     uint64
	endSequence       uint64
	startRPM          int
	minimumRPM        int
	endRPM            int
	maximumRPM        int
	samples           int
	zeroSamples       int
	gapMultiplier     float64
	boostStart        float64
	lap               int
	lastMarkerIndex   *int
	trigger           string
	startThrottle     float64
	minimumThrottle   float64
	recoverySamples   int
	suppressionReason string
}

type boostRegenLogSample struct {
	EventID            string  `json:"eventId"`
	Mode               string  `json:"mode"`
	AlgorithmVersion   int     `json:"algorithmVersion"`
	Trigger            string  `json:"trigger"`
	EndReason          string  `json:"endReason"`
	StartedAtUnixMS    int64   `json:"startedAtUnixMs"`
	EndedAtUnixMS      int64   `json:"endedAtUnixMs"`
	DurationMS         int64   `json:"durationMs"`
	Boot               string  `json:"boot"`
	StartSequence      uint64  `json:"startSequence"`
	EndSequence        uint64  `json:"endSequence"`
	SampleCount        int     `json:"sampleCount"`
	StartRPM           int     `json:"startRpm"`
	MinimumRPM         int     `json:"minimumRpm"`
	EndRPM             int     `json:"endRpm"`
	MaximumRPM         int     `json:"maximumRpm"`
	StartThrottle      float64 `json:"startThrottle"`
	MinimumThrottle    float64 `json:"minimumThrottle"`
	EndThrottle        float64 `json:"endThrottle"`
	ThrottleDrop       float64 `json:"throttleDrop"`
	EnergyFraction     float64 `json:"energyFraction"`
	Intensity          string  `json:"intensity"`
	GapMultiplier      float64 `json:"gapMultiplier"`
	TargetPassiveScale float64 `json:"targetPassiveScale"`
	PointsPerEnergy    float64 `json:"pointsPerEnergy"`
	EventChargeCap     float64 `json:"eventChargeCap"`
	ChargePreview      float64 `json:"chargePreview"`
	Eligible           bool    `json:"eligible"`
	SuppressionReason  string  `json:"suppressionReason,omitempty"`
	BoostStart         float64 `json:"boostStart"`
	BoostEnd           float64 `json:"boostEnd"`
	ActualBoostDelta   float64 `json:"actualBoostDelta"`
	Lap                int     `json:"lap,omitempty"`
	LastMarkerIndex    *int    `json:"lastMarkerIndex,omitempty"`
}

func (r *relay) observeBoostRegenDriveCommand(raw string, health vehicleHealthSnapshot, now time.Time) {
	if r == nil || !r.driveLoggingEnabled.Load() {
		return
	}
	_, powerPWM, ok := parseDriveCommand(raw)
	if !ok {
		return
	}
	throttle, brake := normalizeDrivePower(powerPWM, health.Gear)
	r.boostRegen.observeDrive(boostRegenDriveObservation{
		ObservedAt: now,
		Throttle:   throttle,
		Brake:      brake,
	})
}

func (r *relay) observeBoostRegenTelemetry(raw string, health vehicleHealthSnapshot, impact *vehicleImpactEvent, now time.Time) {
	if r == nil || !r.driveLoggingEnabled.Load() {
		return
	}
	if impact != nil {
		r.boostRegen.observeImpact(now, *impact)
	}
	sample, ok := parseBoostRegenESCSample(raw)
	if !ok {
		return
	}
	completed := r.boostRegen.observeESC(sample, boostRegenObservationContext{
		SourceID:   r.name,
		CarID:      r.raceCarID,
		Health:     health,
		PitPresent: r.vehicleHealth.isPitPresent(),
		Course:     r.courseProgress.snapshot(),
	}, now)
	if completed != nil && r.recorder != nil {
		r.recorder.RecordBoostRegenProbe(r.name, r.raceCarID, *completed)
	}
}

func parseBoostRegenESCSample(raw string) (boostRegenESCSample, bool) {
	// IMU state is more frequent than ESC state. Avoid another JSON decode on that
	// hot path; the full decode below still verifies that src is exactly esc0.
	if !strings.HasPrefix(raw, "TEL:") || !strings.Contains(raw, `"esc0"`) {
		return boostRegenESCSample{}, false
	}
	var payload struct {
		Version      int     `json:"v"`
		Kind         string  `json:"k"`
		Source       string  `json:"src"`
		Boot         string  `json:"boot"`
		Sequence     uint64  `json:"seq"`
		DeviceTimeUS *uint64 `json:"t_us"`
		ESC          struct {
			RPM        *int `json:"rpm"`
			MaximumRPM *int `json:"max"`
		} `json:"esc"`
		Quality struct {
			PeriodUS int64 `json:"p"`
			OK       *bool `json:"ok"`
			AgeMS    int64 `json:"age"`
		} `json:"q"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(raw, "TEL:"))), &payload); err != nil ||
		payload.Source != "esc0" {
		return boostRegenESCSample{}, false
	}
	sample := boostRegenESCSample{
		Boot:      strings.TrimSpace(payload.Boot),
		Sequence:  payload.Sequence,
		PeriodUS:  payload.Quality.PeriodUS,
		AgeMS:     payload.Quality.AgeMS,
		QualityOK: payload.Version == 2 && payload.Kind == "s" && payload.Quality.OK != nil && *payload.Quality.OK,
	}
	if payload.DeviceTimeUS != nil {
		sample.DeviceTimeUS = *payload.DeviceTimeUS
		sample.HasDeviceTime = true
	}
	if payload.ESC.RPM != nil {
		sample.RPM = *payload.ESC.RPM
	}
	if payload.ESC.MaximumRPM != nil {
		sample.MaximumRPM = *payload.ESC.MaximumRPM
	}
	if sample.Boot == "" || payload.ESC.RPM == nil || sample.RPM < 0 || payload.ESC.MaximumRPM == nil || sample.MaximumRPM <= 0 || sample.AgeMS < 0 {
		sample.QualityOK = false
	}
	maximumAgeMS := int64(250)
	if sample.PeriodUS > 0 {
		maximumAgeMS = maxInt64(maximumAgeMS, 3*sample.PeriodUS/1000)
	}
	if sample.AgeMS > maximumAgeMS {
		sample.QualityOK = false
	}
	return sample, true
}

func (probe *boostRegenProbe) observeDrive(observation boostRegenDriveObservation) {
	if probe == nil {
		return
	}
	probe.mu.Lock()
	probe.drive = observation
	probe.mu.Unlock()
}

func (probe *boostRegenProbe) observeImpact(now time.Time, event vehicleImpactEvent) {
	if probe == nil || (event.ImpactClass != "strong" && event.ImpactClass != "severe") {
		return
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.lastImpactAt = now
	if probe.episode != nil && probe.episode.suppressionReason == "" {
		probe.episode.suppressionReason = "impact"
	}
}

func (probe *boostRegenProbe) reset() {
	if probe == nil {
		return
	}
	probe.mu.Lock()
	probe.resetESCLocked()
	probe.drive = boostRegenDriveObservation{}
	probe.mu.Unlock()
}

func (probe *boostRegenProbe) observeESC(sample boostRegenESCSample, context boostRegenObservationContext, now time.Time) *boostRegenLogSample {
	if probe == nil {
		return nil
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()

	gapReason := ""
	if probe.hasESC {
		switch {
		case sample.Boot != probe.lastBoot:
			gapReason = "esc_restart"
		case sample.Sequence != probe.lastSequence+1:
			gapReason = "telemetry_gap"
		default:
			interval := now.Sub(probe.lastSampleAt)
			if probe.hasDeviceTime && sample.HasDeviceTime {
				if sample.DeviceTimeUS <= probe.deviceTimeUS {
					gapReason = "telemetry_gap"
					break
				}
				interval = time.Duration(sample.DeviceTimeUS-probe.deviceTimeUS) * time.Microsecond
			}
			if interval < boostRegenSampleMinimum || interval > boostRegenSampleMaximum {
				gapReason = "telemetry_gap"
			}
		}
	}
	if !sample.QualityOK {
		gapReason = "telemetry_quality"
	}
	if gapReason != "" {
		completed := probe.finishEpisodeLocked(context, now, gapReason)
		probe.resetESCLocked()
		probe.rememberESCSampleLocked(sample, now, sample.QualityOK)
		return completed
	}

	probe.rememberESCSampleLocked(sample, now, true)
	if len(probe.rpmWindow) < 3 {
		return nil
	}
	probe.filteredRPM = medianRPM(probe.rpmWindow)

	if probe.drive.ObservedAt.IsZero() || now.Sub(probe.drive.ObservedAt) < 0 || now.Sub(probe.drive.ObservedAt) > boostRegenDriveMaximumAge {
		completed := probe.finishEpisodeLocked(context, now, "drive_state_stale")
		probe.history = nil
		return completed
	}
	if probe.drive.Brake > boostRegenBrakeMinimum {
		completed := probe.finishEpisodeLocked(context, now, "brake_or_reverse")
		probe.history = nil
		return completed
	}

	if probe.episode != nil {
		previousRPM := probe.episode.endRPM
		probe.episode.endSequence = sample.Sequence
		probe.episode.endRPM = probe.filteredRPM
		probe.episode.minimumRPM = minInt(probe.episode.minimumRPM, probe.filteredRPM)
		probe.episode.maximumRPM = maxInt(probe.episode.maximumRPM, sample.MaximumRPM)
		probe.episode.minimumThrottle = math.Min(probe.episode.minimumThrottle, probe.drive.Throttle)
		probe.episode.samples++
		if probe.filteredRPM > previousRPM {
			probe.episode.recoverySamples++
		} else {
			probe.episode.recoverySamples = 0
		}
		if probe.episode.suppressionReason == "" {
			probe.episode.suppressionReason = boostRegenDynamicSuppression(context, probe.lastImpactAt, now)
		}
		if sample.RPM == 0 {
			probe.episode.zeroSamples++
		} else {
			probe.episode.zeroSamples = 0
		}

		endReason := ""
		switch {
		case probe.episode.zeroSamples >= 2:
			endReason = "rpm_zero"
		case probe.episode.recoverySamples >= boostRegenRecoverySamples &&
			probe.filteredRPM-probe.episode.minimumRPM >= boostRegenRPMRecovery:
			endReason = "rpm_recovery"
		case now.Sub(probe.episode.startedAt) >= boostRegenEpisodeMaximum:
			endReason = "max_duration"
		}
		if endReason == "" {
			return nil
		}
		completed := probe.finishEpisodeLocked(context, now, endReason)
		probe.history = nil
		if probe.filteredRPM > 0 {
			probe.appendHistoryLocked(sample, now)
		}
		return completed
	}

	reference, referenceIndex, startThrottle, ok := probe.regenReferenceLocked(now)
	if ok {
		maximumRPM := maxInt(reference.maximumRPM, sample.MaximumRPM)
		energyFraction := boostRegenEnergyFraction(reference.filteredRPM, probe.filteredRPM, maximumRPM)
		throttleDrop := startThrottle - probe.drive.Throttle
		if reference.filteredRPM >= boostRegenMinimumRPM && startThrottle >= boostRegenThrottleArmMinimum &&
			throttleDrop >= boostRegenThrottleDrop && energyFraction >= boostRegenMinimumEnergy {
			trigger := "partial_lift"
			if probe.drive.Throttle <= boostRegenFullLiftMaximum {
				trigger = "full_lift"
			}
			probe.episode = &boostRegenEpisode{
				startedAt:         reference.observedAt,
				boot:              sample.Boot,
				startSequence:     reference.sequence,
				endSequence:       sample.Sequence,
				startRPM:          reference.filteredRPM,
				minimumRPM:        minInt(reference.filteredRPM, probe.filteredRPM),
				endRPM:            probe.filteredRPM,
				maximumRPM:        maximumRPM,
				samples:           len(probe.history) - referenceIndex + 1,
				gapMultiplier:     boostRegenGapMultiplier(context.Health),
				boostStart:        context.Health.Boost,
				lap:               context.Course.Lap,
				lastMarkerIndex:   cloneIntPointer(context.Course.LastMarkerIndex),
				trigger:           trigger,
				startThrottle:     startThrottle,
				minimumThrottle:   probe.drive.Throttle,
				suppressionReason: boostRegenDynamicSuppression(context, probe.lastImpactAt, now),
			}
			if sample.RPM == 0 {
				probe.episode.zeroSamples = 1
			}
			probe.history = nil
			return nil
		}
	}
	probe.appendHistoryLocked(sample, now)
	return nil
}

func (probe *boostRegenProbe) regenReferenceLocked(now time.Time) (boostRegenHistorySample, int, float64, bool) {
	probe.pruneHistoryLocked(now)
	if len(probe.history) == 0 {
		return boostRegenHistorySample{}, 0, 0, false
	}
	peak := probe.history[0]
	peakIndex := 0
	maximumThrottle := peak.throttle
	for index, candidate := range probe.history[1:] {
		actualIndex := index + 1
		if candidate.filteredRPM > peak.filteredRPM ||
			(candidate.filteredRPM == peak.filteredRPM && candidate.rawRPM >= candidate.filteredRPM) {
			peak = candidate
			peakIndex = actualIndex
		}
		maximumThrottle = math.Max(maximumThrottle, candidate.throttle)
	}
	return peak, peakIndex, maximumThrottle, true
}

func (probe *boostRegenProbe) appendHistoryLocked(sample boostRegenESCSample, now time.Time) {
	probe.pruneHistoryLocked(now)
	probe.history = append(probe.history, boostRegenHistorySample{
		observedAt:  now,
		sequence:    sample.Sequence,
		filteredRPM: probe.filteredRPM,
		rawRPM:      sample.RPM,
		maximumRPM:  sample.MaximumRPM,
		throttle:    probe.drive.Throttle,
	})
}

func (probe *boostRegenProbe) pruneHistoryLocked(now time.Time) {
	first := 0
	for first < len(probe.history) && now.Sub(probe.history[first].observedAt) > boostRegenLookback {
		first++
	}
	probe.history = probe.history[first:]
}

func (probe *boostRegenProbe) rememberESCSampleLocked(sample boostRegenESCSample, now time.Time, includeRPM bool) {
	probe.hasESC = true
	probe.lastBoot = sample.Boot
	probe.lastSequence = sample.Sequence
	probe.lastSampleAt = now
	probe.deviceTimeUS = sample.DeviceTimeUS
	probe.hasDeviceTime = sample.HasDeviceTime
	if !includeRPM {
		return
	}
	probe.rpmWindow = append(probe.rpmWindow, sample.RPM)
	if len(probe.rpmWindow) > 3 {
		probe.rpmWindow = probe.rpmWindow[len(probe.rpmWindow)-3:]
	}
}

func (probe *boostRegenProbe) finishEpisodeLocked(context boostRegenObservationContext, now time.Time, endReason string) *boostRegenLogSample {
	episode := probe.episode
	probe.episode = nil
	if episode == nil {
		return nil
	}
	if suppressionReason := boostRegenEndSuppression(endReason); suppressionReason != "" && episode.suppressionReason == "" {
		episode.suppressionReason = suppressionReason
	}
	if episode.samples < 2 && episode.suppressionReason == "" {
		episode.suppressionReason = "insufficient_samples"
	}
	maximumRPM := maxInt(1, episode.maximumRPM)
	energyFraction := boostRegenEnergyFraction(episode.startRPM, episode.minimumRPM, maximumRPM)
	if energyFraction < boostRegenMinimumEnergy && episode.suppressionReason == "" {
		episode.suppressionReason = "insufficient_deceleration"
	}
	chargePreview := math.Min(boostRegenMaximumEventPoints, boostRegenPointsPerEnergy*energyFraction*episode.gapMultiplier)
	boostDelta := context.Health.Boost - episode.boostStart
	return &boostRegenLogSample{
		EventID:            fmt.Sprintf("%s:%s:regen_shadow:%s:%d", strings.TrimSpace(context.SourceID), strings.TrimSpace(context.CarID), episode.boot, episode.startSequence),
		Mode:               "shadow",
		AlgorithmVersion:   boostRegenAlgorithmVersion,
		Trigger:            episode.trigger,
		EndReason:          endReason,
		StartedAtUnixMS:    episode.startedAt.UnixMilli(),
		EndedAtUnixMS:      now.UnixMilli(),
		DurationMS:         maxInt64(0, now.Sub(episode.startedAt).Milliseconds()),
		Boot:               episode.boot,
		StartSequence:      episode.startSequence,
		EndSequence:        episode.endSequence,
		SampleCount:        episode.samples,
		StartRPM:           episode.startRPM,
		MinimumRPM:         episode.minimumRPM,
		EndRPM:             episode.endRPM,
		MaximumRPM:         maximumRPM,
		StartThrottle:      roundBoostRegen(episode.startThrottle, 3),
		MinimumThrottle:    roundBoostRegen(episode.minimumThrottle, 3),
		EndThrottle:        roundBoostRegen(probe.drive.Throttle, 3),
		ThrottleDrop:       roundBoostRegen(math.Max(0, episode.startThrottle-episode.minimumThrottle), 3),
		EnergyFraction:     roundBoostRegen(energyFraction, 4),
		Intensity:          boostRegenIntensity(energyFraction),
		GapMultiplier:      roundBoostRegen(episode.gapMultiplier, 3),
		TargetPassiveScale: boostRegenTargetPassiveScale,
		PointsPerEnergy:    boostRegenPointsPerEnergy,
		EventChargeCap:     boostRegenMaximumEventPoints,
		ChargePreview:      roundBoostRegen(chargePreview, 3),
		Eligible:           episode.suppressionReason == "",
		SuppressionReason:  episode.suppressionReason,
		BoostStart:         roundBoostRegen(episode.boostStart, 3),
		BoostEnd:           roundBoostRegen(context.Health.Boost, 3),
		ActualBoostDelta:   roundBoostRegen(boostDelta, 3),
		Lap:                episode.lap,
		LastMarkerIndex:    cloneIntPointer(episode.lastMarkerIndex),
	}
}

func boostRegenEndSuppression(endReason string) string {
	switch endReason {
	case "esc_restart", "telemetry_gap", "telemetry_quality", "drive_state_stale", "brake_or_reverse":
		return endReason
	default:
		return ""
	}
}

func (probe *boostRegenProbe) resetESCLocked() {
	probe.hasESC = false
	probe.lastBoot = ""
	probe.lastSequence = 0
	probe.lastSampleAt = time.Time{}
	probe.deviceTimeUS = 0
	probe.hasDeviceTime = false
	probe.rpmWindow = nil
	probe.filteredRPM = 0
	probe.history = nil
	probe.episode = nil
}

func boostRegenDynamicSuppression(context boostRegenObservationContext, lastImpactAt time.Time, now time.Time) string {
	if !lastImpactAt.IsZero() && now.Sub(lastImpactAt) >= 0 && now.Sub(lastImpactAt) <= boostRegenImpactSuppression {
		return "impact"
	}
	if context.PitPresent {
		return "pit"
	}
	if context.Health.BoostState == "active" {
		return "boost_active"
	}
	if context.Health.BoostState == "ready" {
		return "boost_full"
	}
	if context.Health.Fuel <= 0 {
		return "fuel_empty"
	}
	return ""
}

func boostRegenGapMultiplier(health vehicleHealthSnapshot) float64 {
	if health.BoostChargeMS <= 0 {
		return 1
	}
	return clampFloat64(float64(vehicleBoostFallbackCharge.Milliseconds())/float64(health.BoostChargeMS), 0.8, 1.5)
}

func boostRegenEnergyFraction(startRPM int, minimumRPM int, maximumRPM int) float64 {
	maximumRPM = maxInt(1, maximumRPM)
	startFraction := clampFloat64(float64(startRPM)/float64(maximumRPM), 0, 1)
	minimumFraction := clampFloat64(float64(minimumRPM)/float64(maximumRPM), 0, 1)
	return math.Max(0, startFraction*startFraction-minimumFraction*minimumFraction)
}

func boostRegenIntensity(energyFraction float64) string {
	switch {
	case energyFraction < boostRegenMinimumEnergy:
		return "none"
	case energyFraction < 0.10:
		return "light"
	case energyFraction < 0.35:
		return "medium"
	default:
		return "strong"
	}
}

func medianRPM(values []int) int {
	if len(values) < 3 {
		return 0
	}
	left, middle, right := values[len(values)-3], values[len(values)-2], values[len(values)-1]
	if left > middle {
		left, middle = middle, left
	}
	if middle > right {
		middle, right = right, middle
	}
	if left > middle {
		middle = left
	}
	return middle
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func roundBoostRegen(value float64, digits int) float64 {
	scale := math.Pow10(digits)
	return math.Round(value*scale) / scale
}
