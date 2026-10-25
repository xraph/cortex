package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

func TestPreparedTeamPauseRetainsAuthority(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Equal(t, "waiting", team.Status)
	require.Nil(t, team.CompletedAt)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.False(t, account.Closed)
}

func TestPreparedTeamContinuationAcrossEngineRestart(t *testing.T) {
	for _, strategy := range []string{orchestration.StrategySequential, orchestration.StrategyParallel, orchestration.StrategyRouter, orchestration.StrategyHierarchical, orchestration.StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			f := newPreparedTeamFixture(t, strategy)
			refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
			f.client.complete = func(_ context.Context, req *llm.Request, call int) (*llm.Response, error) {
				if call == 1 {
					return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
				}
				output := "answer"
				if strategy == orchestration.StrategyHierarchical && len(req.Messages) > 1 {
					output = `[{"agent":"b","task":"work"}]`
				}
				return &llm.Response{Content: output, FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
			require.Equal(t, orchestration.StatusWaiting, team.Status)
			before := f.client.calls
			fresh, err := engine.New(engine.WithStore(f.s), engine.WithLLM(f.client), engine.WithExternalTool(llm.Tool{Name: "probe"}))
			require.NoError(t, err)
			waiting, err := fresh.ResumePreparedOrchestration(f.ctx, team.ID)
			require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
			require.Equal(t, orchestration.StatusWaiting, waiting.Status)
			require.Equal(t, before, f.client.calls, "strategy replay must not reexecute completed or waiting children")
			var paused id.AgentRunID
			for _, key := range team.AgentRunIDs {
				child, readErr := f.s.GetRun(f.ctx, key)
				require.NoError(t, readErr)
				if child.State == run.StatePaused {
					paused = key
				}
			}
			require.False(t, paused.IsNil())
			child, err := fresh.Resume(f.ctx, paused, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "probe-call", Content: "result"}}})
			require.NoError(t, err)
			require.Equal(t, run.StateCompleted, child.State)
			reservationID, parseErr := id.Parse(child.Provenance.OrchestrationReservationID)
			require.NoError(t, parseErr)
			reservation, readErr := f.s.GetReservation(f.ctx, reservationID)
			require.NoError(t, readErr)
			require.NoError(t, f.s.SettleOrchestrationChild(f.ctx, execution.Settlement{ReservationID: reservationID, RequestDigest: reservation.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: int64(child.Usage.Tokens)}, Known: execution.KnownTokens}, Receipt: "host:test-usage"}))

			// Live configuration changes cannot replace the captured continuation.
			f.cfg.Settings.Rounds = 4
			require.NoError(t, f.s.UpdateOrchestration(f.ctx, f.cfg))
			completed, err := fresh.ResumePreparedOrchestration(f.ctx, team.ID)
			require.NoError(t, err)
			require.Equal(t, orchestration.StatusCompleted, completed.Status)
			require.NotNil(t, completed.CompletedAt)
			require.Contains(t, completed.AgentRunIDs, paused)
			account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
			require.NoError(t, err)
			require.True(t, account.Closed)
			require.Equal(t, len(completed.AgentRunIDs), account.Reservations, "replay must not reserve completed work again")
			count := f.client.calls
			_, err = fresh.ResumePreparedOrchestration(f.ctx, team.ID)
			require.Error(t, err)
			require.Equal(t, count, f.client.calls)
		})
	}
}

func TestPreparedTeamContinuationRetainsCallerDeadline(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(f.ctx, deadline)
	defer cancel()
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.False(t, account.Policy.Deadline.After(deadline), "continuation must not widen the caller's deadline")
}

func TestPreparedTeamContinuationSingleWinner(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.client.complete = func(ctx context.Context, _ *llm.Request, call int) (*llm.Response, error) {
		if call == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		if call == 3 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	_, err = f.e.Resume(f.ctx, team.AgentRunIDs[0], engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "probe-call", Content: "result"}}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, resumeErr := f.e.ResumePreparedOrchestration(ctx, team.ID); finished <- resumeErr }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = f.e.ResumePreparedOrchestration(ctx, team.ID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	account, err := f.s.GetOrchestrationBudgetSnapshot(ctx, team.ID)
	require.NoError(t, err)
	require.False(t, account.Closed, "loser must not close the winner's account")
	unblock()
	require.NoError(t, <-finished)
	require.Equal(t, 3, f.client.calls)
}

func TestPreparedTeamContinuationRejectsChangedParentInput(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	team.Input = "changed after waiting"
	require.NoError(t, f.s.UpdateOrchestrationRun(f.ctx, team))
	failed, err := f.e.ResumePreparedOrchestration(f.ctx, team.ID)
	require.ErrorIs(t, err, cortex.ErrInvalidContinuation)
	require.Equal(t, orchestration.StatusFailed, failed.Status)
	stored, err := f.s.GetOrchestrationRun(f.ctx, team.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusFailed, stored.Status)
	require.Equal(t, 1, f.client.calls)
}
