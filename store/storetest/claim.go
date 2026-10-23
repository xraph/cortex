package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func claimFixture(t *testing.T, s store.Store) (context.Context, coordination.ClaimRequest) {
	t.Helper()
	ctx, p, h := assignmentFixture(t, s)
	return ctx, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
}

func testTaskClaims(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[coordination.ClaimStore](t, backend)
	ctx, req := claimFixture(t, backend)
	claim, err := s.AcquireTaskClaim(ctx, req)
	require.NoError(t, err)
	retry, err := s.AcquireTaskClaim(ctx, req)
	require.NoError(t, err)
	require.Equal(t, claim, retry)
	changed := req
	changed.Worker.Principal = "different"
	_, err = s.AcquireTaskClaim(ctx, changed)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	renewed, err := s.RenewTaskClaim(ctx, claim.Token(), 2*time.Minute)
	require.NoError(t, err)
	retry, err = s.RenewTaskClaim(ctx, claim.Token(), 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, renewed, retry)
	child := id.NewAgentRunID()
	admitted, err := s.BeginTaskDispatch(ctx, renewed.Token(), child)
	require.NoError(t, err)
	require.True(t, admitted.Fresh)
	replay, err := s.BeginTaskDispatch(ctx, renewed.Token(), child)
	require.NoError(t, err)
	require.False(t, replay.Fresh, "historical acknowledgments cannot dispatch twice")
	require.Equal(t, admitted.Claim, replay.Claim)
	_, err = s.BeginTaskDispatch(ctx, renewed.Token(), id.NewAgentRunID())
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	current, err := s.GetTaskClaim(ctx, req.PlanID, req.TaskID)
	require.NoError(t, err)
	require.Equal(t, admitted.Claim, current)
	history, err := s.TaskClaimHistory(ctx, coordination.ClaimHistoryRequest{PlanID: req.PlanID, TaskID: req.TaskID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, history, 3)
	list, err := s.ListTaskClaims(ctx, coordination.ClaimListRequest{PlanID: req.PlanID, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []coordination.Claim{current}, list)
	list, err = s.ListTaskClaims(ctx, coordination.ClaimListRequest{PlanID: req.PlanID, AfterTaskID: req.TaskID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, list)
	_, err = s.GetTaskClaim(context.Background(), req.PlanID, req.TaskID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = s.GetTaskClaim(ctxWithScope("another"), req.PlanID, req.TaskID)
	require.ErrorIs(t, err, coordination.ErrClaimNotFound)
}

func testTaskClaimConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[coordination.ClaimStore](t, backend)
	ctx, req := claimFixture(t, backend)
	var wg sync.WaitGroup
	outcomes := make(chan error, 8)
	for i := 0; i < 8; i++ {
		attempt := req
		attempt.ID = id.New(coordination.PrefixClaim)
		wg.Go(func() { _, err := s.AcquireTaskClaim(ctx, attempt); outcomes <- err })
	}
	wg.Wait()
	close(outcomes)
	wins := 0
	for err := range outcomes {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, coordination.ErrClaimConflict)
		}
	}
	require.Equal(t, 1, wins)
	history, err := s.TaskClaimHistory(ctx, coordination.ClaimHistoryRequest{PlanID: req.PlanID, TaskID: req.TaskID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, history, 1)
}
