package optimization

import (
	"testing"
	"time"
)

func TestTaskResourceRatiosNeedComparableMeasurements(t *testing.T) {
	limit, score := .75, .8
	spec := ExperimentSpec{Budget: Budget{PricingRevision: "p1"}, Objective: Objective{MaxCostRatio: &limit}}
	baseline := DecisionEvidence{Trial: Trial{Execution: &ExecutionRecord{Usage: Usage{USDMicros: 100, MoneyQuality: UsageKnown, PricingRevision: "p1"}, Duration: time.Second}}, Evaluation: &Evaluation{Score: &score}}
	candidate := DecisionEvidence{Trial: Trial{Execution: &ExecutionRecord{Usage: Usage{USDMicros: 50, MoneyQuality: UsageKnown, PricingRevision: "p1"}, Duration: time.Second}}, Evaluation: &Evaluation{Score: &score}}
	if !taskCandidatePasses(spec, &baseline, candidate) {
		t.Fatal("known comparable savings refused")
	}
	for _, kind := range []string{"zero-baseline", "unknown", "different-prices", "over-budget", "zero-latency"} {
		t.Run(kind, func(t *testing.T) {
			base, probe, limits := baseline, candidate, spec
			before, after := *baseline.Trial.Execution, *candidate.Trial.Execution
			base.Trial.Execution, probe.Trial.Execution = &before, &after
			switch kind {
			case "zero-baseline":
				before.Usage.USDMicros, after.Usage.USDMicros = 0, 0
			case "unknown":
				after.Usage.MoneyQuality = UsageUnknown
			case "different-prices":
				after.Usage.PricingRevision = "p2"
			case "over-budget":
				after.Usage.USDMicros = 90
			case "zero-latency":
				limits.Objective.MaxLatencyRatio = &limit
				before.Duration, after.Duration = 0, 0
			}
			if taskCandidatePasses(limits, &base, probe) {
				t.Fatal("unqualified resource comparison selected a candidate")
			}
		})
	}
}

func TestTaskPreservesReadOnlyQualification(t *testing.T) {
	actual := IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}
	required := actual
	required.ReadOnly = true
	spec := ExperimentSpec{Task: &TaskPolicy{Isolation: required}}
	if validateTrialEnvironment(spec, actual) == nil {
		t.Fatal("read-only task admitted a writable environment")
	}
	actual.ReadOnly = true
	if err := validateTrialEnvironment(spec, actual); err != nil {
		t.Fatal(err)
	}
}
