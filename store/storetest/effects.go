package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func storedEffect() execution.EffectRequest {
	return execution.EffectRequest{SchemaVersion: 1, Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "effects"}, {Key: "project", Value: "one"}, {Key: "worker", Value: "one"}}}, RunID: id.NewAgentRunID(), OperationKey: id.New(execution.PrefixEffectAttempt).String(), AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte("payload")), PrincipalRef: "worker", AuthorityRef: "grant-1", Deadline: time.Now().UTC().Add(time.Minute), Requirement: execution.EffectRequirement{Tool: "publish", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "probe-1", Retry: execution.RetryPolicy{MaxAttempts: 1}}}
}
func storedEffectReceipt(t *testing.T, r execution.EffectRequest) execution.EffectReceipt {
	t.Helper()
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	return execution.EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.EffectApplied, ExternalID: "external-1", ExternalVersion: "v1", EvidenceRef: "provider-receipt-1"}
}
func testEffects(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.EffectStore)
	require.True(t, ok, "effect persistence capability missing")
	r := storedEffect()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	created, err := s.BeginEffect(ctx, r)
	require.NoError(t, err)
	require.True(t, created)
	created, err = s.BeginEffect(ctx, r)
	require.NoError(t, err)
	require.False(t, created, "replay must never receive dispatch permission")
	got, err := s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.Equal(t, r, got.Request)
	require.Nil(t, got.Receipt)
	changed := r
	changed.ArgsDigest = evidence.Digest([]byte("different"))
	created, err = s.BeginEffect(ctx, changed)
	require.ErrorIs(t, err, execution.ErrEffectConflict)
	require.False(t, created)
	changed = r
	changed.AttemptID = id.New(execution.PrefixEffectAttempt)
	_, err = s.BeginEffect(ctx, changed)
	require.ErrorIs(t, err, execution.ErrEffectConflict)
	receipt := storedEffectReceipt(t, r)
	require.NoError(t, s.CompleteEffect(ctx, r, receipt))
	require.NoError(t, s.CompleteEffect(ctx, r, receipt))
	got, err = s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.Equal(t, &receipt, got.Receipt)
	conflict := receipt
	conflict.ExternalVersion = "v2"
	require.ErrorIs(t, s.CompleteEffect(ctx, r, conflict), execution.ErrEffectConflict)
	got.Receipt.ExternalID = "caller mutation"
	again, err := s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.Equal(t, &receipt, again.Receipt)
	for _, scope := range []cortex.Scope{{Levels: r.Scope.Levels[:1]}, {Levels: []cortex.Level{{Key: "org", Value: "effects"}, {Key: "project", Value: "one"}, {Key: "worker", Value: "two"}}}} {
		other := cortex.WithScope(ctx, scope)
		_, err = s.GetEffect(other, r.OperationKey)
		require.ErrorIs(t, err, execution.ErrEffectNotFound)
		_, err = s.BeginEffect(other, r)
		require.Error(t, err)
		require.Error(t, s.CompleteEffect(other, r, receipt))
	}
	_, err = s.BeginEffect(context.Background(), r)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = s.GetEffect(context.Background(), r.OperationKey)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	missing := storedEffect()
	require.ErrorIs(t, s.CompleteEffect(ctx, missing, storedEffectReceipt(t, missing)), execution.ErrEffectNotFound)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.BeginEffect(cancelled, r)
	require.ErrorIs(t, err, context.Canceled)
}
func testEffectsConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	s, ok := newStore(t).(execution.EffectStore)
	require.True(t, ok, "effect persistence capability missing")
	r := storedEffect()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	var count atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			created, err := s.BeginEffect(ctx, r)
			if created {
				count.Add(1)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, count.Load())
	a := storedEffectReceipt(t, r)
	b := a
	b.ExternalVersion = "v2"
	errs = make(chan error, 2)
	for _, v := range []execution.EffectReceipt{a, b} {
		wg.Go(func() { errs <- s.CompleteEffect(ctx, r, v) })
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, execution.ErrEffectConflict)
			conflicts++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflicts)
}
