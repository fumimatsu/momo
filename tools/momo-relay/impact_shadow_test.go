package main

import (
	"fmt"
	"testing"
	"time"
)

func TestImpactShadowClassifiesHorizontalDamageCandidate(t *testing.T) {
	tracker := newImpactShadowTracker()
	base := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	feedImpactShadowMotion(t, tracker, base, -300, 0, 50, [3]float64{4, 0.2, 0.1})
	if got := tracker.Observe(impactShadowEvent(20, 13, 300, [3]float64{1, 0, 0.05}), "CP-1", base); len(got) != 0 {
		t.Fatalf("event finalized before future window = %#v", got)
	}
	results := feedImpactShadowMotion(t, tracker, base, 50, 300, 50, [3]float64{4, 0.2, 0.1})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	result := results[0]
	if result.ProposedKind != "collision" || result.AxisProposalKind != "collision" || !result.ProposedDamageAllowed {
		t.Fatalf("collision result = %#v", result)
	}
	if !result.WindowComplete || result.WindowBeforeMS != 300 || result.WindowAfterMS != 300 {
		t.Fatalf("window coverage = %#v", result)
	}
	if result.RuntimeBehaviorChanged {
		t.Fatal("shadow result must not change runtime behavior")
	}
}

func TestImpactShadowClassifiesVerticalReboundAsRoadImpact(t *testing.T) {
	tracker := newImpactShadowTracker()
	base := time.Date(2026, 8, 25, 3, 1, 0, 0, time.UTC)
	feedImpactShadowMotion(t, tracker, base, -300, 0, 50, [3]float64{0.2, 0.1, 4})
	tracker.Observe(impactShadowEvent(21, 16, 800, [3]float64{0.15, 0.1, 0.98}), "CP-2", base)
	results := feedImpactShadowMotion(t, tracker, base, 50, 300, 50, [3]float64{0.2, 0.1, -3})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	result := results[0]
	if result.AxisProposalKind != "road_impact" || result.ProposedKind != "road_impact" || result.ProposedDamageAllowed {
		t.Fatalf("road impact result = %#v", result)
	}
	if result.VerticalReversals < 1 || result.HorizontalActiveMS >= impactShadowHorizontalSustained.Milliseconds() {
		t.Fatalf("road impact features = %#v", result)
	}
}

func TestImpactShadowKeepsMixedSustainedInputAmbiguous(t *testing.T) {
	tracker := newImpactShadowTracker()
	base := time.Date(2026, 8, 25, 3, 2, 0, 0, time.UTC)
	feedImpactShadowMotion(t, tracker, base, -300, 0, 50, [3]float64{4, 1, 3})
	tracker.Observe(impactShadowEvent(22, 16, 800, [3]float64{0.7, 0.2, 0.65}), "CP-3", base)
	results := feedImpactShadowMotion(t, tracker, base, 50, 300, 50, [3]float64{4, 1, -3})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	result := results[0]
	if result.AxisProposalKind != "road_impact" || result.ProposedKind != "ambiguous" || result.HorizontalActiveMS < impactShadowHorizontalSustained.Milliseconds() {
		t.Fatalf("mixed result = %#v", result)
	}
}

func TestImpactShadowFlushesIncompleteCandidateOnce(t *testing.T) {
	tracker := newImpactShadowTracker()
	base := time.Date(2026, 8, 25, 3, 3, 0, 0, time.UTC)
	tracker.Observe(impactShadowState(1, [3]float64{0.1, 0.1, 3}, 0), "CP-4", base)
	event := impactShadowEvent(23, 13, 300, [3]float64{0.1, 0.1, 0.99})
	tracker.Observe(event, "CP-4", base.Add(10*time.Millisecond))
	tracker.Observe(event, "CP-4", base.Add(20*time.Millisecond))
	results := tracker.Flush(base.Add(40 * time.Millisecond))
	if len(results) != 1 {
		t.Fatalf("flush result count = %d, want 1", len(results))
	}
	if results[0].WindowComplete || results[0].ProposedKind != "ambiguous" || results[0].EventID != "CP-4:boot-shadow:23" {
		t.Fatalf("flush result = %#v", results[0])
	}
	if second := tracker.Flush(base.Add(time.Second)); len(second) != 0 {
		t.Fatalf("second flush = %#v, want empty", second)
	}
}

func BenchmarkImpactShadowObserveState(b *testing.B) {
	tracker := newImpactShadowTracker()
	raw := impactShadowState(1, [3]float64{0.4, 1.2, 0.8}, 0.2)
	base := time.Date(2026, 8, 25, 3, 4, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		tracker.Observe(raw, "CP-1", base.Add(time.Duration(index)*33*time.Millisecond))
	}
}

func feedImpactShadowMotion(t *testing.T, tracker *impactShadowTracker, base time.Time, startMS int, endMS int, stepMS int, axis [3]float64) []impactShadowLogSample {
	t.Helper()
	var results []impactShadowLogSample
	sequence := uint64(1000 + startMS + 300)
	for offset := startMS; offset <= endMS; offset += stepMS {
		results = append(results, tracker.Observe(
			impactShadowState(sequence, axis, 0.2),
			"CP-test",
			base.Add(time.Duration(offset)*time.Millisecond),
		)...)
		sequence++
	}
	return results
}

func impactShadowState(sequence uint64, axis [3]float64, yaw float64) string {
	return fmt.Sprintf(
		`TEL:{"v":2,"k":"s","src":"imu0","boot":"boot-shadow","seq":%d,"m":{"a":[%.3f,%.3f,%.3f],"y":%.3f}}`,
		sequence, axis[0], axis[1], axis[2], yaw,
	)
}

func impactShadowEvent(sequence uint64, magnitude float64, jerk float64, axis [3]float64) string {
	return fmt.Sprintf(
		`TEL:{"v":2,"k":"e","boot":"boot-shadow","seq":%d,"e":{"n":"impact_candidate","m":%.3f,"a":[%.3f,%.3f,%.3f],"j":%.3f}}`,
		sequence, magnitude, axis[0], axis[1], axis[2], jerk,
	)
}
