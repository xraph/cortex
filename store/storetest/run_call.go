package storetest

import (
	"context"
	"sort"
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

func storedRunCall() execution.RunCallRequest {
	return execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "calls"}, {Key: "project", Value: "one"}, {Key: "area", Value: "east"}}}, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), ParentRunID: id.NewAgentRunID(), ClaimID: id.New("tclaim"), Kind: execution.RunCallComplete, OperationDigest: evidence.Digest([]byte("generation:0")), RequestDigest: evidence.Digest([]byte("private request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("handoff")), Deadline: time.Now().UTC().Add(time.Minute)}
}

func testRunCalls(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.RunCallStore)
	require.True(t, ok, "durable run-call journal missing")
	r := storedRunCall()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	fresh, err := s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	fresh, err = s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	require.False(t, fresh)
	got, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.NoError(t, got.Validate())
	require.Equal(t, r, got.Request)
	require.Nil(t, got.Completion)
	changed := r
	changed.ID = id.New(execution.PrefixRunCall)
	_, err = s.BeginRunCall(ctx, changed)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	changed = r
	changed.RequestDigest = evidence.Digest([]byte("changed request"))
	_, err = s.BeginRunCall(ctx, changed)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("private result")), TokensReported: true, ReportedTokens: 7}
	require.NoError(t, s.CompleteRunCall(ctx, r, completion))
	require.NoError(t, s.CompleteRunCall(ctx, r, completion))
	got, err = s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.NoError(t, got.Validate())
	require.Equal(t, &completion, got.Completion)
	changedCompletion := completion
	changedCompletion.ReportedTokens++
	require.ErrorIs(t, s.CompleteRunCall(ctx, r, changedCompletion), execution.ErrRunCallConflict)
	fresh, err = s.BeginRunCall(ctx, r)
	require.NoError(t, err)
	require.False(t, fresh, "a recorded return never grants repeat invocation")
	deepScope := r.Scope
	deepScope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	deepScope.Levels[2].Value = "beta"
	for _, scope := range []cortex.Scope{deepScope, {Levels: r.Scope.Levels[:1]}, {Levels: []cortex.Level{{Key: "org", Value: "other"}, {Key: "project", Value: "one"}, {Key: "area", Value: "east"}}}} {
		other := cortex.WithScope(ctx, scope)
		_, err = s.GetRunCall(other, r.RunID, r.OperationDigest)
		require.ErrorIs(t, err, execution.ErrRunCallNotFound)
		_, err = s.BeginRunCall(other, r)
		require.Error(t, err)
		require.Error(t, s.CompleteRunCall(other, r, completion))
		page, listErr := s.ListRunCalls(other, execution.RunCallListRequest{RunID: r.RunID, Limit: 10})
		require.NoError(t, listErr)
		require.Empty(t, page)
	}
	_, err = s.GetRunCall(context.Background(), r.RunID, r.OperationDigest)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.BeginRunCall(cancelled, r)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.CompleteRunCall(cancelled, r, completion), context.Canceled)
	missing := storedRunCall()
	_, missingDigest, err := missing.Freeze()
	require.NoError(t, err)
	completion.RequestDigest = missingDigest
	require.ErrorIs(t, s.CompleteRunCall(ctx, missing, completion), execution.ErrRunCallNotFound)
	missing.Deadline = time.Now().Add(-time.Second)
	_, err = s.BeginRunCall(ctx, missing)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func testRunCallsConcurrentAndDiscovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.RunCallStore)
	require.True(t, ok, "durable run-call journal missing")
	r := storedRunCall()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	var freshCount atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			fresh, err := s.BeginRunCall(ctx, r)
			if fresh {
				freshCount.Add(1)
			}
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, freshCount.Load())
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("return"))}
	completionErrors := make(chan error, 8)
	for range 8 {
		wg.Go(func() { completionErrors <- s.CompleteRunCall(ctx, r, completion) })
	}
	wg.Wait()
	close(completionErrors)
	for completionErr := range completionErrors {
		require.NoError(t, completionErr)
	}
	returned, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.Equal(t, &completion, returned.Completion)
	firstReturn := returned.CompletedAt
	require.NoError(t, s.CompleteRunCall(ctx, r, completion))
	returned, err = s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.NoError(t, err)
	require.Equal(t, firstReturn, returned.CompletedAt)

	requests := []execution.RunCallRequest{r}
	for i := 1; i < 3; i++ {
		next := r
		next.ID = id.New(execution.PrefixRunCall)
		next.OperationDigest = evidence.Digest([]byte{byte(i)})
		fresh, beginErr := s.BeginRunCall(ctx, next)
		require.NoError(t, beginErr)
		require.True(t, fresh)
		requests = append(requests, next)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Key() < requests[j].Key() })
	page, err := s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: r.RunID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, requests[0], page[0].Request)
	require.Equal(t, requests[1], page[1].Request)
	page, err = s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: r.RunID, After: page[1].Request.Key(), Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, requests[2], page[0].Request)
	page[0].Request.Scope.Levels[0].Value = "mutated"
	got, err := s.GetRunCall(ctx, r.RunID, requests[2].OperationDigest)
	require.NoError(t, err)
	require.Equal(t, r.Scope, got.Request.Scope)
	for _, invalid := range []execution.RunCallListRequest{{}, {RunID: r.RunID, Limit: 257}, {RunID: r.RunID, Limit: 1, After: "bad"}} {
		_, err = s.ListRunCalls(ctx, invalid)
		require.ErrorIs(t, err, execution.ErrInvalidRunCall)
	}
}
