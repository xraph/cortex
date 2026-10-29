package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// LeasedClaims proves the claims that take a run's lease, and write its
// first progress snapshot, in the transaction that moves it to running: a
// claimer that dies right after leaves a lease that lapses and a snapshot
// to continue from, never a running run nobody owns.
func LeasedClaims(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("ClaimSuspensionLeased", func(t *testing.T) { testClaimSuspensionLeased(t, newStore) })
	t.Run("ClaimSuspensionLeasedRefusals", func(t *testing.T) { testClaimSuspensionLeasedRefusals(t, newStore) })
	t.Run("ClaimSuspensionLeasedExpired", func(t *testing.T) { testClaimSuspensionLeasedExpired(t, newStore) })
	t.Run("ClaimContinuationLeased", func(t *testing.T) { testClaimContinuationLeased(t, newStore) })
	t.Run("StartResumeLeased", func(t *testing.T) { testStartResumeLeased(t, newStore) })
}

func leasedClaims(t *testing.T, s store.Store) suspension.LeasedClaimStore {
	t.Helper()
	leased, ok := s.(suspension.LeasedClaimStore)
	require.True(t, ok, "%T does not implement suspension.LeasedClaimStore", s)
	return leased
}

func firstSnapshot(runID id.AgentRunID, owner string) *suspension.Progress {
	return &suspension.Progress{
		RunID: runID, Owner: owner,
		Cont:    newSuspension(runID).Cont,
		Done:    []suspension.ProgressResult{{CallID: "call_conformance", Content: "answer", Outcome: "completed"}},
		Started: []suspension.PendingCall{{ID: "call_sent", Name: "sent-tool", Arguments: `{}`}},
	}
}

func requireFirstSnapshot(t *testing.T, s store.Store, ctx context.Context, runID id.AgentRunID, owner string) { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	progress, ok := s.(suspension.ProgressStore)
	require.True(t, ok)
	saved, err := progress.GetProgress(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, owner, saved.Owner)
	require.Equal(t, newSuspension(runID).Cont.StepIndex, saved.Cont.StepIndex)
	require.Len(t, saved.Done, 1)
	require.Equal(t, "answer", saved.Done[0].Content)
	require.Len(t, saved.Started, 1)
	require.Equal(t, "call_sent", saved.Started[0].ID)
}

func testClaimSuspensionLeased(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	leased := leasedClaims(t, s)
	ctx := ctxWithScope("leased-claim")
	r, susp := mustSuspendRun(t, s, ctx, futureDeadline())
	lease := leaseAt("w1", time.Now().Add(time.Minute))

	claimed, err := leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{
		Version: suspension.ClaimVersion{RunID: r.ID, SuspensionID: susp.ID, RunRevision: r.Revision},
		Lease:   lease,
		First:   firstSnapshot(r.ID, "w1"),
	})
	require.NoError(t, err)
	require.Equal(t, susp.ID, claimed.ID)
	require.NotNil(t, claimed.ClaimedRun)
	require.Equal(t, run.StateRunning, claimed.ClaimedRun.State)
	require.Equal(t, r.Revision+1, claimed.ClaimedRun.Revision)
	stored, err := s.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, stored.State)
	require.True(t, sameLease(stored.Lease, lease), "stored lease %+v, want %+v", stored.Lease, lease)
	requireFirstSnapshot(t, s, ctx, r.ID, "w1")
	// The pause stays until the claimer deletes it.
	_, err = s.GetSuspension(ctx, r.ID)
	require.NoError(t, err)

	_, err = leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{
		Version: suspension.ClaimVersion{RunID: r.ID, SuspensionID: susp.ID, RunRevision: stored.Revision},
		Lease:   leaseAt("w2", time.Now().Add(time.Minute)),
	})
	require.ErrorIs(t, err, cortex.ErrNotSuspended, "a claimed pause was claimed twice")
}

func testClaimSuspensionLeasedRefusals(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	leased := leasedClaims(t, s)
	ctx := ctxWithScope("leased-refusals")
	r, susp := mustSuspendRun(t, s, ctx, futureDeadline())
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	version := suspension.ClaimVersion{RunID: r.ID, SuspensionID: susp.ID, RunRevision: r.Revision}

	for name, claim := range map[string]suspension.LeasedClaim{
		"half lease":    {Version: version, Lease: run.Lease{Owner: "w1"}},
		"no lease":      {Version: version},
		"foreign first": {Version: version, Lease: lease, First: firstSnapshot(r.ID, "someone-else")},
	} {
		_, err := leased.ClaimSuspensionLeased(ctx, claim)
		require.ErrorIs(t, err, cortex.ErrRunLeaseLost, name)
	}
	stale := version
	stale.RunRevision++
	_, err := leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{Version: stale, Lease: lease})
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	other := version
	other.SuspensionID = id.NewSuspensionID()
	_, err = leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{Version: other, Lease: lease})
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	_, err = leased.ClaimSuspensionLeased(ctxWithScope("leased-elsewhere"), suspension.LeasedClaim{Version: version, Lease: lease})
	require.ErrorIs(t, err, cortex.ErrNotSuspended)

	stored, err := s.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, stored.State, "a refused claim moved the run")
	require.Equal(t, r.Revision, stored.Revision)
	require.Nil(t, stored.Lease)
	progress, ok := s.(suspension.ProgressStore)
	require.True(t, ok)
	_, err = progress.GetProgress(ctx, r.ID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound, "a refused claim wrote a snapshot")
}

func testClaimSuspensionLeasedExpired(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	leased := leasedClaims(t, s)
	ctx := ctxWithScope("leased-expired")
	past := time.Now().UTC().Add(-time.Minute)
	r, susp := mustSuspendRun(t, s, ctx, &past)
	live, liveSusp := mustSuspendRun(t, s, ctx, futureDeadline())
	lease := leaseAt("sweeper", time.Now().Add(time.Minute))

	_, err := leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{Version: suspension.ClaimVersion{RunID: r.ID, SuspensionID: susp.ID, RunRevision: r.Revision}, Lease: lease})
	require.ErrorIs(t, err, cortex.ErrSuspensionExpired, "a resume claimed a pause past its deadline")
	_, err = leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{Version: suspension.ClaimVersion{RunID: live.ID, SuspensionID: liveSusp.ID, RunRevision: live.Revision}, ExpiredBy: time.Now().UTC(), Lease: lease})
	require.ErrorIs(t, err, cortex.ErrNotSuspended, "the sweeper claimed a pause before its deadline")

	claimed, err := leased.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{Version: suspension.ClaimVersion{RunID: r.ID, SuspensionID: susp.ID, RunRevision: r.Revision}, ExpiredBy: time.Now().UTC(), Lease: lease})
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, claimed.ClaimedRun.State)
	stored, err := s.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.True(t, sameLease(stored.Lease, lease))
}

func testClaimContinuationLeased(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	leased := leasedClaims(t, s)
	ctx := ctxWithScope("leased-continue")
	r := mustReadyRun(t, s, ctx)
	lease := leaseAt("w1", time.Now().Add(time.Minute))

	_, err := leased.ClaimContinuationLeased(ctx, r.ID, run.Lease{}, nil)
	require.ErrorIs(t, err, cortex.ErrRunLeaseLost, "a leased claim took a run with no lease")
	claimed, err := leased.ClaimContinuationLeased(ctx, r.ID, lease, firstSnapshot(r.ID, "w1"))
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, claimed.State)
	require.Nil(t, claimed.Ready)
	require.Equal(t, r.Revision+1, claimed.Revision)
	require.True(t, sameLease(claimed.Lease, lease))
	requireFirstSnapshot(t, s, ctx, r.ID, "w1")
	_, err = leased.ClaimContinuationLeased(ctx, r.ID, leaseAt("w2", time.Now().Add(time.Minute)), nil)
	require.ErrorIs(t, err, cortex.ErrRunNotContinuable)
}

func testStartResumeLeased(t *testing.T, newStore func(t *testing.T) store.Store) {
	backend := newStore(t)
	leased := leasedClaims(t, backend)
	ctx, durable, req := admissionFixture(t, backend)
	admitted, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	lease := leaseAt("w1", time.Now().Add(time.Minute))

	_, err = leased.StartResumeLeased(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest, run.Lease{}, nil)
	require.ErrorIs(t, err, cortex.ErrRunLeaseLost, "a leased start took a run with no lease")
	started, err := leased.StartResumeLeased(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest, lease, firstSnapshot(req.Version.RunID, "w1"))
	require.NoError(t, err)
	require.True(t, started.Fresh)
	require.NotNil(t, started.Suspension)
	require.Equal(t, admitted.ClaimedRevision, started.Suspension.ClaimedRun.Revision, "the start moved the revision the admission recorded")
	current, err := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, current.State)
	require.Equal(t, admitted.ClaimedRevision, current.Revision)
	require.True(t, sameLease(current.Lease, lease))
	requireFirstSnapshot(t, backend, ctx, req.Version.RunID, "w1")

	again, err := leased.StartResumeLeased(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest, leaseAt("w2", time.Now().Add(time.Minute)), nil)
	require.NoError(t, err)
	require.False(t, again.Fresh, "a resume was started twice")
	unchanged, err := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, err)
	require.True(t, sameLease(unchanged.Lease, lease), "a repeated start took the lease")
}
