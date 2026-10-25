package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func terminalUsageFixture(t *testing.T, backend store.Store, driver assurance.Driver, state run.State, metered ...bool) (taskRecoveryFixture, coordination.TaskUsageClosureRequest) {
	t.Helper()
	known := len(metered) == 0 || metered[0]
	f := budgetedTaskRecoveryFixture(t, backend, driver, known)
	current, err := backend.GetRun(f.ctx, f.request.Result.RunID)
	require.NoError(t, err)
	current.State, current.StopReason = state, run.StopExecutionError
	if state == run.StateCancelled {
		current.StopReason = run.StopCancelled
	}
	if state == run.StateIncomplete {
		current.StopReason = run.StopStepLimit
	}
	current.ResultRef, current.VerificationStatus = "", verification.StatusFailed
	ended := f.clock.Now()
	current.CompletedAt = &ended
	require.NoError(t, backend.UpdateRun(f.ctx, current))
	current, err = backend.GetRun(f.ctx, current.ID)
	require.NoError(t, err)
	usage := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	// Decode the public request to exercise its optional terminal-run binding.
	data, err := json.Marshal(coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: "host", Reason: "settle failed work", Termination: &coordination.TaskUsageTermination{RunRevision: current.Revision, State: state, CompletedAt: *current.CompletedAt}})
	require.NoError(t, err)
	var request coordination.TaskUsageClosureRequest
	require.NoError(t, json.Unmarshal(data, &request))
	return f, request
}

func terminalSettlementRequest(t *testing.T, f taskRecoveryFixture, request coordination.TaskUsageClosureRequest, closed coordination.TaskUsageClosure) coordination.TaskUsageSettlementRequest {
	t.Helper()
	allocation, err := f.repo.GetBudgetedTaskClaim(f.ctx, request.Usage.Claim.ClaimID)
	require.NoError(t, err)
	return coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: request.Usage, UsageDigest: request.UsageDigest, ClosureDigest: closed.Digest, ReservationID: allocation.Reservation.Request.ID, ReservationDigest: allocation.Reservation.RequestDigest, Actor: "host", Reason: "account for failed work", Billing: coordination.TaskBillingProof{Issuer: "fixture", Revision: "1", ProofRef: "final bill", ProofDigest: evidence.Digest([]byte("failed-work-bill")), PricingRevision: "prices-1", ObservedAt: f.clock.Now(), ExpiresAt: f.clock.Now().Add(time.Minute)}}
}

func testTaskTerminalUsage(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, state := range []run.State{run.StateFailed, run.StateCancelled, run.StateIncomplete} {
		t.Run(string(state), func(t *testing.T) {
			f, request := terminalUsageFixture(t, backend, driver, state)
			before, err := f.repo.GetTaskClaim(f.ctx, request.Usage.Claim.PlanID, request.Usage.Claim.TaskID)
			require.NoError(t, err)
			closed, err := f.repo.CloseTaskUsage(f.ctx, request)
			require.NoError(t, err)
			settlement := terminalSettlementRequest(t, f, request, closed)
			paid, err := f.repo.SettleTaskUsage(f.ctx, settlement)
			require.NoError(t, err)
			require.True(t, paid.Fresh)
			account, err := f.repo.GetBudgetSnapshot(f.ctx, before.PlanID)
			require.NoError(t, err)
			require.EqualValues(t, 18, account.Charged.Tokens)
			require.EqualValues(t, 8, account.Charged.USDMicros)
			after, err := f.repo.GetTaskClaim(f.ctx, before.PlanID, before.TaskID)
			require.NoError(t, err)
			require.Equal(t, before, after, "accounting must not release or accept ownership")
			require.Equal(t, coordination.ClaimRecovering, after.State)
			require.Nil(t, after.AcceptanceID)
			f.clock.advance(time.Hour)
			replayed, err := assurance.New(driver).WithClock(f.clock).SettleTaskUsage(f.ctx, settlement)
			require.NoError(t, err)
			paid.Fresh = false
			require.Equal(t, paid, replayed)
		})
	}
	t.Run("unknown terminal usage retains allocation", func(t *testing.T) {
		f, request := terminalUsageFixture(t, backend, driver, run.StateFailed, false)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		settlement := terminalSettlementRequest(t, f, request, closed)
		paid, err := f.repo.SettleTaskUsage(f.ctx, settlement)
		require.NoError(t, err)
		account, err := f.repo.GetBudgetSnapshot(f.ctx, request.Usage.Claim.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 100, account.Charged.Tokens)
		require.EqualValues(t, 1000, account.Charged.USDMicros)
		require.EqualValues(t, 11, paid.Settlement.Request.Minimum.Tokens)
	})
	for _, mode := range []string{"binding absent", "stale revision", "changed completion", "running", "paused", "completed", "wrong worker", "wrong claim", "published result", "passed verification", "missing dispatch"} {
		t.Run("reject "+mode, func(t *testing.T) {
			f, request := terminalUsageFixture(t, backend, driver, run.StateFailed)
			current, err := backend.GetRun(f.ctx, request.Usage.Seal.Request.RunID)
			require.NoError(t, err)
			repo := f.repo
			switch mode {
			case "binding absent":
				request.Termination = nil
			case "stale revision":
				request.Termination.RunRevision++
			case "changed completion":
				request.Termination.CompletedAt = request.Termination.CompletedAt.Add(-time.Second)
			case "running":
				current.State = run.StateRunning
			case "paused":
				current.State = run.StatePaused
			case "completed":
				current.State = run.StateCompleted
			case "wrong worker":
				current.AgentID = mustCreateAgent(t, backend, f.ctx, "other-worker")
			case "wrong claim":
				current.Provenance.TaskClaimID = id.New(coordination.PrefixClaim).String()
			case "published result":
				current.ResultRef = "existing-result"
			case "passed verification":
				current.VerificationStatus = verification.StatusPassed
			case "missing dispatch":
				plans, ok := driver.(assurance.PlanDriver)
				require.True(t, ok)
				fault := &submissionFenceDriver{Driver: driver, plans: plans, afterSelect: func(_ context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
					if q.Kind == "task_dispatch" {
						return nil, nil
					}
					return rows, nil
				}}
				repo = assurance.New(fault).WithClock(f.clock)
			}
			if mode != "binding absent" && mode != "stale revision" && mode != "changed completion" && mode != "missing dispatch" {
				require.NoError(t, backend.UpdateRun(f.ctx, current))
				request.Termination.RunRevision = current.Revision
			}
			got, err := repo.CloseTaskUsage(f.ctx, request)
			require.Error(t, err)
			require.Empty(t, got)
			retained, err := f.repo.GetTaskUsageClosure(f.ctx, current.ID)
			require.NoError(t, err)
			require.Nil(t, retained)
		})
	}
	t.Run("fresh settlement rechecks terminal revision", func(t *testing.T) {
		f, request := terminalUsageFixture(t, backend, driver, run.StateFailed)
		closed, err := f.repo.CloseTaskUsage(f.ctx, request)
		require.NoError(t, err)
		settlement := terminalSettlementRequest(t, f, request, closed)
		current, err := backend.GetRun(f.ctx, request.Usage.Seal.Request.RunID)
		require.NoError(t, err)
		current.State = run.StateRunning
		require.NoError(t, backend.UpdateRun(f.ctx, current))
		_, err = f.repo.SettleTaskUsage(f.ctx, settlement)
		require.ErrorIs(t, err, coordination.ErrTaskUsageConflict)
		allocation, err := f.repo.GetReservation(f.ctx, settlement.ReservationID)
		require.NoError(t, err)
		require.Nil(t, allocation.Settlement)
	})
	for _, mode := range []string{"rollback", "lost acknowledgment", "cancel after commit"} {
		t.Run(mode, func(t *testing.T) {
			f, request := terminalUsageFixture(t, backend, driver, run.StateFailed)
			closed, err := f.repo.CloseTaskUsage(f.ctx, request)
			require.NoError(t, err)
			settlement := terminalSettlementRequest(t, f, request, closed)
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			fault := &submissionFenceDriver{Driver: driver, plans: plans, failInsert: mode == "rollback", lostAck: mode == "lost acknowledgment"}
			if mode == "cancel after commit" {
				fault.afterCommit = cancel
			}
			got, err := assurance.New(fault).WithClock(f.clock).SettleTaskUsage(ctx, settlement)
			require.Error(t, err)
			require.Empty(t, got)
			allocation, err := f.repo.GetReservation(f.ctx, settlement.ReservationID)
			require.NoError(t, err)
			if mode == "rollback" {
				require.Nil(t, allocation.Settlement)
			} else {
				require.NotNil(t, allocation.Settlement)
			}
			retry, err := f.repo.SettleTaskUsage(f.ctx, settlement)
			require.NoError(t, err)
			require.Equal(t, mode == "rollback", retry.Fresh)
		})
	}
}
