package cognitive_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func calibrationFixture(t *testing.T) (cognitive.CalibrationBinding, cognitive.CalibrationObservation, cognitive.CalibrationPolicy, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w1"}}}
	binding := cognitive.CalibrationBinding{Scope: scope, TaskClass: "coding", ProviderRevision: "provider:1", ModelRevision: "model:1", PromptRevision: "prompt:1", ControllerRevision: "controller:1", PolicyRevision: "policy:1", EnvironmentRevision: "environment:1", AcceptanceDigest: evidence.Digest([]byte("required checks")), Tools: []cognitive.CalibrationTool{{Name: "tests", Revision: "v1"}}}
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "v1", Producer: "sentinel", MediaType: "application/json", Sensitivity: evidence.SensitivityInternal, CreatedAt: now.Add(-time.Hour), RetainUntil: now.Add(24 * time.Hour)}, Data: []byte("captured cohort")})
	require.NoError(t, err)
	observation := cognitive.CalibrationObservation{Origin: cognitive.CalibrationMeasured, ObservedAt: now, Profile: cognitive.CalibrationProfile{SchemaVersion: 1, Revision: "cohort:1", EvaluatorRevision: "evaluator:1", Evidence: artifact.Ref, Binding: binding, DatasetDigest: evidence.Digest([]byte("dataset")), DiscoveryItems: []string{evidence.Digest([]byte("discovery"))}, HoldoutItems: []string{evidence.Digest([]byte("holdout"))}, WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Trials: 100, Successes: 70, Failures: 10, Missing: 10, GraderErrors: 5, Cancelled: 5, Estimate: .7, Uncertainty: cognitive.CalibrationInterval{Kind: "confidence", Lower: .6, Upper: .8, Level: .95, MethodRevision: "sentinel-method:1"}}}
	return binding, observation, cognitive.CalibrationPolicy{MinimumTrials: 50, MaxAge: 24 * time.Hour}, now
}

func TestCompatibleMeasuredCohortPreservesCountsAndUncertainty(t *testing.T) {
	binding, observation, policy, now := calibrationFixture(t)
	result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
	require.NoError(t, err)
	require.Equal(t, "measured", result.Status)
	require.Equal(t, observation.Profile, *result.Cohort)
	require.Equal(t, 10, result.Cohort.Missing, "missing trials remain in the denominator")
	require.Equal(t, .7, result.Cohort.Estimate, "the adapter preserves the external measurement")
	observation.Profile.Binding.Tools[0].Revision = "mutated"
	observation.Profile.Binding.Scope.Levels[0].Value = "mutated"
	observation.Profile.Evidence.Scope.Levels[0].Value = "mutated"
	observation.Profile.HoldoutItems[0] = "mutated"
	require.Equal(t, "v1", result.Cohort.Binding.Tools[0].Revision)
	require.Equal(t, "w1", result.Cohort.Binding.Scope.Levels[0].Value)
	require.Equal(t, "w1", result.Cohort.Evidence.Scope.Levels[0].Value)
	require.True(t, evidence.ValidDigest(result.Cohort.HoldoutItems[0]))
}

func TestCalibrationCannotReplaceUnknownWithUnusableEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*cognitive.CalibrationObservation)
	}{
		{"sparse", "insufficient_samples", func(o *cognitive.CalibrationObservation) {
			o.Profile.Trials = 1
			o.Profile.Successes = 1
			o.Profile.Failures = 0
			o.Profile.Missing = 0
			o.Profile.GraderErrors = 0
			o.Profile.Cancelled = 0
		}},
		{"expired", "expired", func(o *cognitive.CalibrationObservation) { o.Profile.ExpiresAt = o.ObservedAt }},
		{"stale", "stale", func(o *cognitive.CalibrationObservation) {
			o.Profile.WindowStart = o.ObservedAt.Add(-48 * time.Hour)
			o.Profile.WindowEnd = o.ObservedAt.Add(-25 * time.Hour)
		}},
		{"revoked", "revoked", func(o *cognitive.CalibrationObservation) { o.Revoked = true }},
		{"leaked", "leaked_cohort", func(o *cognitive.CalibrationObservation) { o.Leaked = true }},
		{"overlap", "leaked_cohort", func(o *cognitive.CalibrationObservation) { o.Profile.HoldoutItems[0] = o.Profile.DiscoveryItems[0] }},
		{"model self score", "untrusted_origin", func(o *cognitive.CalibrationObservation) {
			o.Origin = "model"
			o.Profile.Estimate = .99
			o.Profile.Uncertainty.Upper = 1
		}},
		{"evidence retention", "expired", func(o *cognitive.CalibrationObservation) { o.Profile.Evidence.RetainUntil = o.ObservedAt }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding, observation, policy, now := calibrationFixture(t)
			tc.change(&observation)
			result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
			require.NoError(t, err)
			require.Equal(t, "unknown", result.Status)
			require.Equal(t, tc.reason, result.Reason)
			require.Nil(t, result.Cohort)
		})
	}
}

func TestCalibrationRequiresEveryExactBindingPin(t *testing.T) {
	for name, change := range map[string]func(*cognitive.CalibrationBinding){
		"task":        func(b *cognitive.CalibrationBinding) { b.TaskClass = "research" },
		"provider":    func(b *cognitive.CalibrationBinding) { b.ProviderRevision = "provider:2" },
		"model":       func(b *cognitive.CalibrationBinding) { b.ModelRevision = "model:2" },
		"prompt":      func(b *cognitive.CalibrationBinding) { b.PromptRevision = "prompt:2" },
		"controller":  func(b *cognitive.CalibrationBinding) { b.ControllerRevision = "controller:2" },
		"policy":      func(b *cognitive.CalibrationBinding) { b.PolicyRevision = "policy:2" },
		"environment": func(b *cognitive.CalibrationBinding) { b.EnvironmentRevision = "environment:2" },
		"acceptance":  func(b *cognitive.CalibrationBinding) { b.AcceptanceDigest = evidence.Digest([]byte("weaker")) },
		"tools": func(b *cognitive.CalibrationBinding) {
			b.Tools = []cognitive.CalibrationTool{{Name: "tests", Revision: "v2"}}
		},
		"scope": func(b *cognitive.CalibrationBinding) {
			b.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w2"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			binding, observation, policy, now := calibrationFixture(t)
			change(&binding)
			result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
			require.NoError(t, err)
			require.Equal(t, "unknown", result.Status)
			require.Equal(t, "incompatible", result.Reason)
			require.Nil(t, result.Cohort)
		})
	}
}

func TestCalibrationRejectsMalformedMeasuredCohorts(t *testing.T) {
	for name, change := range map[string]func(*cognitive.CalibrationObservation){
		"schema":              func(o *cognitive.CalibrationObservation) { o.Profile.SchemaVersion = 2 },
		"negative count":      func(o *cognitive.CalibrationObservation) { o.Profile.Missing = -1 },
		"missing denominator": func(o *cognitive.CalibrationObservation) { o.Profile.Trials = 90 },
		"huge count":          func(o *cognitive.CalibrationObservation) { o.Profile.Trials = math.MaxInt },
		"nan":                 func(o *cognitive.CalibrationObservation) { o.Profile.Estimate = math.NaN() },
		"infinite interval":   func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Upper = math.Inf(1) },
		"reversed interval":   func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Lower = .9 },
		"outside interval":    func(o *cognitive.CalibrationObservation) { o.Profile.Estimate = .9 },
		"invalid level":       func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Level = 1 },
		"missing method":      func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.MethodRevision = "" },
		"future observation":  func(o *cognitive.CalibrationObservation) { o.ObservedAt = o.ObservedAt.Add(time.Second) },
		"future window":       func(o *cognitive.CalibrationObservation) { o.Profile.WindowEnd = o.ObservedAt.Add(time.Second) },
		"missing window":      func(o *cognitive.CalibrationObservation) { o.Profile.WindowStart = time.Time{} },
		"empty holdout":       func(o *cognitive.CalibrationObservation) { o.Profile.HoldoutItems = nil },
		"duplicate holdout": func(o *cognitive.CalibrationObservation) {
			o.Profile.HoldoutItems = append(o.Profile.HoldoutItems, o.Profile.HoldoutItems[0])
		},
		"invalid dataset": func(o *cognitive.CalibrationObservation) { o.Profile.DatasetDigest = "not a digest" },
		"missing pin":     func(o *cognitive.CalibrationObservation) { o.Profile.Binding.ModelRevision = "" },
		"duplicate tool": func(o *cognitive.CalibrationObservation) {
			o.Profile.Binding.Tools = append(o.Profile.Binding.Tools, o.Profile.Binding.Tools[0])
		},
		"wrong evidence scope": func(o *cognitive.CalibrationObservation) {
			o.Profile.Evidence.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			binding, observation, policy, now := calibrationFixture(t)
			change(&observation)
			result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
			require.ErrorIs(t, err, cognitive.ErrInvalidCalibration)
			require.Nil(t, result.Cohort)
		})
	}
}

func TestCalibrationUnknownWithoutObservationAndRejectsInvalidPolicy(t *testing.T) {
	binding, _, policy, now := calibrationFixture(t)
	result, err := cognitive.ReliabilityFor(binding, nil, policy, now)
	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Nil(t, result.Cohort)
	policy.MinimumTrials = 0
	_, err = cognitive.ReliabilityFor(binding, nil, policy, now)
	require.ErrorIs(t, err, cognitive.ErrInvalidCalibration)
	policy.MinimumTrials = 1
	policy.MaxAge = 0
	_, err = cognitive.ReliabilityFor(binding, nil, policy, now)
	require.ErrorIs(t, err, cognitive.ErrInvalidCalibration)
}

func TestCalibrationMissingTrialsCannotPadMinimumSamples(t *testing.T) {
	binding, observation, policy, now := calibrationFixture(t)
	observation.Profile.Successes = 10
	observation.Profile.Failures = 0
	observation.Profile.Missing = 80
	result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, "insufficient_samples", result.Reason)
	require.Nil(t, result.Cohort)
}

func TestCalibrationToolOrderDoesNotChangeCompatibility(t *testing.T) {
	binding, observation, policy, now := calibrationFixture(t)
	binding.Tools = []cognitive.CalibrationTool{{Name: "tests", Revision: "v1"}, {Name: "search", Revision: "v2"}}
	observation.Profile.Binding.Tools = []cognitive.CalibrationTool{{Name: "search", Revision: "v2"}, {Name: "tests", Revision: "v1"}}
	result, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
	require.NoError(t, err)
	require.Equal(t, "measured", result.Status)
	binding.Tools = binding.Tools[:1]
	result, err = cognitive.ReliabilityFor(binding, &observation, policy, now)
	require.NoError(t, err)
	require.Equal(t, "incompatible", result.Reason)
}

func TestCalibrationRejectsUnboundedAndIncompleteInputs(t *testing.T) {
	for name, change := range map[string]func(*cognitive.CalibrationObservation){
		"missing evaluator":     func(o *cognitive.CalibrationObservation) { o.Profile.EvaluatorRevision = "" },
		"unknown interval kind": func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Kind = "model confidence" },
		"negative interval":     func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Lower = -.1 },
		"zero level":            func(o *cognitive.CalibrationObservation) { o.Profile.Uncertainty.Level = 0 },
		"unbounded revision":    func(o *cognitive.CalibrationObservation) { o.Profile.Revision = string(make([]byte, 257)) },
		"unbounded tools": func(o *cognitive.CalibrationObservation) {
			o.Profile.Binding.Tools = make([]cognitive.CalibrationTool, 257)
		},
		"unbounded holdout": func(o *cognitive.CalibrationObservation) { o.Profile.HoldoutItems = make([]string, 4097) },
		"future evidence": func(o *cognitive.CalibrationObservation) {
			o.Profile.Evidence.CreatedAt = o.ObservedAt.Add(time.Second)
		},
		"missing observation time": func(o *cognitive.CalibrationObservation) { o.ObservedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			binding, observation, policy, now := calibrationFixture(t)
			change(&observation)
			_, err := cognitive.ReliabilityFor(binding, &observation, policy, now)
			require.ErrorIs(t, err, cognitive.ErrInvalidCalibration)
		})
	}
}
