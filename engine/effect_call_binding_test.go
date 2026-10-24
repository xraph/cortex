package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

func TestEffectToolHandlerRetainsGuardedCall(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "sealed"}[sealed], func(t *testing.T) {
			storage := newSQLiteStoreForTest(t)
			ctx := overlayCtx("bound-effect")
			scope := cortex.ScopeFromContext(ctx)
			call := execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: scope, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), ClaimID: id.New("tclaim"), ParentRunID: id.NewAgentRunID(), Kind: execution.RunCallTool, OperationDigest: evidence.Digest([]byte("slot")), RequestDigest: evidence.Digest([]byte("request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: time.Now().UTC().Add(time.Minute)}
			fresh, err := storage.BeginRunCall(ctx, call)
			require.NoError(t, err)
			require.True(t, fresh)
			if sealed {
				_, err = storage.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: scope, RunID: call.RunID, Actor: "recovery", Reason: "worker expired"})
				require.NoError(t, err)
			}
			adapter := &engineEffectAdapter{}
			executor, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: storage, Adapters: []execution.EffectRegistration{{Requirement: engineEffectRequirement(), Adapter: adapter}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
			require.NoError(t, err)
			request := execution.EffectRequest{SchemaVersion: 1, Scope: scope, RunID: call.RunID, OperationKey: "publish", AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte("{}")), PrincipalRef: "host", AuthorityRef: "grant", Deadline: call.Deadline, Requirement: engineEffectRequirement()}
			handler, err := engine.NewEffectToolHandler(executor, func(context.Context, cortex.Invocation) (execution.EffectRequest, error) { return request, nil })
			require.NoError(t, err)
			content, err := handler(execution.WithRunCallContext(ctx, call), cortex.Invocation{Subject: cortex.Subject{Scope: scope, AgentID: call.AgentID, RunID: call.RunID}, Call: llm.ToolCall{ID: "publish-1", Name: "publish", Arguments: "{}"}})
			if sealed {
				require.ErrorIs(t, err, cortex.ErrEffectUncertain)
				require.Zero(t, adapter.calls)
				return
			}
			require.NoError(t, err)
			require.Contains(t, content, `"effect_status":"applied"`)
			retained, err := storage.GetEffect(ctx, "publish")
			require.NoError(t, err)
			require.Equal(t, &call, retained.Request.RunCall)
			require.NoError(t, retained.Receipt.Validate(retained.Request))
			require.Equal(t, 1, adapter.calls)
		})
	}
}
