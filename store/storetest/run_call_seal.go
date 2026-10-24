package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testRunCallSeals(t *testing.T, newStore func(*testing.T) store.Store) {
	s, ok := newStore(t).(execution.RunCallSealStore)
	require.True(t, ok)
	r := storedRunCall()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	absent, err := s.GetRunCallSeal(ctx, r.RunID)
	require.NoError(t, err)
	require.Nil(t, absent)
	fresh, err := s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "freeze call discovery"}
	sealed, err := s.SealRunCalls(ctx, req)
	require.NoError(t, err)
	require.NoError(t, sealed.Validate())
	again, err := s.SealRunCalls(ctx, req)
	require.NoError(t, err)
	require.Equal(t, sealed, again)
	changed := req
	changed.Reason = "different"
	_, err = s.SealRunCalls(ctx, changed)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	// Historical admission is still not fresh, and new slots cannot be admitted.
	fresh, err = s.BeginRunCall(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallsSealed)
	require.False(t, fresh)
	next := r
	next.ID, next.OperationDigest = id.New(execution.PrefixRunCall), evidence.Digest([]byte("next slot"))
	fresh, err = s.BeginRunCall(ctx, next)
	require.ErrorIs(t, err, execution.ErrRunCallsSealed)
	require.False(t, fresh)
	// Closing membership preserves late local return evidence.
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("late return"))}
	require.NoError(t, s.CompleteRunCall(ctx, r, completion))
	row, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.Equal(t, &completion, row.Completion)
	page, err := s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: r.RunID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page, 1)
	got, err := s.GetRunCallSeal(ctx, r.RunID)
	require.NoError(t, err)
	require.Equal(t, &sealed, got)
	other := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
	got, err = s.GetRunCallSeal(other, r.RunID)
	require.NoError(t, err)
	require.Nil(t, got)
	_, err = s.SealRunCalls(other, req)
	require.ErrorIs(t, err, cortex.ErrInvalidScope)
	_, err = s.GetRunCallSeal(context.Background(), r.RunID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	// An empty journal can be sealed before a delayed worker creates its first slot.
	empty := storedRunCall()
	req.RunID = empty.RunID
	_, err = s.SealRunCalls(ctx, req)
	require.NoError(t, err)
	fresh, err = s.BeginRunCall(ctx, empty)
	require.ErrorIs(t, err, execution.ErrRunCallsSealed)
	require.False(t, fresh)
}

func testRunCallSealRace(t *testing.T, newStore func(*testing.T) store.Store) {
	s, ok := newStore(t).(execution.RunCallSealStore)
	require.True(t, ok)
	r := storedRunCall()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "freeze call discovery"}
	const writers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	admitted := make(chan execution.RunCallRequest, writers)
	failures := make(chan error, writers+1)
	for i := range writers {
		wg.Go(func() {
			<-start
			offered := r
			offered.ID, offered.OperationDigest = id.New(execution.PrefixRunCall), evidence.Digest([]byte(fmt.Sprintf("slot-%d", i)))
			fresh, err := s.BeginRunCall(ctx, offered)
			if err != nil && !errors.Is(err, execution.ErrRunCallsSealed) {
				failures <- err
			}
			if fresh {
				admitted <- offered
			}
		})
	}
	wg.Go(func() {
		<-start
		_, err := s.SealRunCalls(ctx, req)
		if err != nil {
			failures <- err
		}
	})
	close(start)
	wg.Wait()
	close(failures)
	close(admitted)
	for err := range failures {
		require.NoError(t, err)
	}
	rows, err := s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: r.RunID, Limit: 256})
	require.NoError(t, err)
	require.Len(t, rows, len(admitted))
	for original := range admitted {
		got, readErr := s.GetRunCall(ctx, original.RunID, original.OperationDigest)
		require.NoError(t, readErr)
		require.Equal(t, original, got.Request)
	}
	got, err := s.GetRunCallSeal(ctx, r.RunID)
	require.NoError(t, err)
	require.NotNil(t, got)
	fresh, err := s.BeginRunCall(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallsSealed)
	require.False(t, fresh)
}
