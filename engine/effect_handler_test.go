package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

type engineEffectAdapter struct {
	calls, probes int
	lostReply     bool
}

func (a *engineEffectAdapter) Execute(_ context.Context, r execution.EffectRequest, _ []byte) (execution.EffectReceipt, error) {
	a.calls++
	if a.lostReply {
		return execution.EffectReceipt{}, errors.New("private provider diagnostic")
	}
	return engineEffectReceipt(r), nil
}
func (a *engineEffectAdapter) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	a.probes++
	receipt := engineEffectReceipt(r)
	return execution.EffectStatus{Outcome: execution.EffectApplied, Receipt: &receipt}, nil
}
func engineEffectReceipt(r execution.EffectRequest) execution.EffectReceipt {
	_, digest, _ := r.Freeze()
	return execution.EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.EffectApplied, ExternalID: "object-1", ExternalVersion: "1", EvidenceRef: "private-provider-evidence"}
}
func engineEffectRequirement() execution.EffectRequirement {
	return execution.EffectRequirement{Tool: "publish", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "probe-1", Retry: execution.RetryPolicy{MaxAttempts: 1}}
}

type engineEffectClient struct{ calls int }

func (c *engineEffectClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	c.calls++
	if c.calls > 1 {
		return &llm.Response{Content: "published", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}, nil
	}
	return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "publish", Arguments: `{"body":"report"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 1}}, nil
}
func (*engineEffectClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused streaming fixture")
}

func TestEffectToolHandlerEngineAndReopenedJournal(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "lost reply"}[lost], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "effects.db")
			storage := openSQLiteStoreForTest(t, path)
			ctx := overlayCtx("effect-engine")
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
			require.NoError(t, storage.Create(ctx, ag))
			adapter := &engineEffectAdapter{lostReply: lost}
			var request execution.EffectRequest
			build := func(store execution.EffectStore) engine.ToolHandler {
				executor, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: store, Adapters: []execution.EffectRegistration{{Requirement: engineEffectRequirement(), Adapter: adapter}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
				require.NoError(t, err)
				handler, err := engine.NewEffectToolHandler(executor, func(ctx context.Context, inv cortex.Invocation) (execution.EffectRequest, error) {
					key := inv.RunID.String() + "/publish"
					record, readErr := store.GetEffect(ctx, key)
					if readErr == nil {
						return record.Request, nil
					}
					if !errors.Is(readErr, execution.ErrEffectNotFound) {
						return execution.EffectRequest{}, readErr
					}
					request = execution.EffectRequest{SchemaVersion: 1, Scope: inv.Scope, RunID: inv.RunID, OperationKey: key, AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte(inv.Call.Arguments)), PrincipalRef: "host", AuthorityRef: "grant-1", Deadline: time.Now().UTC().Add(time.Minute), Requirement: engineEffectRequirement()}
					return request, nil
				})
				require.NoError(t, err)
				return handler
			}
			handler := build(storage)
			client := &engineEffectClient{}
			e, err := engine.New(engine.WithStore(storage), engine.WithLLM(client), engine.WithTool(llm.Tool{Name: "publish"}, handler), engine.WithToolRevision("publish", "1"))
			require.NoError(t, err)
			r, err := e.RunAgent(ctx, ag.Name, "publish report", nil)
			if lost {
				require.ErrorIs(t, err, cortex.ErrEffectUncertain)
				require.Equal(t, run.StateIncomplete, r.State)
				require.Equal(t, 1, client.calls)
			} else {
				require.NoError(t, err)
				require.Equal(t, run.StateCompleted, r.State)
			}
			require.Equal(t, 1, adapter.calls)
			reopened := openSQLiteStoreForTest(t, path)
			recoveredHandler := build(reopened)
			content, err := recoveredHandler(ctx, cortex.Invocation{Subject: cortex.Subject{Scope: cortex.ScopeFromContext(ctx), AgentID: ag.ID, RunID: r.ID}, Call: llm.ToolCall{ID: "call-1", Name: "publish", Arguments: `{"body":"report"}`}})
			require.NoError(t, err)
			require.Contains(t, content, `"effect_status":"applied"`)
			require.Contains(t, content, "object-1")
			require.NotContains(t, content, "private-provider-evidence")
			require.Equal(t, 1, adapter.calls)
			record, err := reopened.GetEffect(ctx, request.OperationKey)
			require.NoError(t, err)
			require.NotNil(t, record.Receipt)
			if lost {
				require.Equal(t, 1, adapter.probes)
				saved, readErr := reopened.GetRun(ctx, r.ID)
				require.NoError(t, readErr)
				require.Equal(t, run.StateIncomplete, saved.State)
			}
		})
	}
}

func TestEffectToolHandlerRejectsUnboundInvocations(t *testing.T) {
	for _, mode := range []string{"scope", "run", "tool", "arguments", "deadline", "call id", "agent", "resolver mutation", "resolver error"} {
		t.Run(mode, func(t *testing.T) {
			store := newSQLiteStoreForTest(t)
			ctx := overlayCtx("effect-validation")
			scope := cortex.ScopeFromContext(ctx)
			originalScope := scope.Canonical()
			adapter := &engineEffectAdapter{}
			executor, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: store, Adapters: []execution.EffectRegistration{{Requirement: engineEffectRequirement(), Adapter: adapter}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
			require.NoError(t, err)
			inv := cortex.Invocation{Subject: cortex.Subject{Scope: scope, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID()}, Call: llm.ToolCall{ID: "call", Name: "publish", Arguments: `{}`}}
			handler, err := engine.NewEffectToolHandler(executor, func(_ context.Context, input cortex.Invocation) (execution.EffectRequest, error) {
				r := execution.EffectRequest{SchemaVersion: 1, Scope: input.Scope, RunID: input.RunID, OperationKey: "operation", AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte(input.Call.Arguments)), PrincipalRef: "host", AuthorityRef: "grant-1", Deadline: time.Now().UTC().Add(time.Minute), Requirement: engineEffectRequirement()}
				switch mode {
				case "scope":
					r.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}
				case "run":
					r.RunID = id.NewAgentRunID()
				case "tool":
					r.Requirement.Tool = "different"
				case "arguments":
					r.ArgsDigest = evidence.Digest([]byte("different"))
				case "deadline":
					r.Deadline = time.Now().Add(-time.Second)
				case "resolver mutation":
					input.Scope.Levels[0].Value = "changed"
				case "resolver error":
					return execution.EffectRequest{}, errors.New("private allocator error")
				}
				return r, nil
			})
			require.NoError(t, err)
			if mode == "call id" {
				inv.Call.ID = ""
			}
			if mode == "agent" {
				inv.AgentID = id.Nil
			}
			content, err := handler(ctx, inv)
			require.ErrorIs(t, err, cortex.ErrEffectUncertain)
			require.Empty(t, content)
			require.Zero(t, adapter.calls)
			require.Zero(t, adapter.probes)
			require.Equal(t, originalScope, cortex.ScopeFromContext(ctx).Canonical())
			_, err = store.GetEffect(ctx, "operation")
			require.ErrorIs(t, err, execution.ErrEffectNotFound)
		})
	}
}

func TestEffectToolHandlerRequiresExecutorAndResolver(t *testing.T) {
	_, err := engine.NewEffectToolHandler(nil, nil)
	require.ErrorIs(t, err, cortex.ErrEffectUnavailable)
}

func TestEffectToolHandlerBoundsOperationResolution(t *testing.T) {
	storage := newSQLiteStoreForTest(t)
	ctx := overlayCtx("effect-resolver-bound")
	adapter := &engineEffectAdapter{}
	executor, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: storage, Adapters: []execution.EffectRegistration{{Requirement: engineEffectRequirement(), Adapter: adapter}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
	require.NoError(t, err)
	handler, err := engine.NewEffectToolHandler(executor, func(ctx context.Context, _ cortex.Invocation) (execution.EffectRequest, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "operation resolution must have a deadline")
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		return execution.EffectRequest{}, errors.New("unavailable allocator")
	})
	require.NoError(t, err)
	_, err = handler(ctx, cortex.Invocation{Subject: cortex.Subject{Scope: cortex.ScopeFromContext(ctx), RunID: id.NewAgentRunID(), AgentID: id.NewAgentID()}, Call: llm.ToolCall{ID: "call", Name: "publish", Arguments: `{}`}})
	require.ErrorIs(t, err, cortex.ErrEffectUncertain)
	require.Zero(t, adapter.calls)
}
