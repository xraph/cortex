package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testProviderCallReceipts(t *testing.T, newStore func(*testing.T) store.Store) {
	s, ok := newStore(t).(execution.ProviderCallReceiptStore)
	require.True(t, ok, "provider call receipt persistence missing")
	r := storedRunCall()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	got, err := s.GetProviderCallReceipt(ctx, r)
	require.NoError(t, err)
	require.Nil(t, got)
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	receipt := execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "v1", AccountRef: "one", OperationID: "op-one", ProofRef: "receipt-one", ProofDigest: evidence.Digest([]byte("trusted proof fixture")), ObservedAt: time.Now().UTC(), ResultDigest: evidence.Digest([]byte("output"))}
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() { errs <- s.PutProviderCallReceipt(ctx, r, receipt) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err = s.GetProviderCallReceipt(ctx, r)
	require.NoError(t, err)
	require.Equal(t, &receipt, got)
	changed := receipt
	changed.ProofDigest = evidence.Digest([]byte("different proof"))
	require.ErrorIs(t, s.PutProviderCallReceipt(ctx, r, changed), execution.ErrRunCallConflict)
	changedRequest := r
	changedRequest.ID = id.New(execution.PrefixRunCall)
	_, err = s.GetProviderCallReceipt(ctx, changedRequest)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.ErrorIs(t, s.PutProviderCallReceipt(ctx, changedRequest, receipt), execution.ErrInvalidRunCall)
	missing := storedRunCall()
	_, err = s.GetProviderCallReceipt(ctx, missing)
	require.ErrorIs(t, err, execution.ErrRunCallNotFound)
	for _, scope := range []cortex.Scope{{Levels: r.Scope.Levels[:1]}, {Levels: []cortex.Level{{Key: "org", Value: "other"}}}} {
		other := cortex.WithScope(ctx, scope)
		_, err = s.GetProviderCallReceipt(other, r)
		require.ErrorIs(t, err, cortex.ErrInvalidScope)
		require.ErrorIs(t, s.PutProviderCallReceipt(other, r, receipt), cortex.ErrInvalidScope)
	}
	_, err = s.GetProviderCallReceipt(context.Background(), r)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.PutProviderCallReceipt(cancelled, r, receipt), context.Canceled)
	_, err = s.GetProviderCallReceipt(cancelled, r)
	require.ErrorIs(t, err, context.Canceled)
	record, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.Nil(t, record.Completion, "external proof cannot invent a local return")
	fresh, err := s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	require.False(t, fresh)
	// Late local observations are retained, even if they expose disagreement.
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("contradictory output"))}
	require.NoError(t, s.CompleteRunCall(ctx, r, completion))
	record, err = s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.ErrorIs(t, got.ValidateObservation(record), execution.ErrRunCallConflict)
	got, err = s.GetProviderCallReceipt(ctx, r)
	require.NoError(t, err)
	require.Equal(t, &receipt, got, "a disagreement must preserve the original proof")
	t.Run("conflicting concurrent proofs", func(t *testing.T) { testConcurrentProviderProofs(t, s) })
}

func testConcurrentProviderProofs(t *testing.T, s execution.ProviderCallReceiptStore) {
	t.Helper()
	r := storedRunCall()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	c := execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallFailed, Provider: "fixture", AdapterRevision: "v1", AccountRef: "one", OperationID: "original", ProofRef: "proof", ObservedAt: time.Now().UTC()}
	const writers = 8
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			offered := c
			offered.ProofDigest = evidence.Digest([]byte(fmt.Sprintf("conflicting-proof-%d", i)))
			errs <- s.PutProviderCallReceipt(ctx, r, offered)
		})
	}
	wg.Wait()
	close(errs)
	successes := 0
	for writeErr := range errs {
		if writeErr == nil {
			successes++
		} else {
			require.True(t, errors.Is(writeErr, execution.ErrRunCallConflict), "%v", writeErr)
		}
	}
	require.Equal(t, 1, successes)
	got, err := s.GetProviderCallReceipt(ctx, r)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NoError(t, s.PutProviderCallReceipt(ctx, r, *got))
}
