package coordination_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

type settlementResolver func(context.Context, coordination.TaskUsageSnapshot, coordination.TaskUsageClosure, execution.Reservation) (*coordination.TaskUsageSettlementRequest, error)

func (f settlementResolver) ResolveTaskUsageSettlement(ctx context.Context, s coordination.TaskUsageSnapshot, c coordination.TaskUsageClosure, r execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
	return f(ctx, s, c, r)
}

type settlementServiceStore struct {
	*providerServiceStore
	snapshot                   coordination.TaskUsageSnapshot
	budget                     coordination.BudgetedClaim
	closed                     *coordination.TaskUsageClosure
	settled                    *coordination.TaskUsageSettlement
	noBudget                   bool
	binding                    verification.Binding
	plan                       planning.Plan
	acceptedList               func(context.Context, coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error)
	reads, closes, settlements atomic.Int32
}

func (s *settlementServiceStore) GetBudgetedTaskClaim(context.Context, id.ID) (coordination.BudgetedClaim, error) {
	if s.noBudget {
		return coordination.BudgetedClaim{}, coordination.ErrClaimNotFound
	}
	return s.budget, nil
}
func (s *settlementServiceStore) SnapshotRunRecovery(context.Context, execution.RunCallSeal) (execution.RunRecoverySnapshot, error) {
	return s.snapshot.Journal, nil
}
func (s *settlementServiceStore) SnapshotTaskUsage(context.Context, coordination.TaskUsageRequest) (coordination.TaskUsageSnapshot, error) {
	s.reads.Add(1)
	return s.snapshot, nil
}
func (s *settlementServiceStore) GetTaskUsageClosure(context.Context, id.AgentRunID) (*coordination.TaskUsageClosure, error) {
	return s.closed, nil
}
func (s *settlementServiceStore) GetTaskUsageSettlement(context.Context, id.AgentRunID) (*coordination.TaskUsageSettlement, error) {
	return s.settled, nil
}

func (s *settlementServiceStore) ListAcceptedTaskClaims(ctx context.Context, r coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error) {
	if s.acceptedList != nil {
		return s.acceptedList(ctx, r)
	}
	if s.claim.State != coordination.ClaimCompleted || (!r.After.IsNil() && r.After.String() >= s.claim.AcceptanceID.String()) {
		return nil, nil
	}
	return []coordination.Claim{s.claim}, nil
}

func (s *settlementServiceStore) SealTaskCalls(_ context.Context, r coordination.TaskCallSealRequest) (execution.RunCallSeal, error) {
	request, digest, err := r.Seal.Freeze()
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	sealed := execution.RunCallSeal{Request: request, RequestDigest: digest, At: s.claim.At}
	s.seal = &sealed
	s.snapshot.Journal.Seal = sealed
	s.snapshot.Journal, err = execution.SealRunRecoverySnapshot(s.snapshot.Journal)
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	s.snapshot, err = coordination.SealTaskUsageSnapshot(s.snapshot)
	return sealed, err
}

func TestTaskSettlementServiceSealsOnlyWithExplicitAuthority(t *testing.T) {
	for _, denied := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, s, cfg := settlementServiceFixture(t)
			s.seal = nil
			cfg.CallSealFence = taskCallSealFence(func(_ context.Context, r coordination.TaskCallSealRequest, next func(coordination.TaskCallSealRequest) error) error {
				return next(r)
			})
			cfg.CallSealAuthorizer = taskCallSealAuthorizer(func(context.Context, coordination.TaskCallSealRequest) error {
				if denied {
					return errors.New("seal denied")
				}
				return nil
			})
			service, err := coordination.NewTaskSettlementService(cfg)
			require.NoError(t, err)
			require.NoError(t, service.Start(ctx))
			synctest.Wait()
			require.NoError(t, service.Stop(ctx))
			if denied {
				require.Nil(t, s.seal)
				require.Zero(t, s.settlements.Load())
			} else {
				require.NotNil(t, s.seal)
				require.EqualValues(t, 1, s.settlements.Load())
			}
		})
	}
}
func (s *settlementServiceStore) CloseTaskUsage(_ context.Context, r coordination.TaskUsageClosureRequest) (coordination.TaskUsageClosure, error) {
	s.closes.Add(1)
	r, digest, err := r.Freeze()
	if err != nil {
		return coordination.TaskUsageClosure{}, err
	}
	c, err := coordination.SealTaskUsageClosure(coordination.TaskUsageClosure{SchemaVersion: 1, Request: r, RequestDigest: digest, At: s.snapshot.Claim.At})
	s.closed = &c
	return c, err
}
func (s *settlementServiceStore) SettleTaskUsage(_ context.Context, r coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error) {
	s.settlements.Add(1)
	if s.settled != nil {
		return *s.settled, nil
	}
	accounting, err := coordination.PrepareTaskUsageSettlement(r, s.snapshot, *s.closed, s.budget.Reservation, s.snapshot.Claim.At)
	if err != nil {
		return coordination.TaskUsageSettlement{}, err
	}
	accounting, digest, err := accounting.Freeze()
	if err != nil {
		return coordination.TaskUsageSettlement{}, err
	}
	r, requestDigest, err := r.Freeze()
	if err != nil {
		return coordination.TaskUsageSettlement{}, err
	}
	receipt, err := coordination.SealTaskUsageSettlement(coordination.TaskUsageSettlement{SchemaVersion: 1, Request: r, RequestDigest: requestDigest, Settlement: execution.SettlementReceipt{Request: accounting, Digest: digest, At: s.snapshot.Claim.At}})
	s.settled = &receipt
	return receipt, err
}

func settlementServiceFixture(t *testing.T) (context.Context, *settlementServiceStore, coordination.TaskSettlementServiceConfig) {
	t.Helper()
	_, assignment, binding, plan, now := reviewFixture(t)
	h := assignment.Handoff
	held, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: plan.ID, PlanDigest: plan.Digest, PlanRevision: plan.Revision, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, plan, now)
	require.NoError(t, err)
	dispatched, err := coordination.BeginDispatch(held, held.Token(), binding.RunID, now)
	require.NoError(t, err)
	completed, err := coordination.CompleteClaim(dispatched, dispatched.Token(), id.New(coordination.PrefixTaskAcceptance), now)
	require.NoError(t, err)
	sealRequest, digest, err := (execution.RunCallSealRequest{SchemaVersion: 1, Scope: held.Scope, RunID: binding.RunID, Actor: "host", Reason: "finished"}).Freeze()
	require.NoError(t, err)
	seal := execution.RunCallSeal{Request: sealRequest, RequestDigest: digest, At: now}
	journal, err := execution.SealRunRecoverySnapshot(execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: seal})
	require.NoError(t, err)
	snapshot, err := coordination.SealTaskUsageSnapshot(coordination.TaskUsageSnapshot{SchemaVersion: 1, Claim: completed, Journal: journal})
	require.NoError(t, err)
	allocation, allocationDigest, err := (execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), Scope: held.Scope, PlanID: plan.ID, PlanDigest: plan.Digest, TaskID: held.TaskID, OwnerRef: held.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Tokens: 100, FanOut: 1, USDMicros: 1000}, Deadline: held.Deadline, MoneyBound: true, PricingRevision: "prices-1"}).Freeze()
	require.NoError(t, err)
	s := &settlementServiceStore{providerServiceStore: &providerServiceStore{claim: completed, seal: &seal}, snapshot: snapshot, binding: binding, plan: plan, budget: coordination.BudgetedClaim{Claim: held, Reservation: execution.Reservation{Request: allocation, RequestDigest: allocationDigest, CreatedAt: now}}}
	cfg := coordination.TaskSettlementServiceConfig{Store: s, ClosureActor: "accounting", PageSize: 2, MaxInFlight: 1,
		ReportError: func(err error) { t.Logf("settlement pass: %v", err) },
		Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: held.Scope, PlanID: plan.ID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil },
		ClosureFence: usageClosureFence(func(_ context.Context, r coordination.TaskUsageClosureRequest, next func(coordination.TaskUsageClosureRequest) error) error {
			return next(r)
		}),
		ClosureAuthorizer: usageClosureAuthorizer(func(context.Context, coordination.TaskUsageClosureRequest) error { return nil }),
		Fence: usageSettlementFence(func(_ context.Context, r coordination.TaskUsageSettlementRequest, next func(coordination.TaskUsageSettlementRequest) error) error {
			return next(r)
		}),
		Authorizer: usageSettlementAuthorizer(func(context.Context, coordination.TaskUsageSettlementRequest) error { return nil }),
	}
	cfg.Resolver = settlementResolver(func(_ context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, allocation execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
		r := coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: coordination.TaskUsageRequest{Claim: snapshot.Claim.Token(), Seal: snapshot.Journal.Seal, SnapshotDigest: snapshot.Journal.Digest}, UsageDigest: snapshot.Digest, ClosureDigest: closed.Digest, ReservationID: allocation.Request.ID, ReservationDigest: allocation.RequestDigest, Actor: "host", Reason: "final billing", Billing: coordination.TaskBillingProof{Issuer: "fixture", Revision: "1", ProofRef: "bill", ProofDigest: evidence.Digest([]byte("bill")), PricingRevision: allocation.Request.PricingRevision, ObservedAt: closed.At, ExpiresAt: closed.At.Add(time.Minute)}}
		return &r, nil
	})
	return cortex.WithScope(t.Context(), held.Scope), s, cfg
}

func TestTaskSettlementServiceClosesAndReplaysWithoutResolvingAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := settlementServiceFixture(t)
		resolver := cfg.Resolver
		var resolutions, authorizations atomic.Int32
		var deny atomic.Bool
		cfg.AuthorizeScan = func(_ context.Context, claim coordination.Claim, seal execution.RunCallSeal) error {
			claim.Scope.Levels[0].Value = "mutated scan claim"
			seal.Request.Scope.Levels[0].Value = "mutated scan seal"
			return nil
		}
		cfg.Resolver = settlementResolver(func(ctx context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, allocation execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
			resolutions.Add(1)
			require.Equal(t, s.snapshot, snapshot)
			require.NotNil(t, s.closed)
			r, err := resolver.ResolveTaskUsageSettlement(ctx, snapshot, closed, allocation)
			require.NoError(t, err)
			frozen, _, freezeErr := r.Freeze()
			require.NoError(t, freezeErr)
			snapshot.Claim.Scope.Levels[0].Value = "mutated resolver input"
			closed.Request.Usage.Seal.Request.Scope.Levels[0].Value = "mutated closure"
			allocation.Request.Scope.Levels[0].Value = "mutated allocation"
			return &frozen, nil
		})
		cfg.Authorizer = usageSettlementAuthorizer(func(context.Context, coordination.TaskUsageSettlementRequest) error {
			authorizations.Add(1)
			if deny.Load() {
				return errors.New("revoked settlement permission")
			}
			return nil
		})
		service, err := coordination.NewTaskSettlementService(cfg)
		require.NoError(t, err)
		defer func() { require.NoError(t, service.Stop(context.Background())) }()
		startup, cancel := context.WithCancel(ctx)
		require.NoError(t, service.Start(startup))
		cancel()
		synctest.Wait()
		require.EqualValues(t, 1, s.settlements.Load())
		require.NoError(t, service.Stop(ctx))
		require.EqualValues(t, 1, s.closes.Load())
		require.EqualValues(t, 1, resolutions.Load())
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.EqualValues(t, 2, s.settlements.Load())
		require.EqualValues(t, 2, authorizations.Load())
		require.EqualValues(t, 1, resolutions.Load())
		require.EqualValues(t, 1, s.reads.Load())
		require.Zero(t, service.Status().Failures)
		deny.Store(true)
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.EqualValues(t, 2, s.settlements.Load())
		require.EqualValues(t, 3, authorizations.Load())
		require.EqualValues(t, 1, resolutions.Load())
		require.Positive(t, service.Status().Failures)
	})
}

func TestTaskSettlementServiceRejectsUnavailableAuthorityAndChangedBindings(t *testing.T) {
	for _, mode := range []string{"unbudgeted", "unsealed", "unfinished", "open job", "pending proof", "scan denied", "closure denied", "billing denied", "snapshot", "request", "reservation", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg := settlementServiceFixture(t)
				cfg.AttemptTimeout = time.Second
				resolver := cfg.Resolver
				switch mode {
				case "unbudgeted":
					s.noBudget = true
				case "unsealed":
					s.seal = nil
				case "unfinished":
					s.claim = s.budget.Claim
				case "open job":
					req, digest, freezeErr := (verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: id.New(verification.PrefixContract), Binding: s.binding, Deadline: s.claim.Deadline, MaxAttempts: 1}).Freeze()
					require.NoError(t, freezeErr)
					job, sealErr := verification.SealJob(verification.Job{SchemaVersion: 1, Request: req, RequestDigest: digest, RunRevision: 2, Revision: 1, State: verification.JobQueued, CreatedAt: s.claim.At, UpdatedAt: s.claim.At})
					require.NoError(t, sealErr)
					s.snapshot.Jobs = []verification.Job{job}
					s.snapshot, sealErr = coordination.SealTaskUsageSnapshot(s.snapshot)
					require.NoError(t, sealErr)
				case "scan denied":
					cfg.AuthorizeScan = func(context.Context, coordination.Claim, execution.RunCallSeal) error { return errors.New("denied") }
				case "closure denied":
					cfg.ClosureAuthorizer = usageClosureAuthorizer(func(context.Context, coordination.TaskUsageClosureRequest) error { return errors.New("denied") })
				case "billing denied":
					cfg.Authorizer = usageSettlementAuthorizer(func(context.Context, coordination.TaskUsageSettlementRequest) error { return errors.New("denied") })
				case "snapshot":
					s.snapshot.Digest = evidence.Digest([]byte("other"))
				}
				cfg.Resolver = settlementResolver(func(ctx context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, allocation execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
					if mode == "pending proof" {
						return nil, nil
					}
					if mode == "timeout" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					r, err := resolver.ResolveTaskUsageSettlement(ctx, snapshot, closed, allocation)
					if mode == "request" {
						r.UsageDigest = evidence.Digest([]byte("other"))
					}
					if mode == "reservation" {
						r.ReservationID = id.New(execution.PrefixReservation)
					}
					return r, err
				})
				service, err := coordination.NewTaskSettlementService(cfg)
				require.NoError(t, err)
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				require.NoError(t, service.Start(ctx))
				synctest.Wait()
				if mode == "timeout" {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				require.NoError(t, service.Stop(ctx))
				require.Zero(t, s.settlements.Load())
				if mode == "scan denied" || mode == "unbudgeted" || mode == "unsealed" || mode == "unfinished" {
					require.Zero(t, s.reads.Load())
					require.Zero(t, s.closes.Load())
				}
				if mode == "open job" {
					require.Zero(t, s.closes.Load())
					require.Zero(t, service.Status().Failures)
				}
			})
		})
	}
}

func TestTaskSettlementServiceRequiresPortsAndBounds(t *testing.T) {
	for _, mode := range []string{"store", "discovery", "scan", "actor", "resolver", "closure fence", "closure authority", "fence", "authority", "timeout", "page"} {
		t.Run(mode, func(t *testing.T) {
			_, _, cfg := settlementServiceFixture(t)
			switch mode {
			case "store":
				cfg.Store = nil
			case "discovery":
				cfg.Discovery = nil
			case "scan":
				cfg.AuthorizeScan = nil
			case "actor":
				cfg.ClosureActor = ""
			case "resolver":
				cfg.Resolver = nil
			case "closure fence":
				cfg.ClosureFence = nil
			case "closure authority":
				cfg.ClosureAuthorizer = nil
			case "fence":
				cfg.Fence = nil
			case "authority":
				cfg.Authorizer = nil
			case "timeout":
				cfg.AttemptTimeout = time.Minute
			case "page":
				cfg.PageSize = 257
			}
			_, err := coordination.NewTaskSettlementService(cfg)
			require.Error(t, err)
		})
	}
}

func TestTaskSettlementServiceJoinsNoncooperativeResolver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := settlementServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		resolver := cfg.Resolver
		cfg.Resolver = settlementResolver(func(ctx context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, allocation execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
			close(entered)
			<-ctx.Done()
			<-release
			return resolver.ResolveTaskUsageSettlement(ctx, snapshot, closed, allocation)
		})
		service, err := coordination.NewTaskSettlementService(cfg)
		require.NoError(t, err)
		defer func() { unblock(); require.NoError(t, service.Stop(context.Background())) }()
		require.NoError(t, service.Start(ctx))
		<-entered
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Positive(t, service.Status().Active)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, s.settlements.Load())
		require.Zero(t, service.Status().Active)
	})
}
