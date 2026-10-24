package coordination_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type taskPlanSource func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error)

func (f taskPlanSource) ListScheduledPlans(ctx context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
	return f(ctx, r)
}

func TestTaskServiceOwnsLifecycleAndPreservesContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx := schedulerFixture(t)
		type identityKey struct{}
		ctx, cancel := context.WithCancel(context.WithValue(ctx, identityKey{}, "service-principal"))
		defer cancel()
		var calls atomic.Int32
		cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, _ coordination.ReadyTaskClaim) error {
			require.Equal(t, "service-principal", ctx.Value(identityKey{}))
			require.Equal(t, p.Scope, cortex.ScopeFromContext(ctx))
			calls.Add(1)
			return nil
		})
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, Discovery: taskPlanSource(func(ctx context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			require.Equal(t, "service-principal", ctx.Value(identityKey{}))
			require.Positive(t, r.Limit)
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: p.ID}}}, nil
		}), Interval: time.Second})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		require.NoError(t, service.Start(ctx))
		cancel() // Startup request cancellation does not terminate the service.
		synctest.Wait()
		require.Equal(t, int32(1), calls.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(2), calls.Load())
		require.NoError(t, service.Stop(context.Background()))
		require.False(t, service.Status().Running)
		before := calls.Load()
		time.Sleep(2 * time.Second)
		require.Equal(t, before, calls.Load())
		require.NoError(t, service.Start(context.WithValue(context.Background(), identityKey{}, "service-principal")))
		synctest.Wait()
		require.Equal(t, before+1, calls.Load())
		require.NoError(t, service.Stop(context.Background()))
	})
}

func TestTaskServiceBoundsDiscoveryAndDrainsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx := schedulerFixture(t)
		release := make(chan struct{})
		entered := make(chan struct{}, 2)
		cfg.MaxConcurrent, cfg.MaxPerScope = 2, 2
		cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
			entered <- struct{}{}
			<-release
			return nil
		})
		var reads atomic.Int32
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, MaxInFlight: 1, Interval: time.Second, Discovery: taskPlanSource(func(_ context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			reads.Add(1)
			require.Equal(t, 1, r.Limit)
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: p.ID}}}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		<-entered
		time.Sleep(3 * time.Second)
		require.Equal(t, int32(1), reads.Load())
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stopCtx), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Equal(t, 1, service.Status().Active)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		close(release)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
	})
}

func TestTaskServiceRejectsMalformedPagesAndRetriesReads(t *testing.T) {
	for _, mode := range []string{"read_error", "scope", "plan", "duplicate", "oversize", "cursor", "panic", "invalid_utf8", "scope_bytes", "scope_depth"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, p, ctx := schedulerFixture(t)
				var workers, reads, stateReads atomic.Int32
				originalSource := cfg.Source
				cfg.Source = schedulerSourceFunc(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
					stateReads.Add(1)
					return originalSource.LoadSchedulingState(ctx, planID)
				})
				cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
					workers.Add(1)
					return nil
				})
				service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, MaxInFlight: 1, Interval: time.Second, Discovery: taskPlanSource(func(_ context.Context, _ coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
					attempt := reads.Add(1)
					page := coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: p.ID}}}
					if attempt > 1 {
						return page, nil
					}
					switch mode {
					case "read_error":
						return page, errors.New("temporary source failure")
					case "scope":
						page.Plans[0].Scope = cortex.Scope{}
					case "plan":
						page.Plans[0].PlanID = id.NewAgentRunID()
					case "duplicate":
						page.Plans = append(page.Plans, page.Plans[0])
					case "oversize":
						page.Next = string(make([]byte, 4097))
					case "cursor":
						page.Next = string([]byte{0xff})
					case "panic":
						panic("source panic")
					case "invalid_utf8", "scope_bytes", "scope_depth":
						page.Plans[0].Scope.Levels = append([]cortex.Level(nil), p.Scope.Levels...)
						page.Plans[0].Scope.Levels[0].Value = string([]byte{0xff})
						if mode == "scope_bytes" {
							page.Plans[0].Scope.Levels[0].Value = strings.Repeat("x", 2<<20)
						}
						if mode == "scope_depth" {
							page.Plans[0].Scope.Levels = make([]cortex.Level, 1<<17)
						}
					}
					return page, nil
				})})
				require.NoError(t, err)
				require.NoError(t, service.Start(ctx))
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				synctest.Wait()
				require.Zero(t, workers.Load())
				require.Zero(t, stateReads.Load())
				require.Equal(t, uint64(1), service.Status().Failures)
				time.Sleep(time.Second)
				synctest.Wait()
				require.Equal(t, int32(1), workers.Load())
				require.NoError(t, service.Stop(context.Background()))
			})
		})
	}
}

func TestTaskServiceCancelsDiscoveryBeforeJoining(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, _, ctx := schedulerFixture(t)
		entered, returned := make(chan struct{}), make(chan struct{})
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, Discovery: taskPlanSource(func(ctx context.Context, _ coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(entered)
			defer close(returned)
			<-ctx.Done()
			return coordination.TaskPlanPage{}, ctx.Err()
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		<-entered
		require.NoError(t, service.Stop(context.Background()))
		select {
		case <-returned:
		default:
			t.Fatal("discovery survived shutdown")
		}
	})
}

func TestTaskServiceRequiresBoundedExplicitConfiguration(t *testing.T) {
	for _, mode := range []string{"discovery", "interval", "timeout", "capacity", "scheduler"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _ := schedulerFixture(t)
			c := coordination.TaskServiceConfig{Scheduler: cfg, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
				return coordination.TaskPlanPage{}, nil
			})}
			switch mode {
			case "discovery":
				c.Discovery = taskPlanSource(nil)
			case "interval":
				c.Interval = time.Nanosecond
			case "timeout":
				c.DiscoveryTimeout = time.Hour
			case "capacity":
				c.MaxInFlight = 4097
			case "scheduler":
				c.Scheduler.Worker = nil
			}
			_, err := coordination.NewTaskService(c)
			require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
		})
	}
}

func TestTaskServicePagesWithoutDuplicatingActivePlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx := schedulerFixture(t)
		var workers atomic.Int32
		cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, _ coordination.ReadyTaskClaim) error {
			workers.Add(1)
			<-ctx.Done()
			return ctx.Err()
		})
		cursors := make(chan string, 8)
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, MaxInFlight: 2, Interval: time.Second, Discovery: taskPlanSource(func(_ context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			cursors <- r.After
			next := "second"
			if r.After != "" {
				next = ""
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: p.ID}}, Next: next}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		for _, expected := range []string{"", "second", "", "second"} {
			require.Equal(t, expected, <-cursors)
			time.Sleep(time.Second)
			synctest.Wait()
		}
		require.Equal(t, int32(1), workers.Load())
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Failures)
	})
}

func TestTaskServiceKeepsDiscoveryCapacityForOtherScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx := schedulerFixture(t)
		cfg.MaxConcurrent, cfg.MaxPerScope = 2, 1
		secondID, otherID := id.New(p.ID.Prefix()), id.New(p.ID.Prefix())
		otherScope := cortex.Scope{Levels: append([]cortex.Level(nil), p.Scope.Levels...)}
		otherScope.Levels[0].Value = "other-tenant"
		seenOther := make(chan struct{}, 1)
		source := cfg.Source
		cfg.Source = schedulerSourceFunc(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
			if planID == otherID {
				select {
				case seenOther <- struct{}{}:
				default:
				}
				return planning.Plan{}, nil, errors.New("observation finished")
			}
			return source.LoadSchedulingState(ctx, planID)
		})
		cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, _ coordination.ReadyTaskClaim) error {
			<-ctx.Done()
			return ctx.Err()
		})
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, MaxInFlight: 2, Interval: time.Second, Discovery: taskPlanSource(func(_ context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if r.After == "other" {
				return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: otherScope, PlanID: otherID}}}, nil
			}
			if r.After == "more" {
				return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: secondID}}, Next: "other"}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: p.ID}}, Next: "more"}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(context.Background())) }()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-seenOther:
		default:
			t.Fatal("busy scope consumed discovery capacity and starved another scope")
		}
	})
}

func TestTaskServiceRotatesSameScopePlansAcrossPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx := schedulerFixture(t)
		plans := []id.ID{p.ID, id.New(p.ID.Prefix()), id.New(p.ID.Prefix()), id.New(p.ID.Prefix())}
		seen := make(chan id.ID, 16)
		cfg.Source = schedulerSourceFunc(func(_ context.Context, plan id.ID) (planning.Plan, []planning.TaskState, error) {
			seen <- plan
			return planning.Plan{}, nil, errors.New("observation complete")
		})
		service, err := coordination.NewTaskService(coordination.TaskServiceConfig{Scheduler: cfg, MaxInFlight: 2, Interval: time.Second, Discovery: taskPlanSource(func(_ context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			first, next := 0, "second"
			if r.After == "second" {
				first, next = 2, ""
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: p.Scope, PlanID: plans[first]}, {Scope: p.Scope, PlanID: plans[first+1]}}, Next: next}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		defer func() { require.NoError(t, service.Stop(context.Background())) }()
		synctest.Wait()
		for range 3 {
			time.Sleep(time.Second)
			synctest.Wait()
		}
		counts := map[id.ID]int{}
		for len(seen) > 0 {
			counts[<-seen]++
		}
		for _, plan := range plans {
			require.Positive(t, counts[plan], "a stable discovery page starved a same-scope plan")
		}
	})
}
