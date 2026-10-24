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
)

type effectServiceStore struct {
	*providerServiceStore
	records   []execution.EffectRecord
	lists     atomic.Int32
	blockList func(context.Context) error
}

func (*effectServiceStore) BeginEffect(context.Context, execution.EffectRequest) (bool, error) {
	panic("recovery cannot admit effects")
}
func (*effectServiceStore) BeginBoundEffect(context.Context, execution.EffectRequest) (bool, error) {
	panic("recovery cannot admit effects")
}
func (s *effectServiceStore) ListEffects(ctx context.Context, r execution.EffectListRequest) ([]execution.EffectRecord, error) {
	s.lists.Add(1)
	if s.blockList != nil {
		if err := s.blockList(ctx); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []execution.EffectRecord
	for _, row := range s.records {
		if row.Request.RunID == r.RunID && evidence.Digest([]byte(row.Request.OperationKey)) > r.After && len(rows) < r.Limit {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (s *effectServiceStore) GetEffect(_ context.Context, key string) (execution.EffectRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.records {
		if row.Request.OperationKey == key {
			return row, nil
		}
	}
	return execution.EffectRecord{}, execution.ErrEffectNotFound
}
func (s *effectServiceStore) CompleteEffect(_ context.Context, r execution.EffectRequest, v execution.EffectReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].Request.OperationKey == r.OperationKey {
			s.records[i].Receipt = &v
			return nil
		}
	}
	return execution.ErrEffectNotFound
}

type effectServiceProbe struct{ probes atomic.Int32 }

func (*effectServiceProbe) Execute(context.Context, execution.EffectRequest, []byte) (execution.EffectReceipt, error) {
	panic("recovery cannot mutate")
}
func (p *effectServiceProbe) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	p.probes.Add(1)
	_, digest, err := r.Freeze()
	if err != nil {
		return execution.EffectStatus{}, err
	}
	return execution.EffectStatus{Outcome: execution.EffectApplied, Receipt: &execution.EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.EffectApplied, ExternalID: "fixture", EvidenceRef: "fixture-proof"}}, nil
}
func effectServiceFixture(t *testing.T) (context.Context, *effectServiceStore, *effectServiceProbe, coordination.EffectRecoveryServiceConfig) {
	t.Helper()
	ctx, base, provider := providerServiceFixture(t)
	s := &effectServiceStore{providerServiceStore: base}
	probe := &effectServiceProbe{}
	requirement := execution.EffectRequirement{Tool: "publish", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "1", Retry: execution.RetryPolicy{MaxAttempts: 1}}
	for _, key := range []string{"one", "two", "three"} {
		s.records = append(s.records, execution.EffectRecord{Request: execution.EffectRequest{SchemaVersion: 1, Scope: base.claim.Scope, RunID: base.claim.RunID, OperationKey: key, AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte("payload")), PrincipalRef: "host", AuthorityRef: "grant", Deadline: time.Now().Add(-time.Minute), Requirement: requirement}})
	}
	sort.Slice(s.records, func(i, j int) bool {
		return evidence.Digest([]byte(s.records[i].Request.OperationKey)) < evidence.Digest([]byte(s.records[j].Request.OperationKey))
	})
	cfg := coordination.EffectRecoveryServiceConfig{Recovery: execution.EffectExecutorConfig{Store: s, Adapters: []execution.EffectRegistration{{Requirement: requirement, Adapter: probe}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, AuthorizeScan: func(context.Context, execution.RunCallSeal) error { return nil }, MaxConcurrent: 1}, Discovery: provider.Discovery, PageSize: 2, MaxInFlight: 1}
	return ctx, s, probe, cfg
}
func TestEffectRecoveryServiceOwnsScopedPagesAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, probe, cfg := effectServiceFixture(t)
		type identityKey struct{}
		cfg.Recovery.AuthorizeScan = func(got context.Context, _ execution.RunCallSeal) error {
			require.Equal(t, "service", got.Value(identityKey{}))
			require.Equal(t, s.claim.Scope, cortex.ScopeFromContext(got))
			return nil
		}
		startup, cancel := context.WithCancel(context.WithValue(ctx, identityKey{}, "service"))
		defer cancel()
		service, err := coordination.NewEffectRecoveryService(cfg)
		require.NoError(t, err)
		cfg.Recovery.Adapters[0].Adapter = nil // Registration changes cannot affect later service starts.
		require.NoError(t, service.Start(startup))
		cancel()
		synctest.Wait()
		require.Equal(t, int32(2), probe.probes.Load())
		require.Equal(t, 1, service.Status().Active)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(3), probe.probes.Load())
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, service.Status().Failures)
		require.NoError(t, service.Start(context.WithValue(ctx, identityKey{}, "service")))
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.Equal(t, int32(3), probe.probes.Load())
		require.Zero(t, service.Status().Active)
	})
}
func TestEffectRecoveryServiceSkipsUnsealedAndActiveClaims(t *testing.T) {
	for _, mode := range []string{"unsealed", "active"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, probe, cfg := effectServiceFixture(t)
				if mode == "unsealed" {
					s.seal = nil
				} else {
					_, fresh, now := expiryFixture(t)
					active, transitionErr := coordination.BeginDispatch(fresh, fresh.Token(), id.NewAgentRunID(), now)
					require.NoError(t, transitionErr)
					s.claim = active
					cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
						return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: s.claim.Scope, PlanID: s.claim.PlanID}}}, nil
					})
				}
				require.NoError(t, s.claim.Validate())
				service, err := coordination.NewEffectRecoveryService(cfg)
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				synctest.Wait()
				require.NoError(t, service.Stop(ctx))
				require.Zero(t, probe.probes.Load())
				require.Zero(t, s.lists.Load())
				require.Zero(t, service.Status().Failures)
			})
		})
	}
}
func TestEffectRecoveryServiceStopTimeoutRetainsWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, _, cfg := effectServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		s.blockList = func(context.Context) error { close(entered); <-release; return errors.New("stopped") }
		service, err := coordination.NewEffectRecoveryService(cfg)
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
func TestEffectRecoveryServiceRejectsIncompleteConfiguration(t *testing.T) {
	for _, mode := range []string{"store", "scan authority", "operation authority", "discovery", "capability", "page size", "timeout", "adapter"} {
		t.Run(mode, func(t *testing.T) {
			_, s, _, cfg := effectServiceFixture(t)
			switch mode {
			case "store":
				cfg.Recovery.Store = nil
			case "scan authority":
				cfg.Recovery.AuthorizeScan = nil
			case "operation authority":
				cfg.Recovery.Authorize = nil
			case "discovery":
				cfg.Discovery = nil
			case "capability":
				cfg.Recovery.Store = struct{ execution.EffectStore }{s}
			case "page size":
				cfg.PageSize = 257
			case "timeout":
				cfg.PageTimeout = time.Hour
			case "adapter":
				cfg.Recovery.Adapters = nil
			}
			service, err := coordination.NewEffectRecoveryService(cfg)
			require.Error(t, err)
			require.Nil(t, service)
		})
	}
}
