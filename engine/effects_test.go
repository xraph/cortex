package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type effectLoopClient struct{ calls atomic.Int32 }

func (c *effectLoopClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	c.calls.Add(1)
	return effectLoopResponse(), nil
}
func (c *effectLoopClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	c.calls.Add(1)
	return &outcomeStream{response: effectLoopResponse()}, nil
}
func effectLoopResponse() *llm.Response {
	return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "first", Name: "write", Arguments: `{"value":1}`}, {ID: "second", Name: "write", Arguments: `{"value":2}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 1}}
}

func TestUncertainEffectStopsEngineBeforeSiblingOrNextModelCall(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval"} {
		t.Run(mode, func(t *testing.T) {
			s := scopespy.New()
			client := &effectLoopClient{}
			hooks := &toolEventRecorder{}
			var effects atomic.Int32
			opts := []Option{WithStore(s), WithLLM(client), WithExtension(hooks), WithTool(llm.Tool{Name: "write"}, func(context.Context, cortex.Invocation) (string, error) {
				effects.Add(1)
				return "secret provider reply", fmt.Errorf("secret provider failure: %w", cortex.ErrEffectUncertain)
			})}
			if mode == "approval" {
				opts = append(opts, WithToolAuthorizer(escalatingAuthorizer{tool: "write"}), WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()))
			}
			e, err := New(opts...)
			require.NoError(t, err)
			ctx := cortex.WithScope(t.Context(), scopeA())
			var r *run.Run
			if mode == "stream" {
				events := make(chan StreamEvent, 64)
				require.NoError(t, e.StreamAgent(ctx, "assistant", "publish", &RunOverrides{MaxSteps: 3}, events))
				var runID id.AgentRunID
				incomplete := false
				for ev := range events {
					if ev.Type == EventRunStarted {
						runID, err = id.ParseAgentRunID(ev.Data["run_id"].(string))
						require.NoError(t, err)
					}
					require.NotEqual(t, EventDone, ev.Type)
					require.NotContains(t, fmt.Sprint(ev.Data), "secret provider")
					incomplete = incomplete || ev.Type == EventIncomplete
				}
				require.True(t, incomplete)
				r, err = s.GetRun(ctx, runID)
				require.NoError(t, err)
			} else {
				r, err = e.RunAgent(ctx, "assistant", "publish", &RunOverrides{MaxSteps: 3})
				if mode == "approval" {
					require.NoError(t, err)
					require.Equal(t, run.StatePaused, r.State)
					require.Zero(t, effects.Load())
					cps := s.Checkpoints()
					require.Len(t, cps, 1)
					err = e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
					require.ErrorIs(t, err, cortex.ErrEffectUncertain)
					decided, decisionErr := s.GetCheckpoint(ctx, cps[0].ID)
					require.NoError(t, decisionErr)
					require.Equal(t, "resolved", decided.State)
					require.NotNil(t, decided.Decision)
					require.True(t, decided.Decision.Approved)
					require.ErrorIs(t, e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true}), cortex.ErrInvalidState)
					r, err = s.GetRun(ctx, r.ID)
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, cortex.ErrEffectUncertain)
				}
			}
			require.NotNil(t, r)
			require.Equal(t, run.StateIncomplete, r.State)
			require.Equal(t, run.StopToolError, r.StopReason)
			require.Equal(t, cortex.ErrEffectUncertain.Error(), r.Error)
			require.Empty(t, r.Output)
			require.EqualValues(t, 1, effects.Load())
			require.EqualValues(t, 1, client.calls.Load())
			require.Equal(t, 1, countEvent(hooks.snapshot(), "failed"))
			require.Zero(t, countEvent(hooks.snapshot(), "completed"))
		})
	}
}

func TestDispatchReportsUncertainEffect(t *testing.T) {
	e, err := New(WithTool(llm.Tool{Name: "write"}, func(context.Context, cortex.Invocation) (string, error) { return "secret", cortex.ErrEffectUncertain }))
	require.NoError(t, err)
	result := e.Dispatch(cortex.WithScope(t.Context(), scopeA()), "write", `{}`)
	require.Contains(t, result, cortex.ErrEffectUncertain.Error())
	require.NotContains(t, result, "secret")
}

type effectStopFailureStore struct{ *scopespy.Spy }

func (s effectStopFailureStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.Error == cortex.ErrEffectUncertain.Error() {
		return fmt.Errorf("terminal storage unavailable")
	}
	return s.Spy.UpdateRun(ctx, r)
}
func TestUncertainEffectDoesNotResolveApprovalWithoutDurableStop(t *testing.T) {
	store := effectStopFailureStore{Spy: scopespy.New()}
	client := &effectLoopClient{}
	var calls int
	e, err := New(WithStore(store), WithLLM(client), WithToolAuthorizer(escalatingAuthorizer{tool: "write"}), WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), WithTool(llm.Tool{Name: "write"}, func(context.Context, cortex.Invocation) (string, error) {
		calls++
		return "", cortex.ErrEffectUncertain
	}))
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), scopeA())
	r, err := e.RunAgent(ctx, "assistant", "publish", nil)
	require.NoError(t, err)
	cps := store.Checkpoints()
	require.Len(t, cps, 1)
	err = e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true})
	require.ErrorIs(t, err, cortex.ErrEffectUncertain)
	saved, err := store.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.False(t, isTerminalRunState(saved.State))
	cp, err := store.GetCheckpoint(ctx, cps[0].ID)
	require.NoError(t, err)
	require.Equal(t, "pending", cp.State)
	require.Nil(t, cp.Decision)
	require.Equal(t, 1, calls)
	require.Error(t, e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true}))
	require.Equal(t, 1, calls)
}

func TestUncertainApprovedEffectPreservesRunDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := scopespy.New()
		e, err := New(WithStore(store), WithLLM(&effectLoopClient{}), WithToolAuthorizer(escalatingAuthorizer{tool: "write"}), WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), WithTool(llm.Tool{Name: "write"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
			<-ctx.Done()
			return "", cortex.ErrEffectUncertain
		}))
		require.NoError(t, err)
		ctx := cortex.WithScope(t.Context(), scopeA())
		r, err := e.RunAgent(ctx, "assistant", "publish", &RunOverrides{Budget: &execution.Budget{MaxSteps: 3, Deadline: time.Now().Add(time.Second)}})
		require.NoError(t, err)
		require.Equal(t, run.StatePaused, r.State)
		cps := store.Checkpoints()
		require.Len(t, cps, 1)
		err = e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true})
		require.ErrorIs(t, err, cortex.ErrEffectUncertain)
		saved, err := store.GetRun(ctx, r.ID)
		require.NoError(t, err)
		require.Equal(t, run.StopDeadline, saved.StopReason)
		require.Equal(t, cortex.ErrEffectUncertain.Error(), saved.Error)
	})
}

func TestUncertainEffectStreamPersistenceFailureStaysUncertain(t *testing.T) {
	store := effectStopFailureStore{Spy: scopespy.New()}
	client := &effectLoopClient{}
	e, err := New(WithStore(store), WithLLM(client), WithTool(llm.Tool{Name: "write"}, func(context.Context, cortex.Invocation) (string, error) { return "", cortex.ErrEffectUncertain }))
	require.NoError(t, err)
	events := make(chan StreamEvent, 64)
	require.NoError(t, e.StreamAgent(cortex.WithScope(t.Context(), scopeA()), "assistant", "publish", &RunOverrides{MaxSteps: 3}, events))
	found := false
	for event := range events {
		require.NotEqual(t, EventDone, event.Type)
		require.NotEqual(t, EventIncomplete, event.Type)
		if event.Type == EventError {
			require.Equal(t, cortex.ErrEffectUncertain.Error(), event.Data["message"])
			found = true
		}
	}
	require.True(t, found)
	require.EqualValues(t, 1, client.calls.Load())
}

func TestUncertainEffectStreamNeverPublishesConcurrentCompletion(t *testing.T) {
	store := scopespy.New()
	e, err := New(WithStore(store), WithLLM(&effectLoopClient{}), WithTool(llm.Tool{Name: "write"}, func(ctx context.Context, inv cortex.Invocation) (string, error) {
		saved, readErr := store.GetRun(ctx, inv.RunID)
		require.NoError(t, readErr)
		saved.State = run.StateCompleted
		saved.Output = "conflicting result"
		require.NoError(t, store.UpdateRun(ctx, saved))
		return "", cortex.ErrEffectUncertain
	}))
	require.NoError(t, err)
	events := make(chan StreamEvent, 64)
	require.NoError(t, e.StreamAgent(cortex.WithScope(t.Context(), scopeA()), "assistant", "publish", &RunOverrides{MaxSteps: 3}, events))
	found := false
	for event := range events {
		require.NotEqual(t, EventDone, event.Type)
		require.NotContains(t, fmt.Sprint(event.Data), "conflicting result")
		if event.Type == EventError {
			require.Equal(t, cortex.ErrEffectUncertain.Error(), event.Data["message"])
			found = true
		}
	}
	require.True(t, found)
}
