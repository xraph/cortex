package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

type runtimeCalibration struct {
	t      *testing.T
	calls  int
	change func(*engine.OperationalCalibrationRequest, *engine.OperationalCalibrationObservation) error
}

func (p *runtimeCalibration) ObserveCalibration(_ context.Context, r engine.OperationalCalibrationRequest) (engine.OperationalCalibrationObservation, error) {
	p.calls++
	now := time.Now().UTC()
	binding := cognitive.CalibrationBinding{Scope: r.Scope, TaskClass: "coding", ProviderRevision: "provider:v1", ModelRevision: r.ModelRevision, PromptRevision: r.PromptDigest, ControllerRevision: "controller:v1", PolicyRevision: r.PolicyDigest, EnvironmentRevision: "environment:v1", AcceptanceDigest: r.AcceptanceDigest, Tools: r.Tools}
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: r.Scope, Version: "1", Producer: "evaluator", MediaType: "application/json", Sensitivity: evidence.SensitivityInternal, CreatedAt: now.Add(-time.Hour), RetainUntil: now.Add(time.Hour)}, Data: []byte("retained cohort")})
	require.NoError(p.t, err)
	result := engine.OperationalCalibrationObservation{Binding: binding, Observation: &cognitive.CalibrationObservation{Origin: cognitive.CalibrationMeasured, ObservedAt: now, Profile: cognitive.CalibrationProfile{SchemaVersion: 1, Revision: "cohort:v1", EvaluatorRevision: "grader:v1", Evidence: artifact.Ref, Binding: binding, DatasetDigest: evidence.Digest([]byte("dataset")), HoldoutItems: []string{evidence.Digest([]byte("holdout"))}, WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Trials: 100, Successes: 80, Failures: 10, Missing: 10, Estimate: .8, Uncertainty: cognitive.CalibrationInterval{Kind: "confidence", Lower: .7, Upper: .9, Level: .95, MethodRevision: "wilson:v1"}}}}
	if p.change != nil {
		err = p.change(&r, &result)
	}
	return result, err
}

func TestOperationalCalibrationRequiresCurrentMeasuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		change               func(*engine.OperationalCalibrationRequest, *engine.OperationalCalibrationObservation) error
	}{
		{name: "matching", status: "measured"},
		{name: "provider failure", status: "unknown", reason: "unavailable", change: func(_ *engine.OperationalCalibrationRequest, _ *engine.OperationalCalibrationObservation) error {
			return errors.New("private adapter detail")
		}},
		{name: "stale model", status: "unknown", reason: "incompatible", change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
			o.Binding.ModelRevision = "old"
			o.Observation.Profile.Binding.ModelRevision = "old"
			return nil
		}},
		{name: "stale prompt", status: "unknown", reason: "incompatible", change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
			o.Binding.PromptRevision = "old"
			return nil
		}},
		{name: "weaker criteria", status: "unknown", reason: "incompatible", change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
			o.Binding.AcceptanceDigest = evidence.Digest([]byte("weak"))
			return nil
		}},
		{name: "mutated request", status: "unknown", reason: "incompatible", change: func(r *engine.OperationalCalibrationRequest, _ *engine.OperationalCalibrationObservation) error {
			r.Scope.Levels[0].Value = "other"
			r.Tools[0].Revision = "changed"
			return nil
		}},
		{name: "revoked", status: "unknown", reason: "revoked", change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
			o.Observation.Revoked = true
			return nil
		}},
		{name: "self confidence", status: "unknown", reason: "untrusted_origin", change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
			o.Observation.Origin = "model"
			return nil
		}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/sync", true: "/stream"}[stream], func(t *testing.T) {
				f := newWorkerFixture(t)
				provider := &runtimeCalibration{t: t, change: tc.change}
				client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
				e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithOperationalAwareness(engine.OperationalAwarenessConfig{Calibration: provider, Policy: cognitive.CalibrationPolicy{MinimumTrials: 50, MaxAge: 24 * time.Hour}}))
				ag, overrides := requiredToolAgent(t, f)
				overrides.Tools = []string{engine.ToolOperationalCapabilities}
				if stream {
					events := make(chan engine.StreamEvent, 32)
					require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "inspect", overrides, events))
					done := false
					for event := range events {
						done = done || event.Type == engine.EventDone
					}
					require.True(t, done)
				} else {
					_, err := e.RunAgent(f.ctx, ag.Name, "inspect", overrides)
					require.NoError(t, err)
				}
				require.Equal(t, 1, provider.calls)
				snap := capabilityResult(t, client.last)
				require.Equal(t, tc.status, snap.Reliability)
				var result struct {
					Measurement cognitive.ReliabilityMeasurement `json:"measurement"`
				}
				for _, m := range client.last.Messages {
					if m.Role == "tool" {
						require.NoError(t, json.Unmarshal([]byte(m.Content), &result))
						require.NotContains(t, m.Content, "private adapter detail")
					}
				}
				require.Equal(t, tc.status, result.Measurement.Status)
				require.Equal(t, tc.reason, result.Measurement.Reason)
				if tc.status == "measured" {
					require.NotNil(t, result.Measurement.Cohort)
					require.Equal(t, 10, result.Measurement.Cohort.Missing)
				} else {
					require.Nil(t, result.Measurement.Cohort)
				}
			})
		}
	}
}

func TestOperationalCalibrationConfigurationRejectsIncompletePorts(t *testing.T) {
	var missing *runtimeCalibration
	for _, c := range []engine.OperationalAwarenessConfig{{Calibration: missing, Policy: cognitive.CalibrationPolicy{MinimumTrials: 1, MaxAge: time.Hour}}, {Calibration: &runtimeCalibration{}, Policy: cognitive.CalibrationPolicy{MinimumTrials: 0, MaxAge: time.Hour}}} {
		_, err := engine.New(engine.WithOperationalAwareness(c))
		require.Error(t, err)
	}
	_, err := engine.New(engine.WithOperationalAwareness(engine.OperationalAwarenessConfig{}, engine.OperationalAwarenessConfig{}))
	require.Error(t, err)
}

func TestOperationalCalibrationDoesNotReadEvidenceWithoutAuthorizationOrPins(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing criteria", true: "denied call"}[denied], func(t *testing.T) {
			ctx := overlayCtx("calibration-gate")
			s := newSQLiteStoreForTest(t)
			p := &runtimeCalibration{t: t}
			client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
			e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithToolAuthorizer(&capabilityAuthority{denied: denied}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithOperationalAwareness(engine.OperationalAwarenessConfig{Calibration: p, Policy: cognitive.CalibrationPolicy{MinimumTrials: 50, MaxAge: time.Hour}}))
			require.NoError(t, err)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "calibration"}
			require.NoError(t, s.Create(ctx, ag))
			_, err = e.RunAgent(ctx, ag.Name, "inspect", nil)
			require.NoError(t, err)
			require.Zero(t, p.calls)
			if !denied {
				require.Equal(t, "unknown", capabilityResult(t, client.last).Reliability)
			}
		})
	}
}

func TestOperationalCalibrationUsesLiveVisibilityAndCallerDeadline(t *testing.T) {
	f := newWorkerFixture(t)
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(f.ctx, deadline)
	defer cancel()
	auth := &capabilityAuthority{}
	client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}, before: func() { auth.hideProbe = true }}
	provider := &runtimeCalibration{t: t, change: func(r *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
		require.True(t, r.Deadline.Equal(deadline), "adapter must see the effective caller deadline")
		require.Equal(t, []cognitive.CalibrationTool{{Name: engine.ToolOperationalCapabilities, Revision: "cortex.capabilities/v2"}}, r.Tools)
		// A stale larger cohort must not re-disclose withdrawn tools.
		o.Binding.Tools = append(o.Binding.Tools, cognitive.CalibrationTool{Name: "probe", Revision: "1"})
		o.Observation.Profile.Binding.Tools = o.Binding.Tools
		return nil
	}}
	e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithTool(namedTool("probe"), noopHandler), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithOperationalAwareness(engine.OperationalAwarenessConfig{Calibration: provider, Policy: cognitive.CalibrationPolicy{MinimumTrials: 50, MaxAge: time.Hour * 24}}))
	ag, overrides := requiredToolAgent(t, f)
	overrides.Tools = []string{engine.ToolOperationalCapabilities, "probe"}
	_, err := e.RunAgent(ctx, ag.Name, "inspect", overrides)
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)
	require.Equal(t, "unknown", capabilityResult(t, client.last).Reliability)
	for _, m := range client.last.Messages {
		if m.Role == "tool" {
			require.NotContains(t, m.Content, "probe")
		}
	}
}

func TestOperationalCalibrationOversizePreservesCapabilityReport(t *testing.T) {
	f := newWorkerFixture(t)
	client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
	provider := &runtimeCalibration{t: t, change: func(_ *engine.OperationalCalibrationRequest, o *engine.OperationalCalibrationObservation) error {
		for i := range 4096 {
			o.Observation.Profile.DiscoveryItems = append(o.Observation.Profile.DiscoveryItems, evidence.Digest([]byte(fmt.Sprint("discovery", i))))
		}
		return nil
	}}
	e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithOperationalAwareness(engine.OperationalAwarenessConfig{Calibration: provider, Policy: cognitive.CalibrationPolicy{MinimumTrials: 50, MaxAge: 24 * time.Hour}}))
	ag, overrides := requiredToolAgent(t, f)
	overrides.Tools = []string{engine.ToolOperationalCapabilities}
	_, err := e.RunAgent(f.ctx, ag.Name, "inspect", overrides)
	require.NoError(t, err)
	snap := capabilityResult(t, client.last)
	require.Equal(t, "unknown", snap.Reliability)
	require.Len(t, snap.Tools, 1)
	for _, m := range client.last.Messages {
		if m.Role == "tool" {
			require.Contains(t, m.Content, "cohort_too_large")
			require.Less(t, len(m.Content), 128<<10)
		}
	}
}
