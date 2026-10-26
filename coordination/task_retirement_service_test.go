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
	"github.com/xraph/cortex/run"
)

type retirementResolver func(context.Context, coordination.Claim, execution.RunRecoverySnapshot, coordination.TaskUsageClosure, coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error)

func (f retirementResolver) ResolveTaskRetirement(ctx context.Context, c coordination.Claim, s execution.RunRecoverySnapshot, closed coordination.TaskUsageClosure, settled coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error) {
	return f(ctx, c, s, closed, settled)
}

type retirementServiceStore struct {
	*providerServiceStore
	coordination.TaskUsageClosureStore
	coordination.TaskUsageSettlementStore
	mu                            sync.Mutex
	snapshot                      execution.RunRecoverySnapshot
	closed                        *coordination.TaskUsageClosure
	settled                       *coordination.TaskUsageSettlement
	retained                      *coordination.TaskRetirement
	lostAck                       bool
	reads, snapshots, retirements atomic.Int32
}

func (s *retirementServiceStore) SnapshotRunRecovery(context.Context, execution.RunCallSeal) (execution.RunRecoverySnapshot, error) {
	s.snapshots.Add(1)
	return s.snapshot, nil
}
func (s *retirementServiceStore) GetTaskUsageClosure(context.Context, id.AgentRunID) (*coordination.TaskUsageClosure, error) {
	s.reads.Add(1)
	return s.closed, nil
}
func (s *retirementServiceStore) GetTaskUsageSettlement(context.Context, id.AgentRunID) (*coordination.TaskUsageSettlement, error) {
	s.reads.Add(1)
	return s.settled, nil
}
func (s *retirementServiceStore) GetTaskRetirement(context.Context, id.ID) (*coordination.TaskRetirement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads.Add(1)
	return s.retained, nil
}
func (s *retirementServiceStore) RetireTaskClaim(_ context.Context, req coordination.TaskRetirementRequest) (coordination.TaskRetirement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retirements.Add(1)
	if s.retained != nil {
		return *s.retained, nil
	}
	retired, err := coordination.RetireRecoveredClaim(s.claim, req, req.Recovery.ObservedAt)
	if err != nil {
		return coordination.TaskRetirement{}, err
	}
	_, digest, err := req.Freeze()
	if err != nil {
		return coordination.TaskRetirement{}, err
	}
	receipt, err := coordination.SealTaskRetirement(coordination.TaskRetirement{SchemaVersion: 1, Request: req, RequestDigest: digest, Claim: retired})
	if err != nil {
		return coordination.TaskRetirement{}, err
	}
	s.retained = &receipt
	if s.lostAck {
		return coordination.TaskRetirement{}, errors.New("commit acknowledgment lost")
	}
	receipt.Fresh = true
	return receipt, nil
}

func retirementServiceFixture(t *testing.T) (context.Context, *retirementServiceStore, coordination.TaskRetirementServiceConfig, coordination.TaskRetirementRequest) {
	t.Helper()
	claim, request := retirementFixture(t)
	snapshot, err := execution.SealRunRecoverySnapshot(execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: request.Recovery.Seal})
	require.NoError(t, err)
	request.Recovery.SnapshotDigest = snapshot.Digest
	usage := coordination.TaskUsageRequest{Claim: claim.Token(), Seal: snapshot.Seal, SnapshotDigest: snapshot.Digest}
	closeReq := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: evidence.Digest([]byte("usage")), Actor: "host", Reason: "failed attempt", Termination: &coordination.TaskUsageTermination{RunRevision: 1, State: run.StateFailed, CompletedAt: claim.At}}
	_, closeDigest, err := closeReq.Freeze()
	require.NoError(t, err)
	closed, err := coordination.SealTaskUsageClosure(coordination.TaskUsageClosure{SchemaVersion: 1, Request: closeReq, RequestDigest: closeDigest, At: claim.At})
	require.NoError(t, err)
	settleReq := coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: usage, UsageDigest: closeReq.UsageDigest, ClosureDigest: closed.Digest, ReservationID: id.New(execution.PrefixReservation), ReservationDigest: evidence.Digest([]byte("allocation")), Actor: "host", Reason: "conservative settlement", Billing: coordination.TaskBillingProof{Issuer: "fixture", Revision: "1", ProofRef: "bounded-bill", ProofDigest: evidence.Digest([]byte("bill")), ObservedAt: claim.At, ExpiresAt: claim.At.Add(time.Minute)}}
	_, settleDigest, err := settleReq.Freeze()
	require.NoError(t, err)
	accounting := execution.Settlement{ReservationID: settleReq.ReservationID, RequestDigest: settleReq.ReservationDigest, Receipt: "task-usage:" + settleDigest}
	_, accountingDigest, err := accounting.Freeze()
	require.NoError(t, err)
	settled, err := coordination.SealTaskUsageSettlement(coordination.TaskUsageSettlement{SchemaVersion: 1, Request: settleReq, RequestDigest: settleDigest, Settlement: execution.SettlementReceipt{Request: accounting, Digest: accountingDigest, At: claim.At}})
	require.NoError(t, err)
	request.ClosureDigest, request.SettlementDigest = closed.Digest, settled.Digest
	s := &retirementServiceStore{providerServiceStore: &providerServiceStore{claim: claim, seal: &snapshot.Seal}, snapshot: snapshot, closed: &closed, settled: &settled}
	config := coordination.TaskRetirementServiceConfig{Store: s, PageSize: 2, MaxInFlight: 1,
		Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil },
		Resolver: retirementResolver(func(context.Context, coordination.Claim, execution.RunRecoverySnapshot, coordination.TaskUsageClosure, coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error) {
			r, _, freezeErr := request.Freeze()
			return &r, freezeErr
		}),
		Fence: retirementFence(func(_ context.Context, r coordination.TaskRetirementRequest, next func(coordination.TaskRetirementRequest) error) error {
			return next(r)
		}),
		Authorizer: retirementAuthorizer(func(context.Context, coordination.TaskRetirementRequest) error { return nil }),
	}
	return cortex.WithScope(t.Context(), claim.Scope), s, config, request
}

func TestTaskRetirementServiceRecoversLostCommitWithCurrentAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg, req := retirementServiceFixture(t)
		s.lostAck = true
		var resolutions, authorizations atomic.Int32
		denied := false
		cfg.AuthorizeScan = func(_ context.Context, claim coordination.Claim, seal execution.RunCallSeal) error {
			if s.retirements.Load() == 0 {
				require.Zero(t, s.reads.Load())
			}
			claim.Scope.Levels[0].Value = "mutated"
			seal.Request.Scope.Levels[0].Value = "mutated"
			return nil
		}
		cfg.Resolver = retirementResolver(func(bounded context.Context, claim coordination.Claim, journal execution.RunRecoverySnapshot, closed coordination.TaskUsageClosure, settled coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error) {
			resolutions.Add(1)
			require.Equal(t, s.claim, claim)
			require.Equal(t, s.snapshot, journal)
			require.Equal(t, *s.closed, closed)
			require.Equal(t, *s.settled, settled)
			require.Equal(t, claim.Scope, cortex.ScopeFromContext(bounded))
			deadline, ok := bounded.Deadline()
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
			claim.Scope.Levels[0].Value = "mutated"
			journal.Seal.Request.Scope.Levels[0].Value = "mutated"
			closed.Request.Usage.Seal.Request.Scope.Levels[0].Value = "mutated"
			settled.Request.Usage.Seal.Request.Scope.Levels[0].Value = "mutated"
			frozen, _, err := req.Freeze()
			return &frozen, err
		})
		cfg.Authorizer = retirementAuthorizer(func(context.Context, coordination.TaskRetirementRequest) error {
			authorizations.Add(1)
			if denied {
				return errors.New("revoked")
			}
			return nil
		})
		service, err := coordination.NewTaskRetirementService(cfg)
		require.NoError(t, err)
		startup, cancel := context.WithCancel(ctx)
		require.NoError(t, service.Start(startup))
		cancel()
		synctest.Wait()
		require.EqualValues(t, 1, s.retirements.Load())
		require.NoError(t, service.Stop(ctx))
		require.Positive(t, service.Status().Failures)
		denied = true
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.EqualValues(t, 1, s.retirements.Load())
		require.EqualValues(t, 1, resolutions.Load())
		denied = false
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.EqualValues(t, 2, s.retirements.Load())
		require.EqualValues(t, 1, resolutions.Load())
		require.EqualValues(t, 3, authorizations.Load())
	})
}

func TestTaskRetirementServiceRejectsChangedEvidence(t *testing.T) {
	for _, mode := range []string{"pending", "unsettled", "unsealed", "denied scan", "missing closure", "journal", "claim", "proof", "settlement", "denied retirement", "cancelled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg, req := retirementServiceFixture(t)
				cfg.AttemptTimeout = time.Second
				cfg.PageSize = 2
				switch mode {
				case "unsettled":
					s.settled = nil
				case "unsealed":
					s.seal = nil
				case "missing closure":
					s.closed = nil
				case "journal":
					s.snapshot.Digest = evidence.Digest(nil)
				case "denied scan":
					cfg.AuthorizeScan = func(context.Context, coordination.Claim, execution.RunCallSeal) error { return errors.New("denied") }
				case "denied retirement":
					cfg.Authorizer = retirementAuthorizer(func(context.Context, coordination.TaskRetirementRequest) error { return errors.New("denied") })
				}
				cfg.Resolver = retirementResolver(func(bounded context.Context, _ coordination.Claim, _ execution.RunRecoverySnapshot, _ coordination.TaskUsageClosure, _ coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error) {
					r, _, err := req.Freeze()
					require.NoError(t, err)
					switch mode {
					case "pending":
						return nil, nil
					case "claim":
						r.Claim.Revision++
					case "proof":
						r.Recovery.SnapshotDigest = evidence.Digest(nil)
					case "settlement":
						r.SettlementDigest = evidence.Digest(nil)
					case "cancelled":
						return nil, context.Canceled
					case "timeout":
						<-bounded.Done()
					}
					return &r, nil
				})
				service, err := coordination.NewTaskRetirementService(cfg)
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				synctest.Wait()
				if mode == "timeout" {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				require.NoError(t, service.Stop(ctx))
				require.Zero(t, s.retirements.Load())
				if mode == "denied scan" {
					require.Zero(t, s.reads.Load())
				}
				if mode == "pending" || mode == "unsettled" || mode == "unsealed" {
					require.Zero(t, service.Status().Failures)
				} else {
					require.Positive(t, service.Status().Failures)
				}
			})
		})
	}
}

func TestTaskRetirementServiceDrainsResolverBeforeRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg, req := retirementServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		cfg.Resolver = retirementResolver(func(bounded context.Context, _ coordination.Claim, _ execution.RunRecoverySnapshot, _ coordination.TaskUsageClosure, _ coordination.TaskUsageSettlement) (*coordination.TaskRetirementRequest, error) {
			close(entered)
			<-bounded.Done()
			<-release
			return &req, nil
		})
		service, err := coordination.NewTaskRetirementService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		<-entered
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Positive(t, service.Status().Active)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		close(release)
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, s.retirements.Load())
	})
}

func TestTaskRetirementServiceRequiresPortsAndBounds(t *testing.T) {
	for _, mode := range []string{"store", "resolver", "fence", "authorizer", "scan", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			_, _, cfg, _ := retirementServiceFixture(t)
			switch mode {
			case "store":
				cfg.Store = nil
			case "resolver":
				cfg.Resolver = nil
			case "fence":
				cfg.Fence = nil
			case "authorizer":
				cfg.Authorizer = nil
			case "scan":
				cfg.AuthorizeScan = nil
			case "timeout":
				cfg.AttemptTimeout = time.Minute
			}
			_, err := coordination.NewTaskRetirementService(cfg)
			require.Error(t, err)
		})
	}
}
