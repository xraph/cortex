package execution

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func effectToolCall(r EffectRequest) RunCallRequest {
	return RunCallRequest{SchemaVersion: 1, ID: id.New(PrefixRunCall), Scope: r.Scope, RunID: r.RunID, AgentID: id.NewAgentID(), ClaimID: id.New("tclaim"), ParentRunID: id.NewAgentRunID(), Kind: RunCallTool, OperationDigest: evidence.Digest([]byte("tool slot")), RequestDigest: evidence.Digest([]byte("leased request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: time.Now().UTC().Add(time.Minute)}
}

type boundEffectMemory struct {
	effectMemoryStore
	bindings int
	fail     error
}

func (s *boundEffectMemory) BeginBoundEffect(ctx context.Context, r EffectRequest) (bool, error) {
	s.bindings++
	if s.fail != nil {
		return false, s.fail
	}
	return s.BeginEffect(ctx, r)
}
func TestEffectExecutorBindsOriginalTaskCall(t *testing.T) {
	r := effectRequest()
	call := effectToolCall(r)
	ctx := WithRunCallContext(cortex.WithScope(t.Context(), r.Scope), call)
	s := &boundEffectMemory{}
	f := &effectFixture{}
	executor := effectExecutor(t, s, f, r, nil)
	receipt, err := executor.Execute(ctx, r, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, 1, f.calls)
	require.Equal(t, 1, s.bindings)
	require.NotNil(t, s.request.RunCall)
	require.Equal(t, call, *s.request.RunCall)
	require.NoError(t, receipt.Validate(*s.request))
	require.ErrorIs(t, receipt.Validate(r), ErrInvalidEffect, "proof must bind the original call too")
	// Explicit recovery reads the retained bound request and never dispatches.
	_, err = executor.Reconcile(cortex.WithScope(t.Context(), r.Scope), *s.request)
	require.NoError(t, err)
	require.Equal(t, 1, f.calls)
}
func TestEffectExecutorRejectsMissingOrChangedCallBinding(t *testing.T) {
	for _, mode := range []string{"unsupported", "sealed", "changed", "foreign run", "non-tool", "missing context", "expired call"} {
		t.Run(mode, func(t *testing.T) {
			r := effectRequest()
			call := effectToolCall(r)
			s := &boundEffectMemory{}
			f := &effectFixture{}
			var storage EffectStore = s
			ctx := cortex.WithScope(t.Context(), r.Scope)
			switch mode {
			case "unsupported":
				storage = &effectMemoryStore{}
			case "sealed":
				s.fail = ErrRunCallsSealed
			case "changed":
				other := call
				other.ID = id.New(PrefixRunCall)
				r.RunCall = &other
			case "foreign run":
				call.RunID = id.NewAgentRunID()
			case "non-tool":
				call.Kind = RunCallComplete
			case "expired call":
				call.Deadline = time.Now().Add(-time.Second)
			case "missing context":
				r.RunCall = &call
			}
			if mode != "missing context" {
				ctx = WithRunCallContext(ctx, call)
			}
			executor := effectExecutor(t, storage, f, r, nil)
			_, err := executor.Execute(ctx, r, []byte("payload"))
			require.Error(t, err)
			require.Zero(t, f.calls)
			require.Zero(t, f.probes)
		})
	}
}
func TestEffectCallBindingDetachesNestedScope(t *testing.T) {
	r := effectRequest()
	call := effectToolCall(r)
	r.RunCall = &call
	frozen, _, err := r.Freeze()
	require.NoError(t, err)
	r.RunCall.Scope.Levels[0].Value = "changed"
	require.Equal(t, "effects", frozen.RunCall.Scope.Levels[0].Value)
	offered := cloneEffect(frozen)
	offered.RunCall.Scope.Levels[0].Value = "host mutation"
	require.Equal(t, "effects", frozen.RunCall.Scope.Levels[0].Value)
}

func TestEffectReconcileCannotRelabelOriginalCall(t *testing.T) {
	r := effectRequest()
	call := effectToolCall(r)
	s := &boundEffectMemory{}
	f := &effectFixture{}
	executor := effectExecutor(t, s, f, r, nil)
	_, err := executor.Execute(cortex.WithScope(t.Context(), r.Scope), r, []byte("payload"))
	require.NoError(t, err)
	_, err = executor.Reconcile(WithRunCallContext(cortex.WithScope(t.Context(), r.Scope), call), r)
	require.ErrorIs(t, err, ErrEffectConflict)
	require.Zero(t, f.probes)
	require.Zero(t, s.bindings)
}
