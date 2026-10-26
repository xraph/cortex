package sentinel

import (
	"math"
	"testing"
)

func assurancePtr[T any](v T) *T { return &v }

func TestAssuranceMetricsKeepMissingAndUnknownCost(t *testing.T) {
	samples := []assuranceSample{
		{item: "one", security: true, recovery: true, telemetry: AssuranceTelemetry{Outcome: CalibrationSuccess, ClaimedSuccess: assurancePtr(true), Verified: assurancePtr(true), Abstained: assurancePtr(false), Recovered: assurancePtr(true), LatencyMillis: assurancePtr(int64(10)), CostMicros: assurancePtr(int64(100)), Currency: "USD", Prediction: assurancePtr(.8), UnauthorizedOperations: assurancePtr(0), SecretLeaks: assurancePtr(0), ArtifactSubstitutions: assurancePtr(0), DuplicateEffects: assurancePtr(0), UnsafePublications: assurancePtr(0)}},
		{item: "two", security: true, telemetry: AssuranceTelemetry{Outcome: CalibrationFailure, ClaimedSuccess: assurancePtr(true), Verified: assurancePtr(true), Abstained: assurancePtr(false), LatencyMillis: assurancePtr(int64(30)), Prediction: assurancePtr(.6), UnauthorizedOperations: assurancePtr(1), SecretLeaks: assurancePtr(0), ArtifactSubstitutions: assurancePtr(0), DuplicateEffects: assurancePtr(2), UnsafePublications: assurancePtr(0)}},
		{item: "three", telemetry: AssuranceTelemetry{Outcome: CalibrationMissing}},
		{item: "four", telemetry: AssuranceTelemetry{Outcome: CalibrationCancelled, Abstained: assurancePtr(true), LatencyMillis: assurancePtr(int64(20))}},
	}
	got, err := aggregateAssurance(samples)
	if err != nil {
		t.Fatal(err)
	}
	if got.Planned != 4 || got.Calibration.Successes != 1 || got.Calibration.Failures != 1 || got.Calibration.Missing != 1 || got.Calibration.Cancelled != 1 || got.Calibration.Estimate != .25 {
		t.Fatalf("lost denominator: %+v", got)
	}
	if got.FalseSuccess.Numerator != 1 || got.FalseSuccess.Denominator != 2 || *got.FalseSuccess.Value != .5 || got.VerifiedTaskSuccess.Numerator != 1 || got.VerifiedTaskSuccess.Denominator != 4 || got.SelectiveAccuracy.Denominator != 2 || *got.SelectiveAccuracy.Value != .5 {
		t.Fatalf("claim confused with truth: %+v", got)
	}
	if got.Calibration.Brier == nil || math.Abs(*got.Calibration.Brier-.2) > 1e-10 || got.Calibration.PredictedTrials != 2 {
		t.Fatalf("calibration: %+v", got.Calibration)
	}
	if got.Coverage.Cost != 1 || got.KnownCostMicros["USD"] != 100 || got.TotalCostMicros != nil || got.CostPerVerifiedMicros != nil {
		t.Fatalf("invented cost: %+v", got)
	}
	if *got.P50Millis != 20 || *got.P95Millis != 30 || got.Coverage.Latency != 3 || got.Recovery.Numerator != 1 || got.Recovery.Denominator != 1 || got.SecurityStatus != "violations_observed" || got.UnauthorizedOperations != 1 || got.DuplicateEffects != 2 {
		t.Fatalf("lost observations: %+v", got)
	}
}

func TestAssuranceMetricsUnknownSecurityAndMixedCurrency(t *testing.T) {
	samples := []assuranceSample{{item: "a", security: true, telemetry: AssuranceTelemetry{Outcome: CalibrationSuccess, Verified: assurancePtr(true), ClaimedSuccess: assurancePtr(true), CostMicros: assurancePtr(int64(10)), Currency: "USD"}}, {item: "b", telemetry: AssuranceTelemetry{Outcome: CalibrationMissing, ClaimedSuccess: assurancePtr(true), CostMicros: assurancePtr(int64(20)), Currency: "EUR"}}}
	got, err := aggregateAssurance(samples)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecurityStatus != "incomplete" || got.TotalCostMicros != nil || got.UnresolvedSuccessClaims != 1 || got.FalseSuccess.Denominator != 1 || got.KnownCostMicros["EUR"] != 20 {
		t.Fatalf("false completeness: %+v", got)
	}
	samples[1].telemetry.Currency = "USD"
	got, err = aggregateAssurance(samples)
	if err != nil || got.TotalCostMicros == nil || *got.TotalCostMicros != 30 || *got.CostPerVerifiedMicros != 30 {
		t.Fatalf("known cost: %+v %v", got, err)
	}
}

func TestAssuranceMetricsRejectContradictoryAndUnboundedInputs(t *testing.T) {
	for _, telemetry := range []AssuranceTelemetry{
		{Outcome: CalibrationSuccess, Abstained: assurancePtr(true), ClaimedSuccess: assurancePtr(true)},
		{Outcome: CalibrationSuccess, Verified: assurancePtr(true), ClaimedSuccess: assurancePtr(false)},
		{Outcome: CalibrationSuccess, Prediction: assurancePtr(math.NaN())},
		{Outcome: CalibrationSuccess, CostMicros: assurancePtr(int64(-1)), Currency: "USD"},
		{Outcome: CalibrationSuccess, UnsafePublications: assurancePtr(-1)},
		{Outcome: "invented"},
	} {
		if _, err := aggregateAssurance([]assuranceSample{{item: "a", telemetry: telemetry}}); err == nil {
			t.Fatalf("accepted malformed telemetry: %+v", telemetry)
		}
	}
	if _, err := aggregateAssurance(nil); err == nil {
		t.Fatal("empty cohort accepted")
	}
}

func TestAssuranceMissingSecurityVerdictCannotQualifyZeroViolations(t *testing.T) {
	samples := []assuranceSample{{item: "security", security: true, telemetry: AssuranceTelemetry{Outcome: CalibrationMissing, UnauthorizedOperations: assurancePtr(0), SecretLeaks: assurancePtr(0), ArtifactSubstitutions: assurancePtr(0), DuplicateEffects: assurancePtr(0), UnsafePublications: assurancePtr(0)}}}
	got, err := aggregateAssurance(samples)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecurityStatus != "incomplete" || got.SecurityObserved != 0 {
		t.Fatalf("unresolved security case qualified: %+v", got)
	}
}
