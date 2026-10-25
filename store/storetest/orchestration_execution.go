package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

type orchestrationRunnerFunc func(context.Context, string, string, *orchestration.RunOpts) (*orchestration.AgentResult, error)

func (f orchestrationRunnerFunc) RunAgent(ctx context.Context, name, input string, opts *orchestration.RunOpts) (*orchestration.AgentResult, error) {
	return f(ctx, name, input, opts)
}

func testOrchestrationExecutionEvidence(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	readCtx := ctxWithScope("team-evidence")
	ctx, cancel := context.WithCancel(readCtx)
	defer cancel()
	first, second := mustCreateRun(t, s, ctx), mustCreateRun(t, s, ctx)
	runner := orchestrationRunnerFunc(func(callCtx context.Context, name, _ string, _ *orchestration.RunOpts) (*orchestration.AgentResult, error) {
		if name == "first" {
			return &orchestration.AgentResult{AgentName: name, RunID: first.ID, Output: "first result"}, nil
		}
		records, err := s.ListOrchestrationRuns(callCtx, &orchestration.RunListFilter{Exact: true, Status: orchestration.StatusRunning})
		if err != nil || len(records) != 1 || !slices.Equal(records[0].AgentRunIDs, []id.AgentRunID{first.ID}) {
			t.Errorf("intermediate evidence unavailable: %+v %v", records, err)
		}
		cancel()
		return &orchestration.AgentResult{AgentName: name, RunID: second.ID, Output: "partial"}, context.Canceled
	})
	cfg := orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team-evidence", Strategy: orchestration.StrategySequential,
		Participants: []orchestration.Participant{{AgentName: "first"}, {AgentName: "second"}}}
	if err := s.CreateOrchestration(ctx, &cfg); err != nil {
		t.Fatal(err)
	}
	svc := orchestration.NewService(runner, nil, s, nil)
	rec, err := svc.RunConfig(ctx, cfg, "work")
	if !errors.Is(err, context.Canceled) || rec == nil {
		t.Fatalf("run: %+v %v", rec, err)
	}
	stored, err := s.GetOrchestrationRun(readCtx, rec.ID)
	if err != nil || stored.Status != orchestration.StatusFailed || stored.CompletedAt == nil || stored.Output != "" ||
		!slices.Equal(stored.AgentRunIDs, []id.AgentRunID{first.ID, second.ID}) || stored.Scope.Canonical() != cortex.ScopeFromContext(readCtx).Canonical() {
		t.Fatalf("final evidence not durable: %+v %v", stored, err)
	}
	if _, err = s.GetOrchestrationRun(ctxWithScope("other-team"), rec.ID); !errors.Is(err, cortex.ErrOrchestrationRunNotFound) {
		t.Fatalf("foreign scope read: %v", err)
	}
}
