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

func usageClosureStoreFixture(t *testing.T, backend store.Store, driver assurance.Driver) (taskRecoveryFixture, coordination.TaskUsageClosureRequest, coordination.TaskUsageSnapshot) {
	t.Helper()
	f := newTaskRecoveryFixture(t, backend, driver, "")
	accepted, err := f.repo.AdoptTaskResult(f.ctx, f.request)
	require.NoError(t, err)
	r := coordination.TaskUsageRequest{Claim: accepted.Claim.Token(), Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, r)
	require.NoError(t, err)
	return f, coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: r, UsageDigest: snapshot.Digest, Actor: "operator", Reason: "close final accounting inputs"}, snapshot
}

func testTaskUsageClosure(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("closed admission and historical retry", func(t *testing.T) {
		f, request, snapshot := usageClosureStoreFixture(t, backend, driver)
		missing, err := f.repo.GetTaskUsageClosure(f.ctx, snapshot.Claim.RunID)
		require.NoError(t, err)
		require.Nil(t, missing)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		require.True(t, closed.Fresh)
		require.NoError(t, closed.Validate())
		closed.Fresh = false
		retained, err := f.repo.GetTaskUsageClosure(f.ctx, snapshot.Claim.RunID)
		require.NoError(t, err)
		require.Equal(t, &closed, retained)
		replayed, err := assurance.New(driver).CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		require.Equal(t, closed, replayed)
		changed := request
		changed.Reason = "different operation"
		_, err = f.repo.CloseTaskUsage(f.ctx, changed)
		require.ErrorIs(t, err, coordination.ErrTaskUsageConflict)

		job := snapshot.Jobs[0]
		newJob := job.Request
		newJob.ID = id.New(verification.PrefixJob)
		_, err = f.repo.EnqueueVerificationJob(f.ctx, newJob)
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		_, err = f.repo.CaptureVerificationJob(f.ctx, newJob)
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		oldJob, err := f.repo.EnqueueVerificationJob(f.ctx, job.Request)
		require.NoError(t, err)
		require.Equal(t, job, oldJob, "historical enqueue acknowledgment grants no new work")
		_, err = f.repo.ClaimVerificationJob(f.ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		lease := verification.JobLease{JobID: job.Request.ID, ClaimID: job.ClaimIDs[0], Generation: job.Generation, Owner: "worker", ExpiresAt: job.Request.Deadline}
		_, err = f.repo.RenewVerificationJob(f.ctx, lease, time.Minute)
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		_, err = f.repo.BeginVerificationCall(f.ctx, lease, verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: "checker", VerifierRevision: "1"})
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		after, err := f.repo.SnapshotTaskUsage(f.ctx, request.Usage)
		require.NoError(t, err)
		require.Equal(t, snapshot, after)
	})
	t.Run("stale evidence never closes admission", func(t *testing.T) {
		f, request, snapshot := usageClosureStoreFixture(t, backend, driver)
		for _, mode := range []string{"usage", "claim", "journal", "cancelled", "scope", "missing scope"} {
			t.Run(mode, func(t *testing.T) {
				ctx, r := f.ctx, request
				switch mode {
				case "usage":
					r.UsageDigest = evidence.Digest([]byte("other history"))
				case "claim":
					r.Usage.Claim.Revision++
				case "journal":
					r.Usage.SnapshotDigest = evidence.Digest([]byte("other journal"))
				case "scope":
					ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
				case "missing scope":
					ctx = context.Background()
				case "cancelled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				got, err := f.repo.CloseTaskUsage(ctx, r)
				require.Error(t, err)
				require.Empty(t, got)
				closed, err := f.repo.GetTaskUsageClosure(f.ctx, snapshot.Claim.RunID)
				require.NoError(t, err)
				require.Nil(t, closed)
			})
		}
	})
	t.Run("transaction faults", func(t *testing.T) {
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		for _, mode := range []string{"rollback", "lost acknowledgment", "cancelled after commit"} {
			t.Run(mode, func(t *testing.T) {
				f, request, snapshot := usageClosureStoreFixture(t, backend, driver)
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				fault := &submissionFenceDriver{Driver: driver, plans: plans, failInsert: mode == "rollback", lostAck: mode == "lost acknowledgment"}
				if mode == "cancelled after commit" {
					fault.afterCommit = cancel
				}
				got, err := assurance.New(fault).WithClock(f.clock).CloseTaskUsage(ctx, request)
				require.Error(t, err)
				require.Empty(t, got)
				closed, err := f.repo.GetTaskUsageClosure(f.ctx, snapshot.Claim.RunID)
				require.NoError(t, err)
				if mode == "rollback" {
					require.Nil(t, closed)
				} else {
					require.NotNil(t, closed)
				}
				retry, err := f.repo.CloseTaskUsage(f.ctx, request)
				require.NoError(t, err)
				require.Equal(t, mode == "rollback", retry.Fresh)
			})
		}
	})
	t.Run("unfinished work cannot close", func(t *testing.T) {
		f := newTaskRecoveryFixture(t, backend, driver, "")
		r := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
		snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, r)
		require.NoError(t, err)
		request := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: r, UsageDigest: snapshot.Digest, Actor: "operator", Reason: "close"}
		_, err = f.repo.CloseTaskUsage(f.ctx, request)
		require.ErrorIs(t, err, coordination.ErrTaskUsageConflict)

		f, request, _ = usageClosureStoreFixture(t, backend, driver)
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		pending := &submissionFenceDriver{Driver: driver, plans: plans}
		pending.afterSelect = func(_ context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
			if q.Kind != assurance.KindJob {
				return rows, nil
			}
			var job verification.Job
			if decodeErr := json.Unmarshal([]byte(rows[0].Body), &job); decodeErr != nil {
				return nil, decodeErr
			}
			job.State, job.ResultID = verification.JobReady, id.ID{}
			job.Lease = &verification.JobLease{JobID: job.Request.ID, ClaimID: job.ClaimIDs[0], Owner: "verifier", Generation: job.Generation, ExpiresAt: job.Request.Deadline}
			sealed, sealErr := verification.SealJob(job)
			if sealErr != nil {
				return nil, sealErr
			}
			data, marshalErr := json.Marshal(sealed)
			if marshalErr != nil {
				return nil, marshalErr
			}
			rows[0].Body, rows[0].Digest = string(data), sealed.Digest
			return rows, nil
		}
		repo := assurance.New(pending).WithClock(f.clock)
		snapshot, err = repo.SnapshotTaskUsage(f.ctx, request.Usage)
		require.NoError(t, err)
		request.UsageDigest = snapshot.Digest
		_, err = repo.CloseTaskUsage(f.ctx, request)
		require.ErrorIs(t, err, coordination.ErrTaskUsageConflict)
		closed, err := f.repo.GetTaskUsageClosure(f.ctx, request.Usage.Seal.Request.RunID)
		require.NoError(t, err)
		require.Nil(t, closed)
	})
	t.Run("concurrent closure has one fresh receipt", func(t *testing.T) {
		f, request, _ := usageClosureStoreFixture(t, backend, driver)
		type outcome struct {
			receipt coordination.TaskUsageClosure
			err     error
		}
		results := make(chan outcome, 6)
		start := make(chan struct{})
		for range 6 {
			go func() { <-start; got, err := f.repo.CloseTaskUsage(f.ctx, request); results <- outcome{got, err} }()
		}
		close(start)
		fresh := 0
		for range 6 {
			got := <-results
			require.NoError(t, got.err)
			if got.receipt.Fresh {
				fresh++
			}
		}
		require.Equal(t, 1, fresh)
	})
	t.Run("enqueue waits for closure commit", func(t *testing.T) {
		f, request, snapshot := usageClosureStoreFixture(t, backend, driver)
		ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
		defer cancel()
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		entered, release := make(chan struct{}), make(chan struct{})
		var signal, unblock sync.Once
		defer unblock.Do(func() { close(release) })
		paused := &submissionFenceDriver{Driver: driver, plans: plans}
		paused.afterSelect = func(tc context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
			if q.Kind == assurance.KindJob {
				signal.Do(func() { close(entered) })
				select {
				case <-release:
				case <-tc.Done():
					return nil, tc.Err()
				}
			}
			return rows, nil
		}
		done := make(chan error, 1)
		go func() { _, err := assurance.New(paused).WithClock(f.clock).CloseTaskUsage(ctx, request); done <- err }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("closure did not reach verifier history")
		}
		job := snapshot.Jobs[0].Request
		job.ID = id.New(verification.PrefixJob)
		started, admitted := make(chan struct{}), make(chan error, 1)
		go func() { close(started); _, err := f.repo.EnqueueVerificationJob(ctx, job); admitted <- err }()
		<-started
		select {
		case err := <-admitted:
			t.Fatalf("enqueue escaped run guard: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		unblock.Do(func() { close(release) })
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("closure did not finish")
		}
		select {
		case err := <-admitted:
			require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		case <-ctx.Done():
			t.Fatal("enqueue did not finish")
		}
	})
}
