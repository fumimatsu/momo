package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestVehicleHealthAppliesDamageAndClampsForwardThrottle(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_damage", "green", 1, 4, base)

	_, published, event := health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`, "CP-1", base.Add(time.Second))
	if !published {
		t.Fatal("strong impact must publish health")
	}
	if event == nil || !event.DamageApplied || event.Damage != 12 || event.EventID != "CP-1:boot-a:1" {
		t.Fatalf("strong impact event = %#v", event)
	}
	snapshot := health.snapshot(base.Add(time.Second))
	if snapshot.HP != 88 || snapshot.Mode != "healthy" {
		t.Fatalf("strong impact snapshot = %#v, want HP 88 healthy", snapshot)
	}
	if got := health.limitCommand("S:1500,T:2000\n", base.Add(1100*time.Millisecond)); got != "S:1500,T:1596\n" {
		t.Fatalf("limited command = %q, want gear-1 and health limit 1596", got)
	}
	if got := health.limitCommand("S:1500,T:1000\n", base.Add(1200*time.Millisecond)); got != "S:1500,T:1000\n" {
		t.Fatalf("damage must not limit reverse escape command: %q", got)
	}
}

func TestVehicleHealthSpeedCapUsesGentleHealthyRange(t *testing.T) {
	tests := []struct {
		hp   float64
		want float64
	}{
		{hp: 100, want: 1.00},
		{hp: 88, want: 0.96},
		{hp: 72, want: 0.9066666667},
		{hp: 70, want: 0.90},
		{hp: 35, want: 0.60},
		{hp: 0, want: 0.35},
	}

	for _, test := range tests {
		if got := vehicleHealthSpeedCap(test.hp); math.Abs(got-test.want) > 0.000001 {
			t.Errorf("HP %.0f speed cap = %.6f, want %.6f", test.hp, got, test.want)
		}
	}
}

func TestVehicleHealthRecoveryRequiresForwardDrivingAndQuietPeriod(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_123", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	if got := health.snapshot(base).HP; got != 80 {
		t.Fatalf("severe impact HP = %.1f, want 80", got)
	}

	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, "CP-1", base.Add(5*time.Second))
	if got := health.snapshot(base.Add(5 * time.Second)).HP; got != 80 {
		t.Fatalf("health recovered without forward command: %.1f", got)
	}
	health.observeRaceState(true, "rr_123", "green", 1, 4, base.Add(5*time.Second))
	health.setDriveEnabled(true, base.Add(5*time.Second))
	health.limitCommand("S:1500,T:2000", base.Add(5*time.Second))
	health.limitCommand("S:1500,T:2000", base.Add(5950*time.Millisecond))
	health.ingestTelemetry(`TEL:{"v":2,"k":"s"}`, "CP-1", base.Add(6*time.Second))
	if got := health.snapshot(base.Add(6 * time.Second)).HP; got <= 80 {
		t.Fatalf("health did not recover during safe forward driving: %.1f", got)
	}
}

func TestVehicleHealthRestoresDamageWhenRaceLeavesGreen(t *testing.T) {
	base := time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_finished_recovery", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	snapshot, changed := health.observeRacePhase("finished", base.Add(time.Second))

	if !changed || snapshot.HP != vehicleHealthMaximum {
		t.Fatalf("finished damage restoration = %#v changed=%t", snapshot, changed)
	}
}

func TestVehicleHealthRestoresDamageWhenRaceDisconnects(t *testing.T) {
	base := time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_disconnected_recovery", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	snapshot, changed := health.markRaceDisconnected(base.Add(time.Second))

	if !changed || snapshot.HP != vehicleHealthMaximum {
		t.Fatalf("disconnected damage restoration = %#v changed=%t", snapshot, changed)
	}
}

func TestVehicleHealthReadyTransitionResetsOnlyOnce(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_ready", "green", 1, 4, base)
	health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base)
	if _, reset := health.observeRacePhase("green", base.Add(time.Second)); reset {
		t.Fatal("green must not reset health")
	}
	snapshot, reset := health.observeRacePhase("ready", base.Add(2*time.Second))
	if !reset || snapshot.HP != 100 {
		t.Fatalf("ready reset = %#v reset=%t", snapshot, reset)
	}
	if _, reset := health.observeRacePhase("ready", base.Add(3*time.Second)); reset {
		t.Fatal("repeated ready must not reset or publish")
	}
}

func TestVehicleHealthClassifiesOnlyImpactEvents(t *testing.T) {
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":15.0,"a":[1,0,0],"j":750}}`); got != "severe" {
		t.Fatalf("V2 severe = %q", got)
	}
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":2,"e":{"n":"impact_candidate","m":15.0,"a":[1,0,0],"j":749}}`); got != "strong" {
		t.Fatalf("lower-jerk V2 impact = %q, want strong", got)
	}
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":3,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":120}}`); got != "weak" {
		t.Fatalf("low-jerk V2 impact = %q, want weak", got)
	}
	if got := classifyRelayImpact(`TEL:{"v":1,"k":"e","boot":"boot-a","seq":3,"evt":{"name":"impact","data":{"mag_mps2":20.0,"jerk_mps3":300}}}`); got != "" {
		t.Fatalf("diagnostic V1 impact became authoritative: %q", got)
	}
	if !isLegacyImpactEvent(`TEL:{"v":1,"k":"e","evt":{"name":"impact","data":{"mag_mps2":20.0}}}`) {
		t.Fatal("diagnostic V1 impact was not identified for logging")
	}
	if got := classifyRelayImpact(`TEL:{"v":2,"k":"s","m":{"a":[20,0,0]}}`); got != "" {
		t.Fatalf("state frame classified as impact: %q", got)
	}
	if got := formatVehicleHealthTelemetry(vehicleHealthSnapshot{HP: 72, SpeedCap: 0.86, Mode: "healthy"}); !strings.HasPrefix(got, "VHS:1,72.0,0.860,healthy") {
		t.Fatalf("formatted health = %q", got)
	}
}

func TestVehicleHealthEpisodeAndDuplicateEventsDoNotApplyDamageTwice(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_cooldown", "green", 1, 4, base)
	firstRaw := `TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`
	_, _, first := health.ingestTelemetry(firstRaw, "CP-1", base)
	if first == nil || !first.DamageApplied || first.HPAfter != 88 {
		t.Fatalf("first event = %#v", first)
	}
	_, _, duplicate := health.ingestTelemetry(firstRaw, "CP-1", base.Add(100*time.Millisecond))
	if duplicate != nil || health.snapshot(base.Add(100*time.Millisecond)).HP != 88 {
		t.Fatalf("duplicate event=%#v hp=%.1f", duplicate, health.snapshot(base.Add(100*time.Millisecond)).HP)
	}
	secondRaw := `TEL:{"v":2,"k":"e","boot":"boot-a","seq":2,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`
	_, _, suppressed := health.ingestTelemetry(secondRaw, "CP-1", base.Add(599*time.Millisecond))
	if suppressed == nil || suppressed.DamageApplied || suppressed.SuppressionReason != "impact_episode" || suppressed.HPAfter != 88 {
		t.Fatalf("episode event = %#v", suppressed)
	}
	thirdRaw := `TEL:{"v":2,"k":"e","boot":"boot-a","seq":3,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`
	_, _, stillSuppressed := health.ingestTelemetry(thirdRaw, "CP-1", base.Add(600*time.Millisecond))
	if stillSuppressed == nil || stillSuppressed.DamageApplied || stillSuppressed.HPAfter != 88 {
		t.Fatalf("event at 600ms = %#v", stillSuppressed)
	}
	fourthRaw := `TEL:{"v":2,"k":"e","boot":"boot-a","seq":4,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`
	_, _, applied := health.ingestTelemetry(fourthRaw, "CP-1", base.Add(1500*time.Millisecond))
	if applied == nil || !applied.DamageApplied || applied.HPAfter != 76 {
		t.Fatalf("new episode event = %#v", applied)
	}
}

func TestVehicleHealthEpisodeEscalationAppliesOnlyDamageDifference(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_escalation", "green", 1, 4, base)
	_, _, strong := health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":300}}`, "CP-1", base)
	if strong == nil || strong.Damage != 12 || strong.HPAfter != 88 {
		t.Fatalf("strong event = %#v", strong)
	}
	_, _, severe := health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":2,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base.Add(600*time.Millisecond))
	if severe == nil || !severe.DamageApplied || severe.Damage != 8 || severe.HPAfter != 80 {
		t.Fatalf("escalated event = %#v", severe)
	}
}

func TestVehicleHealthSuppressesDamageOutsideActiveRace(t *testing.T) {
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		setup func(*vehicleHealth)
		now   time.Time
	}{
		{name: "not_started", setup: func(*vehicleHealth) {}, now: base},
		{name: "ready", setup: func(health *vehicleHealth) {
			health.observeRaceState(true, "rr_ready", "ready", 1, 4, base)
		}, now: base.Add(time.Second)},
		{name: "finished", setup: func(health *vehicleHealth) {
			health.observeRaceState(true, "rr_finished", "finished", 1, 4, base)
		}, now: base.Add(time.Second)},
		{name: "disconnected", setup: func(health *vehicleHealth) {
			health.observeRaceState(false, "rr_disconnected", "green", 1, 4, base)
		}, now: base.Add(time.Second)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			health := newVehicleHealth(base)
			test.setup(health)
			_, _, event := health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", test.now)
			if event == nil || event.DamageApplied || event.Damage != 0 || event.SuppressionReason != "race_inactive" {
				t.Fatalf("inactive race event = %#v", event)
			}
			if snapshot := health.snapshot(test.now); snapshot.HP != vehicleHealthMaximum {
				t.Fatalf("inactive race HP = %.1f, want %.1f", snapshot.HP, vehicleHealthMaximum)
			}
		})
	}
}

func TestVehicleHealthKeepsSparseConnectedGreenRaceActive(t *testing.T) {
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_sparse", "green", 1, 4, base)

	_, _, event := health.ingestTelemetry(
		`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":13.0,"a":[1,0,0],"j":800}}`,
		"CP-1",
		base.Add(20*time.Second),
	)
	if event == nil || !event.DamageApplied || event.SuppressionReason != "" || event.HPAfter != 88 {
		t.Fatalf("sparse connected green race event = %#v", event)
	}
}

func TestVehicleHealthAppliesDamageDuringPractice(t *testing.T) {
	base := time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_practice", "green", 1, 4, base, "practice")

	_, _, event := health.ingestTelemetry(`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`, "CP-1", base.Add(time.Second))
	if event == nil || !event.DamageApplied || event.Damage != vehicleHealthSevereDamage || event.HPAfter != 80 {
		t.Fatalf("practice impact event = %#v", event)
	}
}

func TestVehicleHealthSuppressesDamageOnlyWhileBoostIsActive(t *testing.T) {
	base := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	health := newVehicleHealth(base)
	health.observeRaceState(true, "rr_boost_guard", "green", 1, 4, base)
	health.mu.Lock()
	health.boost = vehicleBoostMaximum
	health.requestedGear = vehicleNormalGearMaximum
	health.mu.Unlock()
	if _, accepted := health.activateBoost(base); !accepted {
		t.Fatal("boost activation was rejected")
	}

	_, _, protected := health.ingestTelemetry(
		`TEL:{"v":2,"k":"e","boot":"boot-a","seq":1,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`,
		"CP-1",
		base.Add(time.Second),
	)
	if protected == nil || protected.DamageApplied || protected.Damage != 0 || protected.SuppressionReason != "boost_active" || protected.HPAfter != vehicleHealthMaximum {
		t.Fatalf("boost-protected impact = %#v", protected)
	}

	_, _, afterBoost := health.ingestTelemetry(
		`TEL:{"v":2,"k":"e","boot":"boot-a","seq":2,"e":{"n":"impact_candidate","m":20.0,"a":[1,0,0],"j":800}}`,
		"CP-1",
		base.Add(vehicleBoostDuration),
	)
	if afterBoost == nil || !afterBoost.DamageApplied || afterBoost.Damage != vehicleHealthSevereDamage || afterBoost.HPAfter != 80 {
		t.Fatalf("post-boost impact = %#v", afterBoost)
	}
}
