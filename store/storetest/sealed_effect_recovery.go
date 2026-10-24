package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type sealedEffectProbe struct {
	t       *testing.T
	probes  int
	unknown string
}

func (s *sealedEffectProbe) Execute(context.Context, execution.EffectRequest, []byte) (execution.EffectReceipt, error) {
	s.t.Error("recovery dispatched a mutation")
	return execution.EffectReceipt{}, execution.ErrEffectUncertain
}
func (s *sealedEffectProbe) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	s.probes++
	if r.OperationKey == s.unknown {
		return execution.EffectStatus{Outcome: execution.EffectUnknown}, nil
	}
	receipt := storedEffectReceipt(s.t, r)
	return execution.EffectStatus{Outcome: receipt.Outcome, Receipt: &receipt}, nil
}
func testSealedEffectRecovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	effects, ok := backend.(execution.BoundEffectStore)
	require.True(t, ok)
	calls, ok := backend.(execution.RunCallSealStore)
	require.True(t, ok)
	r := storedBoundEffect()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	fresh, err := calls.BeginRunCall(ctx, *r.RunCall)
	require.NoError(t, err)
	require.True(t, fresh)
	requests := make([]execution.EffectRequest, 1, 3)
	requests[0] = r
	for range 2 {
		next := r
		next.OperationKey = id.New(execution.PrefixEffectAttempt).String()
		next.AttemptID = id.New(execution.PrefixEffectAttempt)
		requests = append(requests, next)
	}
	requests[2].RunCall = nil
	for _, request := range requests {
		fresh, err = effects.BeginEffect(ctx, request)
		require.NoError(t, err)
		require.True(t, fresh)
	}
	seal, err := calls.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "recover retained effects"})
	require.NoError(t, err)
	probe := &sealedEffectProbe{t: t, unknown: requests[1].OperationKey}
	config := execution.EffectExecutorConfig{Store: effects, Adapters: []execution.EffectRegistration{{Requirement: r.Requirement, Adapter: probe}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, AuthorizeScan: func(_ context.Context, offered execution.RunCallSeal) error {
		require.Equal(t, seal, offered)
		return nil
	}, MaxConcurrent: 1}
	executor, err := execution.NewEffectExecutor(config)
	require.NoError(t, err)
	scan := func() (terminal, unknown int) {
		request := execution.EffectRecoveryPageRequest{Seal: seal, Limit: 2}
		for range 3 {
			page, scanErr := executor.RecoverSealedPage(ctx, request)
			require.NoError(t, scanErr)
			for _, item := range page.Items {
				switch item.Status {
				case execution.EffectRecoveryTerminal:
					terminal++
				case execution.EffectRecoveryUncertain:
					unknown++
				default:
					t.Fatal("unexpected recovery status")
				}
			}
			if page.NextAfter == "" {
				return terminal, unknown
			}
			require.Greater(t, page.NextAfter, request.After)
			request.After = page.NextAfter
		}
		t.Fatal("bounded scan did not terminate")
		return 0, 0
	}
	terminal, unknown := scan()
	require.Equal(t, 2, terminal)
	require.Equal(t, 1, unknown)
	require.Equal(t, 3, probe.probes)
	probe.unknown = ""
	executor, err = execution.NewEffectExecutor(config)
	require.NoError(t, err)
	terminal, unknown = scan()
	require.Equal(t, 3, terminal)
	require.Zero(t, unknown)
	require.Equal(t, 4, probe.probes)
	for _, request := range requests {
		retained, readErr := effects.GetEffect(ctx, request.OperationKey)
		require.NoError(t, readErr)
		require.Equal(t, request.RunCall, retained.Request.RunCall)
		require.NotNil(t, retained.Receipt)
		require.NoError(t, retained.Receipt.Validate(request))
	}
	fresh, err = effects.BeginEffect(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallsSealed)
	require.False(t, fresh)
}
