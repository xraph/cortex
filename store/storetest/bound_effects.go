package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func storedBoundEffect() execution.EffectRequest {
	r := storedEffect()
	call := storedRunCall()
	call.Scope = r.Scope
	call.RunID = r.RunID
	call.Kind = execution.RunCallTool
	r.RunCall = &call
	return r
}
func testBoundEffects(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.BoundEffectStore)
	require.True(t, ok)
	calls, ok := backend.(execution.RunCallSealStore)
	require.True(t, ok)
	r := storedBoundEffect()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	fresh, err := s.BeginBoundEffect(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallNotFound)
	require.False(t, fresh)
	fresh, err = calls.BeginRunCall(ctx, *r.RunCall)
	require.NoError(t, err)
	require.True(t, fresh)
	invalid := r
	wrongCall := *r.RunCall
	wrongCall.ID = id.New(execution.PrefixRunCall)
	invalid.RunCall = &wrongCall
	fresh, err = s.BeginBoundEffect(ctx, invalid)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.False(t, fresh)
	invalid.RunCall = nil
	fresh, err = s.BeginBoundEffect(ctx, invalid)
	require.ErrorIs(t, err, execution.ErrInvalidEffect)
	require.False(t, fresh)
	fresh, err = s.BeginBoundEffect(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	fresh, err = s.BeginBoundEffect(ctx, r)
	require.NoError(t, err)
	require.False(t, fresh)
	got, err := s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.Equal(t, r, got.Request)
	changed := r
	call := *r.RunCall
	call.ID = id.New(execution.PrefixRunCall)
	changed.RunCall = &call
	fresh, err = s.BeginBoundEffect(ctx, changed)
	require.Error(t, err)
	require.False(t, fresh)
	// A legacy unbound operation cannot be reassigned to a call retroactively.
	legacy := r
	legacy.OperationKey = "legacy-" + r.OperationKey
	legacy.RunCall = nil
	fresh, err = s.BeginEffect(ctx, legacy)
	require.NoError(t, err)
	require.True(t, fresh)
	legacy.RunCall = r.RunCall
	fresh, err = s.BeginBoundEffect(ctx, legacy)
	require.ErrorIs(t, err, execution.ErrEffectConflict)
	require.False(t, fresh)
	// Returning from a call prevents fresh effects even while its run is open.
	_, digest, err := r.RunCall.Freeze()
	require.NoError(t, err)
	require.NoError(t, calls.CompleteRunCall(ctx, *r.RunCall, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("tool returned"))}))
	next := r
	next.OperationKey = "late-" + r.OperationKey
	fresh, err = s.BeginBoundEffect(ctx, next)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.False(t, fresh)
	_, err = calls.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "close effect admission"})
	require.NoError(t, err)
	for _, bound := range []bool{false, true} {
		next := r
		next.OperationKey = "sealed-" + r.OperationKey
		if bound {
			fresh, err = s.BeginBoundEffect(ctx, next)
		} else {
			next.RunCall = nil
			fresh, err = s.BeginEffect(ctx, next)
		}
		require.ErrorIs(t, err, execution.ErrRunCallsSealed)
		require.False(t, fresh)
	}
	require.NoError(t, s.CompleteEffect(ctx, r, storedEffectReceipt(t, r)), "late proof remains writable")
	got, err = s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.NotNil(t, got.Receipt)
	require.NoError(t, got.Receipt.Validate(r))
	fresh, err = s.BeginBoundEffect(context.Background(), r)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	require.False(t, fresh)
	other := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
	fresh, err = s.BeginBoundEffect(other, r)
	require.ErrorIs(t, err, cortex.ErrInvalidScope)
	require.False(t, fresh)
}
func testBoundEffectSealRace(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.BoundEffectStore)
	require.True(t, ok)
	calls, ok := backend.(execution.RunCallSealStore)
	require.True(t, ok)
	discovery, ok := backend.(execution.EffectDiscoveryStore)
	require.True(t, ok)
	r := storedBoundEffect()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := calls.BeginRunCall(ctx, *r.RunCall)
	require.NoError(t, err)
	const workers = 8
	var fresh atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, workers+1)
	start := make(chan struct{})
	for range workers {
		next := r
		next.OperationKey = id.New(execution.PrefixEffectAttempt).String()
		wg.Go(func() {
			<-start
			created, beginErr := s.BeginBoundEffect(ctx, next)
			if created {
				fresh.Add(1)
			}
			errors <- beginErr
		})
	}
	wg.Go(func() {
		<-start
		_, sealErr := calls.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "race admission"})
		errors <- sealErr
	})
	close(start)
	wg.Wait()
	close(errors)
	for observed := range errors {
		if observed != nil {
			require.ErrorIs(t, observed, execution.ErrRunCallsSealed)
		}
	}
	rows, err := discovery.ListEffects(ctx, execution.EffectListRequest{RunID: r.RunID, Limit: workers + 1})
	require.NoError(t, err)
	require.Len(t, rows, int(fresh.Load()))
	for _, row := range rows {
		require.Equal(t, r.RunCall, row.Request.RunCall)
	}
}

func testBoundEffectCompletionRace(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.BoundEffectStore)
	require.True(t, ok)
	calls, ok := backend.(execution.RunCallStore)
	require.True(t, ok)
	r := storedBoundEffect()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := calls.BeginRunCall(ctx, *r.RunCall)
	require.NoError(t, err)
	_, digest, err := r.RunCall.Freeze()
	require.NoError(t, err)
	start := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		<-start
		completed <- calls.CompleteRunCall(ctx, *r.RunCall, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("return"))})
	}()
	close(start)
	fresh, err := s.BeginBoundEffect(ctx, r)
	if err != nil {
		require.ErrorIs(t, err, execution.ErrRunCallConflict)
		require.False(t, fresh)
	}
	require.NoError(t, <-completed)
	next := r
	next.OperationKey = "after-return-" + r.OperationKey
	fresh, err = s.BeginBoundEffect(ctx, next)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.False(t, fresh)
}
