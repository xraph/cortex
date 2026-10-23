package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
)

type trialFixtureFactory struct {
	t            *testing.T
	bundle       optimization.ExperimentBundle
	environments map[id.ID]*trialFixtureEnvironment
	calls        int
	lost         bool
	stopUnknown  bool
	stops        int
	sessions     map[id.ID]bool
}
type trialFixtureEnvironment struct {
	factory   *trialFixtureFactory
	receipt   optimization.EnvironmentReceipt
	execution *optimization.ExecutionRecord
}

func (f *trialFixtureFactory) Open(_ context.Context, r optimization.TrialRequest) (optimization.TrialEnvironment, error) {
	if e := f.environments[r.AttemptID]; e != nil {
		return e, nil
	}
	e := &trialFixtureEnvironment{factory: f, receipt: optimization.EnvironmentReceipt{ID: r.AttemptID.String(), Provider: "test-only", EnvironmentID: r.AttemptID.String(), Scope: r.Scope, AttemptID: r.AttemptID, Snapshot: r.Task.EnvironmentSnapshot, Fence: r.Fence, Capabilities: optimization.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}, CreatedAt: time.Now().UTC()}}
	f.environments[r.AttemptID] = e
	return e, nil
}
func (f *trialFixtureFactory) Recover(_ context.Context, r optimization.EnvironmentReceipt) (optimization.TrialEnvironment, error) {
	e := f.environments[r.AttemptID]
	if e == nil {
		return nil, optimization.ErrUnknownAttempt
	}
	return e, nil
}
func (f *trialFixtureFactory) Stop(_ context.Context, r optimization.EnvironmentReceipt, fence optimization.Fence) (optimization.StopReceipt, error) {
	f.stops++
	state := optimization.StopTerminated
	if f.stopUnknown {
		state = optimization.StopUnknown
	}
	return optimization.StopReceipt{EnvironmentReceiptID: r.ID, AttemptID: r.AttemptID, Fence: fence, State: state, Evidence: optimization.ContentRef{Ref: "fixture-stop", Revision: "1", Digest: strings.Repeat("a", 64)}}, nil
}
func (e *trialFixtureEnvironment) Receipt() optimization.EnvironmentReceipt { return e.receipt }
func (e *trialFixtureEnvironment) Close(context.Context) error              { return nil }
func (e *trialFixtureEnvironment) Inspect(context.Context, id.ID) (optimization.AttemptStatus, error) {
	return optimization.AttemptStatus{AttemptID: e.receipt.AttemptID, State: optimization.TrialCompleted, Execution: e.execution, ObservedAt: time.Now().UTC()}, nil
}
func (e *trialFixtureEnvironment) Execute(_ context.Context, p *engine.PreparedAgent, input string, opts engine.PreparedRunOptions) (*optimization.ExecutionRecord, error) {
	f := e.factory
	f.calls++
	if opts.SessionID.IsNil() || f.sessions[opts.SessionID] || opts.Purpose != run.PurposeOptimizationTrial {
		f.t.Fatal("trial session or purpose was not isolated")
	}
	f.sessions[opts.SessionID] = true
	record := capturedExecution(f.t, f.bundle, e.receipt)
	record.PreparedDigest = p.Digest()
	record.Outcome.Output = input
	for _, task := range f.bundle.Experiment.Spec.Tasks {
		if task.Input == input {
			record.InputDigest = task.InputDigest
		}
	}
	sealed, err := optimization.SealExecution(record)
	if err != nil {
		return nil, err
	}
	e.execution = &sealed
	if f.lost {
		f.lost = false
		return nil, errors.New("lost execution response")
	}
	return &sealed, nil
}

type trialFixtureEvaluator struct{ calls int }

func (*trialFixtureEvaluator) Freeze(context.Context, optimization.EvaluationSpec) (optimization.EvaluationPlan, error) {
	return optimization.EvaluationPlan{}, optimization.ErrUnavailable
}
func (*trialFixtureEvaluator) Compare(context.Context, optimization.ComparisonRequest) (optimization.Comparison, error) {
	return optimization.Comparison{}, optimization.ErrUnavailable
}
func (e *trialFixtureEvaluator) Evaluate(_ context.Context, r optimization.EvaluationRequest) (optimization.Evaluation, error) {
	if r.Lease.ExperimentID != r.ExperimentID || r.Lease.Owner == "" || r.Lease.Generation < 1 || !time.Now().Before(r.Lease.ExpiresAt) {
		return optimization.Evaluation{}, optimization.ErrConflict
	}
	e.calls++
	v := 1.0
	now := time.Now().UTC()
	return optimization.Evaluation{ID: fmt.Sprint("job/", r.TrialID), ReportID: fmt.Sprint("report/", r.TrialID), ExperimentID: r.ExperimentID, TrialID: r.TrialID, CaseID: r.CaseID, CaseDigest: r.CaseDigest, Repetition: r.Repetition, PlanDigest: r.Plan.Ref.Digest, ExecutionDigest: r.Execution.Digest, Scorers: r.Plan.Scorers, Criteria: []optimization.CriterionValue{{ID: "quality", Score: &v, Passed: true}}, Score: &v, Complete: true, StartedAt: now, CompletedAt: now}, nil
}

func trialAdapters(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("TrialAdapterRecoversWithoutReplay", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "adapter")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		environments := &trialFixtureFactory{t: t, bundle: bundle, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}, lost: true, stopUnknown: true}
		evaluator := &trialFixtureEvaluator{}
		authority := controllerAuthority{actor: bundle.Experiment.Spec.Requester}
		handler, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Environments: environments, Evaluator: evaluator, Allowance: optimization.TrialAllowance{MaxTokens: 100}})
		if err != nil {
			t.Fatal(err)
		}
		optimizer, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		created, err := optimizer.Create(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = optimizer.Start(ctx, created.ID, created.Revision); err != nil {
			t.Fatal(err)
		}
		recovered := false
		ambiguous := false
		for range 30 {
			err = optimizer.WorkOnce(ctx, created.ID)
			if err != nil && !errors.Is(err, optimization.ErrUnknownAttempt) {
				t.Fatal(err)
			}
			current, getErr := s.GetExperiment(ctx, created.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if errors.Is(err, optimization.ErrUnknownAttempt) && recovered && environments.stopUnknown {
				if environments.calls != 1 {
					t.Fatal("ambiguous stop caused replay")
				}
				environments.stopUnknown = false
				ambiguous = true
			}
			if errors.Is(err, optimization.ErrUnknownAttempt) && !recovered {
				paused, pauseErr := optimizer.Pause(ctx, created.ID, current.Revision)
				if pauseErr != nil {
					t.Fatal(pauseErr)
				}
				if _, resumeErr := optimizer.Resume(ctx, created.ID, paused.Revision); resumeErr != nil {
					t.Fatal(resumeErr)
				}
				optimizer, err = optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
				if err != nil {
					t.Fatal(err)
				}
				recovered = true
			}
			if current.Phase == optimization.PhasePropose {
				break
			}
		}
		current, err := s.GetExperiment(ctx, created.ID)
		if err != nil || current.Phase != optimization.PhasePropose {
			t.Fatalf("baseline did not finish: %+v %v", current, err)
		}
		if !ambiguous || environments.stops < environments.calls || environments.calls != bundle.Experiment.Spec.Limits.Repetitions || evaluator.calls != environments.calls {
			t.Fatalf("replay or missing evaluation: executions=%d evaluations=%d", environments.calls, evaluator.calls)
		}
		trials, err := s.ListTrials(ctx, created.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 100}})
		if err != nil {
			t.Fatal(err)
		}
		for _, trial := range trials.Items {
			if trial.Execution == nil || trial.EvaluationID == "" || len(trial.Attempts) != 1 {
				t.Fatalf("missing durable evidence: %+v", trial)
			}
		}
	})
}

type noCandidateHandler struct {
	trials *optimization.TrialPhaseHandler
}

func (h noCandidateHandler) Execute(ctx context.Context, item optimization.WorkItem) (optimization.Transition, error) {
	if item.Experiment.Phase == optimization.PhasePropose {
		return optimization.Transition{NextState: optimization.StateRunning, NextPhase: optimization.PhaseProbe, Candidates: []optimization.Candidate{{ID: id.New(optimization.PrefixCandidate), ExperimentID: item.Experiment.ID, Origin: optimization.OriginGeneral, Round: item.Experiment.Round, Rejection: "fixture rejects proposal"}}}, nil
	}
	return h.trials.Execute(ctx, item)
}
func trialAdapterLoop(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("TrialAdapterRetainsBaselineAfterRejectedProposal", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "adapter-loop")
		bundle.Experiment.Spec.Limits.MaxRounds = 1
		ctx := scoped(bundle.Experiment.Spec.Scope)
		authority := controllerAuthority{actor: bundle.Experiment.Spec.Requester}
		environments := &trialFixtureFactory{t: t, bundle: bundle, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}}
		handler, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Environments: environments, Evaluator: &trialFixtureEvaluator{}, Allowance: optimization.TrialAllowance{MaxTokens: 100}})
		if err != nil {
			t.Fatal(err)
		}
		optimizer, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: noCandidateHandler{trials: handler}, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		created, err := optimizer.Create(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = optimizer.Start(ctx, created.ID, created.Revision); err != nil {
			t.Fatal(err)
		}
		for range 60 {
			if err = optimizer.WorkOnce(ctx, created.ID); err != nil {
				t.Fatal(err)
			}
			current, getErr := s.GetExperiment(ctx, created.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if current.Phase == optimization.PhaseReflect {
				decision, decisionErr := s.GetDecision(ctx, current.DecisionID)
				if decisionErr != nil || decision.Outcome != optimization.DecisionRetain || decision.Candidate != decision.Baseline {
					t.Fatalf("decision: %+v %v", decision, decisionErr)
				}
				return
			}
		}
		t.Fatal("loop did not reach reflection")
	})
}
