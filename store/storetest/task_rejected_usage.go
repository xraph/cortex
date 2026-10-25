package storetest

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func rejectedUsageFixture(t *testing.T, backend store.Store, driver assurance.Driver, metered ...bool) (taskRecoveryFixture, coordination.TaskUsageClosureRequest) {
	t.Helper()
	known := len(metered) == 0 || metered[0]
	f := budgetedTaskRecoveryFixture(t, backend, driver, known)
	current, err := backend.GetRun(f.ctx, f.request.Result.RunID)
	require.NoError(t, err)
	require.NotNil(t, current.CompletedAt)
	usage := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{"schema_version": 1, "usage": usage, "usage_digest": snapshot.Digest, "actor": "host", "reason": "reject completed task output", "rejection": map[string]any{"run_revision": current.Revision, "completed_at": current.CompletedAt, "result_ref": current.ResultRef, "verification_status": current.VerificationStatus}})
	require.NoError(t, err)
	var request coordination.TaskUsageClosureRequest
	require.NoError(t, json.Unmarshal(data, &request))
	return f, request
}

func testTaskRejectedUsage(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Run("unknown usage retains allocation", func(t *testing.T) {
		f, request := rejectedUsageFixture(t, backend, driver, false)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		_, err = f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, request, closed))
		require.NoError(t, err)
		account, err := f.repo.GetBudgetSnapshot(f.ctx, request.Usage.Claim.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 100, account.Charged.Tokens)
		require.EqualValues(t, 1000, account.Charged.USDMicros)
	})
	t.Run("boundaries", func(t *testing.T) { testRejectedUsageBoundaries(t, backend, driver) })
	t.Run("rejection closes accounting and blocks acceptance", func(t *testing.T) {
		f, request := rejectedUsageFixture(t, backend, driver)
		before, err := backend.GetRun(f.ctx, f.request.Result.RunID)
		require.NoError(t, err)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		require.True(t, closed.Fresh)
		_, err = f.repo.AdoptTaskResult(f.ctx, f.request)
		require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
		settlement := terminalSettlementRequest(t, f, request, closed)
		paid, err := f.repo.SettleTaskUsage(f.ctx, settlement)
		require.NoError(t, err)
		require.True(t, paid.Fresh)
		account, err := f.repo.GetBudgetSnapshot(f.ctx, request.Usage.Claim.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 18, account.Charged.Tokens)
		after, err := backend.GetRun(f.ctx, before.ID)
		require.NoError(t, err)
		require.Equal(t, before, after)
		claim, err := f.repo.GetTaskClaim(f.ctx, request.Usage.Claim.PlanID, request.Usage.Claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, request.Usage.Claim, claim.Token())
		require.Equal(t, coordination.ClaimRecovering, claim.State)
		f.clock.advance(time.Hour)
		replayed, err := assurance.New(driver).WithClock(f.clock).CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		closed.Fresh = false
		require.Equal(t, closed, replayed)
		retried, err := f.repo.SettleTaskUsage(f.ctx, settlement)
		require.NoError(t, err)
		paid.Fresh = false
		require.Equal(t, paid, retried)
	})
	t.Run("acceptance and rejection are exclusive", func(t *testing.T) {
		f, request := rejectedUsageFixture(t, backend, driver)
		results := make(chan error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, err := f.repo.AdoptTaskResult(f.ctx, f.request); results <- err }()
		go func() { defer wg.Done(); <-start; _, err := f.repo.CloseTaskUsage(f.ctx, request); results <- err }()
		close(start)
		wg.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		require.Equal(t, 1, successes)
	})
}

func testRejectedUsageBoundaries(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, mode := range []string{"stale revision", "wrong result", "wrong verification", "wrong time", "active run", "missing completion", "accepted claim", "both modes"} {
		t.Run(mode, func(t *testing.T) {
			f, request := rejectedUsageFixture(t, backend, driver)
			switch mode {
			case "stale revision":
				request.Rejection.RunRevision++
			case "wrong result":
				request.Rejection.ResultRef = "other"
			case "wrong verification":
				request.Rejection.VerificationStatus = verification.StatusFailed
			case "wrong time":
				request.Rejection.CompletedAt = request.Rejection.CompletedAt.Add(-time.Second)
			case "active run", "missing completion":
				current, err := backend.GetRun(f.ctx, f.request.Result.RunID)
				require.NoError(t, err)
				if mode == "active run" {
					current.State = run.StateRunning
				} else {
					current.CompletedAt = nil
				}
				require.NoError(t, backend.UpdateRun(f.ctx, current))
				request.Rejection.RunRevision = current.Revision
			case "accepted claim":
				accepted, err := f.repo.AdoptTaskResult(f.ctx, f.request)
				require.NoError(t, err)
				request.Usage.Claim = accepted.Claim.Token()
				snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, request.Usage)
				require.NoError(t, err)
				request.UsageDigest = snapshot.Digest
			case "both modes":
				request.Termination = &coordination.TaskUsageTermination{RunRevision: request.Rejection.RunRevision, CompletedAt: request.Rejection.CompletedAt, State: run.StateFailed}
			}
			got, err := f.repo.CloseTaskUsage(f.ctx, request)
			require.Error(t, err)
			require.Empty(t, got)
			retained, err := f.repo.GetTaskUsageClosure(f.ctx, f.request.Result.RunID)
			require.NoError(t, err)
			require.Nil(t, retained)
		})
	}
	t.Run("fresh settlement rechecks rejected run", func(t *testing.T) {
		f, request := rejectedUsageFixture(t, backend, driver)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		current, err := backend.GetRun(f.ctx, f.request.Result.RunID)
		require.NoError(t, err)
		current.Output = "changed"
		require.NoError(t, backend.UpdateRun(f.ctx, current))
		settlement := terminalSettlementRequest(t, f, request, closed)
		_, err = f.repo.SettleTaskUsage(f.ctx, settlement)
		require.ErrorIs(t, err, coordination.ErrTaskUsageConflict)
		allocation, err := f.repo.GetReservation(f.ctx, settlement.ReservationID)
		require.NoError(t, err)
		require.Nil(t, allocation.Settlement)
	})
	for _, mode := range []string{"rollback", "lost acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			f, request := rejectedUsageFixture(t, backend, driver)
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			fault := &submissionFenceDriver{Driver: driver, plans: plans, failInsert: mode == "rollback", lostAck: mode == "lost acknowledgment"}
			_, err := assurance.New(fault).WithClock(f.clock).CloseTaskUsage(f.ctx, request)
			require.Error(t, err)
			closed, err := f.repo.GetTaskUsageClosure(f.ctx, f.request.Result.RunID)
			require.NoError(t, err)
			if mode == "rollback" {
				require.Nil(t, closed)
				_, err = f.repo.AdoptTaskResult(f.ctx, f.request)
				require.NoError(t, err)
			} else {
				require.NotNil(t, closed)
				retried, retryErr := assurance.New(driver).WithClock(f.clock).CloseTaskUsage(f.ctx, request)
				require.NoError(t, retryErr)
				require.Equal(t, *closed, retried)
				_, err = f.repo.AdoptTaskResult(f.ctx, f.request)
				require.ErrorIs(t, err, coordination.ErrTaskUsageClosed)
			}
		})
	}
}
