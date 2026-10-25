package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

type teamTrialModel struct{ calls atomic.Int32 }

func (m *teamTrialModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.calls.Add(1)
	return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 7}}, nil
}
func (*teamTrialModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, optimization.ErrUnavailable
}

type teamTrialFactory struct {
	*trialFixtureFactory
	runtime    *engine.Engine
	source     *rootsqlite.Store
	options    engine.PreparedRunOptions
	firstChild id.ID
}
type teamTrialEnvironment struct {
	*trialFixtureEnvironment
	owner *teamTrialFactory
}

func (f *teamTrialFactory) Open(ctx context.Context, r optimization.TrialRequest) (optimization.TrialEnvironment, error) {
	e, err := f.trialFixtureFactory.Open(ctx, r)
	if err != nil {
		return nil, err
	}
	fixture, ok := e.(*trialFixtureEnvironment)
	if !ok {
		return nil, optimization.ErrIncompleteEvidence
	}
	return &teamTrialEnvironment{trialFixtureEnvironment: fixture, owner: f}, nil
}
func (f *teamTrialFactory) Recover(ctx context.Context, r optimization.EnvironmentReceipt) (optimization.TrialEnvironment, error) {
	e, err := f.trialFixtureFactory.Recover(ctx, r)
	if err != nil {
		return nil, err
	}
	fixture, ok := e.(*trialFixtureEnvironment)
	if !ok {
		return nil, optimization.ErrIncompleteEvidence
	}
	return &teamTrialEnvironment{trialFixtureEnvironment: fixture, owner: f}, nil
}
func (e *teamTrialEnvironment) ExecuteOrchestration(ctx context.Context, p *engine.PreparedOrchestration, input string, options engine.PreparedRunOptions) (*optimization.ExecutionRecord, error) {
	f := e.owner
	f.calls++
	f.options = options
	if options.Purpose != run.PurposeOptimizationTrial || !options.SessionID.IsNil() || options.OrchestrationBudget == nil {
		return nil, optimization.ErrInvalidSpec
	}
	team, err := f.runtime.RunPreparedOrchestration(ctx, p, input, options)
	if err != nil {
		return nil, err
	}
	record, err := optimization.CaptureOrchestration(ctx, f.source, f.bundle.Versions[0], e.receipt, team.ID)
	if err != nil {
		return nil, err
	}
	e.execution = &record
	if f.calls == 1 {
		f.firstChild = record.Orchestration.Children[0].Execution.RunID
	}
	if f.calls == 2 {
		forged := forgeRepeatedTeamChild(f.t, record, f.firstChild)
		return &forged, nil
	}
	if f.lost {
		f.lost = false
		return nil, optimization.ErrUnknownAttempt
	}
	return &record, nil
}

func orchestrationTrials(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("TeamTrialRecoversOriginalChildrenWithoutReplay", func(t *testing.T) {
		model := &teamTrialModel{}
		bundle, runtime, source, _ := OrchestrationFixture(t, model)
		s := factory(t, time.Now)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		host := &teamTrialFactory{trialFixtureFactory: &trialFixtureFactory{t: t, bundle: bundle, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}, lost: true}, runtime: runtime, source: source}
		evaluator := &trialFixtureEvaluator{}
		authority := controllerAuthority{actor: bundle.Experiment.Spec.Requester}
		config := optimization.TrialConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Environments: host, Evaluator: evaluator, Allowance: optimization.TrialAllowance{MaxTokens: 200}}
		handler, err := optimization.NewTrialPhaseHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		optimizer, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		current, err := optimizer.Create(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = optimizer.Start(ctx, current.ID, current.Revision); err != nil {
			t.Fatal(err)
		}
		recovered := false
		conflicts := 0
		for range 30 {
			err = optimizer.WorkOnce(ctx, current.ID)
			if err != nil && !errors.Is(err, optimization.ErrUnknownAttempt) && !errors.Is(err, optimization.ErrConflict) {
				t.Fatal(err)
			}
			if errors.Is(err, optimization.ErrConflict) {
				conflicts++
			}
			if errors.Is(err, optimization.ErrUnknownAttempt) {
				recovered = true
			}
			current, err = s.GetExperiment(ctx, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Phase == optimization.PhasePropose {
				break
			}
		}
		if !recovered || conflicts != 1 || current.Phase != optimization.PhasePropose || host.calls != 3 || model.calls.Load() != 6 || evaluator.calls != 3 {
			t.Fatalf("team recovery replayed: phase=%s calls=%d model=%d evaluation=%d recovered=%v conflicts=%d", current.Phase, host.calls, model.calls.Load(), evaluator.calls, recovered, conflicts)
		}
		trials, err := s.ListTrials(ctx, current.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 10}})
		if err != nil || len(trials.Items) != 3 {
			t.Fatalf("trial missing: %v", err)
		}
		trial := trials.Items[0]
		if trial.Execution == nil || trial.Execution.Orchestration == nil || len(trial.Execution.Orchestration.Children) != 2 || trial.Execution.Usage.TokenQuality != optimization.UsageUnknown {
			t.Fatal("original children or unknown usage lost")
		}
		for _, child := range trial.Execution.Orchestration.Children {
			original, readErr := source.GetRun(ctx, child.Reservation.Request.TaskID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if original.Provenance.TrialID != trial.ID.String() || original.Provenance.ExperimentID != current.ID.String() || original.SessionID.IsNil() {
				t.Fatal("trial provenance or isolated session lost")
			}
		}
		again, err := s.GetTrial(ctx, trial.ID)
		if err != nil || again.Execution.Digest != trial.Execution.Digest {
			t.Fatalf("stored team changed: %v", err)
		}
	})
}

func forgeRepeatedTeamChild(t *testing.T, record optimization.ExecutionRecord, prior id.ID) optimization.ExecutionRecord {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var forged optimization.ExecutionRecord
	if err = json.Unmarshal(data, &forged); err != nil {
		t.Fatal(err)
	}
	child := &forged.Orchestration.Children[0]
	original := child.Execution.RunID
	child.Execution.RunID = prior
	child.Reservation.Request.TaskID = prior
	_, child.Reservation.RequestDigest, err = child.Reservation.Request.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	if child.Reservation.Settlement != nil {
		t.Fatal("fixture unexpectedly has billing settlement")
	}
	sealed, err := optimization.SealExecution(*child.Execution)
	if err != nil {
		t.Fatal(err)
	}
	child.Execution = &sealed
	for i, key := range forged.Orchestration.Run.AgentRunIDs {
		if key == original {
			forged.Orchestration.Run.AgentRunIDs[i] = prior
		}
	}
	forged.Usage, err = optimization.OrchestrationUsage(*forged.Orchestration)
	if err != nil {
		t.Fatal(err)
	}
	forged, err = optimization.SealExecution(forged)
	if err != nil {
		t.Fatal(err)
	}
	return forged
}
