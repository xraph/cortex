package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
)

func TestEngineOrchestrationRecoveryContinuesResolvedWaitingTeam(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "await"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		if f.client.calls == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "await", Name: "await", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	type hostKey struct{}
	var authorizations atomic.Int32
	require.NoError(t, engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond}, Authorize: func(ctx context.Context, key id.OrchestrationID) error {
		require.Equal(t, "recovery-host", ctx.Value(hostKey{}))
		require.Equal(t, f.policy.Scope, cortex.ScopeFromContext(ctx))
		require.Equal(t, team.ID, key)
		authorizations.Add(1)
		return nil
	}})(f.e))
	startup, cancel := context.WithCancel(context.WithValue(f.ctx, hostKey{}, "recovery-host"))
	defer cancel()
	require.NoError(t, f.e.Start(startup))
	cancel()
	t.Cleanup(func() { require.NoError(t, f.e.Stop(context.Background())) })
	require.Eventually(t, func() bool { return authorizations.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
	waiting, err := f.s.GetOrchestrationRun(f.ctx, team.ID)
	require.NoError(t, err)
	require.Equal(t, team.ContinuationGeneration, waiting.ContinuationGeneration, "a paused child must not cause repeated ownership claims")
	_, err = f.e.Resume(f.ctx, team.AgentRunIDs[0], engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "await", Content: "ready"}}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		parent, readErr := f.s.GetOrchestrationRun(f.ctx, team.ID)
		return readErr == nil && parent.Status == orchestration.StatusCompleted
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, f.e.Stop(context.Background()))
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
	require.Equal(t, 2, account.Reservations)
	require.Equal(t, 3, f.client.calls)
	status, configured := f.e.OrchestrationRecoveryStatus()
	require.True(t, configured)
	require.False(t, status.Running)
	require.Zero(t, status.Active)
}

func TestEngineOrchestrationRecoveryReclaimsExpiredUnacknowledgedOwner(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	wrapper := &lostTeamCoordinatorClaim{expiringTeamCoordinator: &expiringTeamCoordinator{Store: f.s}}
	initial, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
	require.NoError(t, err)
	_, err = initial.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.Zero(t, f.client.calls)
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	require.NoError(t, engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond}, Authorize: func(context.Context, id.OrchestrationID) error { return nil }})(f.e))
	require.NoError(t, f.e.Start(f.ctx))
	t.Cleanup(func() { require.NoError(t, f.e.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		parent, readErr := f.s.GetOrchestrationRun(f.ctx, f.policy.ExecutionID)
		return readErr == nil && parent.Status == orchestration.StatusCompleted
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, f.e.Stop(context.Background()))
	require.Equal(t, 2, f.client.calls)
}

func TestEngineOrchestrationRecoveryRequiresCurrentHostAuthority(t *testing.T) {
	for _, mode := range []string{"denied", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparedTeamFixture(t, orchestration.StrategySequential)
			wrapper := &lostTeamCoordinatorClaim{expiringTeamCoordinator: &expiringTeamCoordinator{Store: f.s}}
			initial, err := engine.New(engine.WithStore(wrapper), engine.WithLLM(f.client))
			require.NoError(t, err)
			_, err = initial.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.Error(t, err)
			prior, err := f.s.GetOrchestrationCoordinator(f.ctx, f.policy.ExecutionID)
			require.NoError(t, err)
			var allowed atomic.Bool
			allowed.Store(mode == "revoked")
			var failures atomic.Int32
			f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
				allowed.Store(false)
				return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
			require.NoError(t, err)
			config := engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond, ReportError: func(error) { failures.Add(1) }}, Authorize: func(context.Context, id.OrchestrationID) error {
				if !allowed.Load() {
					return errors.New("host recovery revoked")
				}
				return nil
			}}
			require.NoError(t, engine.WithOrchestrationRecovery(config)(f.e))
			timer := time.NewTimer(time.Until(prior.ExpiresAt.Add(20 * time.Millisecond)))
			defer timer.Stop()
			<-timer.C
			require.NoError(t, f.e.Start(f.ctx))
			t.Cleanup(func() { require.NoError(t, f.e.Stop(context.Background())) })
			require.Eventually(t, func() bool { return failures.Load() > 0 }, 10*time.Second, 20*time.Millisecond)
			require.NoError(t, f.e.Stop(context.Background()))
			parent, err := f.s.GetOrchestrationRun(f.ctx, f.policy.ExecutionID)
			require.NoError(t, err)
			if mode == "denied" {
				require.Equal(t, prior.Generation, parent.ContinuationGeneration)
				require.Equal(t, orchestration.StatusRunning, parent.Status)
				require.Zero(t, f.client.calls)
			} else {
				require.Equal(t, orchestration.StatusFailed, parent.Status)
				require.Equal(t, 1, f.client.calls, "revocation must prevent the next participant from calling a provider")
			}
		})
	}
}

func TestEngineOrchestrationRecoveryRejectsMissingAuthorityAndDependencies(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	config := engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source}}
	_, err = engine.New(engine.WithStore(f.s), engine.WithLLM(f.client), engine.WithOrchestrationRecovery(config))
	require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
	config.Authorize = func(context.Context, id.OrchestrationID) error { return nil }
	_, err = engine.New(engine.WithOrchestrationRecovery(config), engine.WithLLM(f.client))
	require.ErrorIs(t, err, cortex.ErrNoStore)
	_, err = engine.New(engine.WithStore(f.s), engine.WithOrchestrationRecovery(config))
	require.ErrorIs(t, err, cortex.ErrNoLLM)
	configured, err := engine.New(engine.WithOrchestrationRecovery(config), engine.WithLLM(f.client), engine.WithStore(f.s))
	require.NoError(t, err)
	_, present := configured.OrchestrationRecoveryStatus()
	require.True(t, present)
}
