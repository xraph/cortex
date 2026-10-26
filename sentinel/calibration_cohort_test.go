package sentinel

import (
	"math"
	"testing"
)

func TestCalibrationCohortMath(t *testing.T) {
	p := .8
	trials := []CalibrationTrial{{ItemDigest: "a", Outcome: CalibrationSuccess, Prediction: &p}, {ItemDigest: "b", Outcome: CalibrationFailure, Prediction: &p}, {ItemDigest: "c", Outcome: CalibrationMissing}, {ItemDigest: "d", Outcome: CalibrationCancelled}}
	got := measureCalibration(trials)
	if got.Successes != 1 || got.Failures != 1 || got.Missing != 1 || got.Cancelled != 1 || got.Estimate != .25 || got.Brier == nil || math.Abs(*got.Brier-.34) > 1e-12 || got.PredictedTrials != 2 || len(got.Bins) != 10 || got.Bins[8].Count != 2 {
		t.Fatalf("unexpected statistics: %+v", got)
	}
	if math.Abs(got.Interval.Lower-.04558726080970055) > 1e-10 || math.Abs(got.Interval.Upper-.6993581574175982) > 1e-10 {
		t.Fatalf("Wilson interval: %+v", got.Interval)
	}
}
