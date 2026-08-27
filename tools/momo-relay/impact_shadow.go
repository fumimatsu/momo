package main

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	impactShadowAlgorithmVersion          = "vertical-window-v2"
	impactShadowWindow                    = 300 * time.Millisecond
	impactShadowCoverageTolerance         = 50 * time.Millisecond
	impactShadowCollisionVerticalShareMax = 0.20
	impactShadowBaselineGuard             = 100 * time.Millisecond
	impactShadowHorizontalActiveMPS2      = 3.0
	impactShadowVerticalReversalMPS2      = 1.0
	impactShadowDedupeWindow              = 5 * time.Second
)

type impactShadowLogSample struct {
	EventID                 string     `json:"eventId"`
	AlgorithmVersion        string     `json:"algorithmVersion"`
	CurrentImpactClass      string     `json:"currentImpactClass,omitempty"`
	AxisProposalKind        string     `json:"axisProposalKind"`
	ProposedKind            string     `json:"proposedKind"`
	ProposedDamageAllowed   bool       `json:"proposedDamageAllowed"`
	ProposedFFBAllowed      bool       `json:"proposedFfbAllowed"`
	RuntimeBehaviorChanged  bool       `json:"runtimeBehaviorChanged"`
	WindowComplete          bool       `json:"windowComplete"`
	WindowBeforeMS          int64      `json:"windowBeforeMs"`
	WindowAfterMS           int64      `json:"windowAfterMs"`
	MotionSamples           int        `json:"motionSamples"`
	MagnitudeMPS2           float64    `json:"magnitudeMps2"`
	JerkMPS3                float64    `json:"jerkMps3"`
	Axis                    [3]float64 `json:"axis"`
	VerticalShare           float64    `json:"verticalShare"`
	HorizontalShare         float64    `json:"horizontalShare"`
	PeakHorizontalMPS2      float64    `json:"peakHorizontalMps2"`
	PeakVerticalMPS2        float64    `json:"peakVerticalMps2"`
	RMSHorizontalMPS2       float64    `json:"rmsHorizontalMps2"`
	RMSVerticalMPS2         float64    `json:"rmsVerticalMps2"`
	HorizontalActiveMS      int64      `json:"horizontalActiveMs"`
	BaselineForwardMPS2     float64    `json:"baselineForwardMps2"`
	BaselineLateralMPS2     float64    `json:"baselineLateralMps2"`
	PeakHorizontalDeltaMPS2 float64    `json:"peakHorizontalDeltaMps2"`
	HorizontalDeltaActiveMS int64      `json:"horizontalDeltaActiveMs"`
	VerticalReversals       int        `json:"verticalReversals"`
	YawActivityRad          float64    `json:"yawActivityRad"`
	Reasons                 []string   `json:"reasons"`
	observedAt              time.Time
	context                 impactDecisionContext
}

type impactDecisionContext struct {
	raceRunID   string
	raceActive  bool
	boostActive bool
}

type impactShadowMotionSample struct {
	observedAt time.Time
	forward    float64
	lateral    float64
	vertical   float64
	yaw        float64
}

type impactShadowPending struct {
	eventID    string
	observedAt time.Time
	candidate  relayImpactCandidate
	context    impactDecisionContext
}

type impactShadowTracker struct {
	mu      sync.Mutex
	boot    string
	samples []impactShadowMotionSample
	pending []impactShadowPending
	seen    map[string]time.Time
}

func newImpactShadowTracker() *impactShadowTracker {
	return &impactShadowTracker{seen: make(map[string]time.Time)}
}

func (tracker *impactShadowTracker) Reset() {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	tracker.boot = ""
	tracker.samples = nil
	tracker.pending = nil
	tracker.seen = make(map[string]time.Time)
	tracker.mu.Unlock()
}

func (tracker *impactShadowTracker) Observe(raw string, carID string, now time.Time, contextProvider func() impactDecisionContext) []impactShadowLogSample {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	results := make([]impactShadowLogSample, 0, 1)
	if boot, sample, ok := parseImpactShadowMotion(raw, now); ok {
		results = append(results, tracker.switchBootLocked(boot, now)...)
		tracker.samples = append(tracker.samples, sample)
	} else if candidate, ok := parseRelayImpactCandidate(raw); ok {
		context := impactDecisionContext{}
		if contextProvider != nil {
			context = contextProvider()
		}
		results = append(results, tracker.switchBootLocked(candidate.Boot, now)...)
		eventID := carID + ":" + candidate.Boot + ":" + strconv.FormatUint(candidate.Sequence, 10)
		if _, duplicate := tracker.seen[eventID]; !duplicate {
			tracker.seen[eventID] = now
			tracker.pending = append(tracker.pending, impactShadowPending{
				eventID:    eventID,
				observedAt: now,
				candidate:  candidate,
				context:    context,
			})
		}
	}

	tracker.pruneSamplesLocked(now)
	results = append(results, tracker.finalizeReadyLocked(now)...)
	return results
}

func (tracker *impactShadowTracker) Flush(now time.Time) []impactShadowLogSample {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	results := tracker.finalizeAllLocked(now)
	tracker.samples = nil
	tracker.pending = nil
	tracker.seen = make(map[string]time.Time)
	tracker.boot = ""
	return results
}

func (tracker *impactShadowTracker) switchBootLocked(boot string, now time.Time) []impactShadowLogSample {
	boot = strings.TrimSpace(boot)
	if boot == "" || tracker.boot == boot {
		return nil
	}
	var results []impactShadowLogSample
	if tracker.boot != "" {
		results = tracker.finalizeAllLocked(now)
	}
	tracker.boot = boot
	tracker.samples = nil
	tracker.pending = nil
	tracker.seen = make(map[string]time.Time)
	return results
}

func (tracker *impactShadowTracker) pruneSamplesLocked(now time.Time) {
	cutoff := now.Add(-(impactShadowWindow*2 + impactShadowCoverageTolerance))
	first := 0
	for first < len(tracker.samples) && tracker.samples[first].observedAt.Before(cutoff) {
		first++
	}
	if first > 0 {
		copy(tracker.samples, tracker.samples[first:])
		tracker.samples = tracker.samples[:len(tracker.samples)-first]
	}
	seenCutoff := now.Add(-impactShadowDedupeWindow)
	for eventID, observedAt := range tracker.seen {
		if observedAt.Before(seenCutoff) {
			delete(tracker.seen, eventID)
		}
	}
}

func (tracker *impactShadowTracker) finalizeReadyLocked(now time.Time) []impactShadowLogSample {
	if len(tracker.pending) == 0 {
		return nil
	}
	results := make([]impactShadowLogSample, 0, len(tracker.pending))
	remaining := tracker.pending[:0]
	for _, pending := range tracker.pending {
		if now.Sub(pending.observedAt) < impactShadowWindow {
			remaining = append(remaining, pending)
			continue
		}
		results = append(results, tracker.buildLogSampleLocked(pending))
	}
	tracker.pending = remaining
	return results
}

func (tracker *impactShadowTracker) finalizeAllLocked(_ time.Time) []impactShadowLogSample {
	results := make([]impactShadowLogSample, 0, len(tracker.pending))
	for _, pending := range tracker.pending {
		results = append(results, tracker.buildLogSampleLocked(pending))
	}
	tracker.pending = nil
	return results
}

func (tracker *impactShadowTracker) buildLogSampleLocked(pending impactShadowPending) impactShadowLogSample {
	windowStart := pending.observedAt.Add(-impactShadowWindow)
	windowEnd := pending.observedAt.Add(impactShadowWindow)
	window := make([]impactShadowMotionSample, 0, len(tracker.samples))
	for _, sample := range tracker.samples {
		if sample.observedAt.Before(windowStart) || sample.observedAt.After(windowEnd) {
			continue
		}
		window = append(window, sample)
	}

	verticalShare, horizontalShare := impactShadowAxisShares(pending.candidate.Axis)
	currentClass := classifyRelayImpactCandidate(pending.candidate)
	features := calculateImpactShadowWindow(window, pending.observedAt)
	axisProposal, proposedKind, proposedDamageAllowed, proposedFFBAllowed, reasons := classifyImpactShadowWindow(
		currentClass,
		verticalShare,
		horizontalShare,
		features,
	)

	return impactShadowLogSample{
		EventID:                 pending.eventID,
		AlgorithmVersion:        impactShadowAlgorithmVersion,
		CurrentImpactClass:      currentClass,
		AxisProposalKind:        axisProposal,
		ProposedKind:            proposedKind,
		ProposedDamageAllowed:   proposedDamageAllowed,
		ProposedFFBAllowed:      proposedFFBAllowed,
		RuntimeBehaviorChanged:  true,
		WindowComplete:          features.complete,
		WindowBeforeMS:          features.before.Milliseconds(),
		WindowAfterMS:           features.after.Milliseconds(),
		MotionSamples:           len(window),
		MagnitudeMPS2:           pending.candidate.Magnitude,
		JerkMPS3:                pending.candidate.Jerk,
		Axis:                    pending.candidate.Axis,
		VerticalShare:           verticalShare,
		HorizontalShare:         horizontalShare,
		PeakHorizontalMPS2:      features.peakHorizontal,
		PeakVerticalMPS2:        features.peakVertical,
		RMSHorizontalMPS2:       features.rmsHorizontal,
		RMSVerticalMPS2:         features.rmsVertical,
		HorizontalActiveMS:      features.horizontalActive.Milliseconds(),
		BaselineForwardMPS2:     features.baselineForward,
		BaselineLateralMPS2:     features.baselineLateral,
		PeakHorizontalDeltaMPS2: features.peakHorizontalDelta,
		HorizontalDeltaActiveMS: features.horizontalDeltaActive.Milliseconds(),
		VerticalReversals:       features.verticalReversals,
		YawActivityRad:          features.yawActivity,
		Reasons:                 reasons,
		observedAt:              pending.observedAt,
		context:                 pending.context,
	}
}

type impactShadowWindowFeatures struct {
	complete              bool
	before                time.Duration
	after                 time.Duration
	peakHorizontal        float64
	peakVertical          float64
	rmsHorizontal         float64
	rmsVertical           float64
	horizontalActive      time.Duration
	baselineForward       float64
	baselineLateral       float64
	peakHorizontalDelta   float64
	horizontalDeltaActive time.Duration
	verticalReversals     int
	yawActivity           float64
}

func calculateImpactShadowWindow(samples []impactShadowMotionSample, eventAt time.Time) impactShadowWindowFeatures {
	features := impactShadowWindowFeatures{}
	if len(samples) == 0 {
		return features
	}

	first := samples[0].observedAt
	last := samples[len(samples)-1].observedAt
	if first.Before(eventAt) {
		features.before = eventAt.Sub(first)
	}
	if last.After(eventAt) {
		features.after = last.Sub(eventAt)
	}
	minimumCoverage := impactShadowWindow - impactShadowCoverageTolerance
	features.complete = features.before >= minimumCoverage && features.after >= minimumCoverage
	features.baselineForward, features.baselineLateral = impactShadowHorizontalBaseline(samples, eventAt)

	var horizontalSquares float64
	var verticalSquares float64
	previousVerticalSign := 0
	for index, sample := range samples {
		horizontal := math.Hypot(sample.forward, sample.lateral)
		horizontalDelta := math.Hypot(
			sample.forward-features.baselineForward,
			sample.lateral-features.baselineLateral,
		)
		vertical := math.Abs(sample.vertical)
		features.peakHorizontal = math.Max(features.peakHorizontal, horizontal)
		features.peakHorizontalDelta = math.Max(features.peakHorizontalDelta, horizontalDelta)
		features.peakVertical = math.Max(features.peakVertical, vertical)
		horizontalSquares += horizontal * horizontal
		verticalSquares += sample.vertical * sample.vertical

		verticalSign := significantImpactShadowSign(sample.vertical)
		if verticalSign != 0 {
			if previousVerticalSign != 0 && verticalSign != previousVerticalSign {
				features.verticalReversals++
			}
			previousVerticalSign = verticalSign
		}

		if index == 0 {
			continue
		}
		previous := samples[index-1]
		interval := sample.observedAt.Sub(previous.observedAt)
		if interval <= 0 || interval > 100*time.Millisecond {
			continue
		}
		previousHorizontal := math.Hypot(previous.forward, previous.lateral)
		if (horizontal+previousHorizontal)/2 >= impactShadowHorizontalActiveMPS2 {
			features.horizontalActive += interval
		}
		previousHorizontalDelta := math.Hypot(
			previous.forward-features.baselineForward,
			previous.lateral-features.baselineLateral,
		)
		if (horizontalDelta+previousHorizontalDelta)/2 >= impactShadowHorizontalActiveMPS2 {
			features.horizontalDeltaActive += interval
		}
		features.yawActivity += math.Abs((sample.yaw+previous.yaw)/2) * interval.Seconds()
	}
	features.rmsHorizontal = math.Sqrt(horizontalSquares / float64(len(samples)))
	features.rmsVertical = math.Sqrt(verticalSquares / float64(len(samples)))
	return features
}

func classifyImpactShadowWindow(
	currentClass string,
	verticalShare float64,
	horizontalShare float64,
	features impactShadowWindowFeatures,
) (string, string, bool, bool, []string) {
	axisProposal := "ambiguous"
	if verticalShare > impactShadowCollisionVerticalShareMax {
		axisProposal = "road_impact"
	} else if horizontalShare > 0 {
		axisProposal = "collision"
	}

	proposedKind := "ambiguous"
	reasons := make([]string, 0, 5)
	switch {
	case !features.complete:
		reasons = append(reasons, "window_incomplete")
		if verticalShare > impactShadowCollisionVerticalShareMax {
			reasons = append(reasons, "vertical_axis_candidate")
		} else {
			reasons = append(reasons, "horizontal_axis_candidate")
		}
	case verticalShare > impactShadowCollisionVerticalShareMax && features.verticalReversals > 0:
		proposedKind = "road_impact"
		reasons = append(reasons, "vertical_axis_candidate", "vertical_rebound")
		if features.horizontalActive > 0 {
			reasons = append(reasons, "horizontal_load_context_only")
		}
	case verticalShare > impactShadowCollisionVerticalShareMax:
		reasons = append(reasons, "vertical_axis_candidate", "vertical_rebound_missing")
	case currentClass == "strong" || currentClass == "severe":
		proposedKind = "collision"
		reasons = append(reasons, "horizontal_axis_candidate", "damage_threshold_met")
	default:
		reasons = append(reasons, "horizontal_axis_candidate", "below_damage_threshold")
	}

	damageAllowed := proposedKind == "collision" && (currentClass == "strong" || currentClass == "severe")
	ffbAllowed := proposedKind == "road_impact" || proposedKind == "collision"
	return axisProposal, proposedKind, damageAllowed, ffbAllowed, reasons
}

func impactShadowHorizontalBaseline(samples []impactShadowMotionSample, eventAt time.Time) (float64, float64) {
	cutoff := eventAt.Add(-impactShadowBaselineGuard)
	forward := make([]float64, 0, len(samples))
	lateral := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.observedAt.After(cutoff) {
			continue
		}
		forward = append(forward, sample.forward)
		lateral = append(lateral, sample.lateral)
	}
	return impactShadowMedian(forward), impactShadowMedian(lateral)
}

func impactShadowMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

func impactShadowAxisShares(axis [3]float64) (float64, float64) {
	norm := math.Sqrt(axis[0]*axis[0] + axis[1]*axis[1] + axis[2]*axis[2])
	if norm <= 0 {
		return 0, 0
	}
	return math.Abs(axis[2]) / norm, math.Hypot(axis[0], axis[1]) / norm
}

func significantImpactShadowSign(value float64) int {
	if math.Abs(value) < impactShadowVerticalReversalMPS2 {
		return 0
	}
	if value > 0 {
		return 1
	}
	return -1
}

func parseImpactShadowMotion(raw string, now time.Time) (string, impactShadowMotionSample, bool) {
	if !strings.HasPrefix(raw, "TEL:") {
		return "", impactShadowMotionSample{}, false
	}
	var payload struct {
		Version int    `json:"v"`
		Kind    string `json:"k"`
		Source  string `json:"src"`
		Boot    string `json:"boot"`
		Motion  struct {
			Axis []float64 `json:"a"`
			Yaw  float64   `json:"y"`
		} `json:"m"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "TEL:")), &payload); err != nil ||
		payload.Version != 2 || payload.Kind != "s" || payload.Source != "imu0" ||
		strings.TrimSpace(payload.Boot) == "" || len(payload.Motion.Axis) != 3 {
		return "", impactShadowMotionSample{}, false
	}
	values := []float64{payload.Motion.Axis[0], payload.Motion.Axis[1], payload.Motion.Axis[2], payload.Motion.Yaw}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", impactShadowMotionSample{}, false
		}
	}
	return payload.Boot, impactShadowMotionSample{
		observedAt: now,
		forward:    payload.Motion.Axis[0],
		lateral:    payload.Motion.Axis[1],
		vertical:   payload.Motion.Axis[2],
		yaw:        payload.Motion.Yaw,
	}, true
}

func (r *relay) observeImpactClassification(raw string, now time.Time) []impactShadowLogSample {
	if r == nil || r.impactShadow == nil {
		return nil
	}
	context := r.vehicleHealth.impactDecisionContext(now)
	if !context.raceActive && !r.driveLoggingEnabled.Load() {
		return nil
	}
	results := r.impactShadow.Observe(raw, r.raceCarID, now, func() impactDecisionContext { return context })
	r.recordImpactClassifications(results)
	return results
}

func (r *relay) flushImpactClassification(now time.Time) []impactShadowLogSample {
	if r == nil || r.impactShadow == nil {
		return nil
	}
	results := r.impactShadow.Flush(now)
	if r.recorder != nil {
		for _, sample := range results {
			r.recorder.RecordImpactShadow(r.name, r.raceCarID, sample)
		}
	}
	return results
}

func (r *relay) recordImpactClassifications(results []impactShadowLogSample) {
	if r == nil || r.recorder == nil || !r.driveLoggingEnabled.Load() {
		return
	}
	for _, sample := range results {
		r.recorder.RecordImpactShadow(r.name, r.raceCarID, sample)
	}
}

func (r *relay) applyImpactClassifications(results []impactShadowLogSample, now time.Time) bool {
	if r == nil || r.vehicleHealth == nil {
		return false
	}
	changed := false
	for _, result := range results {
		health, healthChanged, event := r.vehicleHealth.applyImpactDecision(result, r.raceCarID, now)
		changed = changed || healthChanged
		if event == nil {
			continue
		}
		if result.ProposedKind == "collision" {
			r.observeBoostRegenTelemetry("", health, event, now)
		}
		r.publishVehicleEvent(*event)
	}
	return changed
}
