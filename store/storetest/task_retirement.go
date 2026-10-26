package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func retirementStoreFixture(t *testing.T, backend store.Store, driver assurance.Driver, rejected, known bool) (taskRecoveryFixture, coordination.TaskRetirementRequest) {
	t.Helper()
	var f taskRecoveryFixture
	var closure coordination.TaskUsageClosureRequest
	if rejected {
		f, closure = rejectedUsageFixture(t, backend, driver, known)
	} else {
		f, closure = terminalUsageFixture(t, backend, driver, run.StateFailed, known)
	}
	closed, err := f.repo.CloseTaskUsage(f.ctx, closure)
	require.NoError(t, err)
	settled, err := f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, closure, closed))
	require.NoError(t, err)
	proof := *f.request.Recovery
	proof.ObservedAt = f.clock.Now()
	return f, coordination.TaskRetirementRequest{SchemaVersion: 1, Claim: closure.Usage.Claim, Recovery: proof, ClosureDigest: closed.Digest, SettlementDigest: settled.Digest, Actor: "host", Reason: "retire unsuccessful reconciled attempt"}
}

// TaskRetirement verifies durable retirement, replacement and historical retries.
// Fixture attestations exercise storage contracts, not installed host authority.
func TaskRetirement(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("unresolved journal", func(t *testing.T) { testRetirementUnresolvedJournal(t, backend, driver) })
	for _, rejected := range []bool{false, true} {
		name := "failed"
		if rejected {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			f, request := retirementStoreFixture(t, backend, driver, rejected, false)
			before, err := backend.GetRun(f.ctx, request.Recovery.Seal.Request.RunID)
			require.NoError(t, err)
			original, err := f.repo.GetTaskClaim(f.ctx, request.Claim.PlanID, request.Claim.TaskID)
			require.NoError(t, err)
			p, err := f.repo.GetPlan(f.ctx, request.Claim.PlanID)
			require.NoError(t, err)
			replacement := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: original.TaskID, Worker: original.Worker, GrantRef: original.GrantRef, ExpectedRevision: original.Revision, Duration: time.Minute}
			_, err = f.repo.AcquireTaskClaim(f.ctx, replacement)
			require.ErrorIs(t, err, coordination.ErrClaimConflict, "settlement alone never releases ownership")
			got, err := f.repo.RetireTaskClaim(f.ctx, request)
			require.NoError(t, err)
			require.True(t, got.Fresh)
			require.NoError(t, got.Validate())
			require.Equal(t, original.RunID, got.Claim.RunID)
			require.Equal(t, original.ExpiresAt, got.Claim.ExpiresAt)
			after, err := backend.GetRun(f.ctx, before.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			replacement.ExpectedRevision = got.Claim.Revision
			next, err := f.repo.AcquireTaskClaim(f.ctx, replacement)
			require.NoError(t, err)
			require.Equal(t, original.Fence+1, next.Fence)
			require.Equal(t, 2, next.SchemaVersion)
			require.NoError(t, coordination.ValidateClaimSuccessor(got.Claim, next))
			require.True(t, next.RunID.IsNil())
			_, err = f.repo.BeginTaskDispatch(f.ctx, original.Token(), id.NewAgentRunID())
			require.Error(t, err)
			account, err := f.repo.GetBudgetSnapshot(f.ctx, p.ID)
			require.NoError(t, err)
			require.EqualValues(t, 100, account.Charged.Tokens, "unknown billing remains charged after retirement")
			require.EqualValues(t, 1000, account.Charged.USDMicros)
			f.clock.advance(time.Hour)
			replayed, err := assurance.New(driver).WithClock(f.clock).RetireTaskClaim(f.ctx, request)
			require.NoError(t, err)
			got.Fresh = false
			require.Equal(t, got, replayed)
			current, err := f.repo.GetTaskClaim(f.ctx, p.ID, original.TaskID)
			require.NoError(t, err)
			require.Equal(t, next, current, "old receipt must not replace current ownership")
			history, err := f.repo.TaskClaimHistory(f.ctx, coordination.ClaimHistoryRequest{PlanID: p.ID, TaskID: original.TaskID, Limit: 256})
			require.NoError(t, err)
			require.Equal(t, next, history[len(history)-1])
			request.Reason = "different request"
			_, err = f.repo.RetireTaskClaim(f.ctx, request)
			require.ErrorIs(t, err, coordination.ErrClaimConflict)
			_, err = f.repo.GetTaskRetirement(context.Background(), original.ID)
			require.ErrorIs(t, err, cortex.ErrNoScope)
			missing, err := f.repo.GetTaskRetirement(ctxWithScope("other"), original.ID)
			require.NoError(t, err)
			require.Nil(t, missing)
		})
	}
	for _, mode := range []string{"closure", "settlement", "claim", "expired proof", "late return", "changed terminal run", "rollback", "rollback after receipt", "expired at commit", "lost acknowledgment", "cancel after commit"} {
		t.Run(mode, func(t *testing.T) {
			f, request := retirementStoreFixture(t, backend, driver, false, true)
			repo := f.repo
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			fault := &submissionFenceDriver{Driver: driver, plans: plans}
			switch mode {
			case "closure":
				request.ClosureDigest = evidence.Digest(nil)
			case "settlement":
				request.SettlementDigest = evidence.Digest(nil)
			case "claim":
				request.Claim.Revision++
			case "expired proof":
				f.clock.advance(time.Minute)
			case "late return":
				_, digest, err := f.model.Freeze()
				require.NoError(t, err)
				require.NoError(t, f.repo.CompleteRunCall(f.ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("late disagreement"))}))
			case "changed terminal run":
				current, err := backend.GetRun(f.ctx, f.model.RunID)
				require.NoError(t, err)
				current.Output = "changed after settlement"
				require.NoError(t, backend.UpdateRun(f.ctx, current))
			case "rollback":
				fault.failInsert = true
			case "rollback after receipt":
				fault.afterInsert = func(_ context.Context, row assurance.Record) error {
					if row.Kind == "task_retirement" {
						return errAssignmentInjected
					}
					return nil
				}
			case "expired at commit":
				fault.afterInsert = func(_ context.Context, row assurance.Record) error {
					if row.Kind == "task_retirement" {
						f.clock.advance(time.Minute)
					}
					return nil
				}
			case "lost acknowledgment":
				fault.lostAck = true
			case "cancel after commit":
				fault.afterCommit = cancel
			}
			if mode == "rollback" || mode == "rollback after receipt" || mode == "expired at commit" || mode == "lost acknowledgment" || mode == "cancel after commit" {
				repo = assurance.New(fault).WithClock(f.clock)
			}
			got, err := repo.RetireTaskClaim(ctx, request)
			require.Error(t, err)
			require.Empty(t, got)
			retained, err := f.repo.GetTaskRetirement(f.ctx, request.Claim.ClaimID)
			require.NoError(t, err)
			current, err := f.repo.GetTaskClaim(f.ctx, request.Claim.PlanID, request.Claim.TaskID)
			require.NoError(t, err)
			if mode == "lost acknowledgment" || mode == "cancel after commit" {
				require.NotNil(t, retained)
				require.Equal(t, coordination.ClaimRetired, current.State)
				retried, retryErr := f.repo.RetireTaskClaim(f.ctx, request)
				require.NoError(t, retryErr)
				require.False(t, retried.Fresh)
				require.Equal(t, *retained, retried)
			} else {
				require.Nil(t, retained)
				require.Equal(t, coordination.ClaimRecovering, current.State)
			}
		})
	}
	t.Run("concurrent retirement commits once", func(t *testing.T) {
		f, request := retirementStoreFixture(t, backend, driver, false, true)
		start := make(chan struct{})
		results := make(chan coordination.TaskRetirement, 2)
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				<-start
				result, err := f.repo.RetireTaskClaim(f.ctx, request)
				results <- result
				failures <- err
			})
		}
		close(start)
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			require.NoError(t, err)
		}
		fresh := 0
		for result := range results {
			if result.Fresh {
				fresh++
			}
		}
		require.Equal(t, 1, fresh)
	})
}

func testRetirementUnresolvedJournal(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, mode := range []string{"unknown effect", "missing provider", "missing settlement", "missing closure"} {
		t.Run(mode, func(t *testing.T) {
			f, closure := terminalUsageFixture(t, backend, driver, run.StateFailed, false)
			journal, err := f.repo.SnapshotRunRecovery(f.ctx, closure.Usage.Seal)
			require.NoError(t, err)
			hiddenKind := ""
			switch mode {
			case "unknown effect":
				hiddenKind = "effect_receipt"
				for i := range journal.Effects {
					journal.Effects[i].Receipt = nil
				}
			case "missing provider":
				hiddenKind = "provider_call_receipt"
				for i := range journal.Calls {
					journal.Calls[i].Provider = nil
				}
			}
			journal, err = execution.SealRunRecoverySnapshot(journal)
			require.NoError(t, err)
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			fault := &submissionFenceDriver{Driver: driver, plans: plans, afterSelect: func(_ context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
				if hiddenKind != "" && q.Kind == hiddenKind {
					return nil, nil
				}
				return rows, nil
			}}
			f.repo = assurance.New(fault).WithClock(f.clock)
			closure.Usage.SnapshotDigest = journal.Digest
			snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, closure.Usage)
			require.NoError(t, err)
			closure.UsageDigest = snapshot.Digest
			closed, err := f.repo.CloseTaskUsage(f.ctx, closure)
			require.NoError(t, err)
			settled, err := f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, closure, closed))
			require.NoError(t, err, "conservative billing may settle before external outcomes are resolved")
			proof := *f.request.Recovery
			proof.SnapshotDigest = journal.Digest
			proof.ObservedAt = f.clock.Now()
			request := coordination.TaskRetirementRequest{SchemaVersion: 1, Claim: closure.Usage.Claim, Recovery: proof, ClosureDigest: closed.Digest, SettlementDigest: settled.Digest, Actor: "host", Reason: "retire reconciled work"}
			if mode == "missing settlement" {
				hiddenKind = "task_usage_settlement"
			}
			if mode == "missing closure" {
				hiddenKind = "task_usage_closure"
			}
			retired, err := f.repo.RetireTaskClaim(f.ctx, request)
			require.Error(t, err)
			require.Empty(t, retired)
			current, err := f.repo.GetTaskClaim(f.ctx, request.Claim.PlanID, request.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, coordination.ClaimRecovering, current.State)
		})
	}
}
