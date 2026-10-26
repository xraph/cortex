package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

type unavailableQuestionRelease struct{ *sqlite.Store }

func (*unavailableQuestionRelease) ReleaseHeldAsk(context.Context, a2a.HeldAskRequest) (a2a.HeldAskReceipt, error) {
	return a2a.HeldAskReceipt{}, errors.New("release worker interrupted")
}

func TestHeldQuestionRecoversThroughFreshEngineLifecycle(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "question", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"check"}`}})
	native, ok := st.(*sqlite.Store)
	require.True(t, ok)
	e.store = &unavailableQuestionRelease{Store: native}
	require.NoError(t, e.buildA2A())
	current, err := e.RunAgent(ctx, "planner", "ask", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, current.State)
	paused, err := native.GetSuspension(ctx, current.ID)
	require.NoError(t, err)
	queue, err := native.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, queue, "the failure must leave a persisted pause with no routed question")
	_, err = native.GetHeldAskRelease(ctx, paused.Pending[0].ReplyWith)
	require.ErrorIs(t, err, a2a.ErrHeldAskNotFound)

	source, err := coordination.NewScopedAgentReplySource(native, []cortex.Scope{cortex.ScopeFromContext(ctx)}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	var allowed atomic.Bool
	var denied atomic.Int32
	client := &backgroundReplyClient{scriptedLLM: &scriptedLLM{answered: true}}
	fresh, err := New(WithStore(native), WithLLM(client), WithA2A(a2a.Options{Workers: 1}), WithAgentReplyRecovery(AgentReplyRecoveryConfig{
		Service: coordination.AgentReplyRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond},
		Authorize: func(_ context.Context, key id.AgentRunID) error {
			require.Equal(t, current.ID, key)
			if !allowed.Load() {
				denied.Add(1)
				return errors.New("recovery denied")
			}
			return nil
		},
	}))
	require.NoError(t, err)
	require.NoError(t, fresh.Start(ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	require.Eventually(t, func() bool { return denied.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	queue, err = native.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, queue)
	require.Zero(t, client.calls.Load())
	allowed.Store(true)
	require.Eventually(t, func() bool {
		saved, readErr := native.GetRun(ctx, current.ID)
		return readErr == nil && saved.State == run.StateCompleted
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, fresh.Stop(context.Background()))
	require.EqualValues(t, 1, client.calls.Load())
	receipt, err := native.GetHeldAskRelease(ctx, paused.Pending[0].ReplyWith)
	require.NoError(t, err)
	require.Len(t, receipt.Deliveries, 1)
	runs, err := native.ListRuns(ctx, nil)
	require.NoError(t, err)
	require.Len(t, runs, 2, "recovery must create one recipient, not duplicate its execution")
	// Retrying the original release after completion returns the same queue identity.
	replay, err := native.ReleaseHeldAsk(ctx, a2a.HeldAskRequest{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision, ReplyWith: paused.Pending[0].ReplyWith})
	require.NoError(t, err)
	require.False(t, replay.Fresh)
	require.Equal(t, receipt.Deliveries, replay.Deliveries)
	var _ suspension.ReplyDiscoveryStore = native
}
