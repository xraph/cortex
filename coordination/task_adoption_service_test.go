package coordination_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

type taskRecoveryResolverFunc func(context.Context, coordination.Claim, execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error)

func (f taskRecoveryResolverFunc) ResolveTaskRecovery(ctx context.Context, c coordination.Claim, s execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
	return f(ctx, c, s)
}

type adoptionServiceStore struct {
	*providerServiceStore
	snapshot             execution.RunRecoverySnapshot
	receipt              coordination.TaskAcceptance
	snapshots, adoptions atomic.Int32
}

func (s *adoptionServiceStore) SnapshotRunRecovery(context.Context, execution.RunCallSeal) (execution.RunRecoverySnapshot, error) {
	s.snapshots.Add(1)
	return s.snapshot, nil
}
func (s *adoptionServiceStore) AdoptTaskResult(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	s.adoptions.Add(1)
	return s.receipt, nil
}

func adoptionServiceFixture(t *testing.T) (context.Context, *adoptionServiceStore, coordination.TaskAdoptionServiceConfig) {
	t.Helper()
	receipt, claim := taskAcceptanceClaimFixture(t, true)
	snapshot, err := execution.SealRunRecoverySnapshot(execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: receipt.Request.Recovery.Seal})
	require.NoError(t, err)
	s := &adoptionServiceStore{providerServiceStore: &providerServiceStore{claim: claim, seal: &snapshot.Seal}, snapshot: snapshot, receipt: receipt}
	cfg := coordination.TaskAdoptionServiceConfig{Store: s, PageSize: 1, MaxInFlight: 1,
		Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil },
		Resolver: taskRecoveryResolverFunc(func(context.Context, coordination.Claim, execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
			r, _, freezeErr := receipt.Request.Freeze()
			return &r, freezeErr
		}),
		Fence: taskRecoveryFenceFunc(func(_ context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
			return next(r)
		}),
		Authorizer: taskRecoveryAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return nil }),
	}
	return cortex.WithScope(t.Context(), claim.Scope), s, cfg
}

func TestTaskAdoptionServiceAuthenticatesSnapshotAndAdopts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := adoptionServiceFixture(t)
		var steps []string
		cfg.AuthorizeScan = func(_ context.Context, c coordination.Claim, seal execution.RunCallSeal) error {
			require.Equal(t, s.adoptions.Load(), s.snapshots.Load())
			steps = append(steps, "scan authority")
			c.Scope.Levels[0].Value = "mutated"
			seal.Request.Scope.Levels[0].Value = "mutated"
			return nil
		}
		cfg.Resolver = taskRecoveryResolverFunc(func(bounded context.Context, c coordination.Claim, snapshot execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
			require.Equal(t, s.claim, c)
			require.Equal(t, s.snapshot, snapshot)
			require.Equal(t, c.Scope, cortex.ScopeFromContext(bounded))
			deadline, ok := bounded.Deadline()
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
			steps = append(steps, "resolve")
			c.Scope.Levels[0].Value = "mutated"
			snapshot.Seal.Request.Scope.Levels[0].Value = "mutated"
			r, _, err := s.receipt.Request.Freeze()
			return &r, err
		})
		cfg.Authorizer = taskRecoveryAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error {
			steps = append(steps, "adoption authority")
			return nil
		})
		service, err := coordination.NewTaskAdoptionService(cfg)
		require.NoError(t, err)
		startup, cancel := context.WithCancel(ctx)
		require.NoError(t, service.Start(startup))
		defer func() { require.NoError(t, service.Stop(context.Background())) }()
		cancel()
		synctest.Wait()
		require.Equal(t, int32(1), s.adoptions.Load())
		require.Equal(t, []string{"scan authority", "resolve", "adoption authority"}, steps)
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, service.Status().Failures)
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		require.Equal(t, int32(2), s.adoptions.Load())
		require.NoError(t, service.Stop(ctx))
	})
}

func TestTaskAdoptionServiceRejectsChangedEvidenceBeforeStorage(t *testing.T) {
	for _, mode := range []string{"pending", "denied scan", "snapshot", "claim", "proof", "binding", "cancelled", "timeout", "denied adoption"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg := adoptionServiceFixture(t)
				cfg.AttemptTimeout = time.Second
				cfg.PageSize = 2
				if mode == "denied scan" {
					cfg.AuthorizeScan = func(context.Context, coordination.Claim, execution.RunCallSeal) error { return errors.New("denied") }
				}
				if mode == "snapshot" {
					s.snapshot.Digest = evidence.Digest([]byte("other"))
				}
				if mode == "denied adoption" {
					cfg.Authorizer = taskRecoveryAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return errors.New("denied") })
				}
				cfg.Resolver = taskRecoveryResolverFunc(func(bounded context.Context, _ coordination.Claim, _ execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
					r, _, err := s.receipt.Request.Freeze()
					require.NoError(t, err)
					switch mode {
					case "pending":
						return nil, nil
					case "claim":
						r.Claim.Revision--
					case "proof":
						r.Recovery.SnapshotDigest = evidence.Digest([]byte("other"))
					case "binding":
						r.Recovery.ClaimDigest = evidence.Digest([]byte("other"))
					case "cancelled":
						return nil, context.Canceled
					case "timeout":
						<-bounded.Done()
					}
					return &r, nil
				})
				service, err := coordination.NewTaskAdoptionService(cfg)
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				synctest.Wait()
				if mode == "timeout" {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				require.Zero(t, s.adoptions.Load())
				if mode == "denied scan" {
					require.Zero(t, s.snapshots.Load())
				}
				require.NoError(t, service.Stop(ctx))
				if mode == "pending" {
					require.Zero(t, service.Status().Failures)
				} else {
					require.Positive(t, service.Status().Failures)
				}
			})
		})
	}
}

func TestTaskAdoptionServiceRequiresExplicitPortsAndBounds(t *testing.T) {
	for _, mode := range []string{"store", "resolver", "fence", "authorizer", "scan", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			_, _, cfg := adoptionServiceFixture(t)
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
			_, err := coordination.NewTaskAdoptionService(cfg)
			require.Error(t, err)
		})
	}
}

func TestTaskAdoptionServiceOwnsResolverUntilItReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := adoptionServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		cfg.Resolver = taskRecoveryResolverFunc(func(bounded context.Context, _ coordination.Claim, _ execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
			close(entered)
			<-bounded.Done()
			<-release
			request, _, err := s.receipt.Request.Freeze()
			return &request, err
		})
		service, err := coordination.NewTaskAdoptionService(cfg)
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
		require.Zero(t, s.adoptions.Load())
		require.Zero(t, service.Status().Active)
	})
}
