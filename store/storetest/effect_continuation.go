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
)

func effectContinuationFixture(t *testing.T, backend store.Store, driver assurance.Driver, class execution.EffectClass, pending bool) (taskRecoveryFixture, execution.EffectContinuationRequest) {
	t.Helper()
	requirement := storedEffect().Requirement
	requirement.Class = class
	requirement.Retry = execution.RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Second}
	if class == execution.EffectIdempotent {
		requirement.IdempotencyRevision = "provider-v1"
	}
	if class == execution.EffectCompensatable {
		requirement.Compensation = execution.EffectReference{Tool: "unpublish", Revision: "undo-v1"}
	}
	option := taskRecoveryOptions{Effect: &requirement, EffectNotApplied: class != execution.EffectCompensatable, EffectPending: pending}
	option.Acquire = func(ctx context.Context, claim coordination.ClaimRequest, p planning.Plan) (coordination.Claim, error) {
		repo := assurance.New(driver)
		policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, USDMicros: 10000}, Deadline: p.Budget.Deadline, PricingRevision: "prices-1"}
		require.NoError(t, repo.CreateReservationPolicy(ctx, policy))
		reserved := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: claim.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 100, ToolCalls: 1, FanOut: 1, USDMicros: 1000}, Deadline: policy.Deadline, MoneyBound: true, PricingRevision: "prices-1"}
		got, err := repo.AcquireBudgetedTaskClaim(ctx, coordination.BudgetedClaimRequest{Claim: claim, Reservation: reserved})
		return got.Claim, err
	}
	f := newTaskRecoveryFixture(t, backend, driver, "", option)
	f.clock.advance(time.Second)
	claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
	require.NoError(t, err)
	p, err := f.repo.GetPlan(f.ctx, claim.PlanID)
	require.NoError(t, err)
	original, err := f.repo.GetEffect(f.ctx, f.effect.OperationKey)
	require.NoError(t, err)
	predecessor, err := execution.EffectPredecessorDigest(original.Request, original.Receipt)
	require.NoError(t, err)
	op := f.effect
	op.RunCall = nil
	op.AttemptID = id.New(execution.PrefixEffectAttempt)
	op.Deadline = f.clock.Now().Add(time.Minute)
	action, mode, revision := execution.EffectActionRetry, execution.ContinuationAbsence, "probe-v1"
	if class == execution.EffectIdempotent {
		mode = execution.ContinuationIdempotent
		revision = "provider-v1"
	}
	if class == execution.EffectCompensatable {
		action = execution.EffectActionCompensate
		mode = execution.ContinuationApplied
		op.OperationKey = execution.CompensationOperationKey(f.effect)
		op.Requirement = execution.EffectRequirement{Tool: "unpublish", Revision: "undo-v1", Class: execution.EffectNonIdempotent, Retry: execution.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Second}, ReconcileRevision: "probe-undo"}
	}
	r := execution.EffectContinuationRequest{SchemaVersion: 1, ResultID: id.New(evidence.PrefixArtifact), ResultRetention: 24 * time.Hour, Original: f.effect, Operation: op, Sequence: 1, Action: action, ClaimDigest: claim.Digest, Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest, PredecessorDigest: predecessor, Proof: execution.EffectContinuationProof{Mode: mode, Issuer: "fixture-provider", Revision: revision, Reference: "quiescence-and-provider-proof", Digest: evidence.Digest([]byte("proof")), ObservedAt: f.clock.Now(), ExpiresAt: f.clock.Now().Add(time.Minute)}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: claim.TaskID, OwnerRef: op.AttemptID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{ToolCalls: 1, FanOut: 1, USDMicros: 100}, Deadline: op.Deadline, MoneyBound: true, PricingRevision: "prices-1"}}
	_, _, err = r.Freeze()
	require.NoError(t, err)
	return f, r
}

type continuationAuthority struct {
	authorize func(execution.EffectAction) error
}

func (a continuationAuthority) WithEffectContinuationAuthority(_ context.Context, r execution.EffectContinuationRequest, next func(execution.EffectContinuationRequest) error) error {
	return next(r)
}
func (a continuationAuthority) AuthorizeEffectContinuation(_ context.Context, _ execution.EffectContinuationRequest, action execution.EffectAction) error {
	if a.authorize != nil {
		return a.authorize(action)
	}
	return nil
}

type continuationAdapter struct {
	calls, probes int
	unknown       bool
	notApplied    bool
}

func (a *continuationAdapter) Execute(_ context.Context, r execution.EffectRequest, _ []byte) (execution.EffectReceipt, error) {
	a.calls++
	if a.unknown {
		return execution.EffectReceipt{}, execution.ErrEffectUncertain
	}
	return continuationReceipt(r, a.notApplied), nil
}
func (a *continuationAdapter) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	a.probes++
	if a.unknown {
		return execution.EffectStatus{Outcome: execution.EffectUnknown}, nil
	}
	receipt := continuationReceipt(r, a.notApplied)
	return execution.EffectStatus{Outcome: receipt.Outcome, Receipt: &receipt}, nil
}
func continuationReceipt(r execution.EffectRequest, absent bool) execution.EffectReceipt {
	_, digest, err := r.Freeze()
	if err != nil {
		panic(err)
	}
	receipt := execution.EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.EffectApplied, ExternalID: "object", ExternalVersion: "v2", EvidenceRef: "provider-confirmation"}
	if absent {
		receipt.Outcome = execution.EffectNotApplied
		receipt.ExternalID = ""
		receipt.ExternalVersion = ""
	}
	return receipt
}
func continuationExecutor(t *testing.T, s execution.EffectStore, r execution.EffectContinuationRequest, a *continuationAdapter) *execution.EffectExecutor {
	t.Helper()
	executor, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: s, Adapters: []execution.EffectRegistration{{Requirement: r.Operation.Requirement, Adapter: a}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 2})
	require.NoError(t, err)
	return executor
}

// EffectContinuationExecution verifies durable attempts against each backend.
// Provider fixtures do not qualify installed authority or idempotency guarantees.
func EffectContinuationExecution(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("managed", func(t *testing.T) { testManagedEffectContinuation(t, backend, driver) })
	t.Run("retirement", func(t *testing.T) { testContinuationRetirement(t, backend, driver) })
	t.Run("completion faults", func(t *testing.T) { testContinuationCompletionFaults(t, backend, driver) })
	for _, class := range []execution.EffectClass{execution.EffectNonIdempotent, execution.EffectCompensatable} {
		t.Run(string(class), func(t *testing.T) {
			f, r := effectContinuationFixture(t, backend, driver, class, false)
			before, err := f.repo.GetEffect(f.ctx, r.Original.OperationKey)
			require.NoError(t, err)
			adapter := &continuationAdapter{}
			executor := continuationExecutor(t, f.repo, r, adapter)
			result, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
			require.NoError(t, err)
			require.Equal(t, 1, adapter.calls)
			artifact, err := f.repo.LoadEvidence(f.ctx, result.Evidence)
			require.NoError(t, err)
			require.Contains(t, string(artifact.Data), string(r.Action))
			history, err := f.repo.ListEffectContinuations(f.ctx, r.Original)
			require.NoError(t, err)
			require.Len(t, history, 1)
			require.NoError(t, result.Validate(history[0]))
			account, err := f.repo.GetBudgetSnapshot(f.ctx, r.Reservation.PlanID)
			require.NoError(t, err)
			require.EqualValues(t, 1100, account.Charged.USDMicros)
			require.EqualValues(t, 2, account.Charged.ToolCalls)
			after, err := f.repo.GetEffect(f.ctx, r.Original.OperationKey)
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = f.repo.AdoptTaskResult(f.ctx, f.request)
			require.ErrorIs(t, err, execution.ErrEffectConflict, "old candidate cannot certify follow-up")
			restart := continuationExecutor(t, assurance.New(driver).WithClock(f.clock), r, adapter)
			got, err := restart.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
			require.NoError(t, err)
			require.Equal(t, result, got)
			require.Equal(t, 1, adapter.calls)
			_, err = restart.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{authorize: func(execution.EffectAction) error { return execution.ErrEffectUnavailable }})
			require.Error(t, err)
			require.Equal(t, 1, adapter.calls)
			_, err = f.repo.AdmitEffectContinuation(context.Background(), r)
			require.ErrorIs(t, err, cortex.ErrNoScope)
			_, err = f.repo.AdmitEffectContinuation(ctxWithScope("other"), r)
			require.Error(t, err)
			_, err = executor.Execute(f.ctx, r.Operation, []byte("payload"))
			require.Error(t, err, "ordinary execution cannot reopen the sealed run")
		})
	}
	t.Run("unknown nonidempotent blocks", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, true)
		_, err := f.repo.AdmitEffectContinuation(f.ctx, r)
		require.ErrorIs(t, err, execution.ErrEffectConflict)
		account, err := f.repo.GetBudgetSnapshot(f.ctx, r.Reservation.PlanID)
		require.NoError(t, err)
		require.Equal(t, 1, account.Reservations)
	})
	t.Run("finite idempotent unknown attempts stay charged", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectIdempotent, true)
		adapter := &continuationAdapter{unknown: true}
		executor := continuationExecutor(t, f.repo, r, adapter)
		for i := 1; i <= 2; i++ {
			_, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
			require.ErrorIs(t, err, execution.ErrEffectUncertain)
			r = nextContinuation(t, f, r, nil)
		}
		require.Equal(t, 2, adapter.calls)
		_, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		require.Equal(t, 2, adapter.calls)
		account, err := f.repo.GetBudgetSnapshot(f.ctx, r.Reservation.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 1200, account.Charged.USDMicros)
		require.EqualValues(t, 3, account.Reservations)
	})
	t.Run("revoked after reservation", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
		adapter := &continuationAdapter{}
		executor := continuationExecutor(t, f.repo, r, adapter)
		grants := 0
		_, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{authorize: func(action execution.EffectAction) error {
			if action == execution.EffectActionRetry {
				grants++
				if grants == 2 {
					return execution.ErrEffectUnavailable
				}
			}
			return nil
		}})
		require.ErrorIs(t, err, execution.ErrEffectUncertain)
		require.Zero(t, adapter.calls)
		history, err := f.repo.ListEffectContinuations(f.ctx, r.Original)
		require.NoError(t, err)
		require.Len(t, history, 1)
	})
	t.Run("concurrent sequence only one admission", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
		var wg sync.WaitGroup
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() { got, err := f.repo.AdmitEffectContinuation(f.ctx, r); results <- got.Fresh; errs <- err })
		}
		wg.Wait()
		close(results)
		close(errs)
		fresh := 0
		for got := range results {
			if got {
				fresh++
			}
		}
		require.Equal(t, 1, fresh)
		for err := range errs {
			require.NoError(t, err)
		}
	})
	for _, mode := range []string{"snapshot", "claim", "future proof", "expired proof", "no allowance", "rollback", "lost acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
			repo := f.repo
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			fault := &submissionFenceDriver{Driver: driver, plans: plans}
			switch mode {
			case "snapshot":
				r.SnapshotDigest = evidence.Digest(nil)
			case "claim":
				r.ClaimDigest = evidence.Digest(nil)
			case "future proof":
				r.Proof.ObservedAt = f.clock.Now().Add(time.Second)
			case "expired proof":
				f.clock.advance(time.Minute)
			case "no allowance":
				r.Reservation.Allocation.USDMicros = 100000
			case "rollback":
				fault.afterInsert = func(_ context.Context, row assurance.Record) error {
					if row.Kind == "effect_continuation" {
						return errAssignmentInjected
					}
					return nil
				}
				repo = assurance.New(fault).WithClock(f.clock)
			case "lost acknowledgment":
				fault.lostAck = true
				repo = assurance.New(fault).WithClock(f.clock)
			}
			adapter := &continuationAdapter{}
			executor := continuationExecutor(t, repo, r, adapter)
			_, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
			require.Error(t, err)
			require.Zero(t, adapter.calls)
			history, err := f.repo.ListEffectContinuations(f.ctx, r.Original)
			require.NoError(t, err)
			account, err := f.repo.GetBudgetSnapshot(f.ctx, r.Reservation.PlanID)
			require.NoError(t, err)
			if mode == "lost acknowledgment" {
				require.Len(t, history, 1)
				require.Equal(t, 2, account.Reservations)
				recovery := continuationExecutor(t, f.repo, r, adapter)
				_, err = recovery.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
				require.NoError(t, err)
				require.Zero(t, adapter.calls)
				require.Equal(t, 1, adapter.probes)
			} else {
				require.Empty(t, history)
				require.Equal(t, 1, account.Reservations)
			}
		})
	}
}
func nextContinuation(t *testing.T, f taskRecoveryFixture, r execution.EffectContinuationRequest, receipt *execution.EffectReceipt) execution.EffectContinuationRequest {
	t.Helper()
	predecessor, err := execution.EffectPredecessorDigest(r.Operation, receipt)
	require.NoError(t, err)
	f.clock.advance(time.Second)
	r.Sequence++
	r.PredecessorDigest = predecessor
	r.Operation.AttemptID = id.New(execution.PrefixEffectAttempt)
	r.Reservation.ID = id.New(execution.PrefixReservation)
	r.Reservation.OwnerRef = r.Operation.AttemptID.String()
	r.ResultID = id.New(evidence.PrefixArtifact)
	r.Proof.ObservedAt = f.clock.Now()
	return r
}
