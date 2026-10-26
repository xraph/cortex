package cognitive_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func reflectionInput() cognitive.ReflectionSnapshot {
	return cognitive.ReflectionSnapshot{Binding: cognitive.ReflectionBinding{Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "reflection"}}}, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), PreparedDigest: evidence.Digest([]byte("prepared")), Budget: execution.Budget{MaxSteps: 8, MaxTokens: 1000}}, ObservedAt: time.Now().UTC(), Step: 3, NoProgressSteps: 3, ProgressDigest: evidence.Digest([]byte("progress"))}
}
func TestOnStuckTriggersOneBoundedReflection(t *testing.T) {
	s := reflectionInput()
	p := cognitive.ReflectionPolicy{Revision: "controller/v1", OnStuck: cognitive.ActionReplan}
	d := cognitive.Decide(s, p, cognitive.ReflectionState{})
	require.Equal(t, cognitive.ActionReplan, d.Action)
	require.True(t, d.Reflect)
	next := cognitive.ReflectionState{Reflections: 1, LastEvidenceDigest: d.EvidenceDigest}
	d = cognitive.Decide(s, p, next)
	require.Equal(t, cognitive.ActionStop, d.Action)
	require.Equal(t, cognitive.ReflectionNoNewEvidence, d.Reason)
}
func TestReflectionCannotRaiseBudget(t *testing.T) {
	s := reflectionInput()
	s.Binding.Budget.MaxTokens = 1
	s.Usage.Tokens = 1
	d := cognitive.Decide(s, cognitive.ReflectionPolicy{Revision: "v1", OnStuck: cognitive.ActionReplan}, cognitive.ReflectionState{})
	require.Equal(t, cognitive.ActionStop, d.Action)
	require.Equal(t, cognitive.ReflectionBudget, d.Reason)
}
func TestReflectionCountCannotBeResetByProgress(t *testing.T) {
	s := reflectionInput()
	d := cognitive.Decide(s, cognitive.ReflectionPolicy{Revision: "v1"}, cognitive.ReflectionState{Reflections: 2})
	require.Equal(t, cognitive.ActionStop, d.Action)
	require.Equal(t, cognitive.ReflectionLimit, d.Reason)
}
func TestUnsupportedStrategyRejected(t *testing.T) {
	_, err := (cognitive.ReflectionPolicy{Revision: "v1", Phases: []cognitive.Phase{{Strategy: cognitive.StrategyCreative}}}).Normalize()
	require.ErrorIs(t, err, cognitive.ErrUnsupportedReflection)
	_, err = (cognitive.ReflectionPolicy{Revision: "v1", OnStuck: cognitive.ActionDelegate}).Normalize()
	require.ErrorIs(t, err, cognitive.ErrUnsupportedReflection)
}
func TestReflectionTransitionsFromRetainedConditions(t *testing.T) {
	for _, condition := range []cognitive.TransitionCondition{cognitive.TransitionAfterSteps, cognitive.TransitionOnStuck, cognitive.TransitionOnPlanComplete, cognitive.TransitionOnError} {
		t.Run(string(condition), func(t *testing.T) {
			s := reflectionInput()
			s.PlanComplete = true
			s.HadError = true
			p := cognitive.ReflectionPolicy{Revision: "v1", Phases: []cognitive.Phase{{Strategy: cognitive.StrategyReactive, MaxSteps: 2, Transition: condition}, {Strategy: cognitive.StrategyReflective}}}
			d := cognitive.Decide(s, p, cognitive.ReflectionState{})
			require.Equal(t, 1, d.Phase)
			require.True(t, d.PhaseChanged)
		})
	}
}
func TestModelAssumptionCannotCertifyItself(t *testing.T) {
	s := reflectionInput()
	s.Assumptions = []cognitive.Assumption{{Key: "exists", Summary: "resource exists", Origin: cognitive.AssumptionModel, State: cognitive.AssumptionSupported}}
	require.ErrorIs(t, s.Validate(), cognitive.ErrInvalidReflection)
}

func TestReflectionWithoutNewEvidenceStops(t *testing.T) {
	s := reflectionInput()
	s.Assumptions = []cognitive.Assumption{{Key: "possibility", Summary: "first hypothesis", Origin: cognitive.AssumptionModel, State: cognitive.AssumptionUnresolved}}
	first := cognitive.Decide(s, cognitive.ReflectionPolicy{Revision: "v1"}, cognitive.ReflectionState{})
	s.Assumptions[0].Summary = "different model phrasing"
	next := cognitive.Decide(s, cognitive.ReflectionPolicy{Revision: "v1"}, cognitive.ReflectionState{Reflections: 1, LastEvidenceDigest: first.EvidenceDigest})
	require.Equal(t, cognitive.ReflectionNoNewEvidence, next.Reason)
}
func TestServiceObservationExpiresToUnknown(t *testing.T) {
	s := reflectionInput()
	now := s.ObservedAt
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: s.Binding.Scope, MediaType: "application/json", Version: "v1", Producer: "health-monitor", Sensitivity: evidence.SensitivityInternal, CreatedAt: now.Add(-time.Second), RetainUntil: now.Add(time.Hour)}, Data: []byte(`{"reachable":true}`)})
	require.NoError(t, err)
	health := cognitive.ServiceObservation{Service: "provider", Health: cognitive.HealthHealthy, ObservedAt: now, ExpiresAt: now.Add(time.Second), Evidence: a.Ref}
	require.Equal(t, cognitive.HealthHealthy, health.CurrentHealth(now))
	require.Equal(t, cognitive.HealthUnknown, health.CurrentHealth(now.Add(time.Second)))
}

func TestReflectionStrategiesHaveConcreteActions(t *testing.T) {
	s := reflectionInput()
	p := cognitive.ReflectionPolicy{Revision: "v1", OnStuck: cognitive.ActionReplan, Phases: []cognitive.Phase{{Strategy: cognitive.StrategyReactive}}}
	require.Equal(t, cognitive.ActionStop, cognitive.Decide(s, p, cognitive.ReflectionState{}).Action)
	p.Phases[0].Strategy = cognitive.StrategyReflective
	require.Equal(t, cognitive.ActionReplan, cognitive.Decide(s, p, cognitive.ReflectionState{}).Action)
	p.Phases[0].Strategy = cognitive.StrategyAnalytical
	s.NoProgressSteps = 0
	s.Assumptions = []cognitive.Assumption{{Key: "hypothesis", Summary: "Not yet checked", Origin: cognitive.AssumptionModel, State: cognitive.AssumptionUnresolved}}
	decision := cognitive.Decide(s, p, cognitive.ReflectionState{})
	require.Equal(t, cognitive.ActionStop, decision.Action)
	require.Equal(t, cognitive.ReflectionInsufficientEvidence, decision.Reason)
}

func TestServiceObservationRejectsFutureEvidence(t *testing.T) {
	s := reflectionInput()
	now := s.ObservedAt
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: s.Binding.Scope, MediaType: "application/json", Version: "v1", Producer: "health-monitor", Sensitivity: evidence.SensitivityInternal, CreatedAt: now.Add(time.Second), RetainUntil: now.Add(time.Hour)}, Data: []byte(`{"reachable":true}`)})
	require.NoError(t, err)
	health := cognitive.ServiceObservation{Service: "provider", Health: cognitive.HealthHealthy, ObservedAt: now, ExpiresAt: now.Add(time.Minute), Evidence: a.Ref}
	require.Equal(t, cognitive.HealthUnknown, health.CurrentHealth(now))
}
