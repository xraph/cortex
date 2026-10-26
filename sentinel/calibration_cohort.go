package sentinel

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
)

// CalibrationOutcome partitions every frozen holdout trial, including absent results.
type CalibrationOutcome string

const (
	CalibrationSuccess     CalibrationOutcome = "success"
	CalibrationFailure     CalibrationOutcome = "failure"
	CalibrationMissing     CalibrationOutcome = "missing"
	CalibrationGraderError CalibrationOutcome = "grader_error"
	CalibrationCancelled   CalibrationOutcome = "cancelled"
)

// CalibrationTrial is one independent holdout item. Prediction, when present,
// must be captured before its outcome by the trusted evaluator. It is measured
// for calibration only and never establishes execution authority.
type CalibrationTrial struct {
	ItemDigest string             `json:"item_digest"`
	Outcome    CalibrationOutcome `json:"outcome"`
	Prediction *float64           `json:"prediction,omitempty"`
}

// CalibrationCohort retains the frozen dataset roster and every trial outcome.
// This first method permits one trial per distinct item. Repeated or correlated
// samples need a separately versioned cluster-aware estimator.
type CalibrationCohort struct {
	SchemaVersion     int                          `json:"schema_version"`
	Revision          string                       `json:"revision"`
	EvaluatorRevision string                       `json:"evaluator_revision"`
	Binding           cognitive.CalibrationBinding `json:"binding"`
	DiscoveryItems    []string                     `json:"discovery_items"`
	HoldoutItems      []string                     `json:"holdout_items"`
	WindowStart       time.Time                    `json:"window_start"`
	WindowEnd         time.Time                    `json:"window_end"`
	ExpiresAt         time.Time                    `json:"expires_at"`
	Trials            []CalibrationTrial           `json:"trials"`
}

// CalibrationBin retains prediction coverage alongside observed success.
type CalibrationBin struct {
	Lower          float64 `json:"lower"`
	Upper          float64 `json:"upper"`
	Count          int     `json:"count"`
	MeanPrediction float64 `json:"mean_prediction"`
	SuccessRate    float64 `json:"success_rate"`
}

// CalibrationStatistics uses all planned holdout trials as the task-success
// denominator. Brier and bins use only resolved trials with prior predictions;
// PredictedTrials makes that narrower denominator explicit.
type CalibrationStatistics struct {
	Successes       int                           `json:"successes"`
	Failures        int                           `json:"failures"`
	Missing         int                           `json:"missing"`
	GraderErrors    int                           `json:"grader_errors"`
	Cancelled       int                           `json:"cancelled"`
	Estimate        float64                       `json:"estimate"`
	Interval        cognitive.CalibrationInterval `json:"interval"`
	PredictedTrials int                           `json:"predicted_trials"`
	Brier           *float64                      `json:"brier,omitempty"`
	Bins            []CalibrationBin              `json:"bins"`
}

// CalibrationMeasurement binds the absolute profile and diagnostic statistics.
// Paired candidate deltas remain owned by Sentinel's comparison package.
type CalibrationMeasurement struct {
	Profile    cognitive.CalibrationProfile `json:"profile"`
	Statistics CalibrationStatistics        `json:"statistics"`
}

// MeasureCalibration computes a reproducible profile from authenticated cohort
// bytes. This pure function does not authenticate the producer or publish evidence.
func MeasureCalibration(c CalibrationCohort, ref evidence.Ref) (CalibrationMeasurement, error) {
	var zero CalibrationMeasurement
	if c.SchemaVersion != 1 || c.Binding.Validate() != nil || len(c.HoldoutItems) == 0 || len(c.HoldoutItems) > 4096 || len(c.DiscoveryItems) > 4096 || len(c.Trials) != len(c.HoldoutItems) {
		return zero, cognitive.ErrInvalidCalibration
	}
	seen := make(map[string]bool, len(c.DiscoveryItems)+len(c.HoldoutItems))
	for _, set := range [][]string{c.DiscoveryItems, c.HoldoutItems} {
		for _, digest := range set {
			if !evidence.ValidDigest(digest) || seen[digest] {
				return zero, cognitive.ErrInvalidCalibration
			}
			seen[digest] = true
		}
	}
	for i, t := range c.Trials {
		if t.ItemDigest != c.HoldoutItems[i] || (t.Prediction != nil && (math.IsNaN(*t.Prediction) || math.IsInf(*t.Prediction, 0) || *t.Prediction < 0 || *t.Prediction > 1)) {
			return zero, cognitive.ErrInvalidCalibration
		}
		switch t.Outcome {
		case CalibrationSuccess, CalibrationFailure, CalibrationMissing, CalibrationGraderError, CalibrationCancelled:
		default:
			return zero, cognitive.ErrInvalidCalibration
		}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return zero, err
	}
	if (evidence.Artifact{Ref: ref, Data: body}).Validate() != nil || ref.Scope.Canonical() != c.Binding.Scope.Canonical() {
		return zero, cognitive.ErrInvalidCalibration
	}
	roster, err := json.Marshal(struct{ Discovery, Holdout []string }{c.DiscoveryItems, c.HoldoutItems})
	if err != nil {
		return zero, err
	}
	stats := measureCalibration(c.Trials)
	p := cognitive.CalibrationProfile{SchemaVersion: 1, Revision: c.Revision, EvaluatorRevision: c.EvaluatorRevision, Evidence: ref, Binding: c.Binding, DatasetDigest: evidence.Digest(roster), DiscoveryItems: slices.Clone(c.DiscoveryItems), HoldoutItems: slices.Clone(c.HoldoutItems), WindowStart: c.WindowStart, WindowEnd: c.WindowEnd, ExpiresAt: c.ExpiresAt, Trials: len(c.Trials), Successes: stats.Successes, Failures: stats.Failures, Missing: stats.Missing, GraderErrors: stats.GraderErrors, Cancelled: stats.Cancelled, Estimate: stats.Estimate, Uncertainty: stats.Interval}
	if p.Validate() != nil {
		return zero, cognitive.ErrInvalidCalibration
	}
	return CalibrationMeasurement{Profile: p, Statistics: stats}, nil
}

func measureCalibration(trials []CalibrationTrial) CalibrationStatistics {
	s := CalibrationStatistics{Bins: make([]CalibrationBin, 10)}
	for i := range s.Bins {
		s.Bins[i].Lower = float64(i) / 10
		s.Bins[i].Upper = float64(i+1) / 10
	}
	brier := 0.0
	for _, t := range trials {
		switch t.Outcome {
		case CalibrationSuccess:
			s.Successes++
		case CalibrationFailure:
			s.Failures++
		case CalibrationMissing:
			s.Missing++
		case CalibrationGraderError:
			s.GraderErrors++
		case CalibrationCancelled:
			s.Cancelled++
		}
		if t.Prediction == nil || (t.Outcome != CalibrationSuccess && t.Outcome != CalibrationFailure) {
			continue
		}
		outcome := 0.0
		if t.Outcome == CalibrationSuccess {
			outcome = 1
		}
		p := *t.Prediction
		index := min(int(p*10), 9)
		bin := &s.Bins[index]
		bin.Count++
		bin.MeanPrediction += p
		bin.SuccessRate += outcome
		s.PredictedTrials++
		brier += (p - outcome) * (p - outcome)
	}
	if s.PredictedTrials > 0 {
		brier /= float64(s.PredictedTrials)
		s.Brier = &brier
	}
	for i := range s.Bins {
		bin := &s.Bins[i]
		if bin.Count > 0 {
			bin.MeanPrediction /= float64(bin.Count)
			bin.SuccessRate /= float64(bin.Count)
		}
	}
	n := float64(len(trials))
	s.Estimate = float64(s.Successes) / n
	// Wilson score interval at fixed 95% coverage, with unresolved trials retained
	// as unconfirmed success. This measures confirmed success per planned trial.
	const z = 1.959963984540054
	z2 := z * z
	den := 1 + z2/n
	center := (s.Estimate + z2/(2*n)) / den
	half := z * math.Sqrt(s.Estimate*(1-s.Estimate)/n+z2/(4*n*n)) / den
	s.Interval = cognitive.CalibrationInterval{Kind: "confidence", Lower: math.Max(0, center-half), Upper: math.Min(1, center+half), Level: .95, MethodRevision: "wilson-confirmed-planned-v1"}
	return s
}
