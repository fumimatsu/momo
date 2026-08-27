package main

import (
	"fmt"
	"time"
)

// ingestTelemetry preserves the concise setup used by gameplay unit tests.
// Production code has no raw-candidate damage path: this helper converts a
// valid V2 candidate into an already completed collision decision first.
func (health *vehicleHealth) ingestTelemetry(raw string, carID string, now time.Time) (vehicleHealthSnapshot, bool, *vehicleImpactEvent) {
	candidate, ok := parseRelayImpactCandidate(raw)
	if !ok {
		snapshot, changed := health.observeTelemetry(now)
		return snapshot, changed, nil
	}
	impactClass := classifyRelayImpactCandidate(candidate)
	decision := impactShadowLogSample{
		EventID:                fmt.Sprintf("%s:%s:%d", carID, candidate.Boot, candidate.Sequence),
		AlgorithmVersion:       impactShadowAlgorithmVersion,
		CurrentImpactClass:     impactClass,
		AxisProposalKind:       "collision",
		ProposedKind:           "collision",
		ProposedDamageAllowed:  impactClass == "strong" || impactClass == "severe",
		ProposedFFBAllowed:     impactClass != "",
		RuntimeBehaviorChanged: true,
		WindowComplete:         true,
		WindowBeforeMS:         impactShadowWindow.Milliseconds(),
		WindowAfterMS:          impactShadowWindow.Milliseconds(),
		MagnitudeMPS2:          candidate.Magnitude,
		JerkMPS3:               candidate.Jerk,
		Axis:                   candidate.Axis,
		observedAt:             now,
		context:                health.impactDecisionContext(now),
	}
	return health.applyImpactDecision(decision, carID, now)
}
