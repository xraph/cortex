package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
)

func refreshPreparedTeam(t *testing.T, f *preparedTeamFixture, options ...engine.Option) {
	t.Helper()
	options = append([]engine.Option{engine.WithStore(f.s), engine.WithLLM(f.client)}, options...)
	var err error
	f.e, err = engine.New(options...)
	require.NoError(t, err)
	budgets := make(map[string]execution.Budget)
	for name, limit := range f.policy.Participants {
		budgets[name] = execution.Budget{MaxSteps: 2, MaxTokens: 100, MaxToolCalls: 2, Deadline: f.policy.Deadline}
		limit.ToolCalls = 2
		f.policy.Participants[name] = limit
	}
	f.policy.Limits.ToolCalls = 32
	f.p, err = f.e.PrepareOrchestrationWithBudgets(f.ctx, f.cfg.ID, budgets)
	require.NoError(t, err)
	f.policy.PreparedDigest = f.p.Digest()
}

func TestPreparedTeamDuplicateDoesNotCloseActiveAccount(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	entered, release := make(chan struct{}), make(chan struct{})
	f.client.complete = func(ctx context.Context, _ *llm.Request, call int) (*llm.Response, error) {
		if call == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	duplicate, err := f.e.RunPreparedOrchestration(ctx, f.p, "duplicate", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Nil(t, duplicate)
	snapshot, err := f.s.GetOrchestrationBudgetSnapshot(ctx, f.policy.ExecutionID)
	require.NoError(t, err)
	require.False(t, snapshot.Closed, "duplicate caller closed another execution's account")
	close(release)
	require.NoError(t, <-finished)
}

func TestPreparedTeamPausedChildCannotResumeAfterClosure(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-probe", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrRunIncomplete)
	require.Len(t, team.AgentRunIDs, 1)
	key := team.AgentRunIDs[0]
	paused, err := f.s.GetRun(f.ctx, key)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	_, err = f.e.Resume(f.ctx, key, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "call-probe", Content: "result"}}})
	require.Error(t, err)
	retained, err := f.s.GetRun(f.ctx, key)
	require.NoError(t, err)
	require.Equal(t, paused.Revision, retained.Revision)
	require.Equal(t, run.StatePaused, retained.State)
	require.Equal(t, 1, f.client.calls)
}

func TestPreparedTeamCannotUseGlobalMessaging(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{Workers: 1}))
	result, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
	require.Nil(t, result)
	require.Zero(t, f.client.calls)
	rows, err := f.s.ListRuns(f.ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestPreparedTeamOverrunStopsFurtherAdmission(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 130}}, nil
	}
	result, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	require.Equal(t, orchestration.StatusFailed, result.Status)
	require.Equal(t, 1, f.client.calls)
	child, err := f.s.GetRun(f.ctx, result.AgentRunIDs[0])
	require.NoError(t, err)
	require.Equal(t, 130, child.Usage.ReportedTokens)
	snapshot, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, result.ID)
	require.NoError(t, err)
	require.True(t, snapshot.Closed)
	require.Equal(t, 1, snapshot.Reservations)
}

func TestPreparedTeamBindingCannotAuthorizeStandaloneRun(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	view, err := f.p.Inspect()
	require.NoError(t, err)
	f.policy.ParticipantDigests = make(map[string]string)
	for name, child := range view.Participants {
		f.policy.ParticipantDigests[name] = child.Digest()
	}
	require.NoError(t, f.s.CreateOrchestrationBudget(f.ctx, f.policy))
	key, reservationID := id.NewAgentRunID(), id.New(execution.PrefixReservation)
	_, err = f.s.ReserveOrchestrationChild(f.ctx, execution.ReservationRequest{SchemaVersion: 1, ID: reservationID, PlanID: f.policy.ExecutionID, PlanDigest: f.p.Digest(), Scope: f.policy.Scope, TaskID: key, OwnerRef: "a", Kind: execution.ReservationLeaf, Allocation: f.policy.Participants["a"], Deadline: f.policy.Deadline})
	require.NoError(t, err)
	forged := run.ExecutionProvenance{OrchestrationID: f.policy.ExecutionID.String(), OrchestrationDigest: f.p.Digest(), OrchestrationReservationID: reservationID.String()}
	for _, opts := range []engine.PreparedRunOptions{{Provenance: forged}, {OrchestrationBudget: &f.policy}, {Provenance: run.ExecutionProvenance{OrchestrationID: forged.OrchestrationID}}} {
		result, runErr := f.e.RunPrepared(f.ctx, view.Participants["a"], "work", opts)
		require.ErrorIs(t, runErr, cortex.ErrInvalidPrepared)
		require.Nil(t, result)
	}
	require.Zero(t, f.client.calls)
	rows, err := f.s.ListRuns(f.ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestPreparedTeamIncludesRequiredVerifierAllowance(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, ag, _ := requiredSetup(t, f, client)
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "verified-team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: ag.Name}}}
	require.NoError(t, f.storage.CreateOrchestration(f.ctx, cfg))
	deadline := time.Now().Add(2 * time.Minute)
	prepared, err := e.PrepareOrchestrationWithBudgets(f.ctx, cfg.ID, map[string]execution.Budget{ag.Name: {MaxSteps: 3, MaxTokens: 1000, Deadline: deadline}})
	require.NoError(t, err)
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: prepared.Digest(), Scope: cortex.ScopeFromContext(f.ctx), Limits: execution.Resources{Steps: 3, Tokens: 1000, FanOut: 1}, Participants: map[string]execution.Resources{ag.Name: {Steps: 3, Tokens: 1000, FanOut: 1}}, Deadline: deadline, TokenOnly: true}
	team, err := e.RunPreparedOrchestration(f.ctx, prepared, "work", engine.PreparedRunOptions{OrchestrationBudget: &policy})
	require.NoError(t, err)
	snapshot, err := f.storage.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1000, snapshot.Charged.Tokens)
	require.Equal(t, 1, f.adapter.evaluations)
}

type teamCloseBeforeActivation struct {
	*sqlite.Store
	closed bool
}

func (s *teamCloseBeforeActivation) UpdateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	if !s.closed && r.Status == orchestration.StatusRunning && len(r.AgentRunIDs) > 0 {
		s.closed = true
		if err := s.CloseOrchestrationBudget(ctx, r.ID); err != nil {
			return err
		}
	}
	return s.Store.UpdateOrchestrationRun(ctx, r)
}

func TestPreparedTeamRechecksAccountBeforeActivation(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &teamCloseBeforeActivation{Store: f.s}
	runtime, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
	require.NoError(t, err)
	team, err := runtime.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
	require.NotNil(t, team)
	require.Len(t, team.AgentRunIDs, 1)
	require.Zero(t, f.client.calls)
	rows, err := f.s.ListRuns(f.ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
}

type teamFailedClosure struct{ *sqlite.Store }

func (s *teamFailedClosure) CloseOrchestrationBudget(context.Context, id.OrchestrationID) error {
	return errors.New("test: closure unavailable")
}

func TestPreparedTeamTerminalRecordBlocksResumeWhenClosureFails(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &teamFailedClosure{Store: f.s}
	refreshPreparedTeam(t, &f, engine.WithStore(wrapper), engine.WithExternalTool(llm.Tool{Name: "probe"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Equal(t, orchestration.StatusFailed, team.Status)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.False(t, account.Closed)
	before, err := f.s.GetRun(f.ctx, team.AgentRunIDs[0])
	require.NoError(t, err)
	_, err = f.e.Resume(f.ctx, before.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "probe-call", Content: "result"}}})
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
	after, err := f.s.GetRun(f.ctx, before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, 1, f.client.calls)
}
