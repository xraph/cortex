package cognitive

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
)

// ErrInvalidCalibration rejects malformed measurements or compatibility pins.
var ErrInvalidCalibration = errors.New("cortex: invalid calibration observation")

// CalibrationMeasured identifies a measurement authenticated by the host's
// evaluation adapter. You must never decode this authority from model output.
const CalibrationMeasured = "measured_cohort"

// CalibrationTool pins a tool implementation in a measured configuration.
type CalibrationTool struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// CalibrationBinding identifies the configuration and task covered by a cohort.
// Revisions must identify immutable configurations, including provider settings.
// AcceptanceDigest pins the frozen success criteria used for evaluation.
type CalibrationBinding struct {
	Scope               cortex.Scope      `json:"scope"`
	TaskClass           string            `json:"task_class"`
	ProviderRevision    string            `json:"provider_revision"`
	ModelRevision       string            `json:"model_revision"`
	PromptRevision      string            `json:"prompt_revision"`
	ControllerRevision  string            `json:"controller_revision"`
	PolicyRevision      string            `json:"policy_revision"`
	EnvironmentRevision string            `json:"environment_revision"`
	AcceptanceDigest    string            `json:"acceptance_digest"`
	Tools               []CalibrationTool `json:"tools"`
}

// CalibrationInterval preserves uncertainty computed by the evaluation service.
// Kind identifies confidence or credible intervals; Level is their coverage.
// Cortex validates bounds and retains the method revision without recalculating it.
type CalibrationInterval struct {
	Kind           string  `json:"kind"`
	Lower          float64 `json:"lower"`
	Upper          float64 `json:"upper"`
	Level          float64 `json:"level"`
	MethodRevision string  `json:"method_revision"`
}

// CalibrationProfile references a versioned evaluation cohort. Counts partition
// all planned trials, including missing results, grader errors and cancellations.
// Estimate is the evaluator's measured task success, never a model self-score.
// DiscoveryItems and HoldoutItems identify exact dataset items by content digest.
type CalibrationProfile struct {
	SchemaVersion     int                 `json:"schema_version"`
	Revision          string              `json:"revision"`
	EvaluatorRevision string              `json:"evaluator_revision"`
	Evidence          evidence.Ref        `json:"evidence"`
	Binding           CalibrationBinding  `json:"binding"`
	DatasetDigest     string              `json:"dataset_digest"`
	DiscoveryItems    []string            `json:"discovery_items"`
	HoldoutItems      []string            `json:"holdout_items"`
	WindowStart       time.Time           `json:"window_start"`
	WindowEnd         time.Time           `json:"window_end"`
	ExpiresAt         time.Time           `json:"expires_at"`
	Trials            int                 `json:"trials"`
	Successes         int                 `json:"successes"`
	Failures          int                 `json:"failures"`
	Missing           int                 `json:"missing"`
	GraderErrors      int                 `json:"grader_errors"`
	Cancelled         int                 `json:"cancelled"`
	Estimate          float64             `json:"estimate"`
	Uncertainty       CalibrationInterval `json:"uncertainty"`
}

// CalibrationObservation comes from a trusted, scoped evaluation adapter. That
// adapter authenticates the producer, checks artifact availability and current
// revocation, and attests that the profile matches its immutable evidence bytes.
// A digest alone cannot establish those facts. Neither models nor recalled
// lessons may supply this observation. Leaked includes known contamination even
// when discovery and holdout content hashes differ.
type CalibrationObservation struct {
	Origin     string
	ObservedAt time.Time
	Profile    CalibrationProfile
	Revoked    bool
	Leaked     bool
}

// CalibrationPolicy applies host-defined eligibility bounds to measurements.
// MinimumTrials counts resolved trials with successful or failed task verdicts.
// Missing results and evaluator failures stay in the profile's total denominator.
type CalibrationPolicy struct {
	MinimumTrials int
	MaxAge        time.Duration
}

// ReliabilityMeasurement is a detached observation, never permission or a check
// verdict. Unknown results carry no cohort or numeric confidence.
type ReliabilityMeasurement struct {
	Status string              `json:"status"`
	Reason string              `json:"reason,omitempty"`
	Cohort *CalibrationProfile `json:"cohort,omitempty"`
}

// ReliabilityFor selects one compatible externally measured cohort. The host
// supplies current binding pins and time; it must authorize any later disclosure.
// Required checks, authorization and execution budgets are unaffected.
func ReliabilityFor(binding CalibrationBinding, observation *CalibrationObservation, policy CalibrationPolicy, now time.Time) (ReliabilityMeasurement, error) {
	unknown := func(reason string) ReliabilityMeasurement {
		return ReliabilityMeasurement{Status: "unknown", Reason: reason}
	}
	if binding.Validate() != nil || policy.MinimumTrials < 1 || policy.MinimumTrials > 1_000_000_000 || policy.MaxAge <= 0 || now.IsZero() {
		return unknown("invalid"), ErrInvalidCalibration
	}
	if observation == nil {
		return unknown("no_cohort"), nil
	}
	if observation.Origin != CalibrationMeasured {
		return unknown("untrusted_origin"), nil
	}
	p := observation.Profile
	if p.Validate() != nil || observation.ObservedAt.IsZero() || observation.ObservedAt.After(now) || p.WindowEnd.After(observation.ObservedAt) || p.Evidence.CreatedAt.After(observation.ObservedAt) {
		return unknown("invalid"), ErrInvalidCalibration
	}
	switch {
	case observation.Revoked:
		return unknown("revoked"), nil
	case observation.Leaked || calibrationOverlap(p.DiscoveryItems, p.HoldoutItems):
		return unknown("leaked_cohort"), nil
	case !now.Before(p.ExpiresAt) || !now.Before(p.Evidence.RetainUntil):
		return unknown("expired"), nil
	case now.Sub(p.WindowEnd) > policy.MaxAge || now.Sub(observation.ObservedAt) > policy.MaxAge:
		return unknown("stale"), nil
	case !sameCalibrationBinding(binding, p.Binding):
		return unknown("incompatible"), nil
	case p.Successes+p.Failures < policy.MinimumTrials:
		return unknown("insufficient_samples"), nil
	}
	p.Binding.Scope.Levels = slices.Clone(p.Binding.Scope.Levels)
	p.Binding.Tools = slices.Clone(p.Binding.Tools)
	p.Evidence.Scope.Levels = slices.Clone(p.Evidence.Scope.Levels)
	p.DiscoveryItems = slices.Clone(p.DiscoveryItems)
	p.HoldoutItems = slices.Clone(p.HoldoutItems)
	return ReliabilityMeasurement{Status: "measured", Cohort: &p}, nil
}

// Validate rejects absent pins and duplicate tool names. An empty tool set is a
// complete configuration for tasks that use no tools; blank tool pins are invalid.
func (b CalibrationBinding) Validate() error {
	if cortex.ValidateScope(b.Scope) != nil || !evidence.ValidDigest(b.AcceptanceDigest) || len(b.Tools) > 256 {
		return ErrInvalidCalibration
	}
	for _, level := range b.Scope.Levels {
		if !contextText(level.Key, 256) || !contextText(level.Value, 256) {
			return ErrInvalidCalibration
		}
	}
	for _, pin := range []string{b.TaskClass, b.ProviderRevision, b.ModelRevision, b.PromptRevision, b.ControllerRevision, b.PolicyRevision, b.EnvironmentRevision} {
		if !contextText(pin, 256) {
			return ErrInvalidCalibration
		}
	}
	seen := make(map[string]bool, len(b.Tools))
	for _, tool := range b.Tools {
		if !contextText(tool.Name, 256) || !contextText(tool.Revision, 256) || seen[tool.Name] {
			return ErrInvalidCalibration
		}
		seen[tool.Name] = true
	}
	return nil
}

// Validate checks structure and count conservation. You still need an
// authenticated adapter to establish measurement provenance and split integrity.
func (p CalibrationProfile) Validate() error {
	if p.SchemaVersion != 1 || !contextText(p.Revision, 256) || !contextText(p.EvaluatorRevision, 256) || p.Binding.Validate() != nil || p.Evidence.Validate() != nil || p.Evidence.Scope.Canonical() != p.Binding.Scope.Canonical() || !evidence.ValidDigest(p.DatasetDigest) {
		return ErrInvalidCalibration
	}
	if !contextText(p.Evidence.Version, 256) || !contextText(p.Evidence.Producer, 256) || !contextText(p.Evidence.MediaType, 256) || len(p.Evidence.SourceURI) > 2048 {
		return ErrInvalidCalibration
	}
	if p.WindowStart.IsZero() || !p.WindowEnd.After(p.WindowStart) || p.Evidence.CreatedAt.Before(p.WindowEnd) || !p.ExpiresAt.After(p.WindowEnd) {
		return ErrInvalidCalibration
	}
	if !calibrationDigests(p.DiscoveryItems, false) || !calibrationDigests(p.HoldoutItems, true) {
		return ErrInvalidCalibration
	}
	total := int64(0)
	for _, count := range []int{p.Successes, p.Failures, p.Missing, p.GraderErrors, p.Cancelled} {
		if count < 0 || count > 1_000_000_000 {
			return ErrInvalidCalibration
		}
		total += int64(count)
	}
	if p.Trials < 0 || p.Trials > 1_000_000_000 || int64(p.Trials) != total {
		return ErrInvalidCalibration
	}
	interval := p.Uncertainty
	if !calibrationProbability(p.Estimate) || !calibrationProbability(interval.Lower) || !calibrationProbability(interval.Upper) || !calibrationProbability(interval.Level) || interval.Level == 0 || interval.Level == 1 || interval.Lower > p.Estimate || interval.Upper < p.Estimate || !contextText(interval.MethodRevision, 256) || (interval.Kind != "confidence" && interval.Kind != "credible") {
		return ErrInvalidCalibration
	}
	return nil
}

func calibrationProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func calibrationDigests(values []string, required bool) bool {
	if len(values) > 4096 || (required && len(values) == 0) {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !evidence.ValidDigest(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func calibrationOverlap(discovery, holdout []string) bool {
	seen := make(map[string]bool, len(discovery))
	for _, item := range discovery {
		seen[item] = true
	}
	for _, item := range holdout {
		if seen[item] {
			return true
		}
	}
	return false
}

func sameCalibrationBinding(a, b CalibrationBinding) bool {
	if a.Scope.Canonical() != b.Scope.Canonical() || a.TaskClass != b.TaskClass || a.ProviderRevision != b.ProviderRevision || a.ModelRevision != b.ModelRevision || a.PromptRevision != b.PromptRevision || a.ControllerRevision != b.ControllerRevision || a.PolicyRevision != b.PolicyRevision || a.EnvironmentRevision != b.EnvironmentRevision || a.AcceptanceDigest != b.AcceptanceDigest || len(a.Tools) != len(b.Tools) {
		return false
	}
	tools := make(map[string]string, len(a.Tools))
	for _, tool := range a.Tools {
		tools[tool.Name] = tool.Revision
	}
	for _, tool := range b.Tools {
		if tools[tool.Name] != tool.Revision {
			return false
		}
	}
	return true
}
