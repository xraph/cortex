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
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func usageSettlementStoreFixture(t *testing.T, backend store.Store, driver assurance.Driver, metered bool) (taskRecoveryFixture, coordination.TaskUsageSettlementRequest) {
	t.Helper()
	option := taskRecoveryOptions{ProviderUsage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 11, USDMicros: 5}, Known: execution.KnownTokens | execution.KnownMoney}}
	if metered {
		option.VerifierBudget = &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, MaxUSDMicros: 100, PricingRevision: "prices-1"}
	}
	option.Acquire = func(ctx context.Context, claim coordination.ClaimRequest, p planning.Plan) (coordination.Claim, error) {
		repo := assurance.New(driver)
		policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, USDMicros: 10000}, Deadline: p.Budget.Deadline, PricingRevision: "prices-1"}
		require.NoError(t, repo.CreateReservationPolicy(ctx, policy))
		reserved := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: claim.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 100, ToolCalls: 1, FanOut: 1, USDMicros: 1000}, Deadline: policy.Deadline, PricingRevision: policy.PricingRevision, MoneyBound: true}
		got, err := repo.AcquireBudgetedTaskClaim(ctx, coordination.BudgetedClaimRequest{Claim: claim, Reservation: reserved})
		return got.Claim, err
	}
	f := newTaskRecoveryFixture(t, backend, driver, "", option)
	accepted, err := f.repo.AdoptTaskResult(f.ctx, f.request)
	require.NoError(t, err)
	usage := coordination.TaskUsageRequest{Claim: accepted.Claim.Token(), Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	closed, err := f.repo.CloseTaskUsage(f.ctx, coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: "host", Reason: "final accounting"})
	require.NoError(t, err)
	allocation, err := f.repo.GetBudgetedTaskClaim(f.ctx, accepted.Claim.ID)
	require.NoError(t, err)
	r := coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, ClosureDigest: closed.Digest, ReservationID: allocation.Reservation.Request.ID, ReservationDigest: allocation.Reservation.RequestDigest, Actor: "host", Reason: "settle accepted task", Billing: coordination.TaskBillingProof{Issuer: "fixture", Revision: "1", ProofRef: "final-billing", ProofDigest: evidence.Digest([]byte("bill")), PricingRevision: "prices-1", ObservedAt: f.clock.Now(), ExpiresAt: f.clock.Now().Add(time.Minute)}}
	return f, r
}

func testTaskUsageSettlement(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, metered := range []bool{false, true} {
		name := "unknown usage"
		if metered {
			name = "known usage"
		}
		t.Run(name, func(t *testing.T) {
			f, r := usageSettlementStoreFixture(t, backend, driver, metered)
			got, err := f.repo.SettleTaskUsage(f.ctx, r)
			require.NoError(t, err)
			require.True(t, got.Fresh)
			require.NoError(t, got.Validate())
			account, err := f.repo.GetBudgetSnapshot(f.ctx, r.Usage.Claim.PlanID)
			require.NoError(t, err)
			if metered {
				require.EqualValues(t, 18, account.Charged.Tokens)
				require.EqualValues(t, 8, account.Charged.USDMicros)
			} else {
				require.EqualValues(t, 100, account.Charged.Tokens)
				require.EqualValues(t, 1000, account.Charged.USDMicros)
				require.EqualValues(t, 11, got.Settlement.Request.Minimum.Tokens)
			}
			got.Fresh = false
			stored, err := assurance.New(driver).GetTaskUsageSettlement(f.ctx, r.Usage.Seal.Request.RunID)
			require.NoError(t, err)
			require.Equal(t, &got, stored)
			_, err = f.repo.GetTaskUsageSettlement(context.Background(), r.Usage.Seal.Request.RunID)
			require.ErrorIs(t, err, cortex.ErrNoScope)
			missing, err := f.repo.GetTaskUsageSettlement(ctxWithScope("other"), r.Usage.Seal.Request.RunID)
			require.NoError(t, err)
			require.Nil(t, missing)
			f.clock.advance(time.Hour)
			retry, err := f.repo.SettleTaskUsage(f.ctx, r)
			require.NoError(t, err)
			require.Equal(t, got, retry)
			r.Reason = "changed"
			_, err = f.repo.SettleTaskUsage(f.ctx, r)
			require.ErrorIs(t, err, coordination.ErrTaskSettlementConflict)
		})
	}
	for _, mode := range []string{"usage", "closure", "claim", "expired", "cancelled", "scope", "independent reservation", "missing closure", "missing budget binding", "manual settlement", "late evidence", "rollback", "rollback after accounting", "proof expires at commit", "lost ack", "cancel after commit"} {
		t.Run(mode, func(t *testing.T) {
			f, r := usageSettlementStoreFixture(t, backend, driver, true)
			ctx := f.ctx
			repo := f.repo
			switch mode {
			case "usage":
				r.UsageDigest = evidence.Digest([]byte("changed"))
			case "closure":
				r.ClosureDigest = evidence.Digest([]byte("changed"))
			case "claim":
				r.Usage.Claim.Revision--
			case "expired":
				f.clock.advance(time.Hour)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "scope":
				ctx = ctxWithScope("other")
			case "independent reservation":
				original, readErr := repo.GetReservation(ctx, r.ReservationID)
				require.NoError(t, readErr)
				original.Request.ID = id.New(execution.PrefixReservation)
				independent, reserveErr := repo.Reserve(ctx, original.Request)
				require.NoError(t, reserveErr)
				r.ReservationID, r.ReservationDigest = independent.Request.ID, independent.RequestDigest
			case "manual settlement":
				require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: r.ReservationID, RequestDigest: r.ReservationDigest}))
			case "late evidence":
				proof := snapshotProof(t, f.model)
				require.NoError(t, repo.CompleteRunCall(ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest}))
			case "missing closure", "missing budget binding", "rollback", "rollback after accounting", "proof expires at commit", "lost ack", "cancel after commit":
				plans, ok := driver.(assurance.PlanDriver)
				require.True(t, ok)
				fault := &submissionFenceDriver{Driver: driver, plans: plans, failInsert: mode == "rollback", lostAck: mode == "lost ack"}
				if mode == "missing closure" || mode == "missing budget binding" {
					missingKind := "task_usage_closure"
					if mode == "missing budget binding" {
						missingKind = "task_claim_budget"
					}
					fault.afterSelect = func(_ context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
						if q.Kind == missingKind {
							return nil, nil
						}
						return rows, nil
					}
				}
				if mode == "rollback after accounting" || mode == "proof expires at commit" {
					fault.afterInsert = func(_ context.Context, row assurance.Record) error {
						if row.Kind != "task_usage_settlement" {
							return nil
						}
						if mode == "rollback after accounting" {
							return errAssignmentInjected
						}
						f.clock.advance(time.Hour)
						return nil
					}
				}
				if mode == "cancel after commit" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					fault.afterCommit = cancel
				}
				repo = assurance.New(fault).WithClock(f.clock)
			}
			got, err := repo.SettleTaskUsage(ctx, r)
			require.Error(t, err)
			require.Zero(t, got)
			retained, err := f.repo.GetTaskUsageSettlement(f.ctx, r.Usage.Seal.Request.RunID)
			require.NoError(t, err)
			if mode == "lost ack" || mode == "cancel after commit" {
				require.NotNil(t, retained)
				retry, err := f.repo.SettleTaskUsage(f.ctx, r)
				require.NoError(t, err)
				require.False(t, retry.Fresh)
			} else {
				require.Nil(t, retained)
				reservation, err := f.repo.GetReservation(f.ctx, r.ReservationID)
				require.NoError(t, err)
				if mode != "manual settlement" {
					require.Nil(t, reservation.Settlement)
				}
			}
		})
	}
	t.Run("concurrent settlement", func(t *testing.T) {
		f, r := usageSettlementStoreFixture(t, backend, driver, true)
		results := make(chan coordination.TaskUsageSettlement, 4)
		failures := make(chan error, 4)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { got, err := f.repo.SettleTaskUsage(f.ctx, r); results <- got; failures <- err })
		}
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
	t.Run("late journal evidence waits for settlement", func(t *testing.T) {
		f, r := usageSettlementStoreFixture(t, backend, driver, true)
		bounded, cancel := context.WithTimeout(f.ctx, 15*time.Second)
		defer cancel()
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		fault := &submissionFenceDriver{Driver: driver, plans: plans}
		var enteredOnce sync.Once
		fault.afterSelect = func(ctx context.Context, query assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
			if query.Kind == assurance.KindJob {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return rows, nil
		}
		settled := make(chan error, 1)
		go func() { _, err := assurance.New(fault).WithClock(f.clock).SettleTaskUsage(bounded, r); settled <- err }()
		select {
		case <-entered:
		case err := <-settled:
			t.Fatalf("settlement ended before journal guard: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("settlement did not reach journal guard")
		}
		late := make(chan error, 1)
		started := make(chan struct{})
		proof := snapshotProof(t, f.model)
		go func() {
			close(started)
			late <- f.repo.CompleteRunCall(bounded, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest})
		}()
		<-started
		select {
		case err := <-late:
			t.Fatalf("late evidence escaped journal guard: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		once.Do(func() { close(release) })
		for _, finished := range []<-chan error{settled, late} {
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-bounded.Done():
				t.Fatal("transaction did not finish")
			}
		}
		journal, err := f.repo.SnapshotRunRecovery(f.ctx, r.Usage.Seal)
		require.NoError(t, err)
		require.NotEqual(t, r.Usage.SnapshotDigest, journal.Digest)
		retry, err := f.repo.SettleTaskUsage(f.ctx, r)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	t.Run("fresh proof can include observations after closure", func(t *testing.T) {
		f, r := usageSettlementStoreFixture(t, backend, driver, true)
		proof := snapshotProof(t, f.model)
		require.NoError(t, f.repo.CompleteRunCall(f.ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest}))
		journal, err := f.repo.SnapshotRunRecovery(f.ctx, r.Usage.Seal)
		require.NoError(t, err)
		r.Usage.SnapshotDigest = journal.Digest
		snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, r.Usage)
		require.NoError(t, err)
		r.UsageDigest = snapshot.Digest
		r.Billing.ProofDigest = evidence.Digest([]byte("refreshed billing"))
		got, err := f.repo.SettleTaskUsage(f.ctx, r)
		require.NoError(t, err)
		require.True(t, got.Fresh)
		require.EqualValues(t, 18, got.Settlement.Request.Usage.Actual.Tokens)
	})
	t.Run("retry requires matching accounting record", func(t *testing.T) {
		f, r := usageSettlementStoreFixture(t, backend, driver, true)
		_, err := f.repo.SettleTaskUsage(f.ctx, r)
		require.NoError(t, err)
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		fault := &submissionFenceDriver{Driver: driver, plans: plans, afterSelect: func(_ context.Context, query assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
			if query.Kind == "budget_settlement" {
				return nil, nil
			}
			return rows, nil
		}}
		_, err = assurance.New(fault).WithClock(f.clock).SettleTaskUsage(f.ctx, r)
		require.ErrorIs(t, err, coordination.ErrTaskSettlementConflict)
	})
}
