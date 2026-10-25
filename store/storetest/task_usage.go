package storetest

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func testTaskUsageSnapshot(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	f := newTaskRecoveryFixture(t, backend, driver, "")
	request := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, request)
	require.NoError(t, err)
	require.NoError(t, snapshot.Validate())
	require.Equal(t, request.Claim, snapshot.Claim.Token())
	require.Equal(t, request.SnapshotDigest, snapshot.Journal.Digest)
	jobs, err := f.repo.ListVerificationJobs(f.ctx, verification.JobListRequest{RunID: snapshot.Claim.RunID, Limit: 256})
	require.NoError(t, err)
	require.NotEmpty(t, jobs)
	require.Equal(t, jobs, snapshot.Jobs)
	usage, err := coordination.SummarizeTaskUsage(snapshot)
	require.NoError(t, err)
	require.Equal(t, 1, usage.ProviderCalls)
	require.Zero(t, usage.Known, "legacy verifier fixture has no metering")

	for _, mode := range []string{"scope", "no scope", "claim", "journal", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req := f.ctx, request
			switch mode {
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "no scope":
				ctx = context.Background()
			case "claim":
				req.Claim.Revision++
			case "journal":
				req.SnapshotDigest = evidence.Digest([]byte("changed"))
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, readErr := f.repo.SnapshotTaskUsage(ctx, req)
			require.Error(t, readErr)
			require.Empty(t, got)
		})
	}
	accepted, err := f.repo.AdoptTaskResult(f.ctx, f.request)
	require.NoError(t, err)
	got, err := f.repo.SnapshotTaskUsage(f.ctx, request)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	require.Empty(t, got)
	request.Claim = accepted.Claim.Token()
	current, err := f.repo.SnapshotTaskUsage(f.ctx, request)
	require.NoError(t, err)
	require.Equal(t, snapshot.Jobs, current.Jobs)
	require.NotEqual(t, snapshot.Digest, current.Digest)

	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	for _, mode := range []string{"lost acknowledgement", "cancelled after commit", "oversized history", "malformed job"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			faulty := &submissionFenceDriver{Driver: driver, plans: plans, lostAck: mode == "lost acknowledgement"}
			if mode == "cancelled after commit" {
				faulty.afterCommit = cancel
			}
			faulty.afterSelect = func(_ context.Context, query assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
				if query.Kind == assurance.KindJob {
					switch mode {
					case "oversized history":
						return make([]assurance.Record, coordination.MaxTaskUsageJobs+1), nil
					case "malformed job":
						rows[0].Body = "{}"
					}
				}
				return rows, nil
			}
			got, readErr := assurance.New(faulty).SnapshotTaskUsage(ctx, request)
			require.Error(t, readErr)
			require.Empty(t, got)
		})
	}

	t.Run("verifier history waits through snapshot", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var signal, unblock sync.Once
		defer unblock.Do(func() { close(release) })
		paused := &submissionFenceDriver{Driver: driver, plans: plans}
		paused.afterSelect = func(tc context.Context, query assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
			if query.Kind == assurance.KindJob {
				signal.Do(func() { close(entered) })
				select {
				case <-release:
				case <-tc.Done():
					return nil, tc.Err()
				}
			}
			return rows, nil
		}
		type result struct {
			snapshot coordination.TaskUsageSnapshot
			err      error
		}
		done := make(chan result, 1)
		go func() {
			got, readErr := assurance.New(paused).SnapshotTaskUsage(ctx, request)
			done <- result{got, readErr}
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("snapshot did not reach the verifier catalog")
		}
		// Insert a valid retained prior-attempt record under the same run guard
		// used by job enqueue and mutation. This is fixture history, not authority.
		prior := current.Jobs[0]
		prior.Request.ID = id.New(verification.PrefixJob)
		prior.Request, prior.RequestDigest, err = prior.Request.Freeze()
		require.NoError(t, err)
		prior, err = verification.SealJob(prior)
		require.NoError(t, err)
		body, err := json.Marshal(prior)
		require.NoError(t, err)
		columns := assurance.Columns(prior.Request.Binding.Scope)
		row := assurance.Record{ID: prior.Request.ID.String(), Kind: assurance.KindJob, ScopeL0: columns[0], ScopeL1: columns[1], ScopeL2: columns[2], ScopeCanon: prior.Request.Binding.Scope.Canonical(), ScopeExtra: "{}", Digest: prior.Digest, RunID: prior.Request.Binding.RunID.String(), Body: string(body)}
		runs, supported := driver.(assurance.RunDriver)
		require.True(t, supported)
		started, written := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			written <- runs.TransactRun(ctx, prior.Request.Binding.RunID, func(tc context.Context, tx assurance.RunTransaction) error { return tx.Insert(tc, row) })
		}()
		<-started
		select {
		case writeErr := <-written:
			t.Fatalf("verifier writer escaped snapshot guard: %v", writeErr)
		case <-time.After(150 * time.Millisecond):
		}
		unblock.Do(func() { close(release) })
		select {
		case got := <-done:
			require.NoError(t, got.err)
			require.Len(t, got.snapshot.Jobs, 1)
		case <-ctx.Done():
			t.Fatal("snapshot did not finish")
		}
		select {
		case writeErr := <-written:
			require.NoError(t, writeErr)
		case <-ctx.Done():
			t.Fatal("verifier writer did not finish")
		}
		updated, readErr := f.repo.SnapshotTaskUsage(ctx, request)
		require.NoError(t, readErr)
		require.Len(t, updated.Jobs, 2)
		require.NotEqual(t, current.Digest, updated.Digest)
	})
}
