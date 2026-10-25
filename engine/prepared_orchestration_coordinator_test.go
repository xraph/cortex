package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/sqlite"
)

func TestPreparedTeamUsesAcknowledgedCoordinator(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	f.client.complete = func(ctx context.Context, _ *llm.Request, _ int) (*llm.Response, error) {
		lease, err := f.s.GetOrchestrationCoordinator(ctx, f.policy.ExecutionID)
		require.NoError(t, err)
		require.False(t, lease.Released)
		_, err = f.s.CheckOrchestrationCoordinator(ctx, lease)
		require.NoError(t, err)
		return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, team.Status)
	lease, err := f.s.GetOrchestrationCoordinator(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, lease.Released)
	require.Equal(t, lease.Generation, team.ContinuationGeneration)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
}

type expiringTeamCoordinator struct{ *sqlite.Store }

func (s *expiringTeamCoordinator) AcquireOrchestrationCoordinator(ctx context.Context, req orchestration.CoordinatorRequest) (orchestration.CoordinatorClaim, error) {
	req.Duration = 3 * time.Second
	return s.Store.AcquireOrchestrationCoordinator(ctx, req)
}
func (s *expiringTeamCoordinator) RenewOrchestrationCoordinator(context.Context, orchestration.CoordinatorLease, time.Duration) (orchestration.CoordinatorLease, error) {
	return orchestration.CoordinatorLease{}, errors.New("test: renewal acknowledgment lost")
}

func TestPreparedTeamExpiredCoordinatorCannotCloseSuccessor(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &expiringTeamCoordinator{Store: f.s}
	runtime, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		close(entered)
		<-release
		// Even late over-limit usage cannot let the stale owner close the successor.
		return &llm.Response{Content: "late", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 130}}, nil
	}
	finished := make(chan error, 1)
	go func() {
		_, runErr := runtime.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
		finished <- runErr
	}()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	select {
	case <-entered:
	case runErr := <-finished:
		t.Fatalf("pass ended before provider: %v", runErr)
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	prior, err := f.s.GetOrchestrationCoordinator(f.ctx, f.policy.ExecutionID)
	require.NoError(t, err)
	_, err = f.e.ResumePreparedOrchestration(f.ctx, f.policy.ExecutionID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	timer := time.NewTimer(time.Until(prior.ExpiresAt.Add(20 * time.Millisecond)))
	defer timer.Stop()
	<-timer.C
	recovered, err := f.e.ResumePreparedOrchestration(f.ctx, f.policy.ExecutionID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	require.Equal(t, prior.Generation+1, recovered.ContinuationGeneration)
	require.Len(t, recovered.AgentRunIDs, 1)
	unblock()
	select {
	case err = <-finished:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stale pass did not drain")
	}
	parent, err := f.s.GetOrchestrationRun(f.ctx, recovered.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusWaiting, parent.Status)
	require.Equal(t, recovered.ContinuationGeneration, parent.ContinuationGeneration)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, recovered.ID)
	require.NoError(t, err)
	require.False(t, account.Closed)
	require.Equal(t, 1, account.Reservations)
	require.Equal(t, 1, f.client.calls)
}

func TestPreparedTeamPanicFinishesOwnedAccount(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) { panic("test provider") }
	require.Panics(t, func() {
		_, _ = f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	})
	parent, err := f.s.GetOrchestrationRun(f.ctx, f.policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusFailed, parent.Status)
	lease, err := f.s.GetOrchestrationCoordinator(f.ctx, parent.ID)
	require.NoError(t, err)
	require.True(t, lease.Released)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, parent.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
}

func TestPreparedTeamChecksOwnershipBeforeToolDispatch(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	calls := 0
	refreshPreparedTeam(t, &f, engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { calls++; return "effect", nil }))
	f.client.complete = func(ctx context.Context, _ *llm.Request, _ int) (*llm.Response, error) {
		require.NoError(t, f.s.CloseOrchestrationBudget(ctx, f.policy.ExecutionID))
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	_, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Zero(t, calls)
	require.Equal(t, 1, f.client.calls)
}

type renewingTeamCoordinator struct{ *expiringTeamCoordinator }

func (s *renewingTeamCoordinator) RenewOrchestrationCoordinator(ctx context.Context, lease orchestration.CoordinatorLease, duration time.Duration) (orchestration.CoordinatorLease, error) {
	return s.Store.RenewOrchestrationCoordinator(ctx, lease, duration)
}

func TestPreparedTeamRenewsOwnershipDuringProviderCall(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &renewingTeamCoordinator{expiringTeamCoordinator: &expiringTeamCoordinator{Store: f.s}}
	runtime, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
	require.NoError(t, err)
	f.client.complete = func(ctx context.Context, _ *llm.Request, call int) (*llm.Response, error) {
		if call == 1 {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				lease, readErr := f.s.GetOrchestrationCoordinator(ctx, f.policy.ExecutionID)
				if readErr != nil {
					return nil, readErr
				}
				if lease.Revision > 1 {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-ticker.C:
				}
			}
		}
		return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := runtime.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	lease, err := f.s.GetOrchestrationCoordinator(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, lease.Released)
	require.GreaterOrEqual(t, lease.Revision, int64(3))
}

type lostTeamCoordinatorClaim struct{ *expiringTeamCoordinator }

func (s *lostTeamCoordinatorClaim) AcquireOrchestrationCoordinator(ctx context.Context, request orchestration.CoordinatorRequest) (orchestration.CoordinatorClaim, error) {
	_, err := s.expiringTeamCoordinator.AcquireOrchestrationCoordinator(ctx, request)
	if err != nil {
		return orchestration.CoordinatorClaim{}, err
	}
	return orchestration.CoordinatorClaim{}, errors.New("test: claim acknowledgment lost")
}

func TestPreparedTeamRecoversLostClaimWithoutReadAdmission(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &lostTeamCoordinatorClaim{expiringTeamCoordinator: &expiringTeamCoordinator{Store: f.s}}
	runtime, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
	require.NoError(t, err)
	unknown, err := runtime.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Nil(t, unknown)
	require.Zero(t, f.client.calls)
	prior, err := f.s.GetOrchestrationCoordinator(f.ctx, f.policy.ExecutionID)
	require.NoError(t, err)
	_, err = f.e.ResumePreparedOrchestration(f.ctx, f.policy.ExecutionID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	require.Zero(t, f.client.calls)
	timer := time.NewTimer(time.Until(prior.ExpiresAt.Add(20 * time.Millisecond)))
	defer timer.Stop()
	<-timer.C
	completed, err := f.e.ResumePreparedOrchestration(f.ctx, f.policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, completed.Status)
	require.Equal(t, prior.Generation+1, completed.ContinuationGeneration)
	require.Equal(t, 2, f.client.calls)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, completed.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
	require.Equal(t, 2, account.Reservations)
}

func TestPreparedTeamCallerCancellationFinishesOwnedAccount(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		cancel()
		return nil, context.Canceled
	}
	team, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, team)
	parent, err := f.s.GetOrchestrationRun(f.ctx, team.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusFailed, parent.Status)
	lease, err := f.s.GetOrchestrationCoordinator(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, lease.Released)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
}
