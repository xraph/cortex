package coordination_test

import (
	"context"
	"errors"
	"sort"
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
)

type providerServiceStore struct {
	mu       sync.Mutex
	sealRead func(id.AgentRunID) (*execution.RunCallSeal, error)
	execution.RunCallSealStore
	claim               coordination.Claim
	calls               []execution.RunCallRecord
	seal                *execution.RunCallSeal
	reads, seals, scans atomic.Int32
	list                func(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error)
}

func (s *providerServiceStore) ListTaskClaims(ctx context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
	s.reads.Add(1)
	if s.list != nil {
		return s.list(ctx, r)
	}
	if !r.AfterTaskID.IsNil() {
		return nil, nil
	}
	return []coordination.Claim{s.claim}, nil
}
func (s *providerServiceStore) GetRunCallSeal(_ context.Context, runID id.AgentRunID) (*execution.RunCallSeal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seals.Add(1)
	if s.sealRead != nil {
		return s.sealRead(runID)
	}
	return s.seal, nil
}
func (s *providerServiceStore) ListRunCalls(_ context.Context, r execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
	s.scans.Add(1)
	var rows []execution.RunCallRecord
	for _, record := range s.calls {
		if record.Request.Key() > r.After && len(rows) < r.Limit {
			rows = append(rows, record)
		}
	}
	return rows, nil
}
func (*providerServiceStore) GetProviderCallReceipt(context.Context, execution.RunCallRequest) (*execution.ProviderCallReceipt, error) {
	panic("no calls in journal")
}
func (*providerServiceStore) PutProviderCallReceipt(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error {
	panic("no calls in journal")
}

type unusedProviderProbe struct{}

func (unusedProviderProbe) ReconcileProviderCall(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
	panic("no calls in journal")
}

type unusedProviderFence struct{}

func (unusedProviderFence) WithProviderCallFence(context.Context, execution.RunCallRequest, func() error) error {
	panic("no calls in journal")
}

func providerServiceFixture(t *testing.T) (context.Context, *providerServiceStore, coordination.ProviderRecoveryServiceConfig) {
	t.Helper()
	ctx, claim, now := expiryFixture(t)
	claim, err := coordination.BeginDispatch(claim, claim.Token(), id.NewAgentRunID(), now)
	require.NoError(t, err)
	claim, err = coordination.ExpireClaim(claim, claim.Revision, claim.ExpiresAt)
	require.NoError(t, err)
	request := execution.RunCallSealRequest{SchemaVersion: 1, Scope: claim.Scope, RunID: claim.RunID, Actor: "recovery", Reason: "interrupted"}
	_, digest, err := request.Freeze()
	require.NoError(t, err)
	s := &providerServiceStore{claim: claim, seal: &execution.RunCallSeal{Request: request, RequestDigest: digest, At: claim.At}}
	cfg := coordination.ProviderRecoveryServiceConfig{
		Recovery: execution.ProviderRecoveryConfig{Store: s, Adapter: unusedProviderProbe{}, Fence: unusedProviderFence{}, Authorize: func(context.Context, execution.RunCallRequest) error { return nil }, Authenticate: func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error { return nil }, AuthorizeScan: func(context.Context, execution.RunCallSeal) error { return nil }},
		PageSize: 1, MaxInFlight: 1,
		Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		}),
	}
	return ctx, s, cfg
}

func TestProviderRecoveryServiceOwnsScopedPagesAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		type identityKey struct{}
		startup, cancel := context.WithCancel(context.WithValue(ctx, identityKey{}, "recovery-identity"))
		defer cancel()
		cfg.Recovery.AuthorizeScan = func(got context.Context, seal execution.RunCallSeal) error {
			require.Equal(t, "recovery-identity", got.Value(identityKey{}))
			require.Equal(t, s.claim.Scope, cortex.ScopeFromContext(got))
			require.Equal(t, *s.seal, seal)
			return nil
		}
		var discoveries atomic.Int32
		cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: s.claim.Scope, PlanID: s.claim.PlanID}}}, nil
		})
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(startup))
		cancel()
		synctest.Wait()
		require.Equal(t, int32(1), s.scans.Load())
		require.Equal(t, 1, service.Status().Active)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(2), s.reads.Load())
		require.Zero(t, service.Status().Active)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Failures)
		discoveries.Store(0)
		require.NoError(t, service.Start(context.WithValue(ctx, identityKey{}, "recovery-identity")))
		synctest.Wait()
		require.Equal(t, int32(2), s.scans.Load())
		service.RequestStop()
		require.NoError(t, service.Stop(ctx))
	})
}

func TestProviderRecoveryServiceRetainsUnsealedAndActiveClaims(t *testing.T) {
	for _, mode := range []string{"unsealed", "active"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg := providerServiceFixture(t)
				if mode == "unsealed" {
					s.seal = nil
				} else {
					_, s.claim, _ = expiryFixture(t)
					cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
						return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: s.claim.Scope, PlanID: s.claim.PlanID}}}, nil
					})
				}
				service, err := coordination.NewProviderRecoveryService(cfg)
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				synctest.Wait()
				require.Zero(t, s.scans.Load())
				require.Zero(t, service.Status().Failures)
				if mode == "active" {
					require.Zero(t, s.seals.Load())
				}
				require.NoError(t, service.Stop(ctx))
			})
		})
	}
}

func TestProviderRecoveryServiceValidatesClaimsBeforeReadingSeals(t *testing.T) {
	for _, mode := range []string{"duplicate", "binding", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg := providerServiceFixture(t)
				cfg.PageSize = 2
				rows := []coordination.Claim{s.claim, s.claim}
				switch mode {
				case "binding":
					rows[1].Digest = "bad"
				case "oversize":
					cfg.PageSize = 1
				}
				s.list = func(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error) { return rows, nil }
				service, err := coordination.NewProviderRecoveryService(cfg)
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				synctest.Wait()
				require.Equal(t, uint64(1), service.Status().Failures)
				require.Zero(t, s.seals.Load())
				require.NoError(t, service.Stop(ctx))
			})
		})
	}
}

func TestProviderRecoveryServiceStopTimeoutRetainsOwnedCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		s.list = func(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error) {
			close(entered)
			<-release
			return nil, nil
		}
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		<-entered
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Equal(t, 1, service.Status().Active)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, service.Status().Active)
	})
}

func TestProviderRecoveryServiceRejectsIncompleteConfiguration(t *testing.T) {
	_, _, base := providerServiceFixture(t)
	for _, change := range []func(*coordination.ProviderRecoveryServiceConfig){
		func(c *coordination.ProviderRecoveryServiceConfig) { c.Recovery.Store = nil }, func(c *coordination.ProviderRecoveryServiceConfig) { c.Recovery.AuthorizeScan = nil }, func(c *coordination.ProviderRecoveryServiceConfig) { c.Discovery = nil },
		func(c *coordination.ProviderRecoveryServiceConfig) { c.PageSize = 257 }, func(c *coordination.ProviderRecoveryServiceConfig) { c.PageTimeout = time.Hour }, func(c *coordination.ProviderRecoveryServiceConfig) { c.MaxInFlight = 4097 }, func(c *coordination.ProviderRecoveryServiceConfig) { c.MaxPerScope = -1 },
	} {
		cfg := base
		change(&cfg)
		_, err := coordination.NewProviderRecoveryService(cfg)
		require.Error(t, err)
	}
}

func TestProviderRecoveryServiceTraversesCallPagesBeforeAdvancingClaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		for _, name := range []string{"one", "two"} {
			request := execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: s.claim.Scope, RunID: s.claim.RunID, AgentID: id.NewAgentID(), ClaimID: s.claim.ID, ParentRunID: id.NewAgentRunID(), Kind: execution.RunCallTool, OperationDigest: evidence.Digest([]byte(name)), RequestDigest: evidence.Digest([]byte("original")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: time.Now().Add(time.Minute)}
			s.calls = append(s.calls, execution.RunCallRecord{Request: request, StartedAt: time.Now()})
		}
		sort.Slice(s.calls, func(i, j int) bool { return s.calls[i].Request.Key() < s.calls[j].Request.Key() })
		var discoveries atomic.Int32
		cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: s.claim.Scope, PlanID: s.claim.PlanID}}}, nil
		})
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(ctx)) }()
		synctest.Wait()
		require.Equal(t, int32(1), s.scans.Load())
		require.Equal(t, int32(1), s.reads.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(2), s.scans.Load())
		require.Equal(t, int32(1), s.reads.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(3), s.scans.Load())
		require.Equal(t, 1, service.Status().Active)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(2), s.reads.Load())
		require.Zero(t, service.Status().Active)
		require.Zero(t, service.Status().Failures)
	})
}

func TestProviderRecoveryServiceBoundsClaimRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		cfg.PageTimeout = time.Second
		s.list = func(ctx context.Context, _ coordination.ClaimListRequest) ([]coordination.Claim, error) {
			<-ctx.Done()
			return nil, nil
		}
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(ctx)) }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, uint64(1), service.Status().Failures)
		require.Zero(t, s.scans.Load())
	})
}

func TestProviderRecoveryServiceRevisitsRunAfterSealArrives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		seal := s.seal
		s.seal = nil
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(ctx)) }()
		synctest.Wait()
		require.Zero(t, s.scans.Load())
		s.mu.Lock()
		s.seal = seal
		s.mu.Unlock()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.Positive(t, s.scans.Load())
		require.Zero(t, service.Status().Failures)
	})
}

func TestProviderRecoveryServiceRunFailureDoesNotStarveLaterClaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg := providerServiceFixture(t)
		h, plan, _, now := handoffFixture(t)
		task := plan.Tasks[0]
		task.ID = id.New(planning.PrefixTask)
		task.DependsOn = []id.ID{plan.Tasks[0].ID}
		task.Output.Name = "second report"
		plan.Tasks = append(plan.Tasks, task)
		plan, err := planning.Seal(plan, planning.Limits{})
		require.NoError(t, err)
		claims := make([]coordination.Claim, 0, 2)
		for _, task := range plan.Tasks {
			claim, claimErr := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, TaskID: task.ID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, plan, now)
			require.NoError(t, claimErr)
			claim, claimErr = coordination.BeginDispatch(claim, claim.Token(), id.NewAgentRunID(), now)
			require.NoError(t, claimErr)
			claim, claimErr = coordination.ExpireClaim(claim, claim.Revision, claim.ExpiresAt)
			require.NoError(t, claimErr)
			claims = append(claims, claim)
		}
		sort.Slice(claims, func(i, j int) bool { return claims[i].TaskID.String() < claims[j].TaskID.String() })
		request := s.seal.Request
		request.RunID = claims[1].RunID
		_, digest, err := request.Freeze()
		require.NoError(t, err)
		s.seal = &execution.RunCallSeal{Request: request, RequestDigest: digest, At: now}
		s.sealRead = func(runID id.AgentRunID) (*execution.RunCallSeal, error) {
			if runID == claims[0].RunID {
				return nil, errors.New("first run unavailable")
			}
			return s.seal, nil
		}
		s.list = func(_ context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
			if !r.AfterTaskID.IsNil() {
				return nil, nil
			}
			return claims, nil
		}
		cfg.PageSize = 2
		var discoveries atomic.Int32
		cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: plan.Scope, PlanID: plan.ID}}}, nil
		})
		service, err := coordination.NewProviderRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(ctx)) }()
		synctest.Wait()
		require.Equal(t, int32(1), s.scans.Load(), "a failed run must not prevent later claim recovery")
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, uint64(1), service.Status().Failures)
		require.Zero(t, service.Status().Active)
	})
}
