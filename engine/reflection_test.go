package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
)

type reflectionHost struct {
	mu          sync.Mutex
	denied      bool
	mutate      bool
	observation func(engine.ReflectionObservationInput) cognitive.ReflectionSnapshot
}

func (h *reflectionHost) WithReflectionFence(_ context.Context, b cognitive.ReflectionBinding, fn func(cognitive.ReflectionBinding) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fn(b)
}
func (h *reflectionHost) AuthorizeReflectionRead(context.Context, cognitive.ReflectionBinding) error {
	if h.denied {
		return errors.New("revoked")
	}
	return nil
}
func (h *reflectionHost) AuthorizeReflection(_ context.Context, r cognitive.ReflectionRequest) error {
	if h.denied {
		return errors.New("revoked")
	}
	if h.mutate {
		r.Policy.Phases[0].Strategy = cognitive.StrategyCreative
		r.Snapshot.Binding.Scope.Levels[0].Value = "changed"
	}
	return nil
}
func (h *reflectionHost) ObserveReflection(_ context.Context, in engine.ReflectionObservationInput) (cognitive.ReflectionSnapshot, error) {
	return h.observation(in), nil
}
func observation(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
	return cognitive.ReflectionSnapshot{Binding: in.Binding, Step: in.Step, Usage: in.Usage, ObservedAt: time.Now().UTC(), ProgressDigest: evidence.Digest([]byte("progress"))}
}
func TestReflectionRequiredInputStopsBeforeModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			backend := newSQLiteStoreForTest(t)
			ctx := overlayCtx("reflection")
			host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
				s := observation(in)
				s.RequiredInput = cognitive.ReflectionMissingInput
				return s
			}}
			calls := 0
			client := replanClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				calls++
				return &llm.Response{Content: "wrong", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}, nil
			}}
			e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}))
			require.NoError(t, err)
			ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
			require.NoError(t, backend.Create(ctx, ag))
			budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
			p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
			require.NoError(t, err)
			var result *run.Run
			if stream {
				events := make(chan engine.StreamEvent, 16)
				require.NoError(t, e.StreamPrepared(ctx, p, "work", engine.PreparedRunOptions{}, events))
				var runID id.AgentRunID
				for ev := range events {
					if ev.Type == engine.EventRunStarted {
						identifier, parseErr := id.ParseAgentRunID(ev.Data["run_id"].(string))
						require.NoError(t, parseErr)
						runID = identifier
					}
				}
				result, err = backend.GetRun(ctx, runID)
				require.NoError(t, err)
				require.Equal(t, run.StateIncomplete, result.State)
				require.Equal(t, "missing_input", result.Metadata["reflection_reason"])
			} else {
				result, err = e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
				require.NoError(t, err)
				require.Equal(t, run.StateIncomplete, result.State)
				require.Equal(t, "missing_input", result.Metadata["reflection_reason"])
			}
			require.Zero(t, calls)
		})
	}
}
func TestReflectionCurrentGrantAndCallbackIsolation(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "detached", true: "revoked"}[denied], func(t *testing.T) {
			backend := newSQLiteStoreForTest(t)
			ctx := overlayCtx("reflection")
			host := &reflectionHost{denied: denied, mutate: true, observation: observation}
			client := &requiredClient{response: &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}}
			e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}))
			require.NoError(t, err)
			ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
			require.NoError(t, backend.Create(ctx, ag))
			budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
			p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
			require.NoError(t, err)
			r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
			require.NoError(t, err)
			if denied {
				require.Zero(t, client.calls)
				require.Equal(t, run.StateIncomplete, r.State)
			} else {
				require.Equal(t, 1, client.calls)
				require.Equal(t, run.StateCompleted, r.State)
			}
		})
	}
}

type reflectionPhaseRecorder struct{ calls int }

func (*reflectionPhaseRecorder) Name() string { return "reflection-phases" }
func (r *reflectionPhaseRecorder) OnCognitivePhaseChanged(context.Context, id.AgentRunID, string, string) error {
	r.calls++
	return nil
}

func TestReflectionAblationAndPhaseTransitionEmitsOnce(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			backend := newSQLiteStoreForTest(t)
			ctx := overlayCtx("reflection-ablation")
			host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
				s := observation(in)
				s.NoProgressSteps = in.Step
				return s
			}}
			phases := &reflectionPhaseRecorder{}
			calls := 0
			client := replanClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				calls++
				if calls < 3 {
					return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 1}}, nil
				}
				return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}, nil
			}}
			options := []engine.Option{engine.WithStore(backend), engine.WithLLM(client), engine.WithExtension(phases), engine.WithTool(llm.Tool{Name: "probe", Parameters: map[string]any{"type": "object"}}, func(context.Context, cortex.Invocation) (string, error) { return "unchanged", nil })}
			if enabled {
				options = append(options, engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1", StuckSteps: 1, Phases: []cognitive.Phase{{Strategy: cognitive.StrategyReactive, MaxSteps: 1, Transition: cognitive.TransitionAfterSteps}, {Strategy: cognitive.StrategyReflective}}}, Observer: host, Fence: host, Authorizer: host}))
			}
			e, err := engine.New(options...)
			require.NoError(t, err)
			ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
			require.NoError(t, backend.Create(ctx, ag))
			budget := execution.Budget{MaxSteps: 5, MaxTokens: 100, MaxToolCalls: 5}
			p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
			require.NoError(t, err)
			result, err := e.RunPrepared(ctx, p, "repeat probe", engine.PreparedRunOptions{})
			require.NoError(t, err)
			if enabled {
				require.Equal(t, 1, calls)
				require.Equal(t, 1, phases.calls)
				require.Equal(t, run.StateIncomplete, result.State)
				receipt, readErr := backend.GetReflection(ctx, result.ID)
				require.NoError(t, readErr)
				require.Equal(t, 1, receipt.State.Reflections)
				require.Equal(t, 1, receipt.State.Phase)
				require.False(t, receipt.Fresh)
			} else {
				require.Equal(t, 3, calls)
				require.Zero(t, phases.calls)
				require.Equal(t, run.StateCompleted, result.State)
			}
			// This ablation measures bounded invocation cost, not semantic improvement.
		})
	}
}

type lostReflectionAckStore struct {
	*sqlite.Store
	lose bool
}

func (s *lostReflectionAckStore) AdmitReflection(ctx context.Context, req cognitive.ReflectionRequest) (cognitive.ReflectionReceipt, error) {
	receipt, err := s.Store.AdmitReflection(ctx, req)
	if err == nil && s.lose {
		s.lose = false
		return cognitive.ReflectionReceipt{}, errors.New("acknowledgement lost")
	}
	return receipt, err
}
func TestReflectionUnknownAcknowledgementPreventsDispatch(t *testing.T) {
	backend := &lostReflectionAckStore{Store: newSQLiteStoreForTest(t), lose: true}
	ctx := overlayCtx("reflection-ack")
	host := &reflectionHost{observation: observation}
	client := &requiredClient{response: &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	result, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, result.State)
	require.Zero(t, client.calls)
	receipt, err := backend.GetReflection(ctx, result.ID)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.False(t, receipt.Fresh)
}
func TestReflectionRunRevisionRacePreventsDispatch(t *testing.T) {
	backend := newSQLiteStoreForTest(t)
	ctx := overlayCtx("reflection-revision")
	host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
		r, err := backend.GetRun(ctx, in.Binding.RunID)
		require.NoError(t, err)
		r.State = run.StateCancelled
		require.NoError(t, backend.UpdateRun(ctx, r))
		return observation(in)
	}}
	client := &requiredClient{response: &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	result, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	require.Equal(t, run.StateCancelled, result.State)
	require.Zero(t, client.calls)
	receipt, err := backend.GetReflection(ctx, result.ID)
	require.NoError(t, err)
	require.Nil(t, receipt)
}

func TestReflectionErrorTransitionsWithoutAnotherModelLoop(t *testing.T) {
	backend := newSQLiteStoreForTest(t)
	ctx := overlayCtx("reflection-error")
	host := &reflectionHost{observation: observation}
	phases := &reflectionPhaseRecorder{}
	client := &requiredClient{failure: errors.New("provider unavailable")}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithExtension(phases), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1", Phases: []cognitive.Phase{{Strategy: cognitive.StrategyReactive, Transition: cognitive.TransitionOnError}, {Strategy: cognitive.StrategyReflective}}}, Observer: host, Fence: host, Authorizer: host}))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	_, err = e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	require.Error(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, phases.calls)
}
func TestReflectionUserInterventionSurvivesRestart(t *testing.T) {
	backend := newSQLiteStoreForTest(t)
	ctx := overlayCtx("reflection-correction")
	host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
		s := observation(in)
		s.RequiredInput = cognitive.ReflectionMissingInput
		return s
	}}
	config := engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(config))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, r.State)
	restarted, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(config))
	require.NoError(t, err)
	receipt, err := restarted.InterveneReflection(ctx, r.ID, cognitive.ContextSource{Reference: "user:correction", Digest: evidence.Digest([]byte("corrected requirement"))})
	require.NoError(t, err)
	require.EqualValues(t, 2, receipt.State.ControlRevision)
	require.Equal(t, 1, receipt.State.Reflections)
	require.False(t, receipt.State.Pending)
	require.Zero(t, client.calls)
	host.denied = true
	_, err = restarted.InterveneReflection(ctx, r.ID, cognitive.ContextSource{Reference: "user:correction", Digest: evidence.Digest([]byte("unauthorized correction"))})
	require.Error(t, err)
	retained, err := backend.GetReflection(ctx, r.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, retained.State.ControlRevision)
}
func TestReflectionExpiredCallbackCannotDispatch(t *testing.T) {
	backend := newSQLiteStoreForTest(t)
	ctx := overlayCtx("reflection-timeout")
	host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
		time.Sleep(15 * time.Millisecond)
		return observation(in)
	}}
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host, Timeout: time.Millisecond}))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, r.State)
	require.Zero(t, client.calls)
	receipt, err := backend.GetReflection(ctx, r.ID)
	require.NoError(t, err)
	require.Nil(t, receipt)
}
