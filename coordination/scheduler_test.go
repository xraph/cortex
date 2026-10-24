package coordination_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type schedulerSourceFunc func(context.Context, id.ID) (planning.Plan, []planning.TaskState, error)

func (f schedulerSourceFunc) LoadSchedulingState(ctx context.Context, plan id.ID) (planning.Plan, []planning.TaskState, error) {
	return f(ctx, plan)
}

type schedulerResolverFunc func(context.Context, planning.Plan, id.ID) (coordination.ReadyTaskClaimRequest, error)

func (f schedulerResolverFunc) ResolveTaskClaim(ctx context.Context, plan planning.Plan, task id.ID) (coordination.ReadyTaskClaimRequest, error) {
	return f(ctx, plan, task)
}

type schedulerAdmitterFunc func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error)

func (f schedulerAdmitterFunc) Admit(ctx context.Context, req coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
	return f(ctx, req)
}

type schedulerWorkerFunc func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error

func (f schedulerWorkerFunc) RunClaimedTask(ctx context.Context, req coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim) error {
	return f(ctx, req, receipt)
}

type schedulerClock struct{ at time.Time }

func (c schedulerClock) Now() time.Time { return c.at }

func schedulerFixture(t *testing.T) (coordination.SchedulerConfig, planning.Plan, context.Context) {
	t.Helper()
	h, p, _, now := handoffFixture(t)
	cfg := coordination.SchedulerConfig{MaxConcurrent: 2, MaxPerScope: 1, MaxQueued: 8, TickTimeout: time.Minute, Clock: schedulerClock{now}}
	cfg.Source = schedulerSourceFunc(func(context.Context, id.ID) (planning.Plan, []planning.TaskState, error) { return p, nil, nil })
	cfg.Resolver = schedulerResolverFunc(func(_ context.Context, current planning.Plan, task id.ID) (coordination.ReadyTaskClaimRequest, error) {
		cr := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: current.ID, PlanRevision: current.Revision, PlanDigest: current.Digest, TaskID: task, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
		return coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: cr, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: current.ID, PlanDigest: current.Digest, Scope: current.Scope, TaskID: task, OwnerRef: cr.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{FanOut: 1}, Deadline: current.Budget.Deadline}}}, nil
	})
	cfg.Admitter = schedulerAdmitterFunc(func(_ context.Context, req coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		claim, err := coordination.AcquireClaim(nil, req.Acquisition.Claim, p, now)
		if err != nil {
			return coordination.ReadyTaskClaim{}, err
		}
		_, digest, err := req.Freeze()
		if err != nil {
			return coordination.ReadyTaskClaim{}, err
		}
		rr := req.Acquisition.Reservation
		_, rd, err := rr.Freeze()
		if err != nil {
			return coordination.ReadyTaskClaim{}, err
		}
		return coordination.ReadyTaskClaim{Fresh: true, RequestDigest: digest, Acquisition: coordination.BudgetedClaim{Claim: claim, Reservation: execution.Reservation{Request: rr, RequestDigest: rd, CreatedAt: now}}, Outbox: coordination.TaskClaimOutbox{EventID: id.New(coordination.PrefixTaskClaimedEvent), Type: "task_claimed", ClaimID: claim.ID, PlanID: p.ID, TaskID: claim.TaskID}}, nil
	})
	cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
		return nil
	})
	return cfg, p, cortex.WithScope(context.Background(), p.Scope)
}

func TestSchedulerRequiresFreshGuardedAdmission(t *testing.T) {
	for _, mode := range []string{"fresh", "historical", "lost_ack", "invalid_receipt", "cancelled_after_commit", "changed_request", "source_scope", "source_plan", "source_digest", "resolver_mutation"} {
		t.Run(mode, func(t *testing.T) {
			cfg, p, ctx := schedulerFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var calls atomic.Int32
			cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim) error {
				calls.Add(1)
				require.NoError(t, receipt.Validate(req))
				require.NoError(t, ctx.Err())
				return nil
			})
			source := cfg.Source
			cfg.Source = schedulerSourceFunc(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
				current, states, err := source.LoadSchedulingState(ctx, planID)
				switch mode {
				case "source_scope":
					current.Scope.Levels = []cortex.Level{{Key: "org", Value: "another"}}
				case "source_plan":
					current.ID = id.New(planning.PrefixPlan)
				case "source_digest":
					current.Digest = "changed"
				}
				return current, states, err
			})
			resolver := cfg.Resolver
			cfg.Resolver = schedulerResolverFunc(func(ctx context.Context, current planning.Plan, task id.ID) (coordination.ReadyTaskClaimRequest, error) {
				req, err := resolver.ResolveTaskClaim(ctx, current, task)
				switch mode {
				case "changed_request":
					req.Acquisition.Claim.PlanRevision++
				case "resolver_mutation":
					current.Scope.Levels[0].Value = "changed"
				}
				return req, err
			})
			admitter := cfg.Admitter
			cfg.Admitter = schedulerAdmitterFunc(func(ctx context.Context, req coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
				receipt, err := admitter.Admit(ctx, req)
				switch mode {
				case "historical":
					receipt.Fresh = false
				case "lost_ack":
					return receipt, errors.New("lost acknowledgement")
				case "invalid_receipt":
					receipt.RequestDigest = "changed"
				case "cancelled_after_commit":
					cancel()
				}
				return receipt, err
			})
			s, err := coordination.NewScheduler(cfg)
			require.NoError(t, err)
			err = s.Tick(ctx, p.ID)
			if mode == "fresh" {
				require.NoError(t, err)
				require.EqualValues(t, 1, calls.Load())
			} else {
				require.Error(t, err)
				require.Zero(t, calls.Load())
			}
			require.NoError(t, s.Shutdown(context.Background()))
		})
	}
}

func TestSchedulerReadyOrderAndFailedDependency(t *testing.T) {
	for _, mode := range []string{"ordered", "failed", "waiting", "verifying", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			cfg, p, ctx := schedulerFixture(t)
			task := p.Tasks[0]
			task.ID = id.New(planning.PrefixTask)
			task.Output.Name = "next"
			task.DependsOn = []id.ID{p.Tasks[0].ID}
			p.Tasks = append(p.Tasks, task)
			var err error
			p, err = planning.Seal(p, planning.Limits{})
			require.NoError(t, err)
			var states []planning.TaskState
			if mode != "ordered" && mode != "expired" {
				states = []planning.TaskState{{TaskID: p.Tasks[0].ID, PlanDigest: p.Digest, Status: planning.Status(mode)}}
			}
			if mode == "expired" {
				cfg.Clock = schedulerClock{p.Budget.Deadline}
			}
			cfg.Source = schedulerSourceFunc(func(context.Context, id.ID) (planning.Plan, []planning.TaskState, error) { return p, states, nil })
			var selected id.ID
			cfg.Resolver = schedulerResolverFunc(func(_ context.Context, _ planning.Plan, taskID id.ID) (coordination.ReadyTaskClaimRequest, error) {
				selected = taskID
				return coordination.ReadyTaskClaimRequest{}, errors.New("selected")
			})
			s, err := coordination.NewScheduler(cfg)
			require.NoError(t, err)
			err = s.Tick(ctx, p.ID)
			if mode == "ordered" {
				require.Error(t, err)
				require.Equal(t, p.Tasks[0].ID, selected)
			} else {
				require.NoError(t, err)
				require.True(t, selected.IsNil())
			}
			require.NoError(t, s.Shutdown(context.Background()))
		})
	}
}

func TestSchedulerQueueCancellationAndShutdown(t *testing.T) {
	cfg, p, ctx := schedulerFixture(t)
	cfg.MaxConcurrent = 1
	cfg.MaxQueued = 1
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, _ coordination.ReadyTaskClaim) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	s, err := coordination.NewScheduler(cfg)
	require.NoError(t, err)
	first := make(chan error, 1)
	go func() { first <- s.Tick(ctx, p.ID) }()
	<-started
	queuedCtx, cancel := context.WithCancel(ctx)
	second := make(chan error, 1)
	go func() { second <- s.Tick(queuedCtx, p.ID) }()
	require.Eventually(t, func() bool { return s.Status().Queued == 1 }, time.Second, time.Millisecond)
	require.ErrorIs(t, s.Tick(ctx, p.ID), coordination.ErrSchedulerFull)
	cancel()
	require.ErrorIs(t, <-second, context.Canceled)
	require.Eventually(t, func() bool { return s.Status().Queued == 0 }, time.Second, time.Millisecond)
	shutdownCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	require.ErrorIs(t, s.Shutdown(shutdownCtx), context.DeadlineExceeded)
	require.ErrorIs(t, s.Tick(ctx, p.ID), coordination.ErrSchedulerStopped)
	close(release)
	require.ErrorIs(t, <-first, context.Canceled)
	require.NoError(t, s.Shutdown(context.Background()))
	require.EqualValues(t, 1, calls.Load())
	require.Zero(t, s.Status().Active)
}

func TestSchedulerFairScopeQueueAndConcurrentBound(t *testing.T) {
	cfg, _, _ := schedulerFixture(t)
	cfg.MaxConcurrent = 1
	// The source blocks inside the admitted slot, so queue ordering is observable
	// without timers deciding which competing task reaches a provider first.
	entered := make(chan string, 8)
	release := make(chan struct{}, 8)
	cfg.Source = schedulerSourceFunc(func(ctx context.Context, _ id.ID) (planning.Plan, []planning.TaskState, error) {
		entered <- cortex.ScopeFromContext(ctx).Levels[0].Value
		select {
		case <-release:
		case <-ctx.Done():
			return planning.Plan{}, nil, ctx.Err()
		}
		return planning.Plan{}, nil, errors.New("observed")
	})
	s, err := coordination.NewScheduler(cfg)
	require.NoError(t, err)
	var wg sync.WaitGroup
	enqueue := func(scope string, wantQueued int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: scope}}})
			_ = s.Tick(ctx, id.New(planning.PrefixPlan))
		}()
		if wantQueued > 0 {
			require.Eventually(t, func() bool { return s.Status().Queued == wantQueued }, time.Second, time.Millisecond)
		}
	}
	enqueue("first", 0)
	require.Equal(t, "first", <-entered)
	enqueue("a", 1)
	enqueue("a", 2)
	enqueue("b", 3)
	enqueue("c", 4)
	for _, want := range []string{"a", "b", "c", "a"} {
		release <- struct{}{}
		require.Equal(t, want, <-entered)
		require.Equal(t, 1, s.Status().Active)
	}
	release <- struct{}{}
	wg.Wait()
	require.NoError(t, s.Shutdown(context.Background()))
}

func TestSchedulerPerScopeBoundRefreshAndWaitingRelease(t *testing.T) {
	cfg, p, ctx := schedulerFixture(t)
	cfg.MaxConcurrent = 2
	cfg.MaxPerScope = 1
	var loads atomic.Int32
	var waiting atomic.Bool
	source := cfg.Source
	cfg.Source = schedulerSourceFunc(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
		loads.Add(1)
		current, states, err := source.LoadSchedulingState(ctx, planID)
		if waiting.Load() {
			states = []planning.TaskState{{TaskID: p.Tasks[0].ID, PlanDigest: p.Digest, Status: planning.StatusWaiting}}
		}
		return current, states, err
	})
	started := make(chan struct{})
	release := make(chan struct{})
	var workers atomic.Int32
	cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
		workers.Add(1)
		close(started)
		<-release
		waiting.Store(true)
		return nil
	})
	s, err := coordination.NewScheduler(cfg)
	require.NoError(t, err)
	first := make(chan error, 1)
	go func() { first <- s.Tick(ctx, p.ID) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- s.Tick(ctx, p.ID) }()
	require.Eventually(t, func() bool { return s.Status().Queued == 1 }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, loads.Load())
	require.Equal(t, 1, s.Status().Active)
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	require.EqualValues(t, 2, loads.Load())
	require.EqualValues(t, 1, workers.Load())
	require.Zero(t, s.Status().Active)
	require.NoError(t, s.Shutdown(context.Background()))
}

func TestSchedulerCancellationDeadlineAndPanicRelease(t *testing.T) {
	for _, mode := range []string{"parent", "tick_deadline", "claim_expiry", "panic", "source_context_isolation"} {
		t.Run(mode, func(t *testing.T) {
			cfg, p, ctx := schedulerFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "tick_deadline" {
				cfg.TickTimeout = 20 * time.Millisecond
			}
			if mode == "claim_expiry" {
				resolver := cfg.Resolver
				cfg.Resolver = schedulerResolverFunc(func(ctx context.Context, p planning.Plan, task id.ID) (coordination.ReadyTaskClaimRequest, error) {
					req, err := resolver.ResolveTaskClaim(ctx, p, task)
					req.Acquisition.Claim.Duration = 20 * time.Millisecond
					return req, err
				})
			}
			if mode == "source_context_isolation" {
				source := cfg.Source
				cfg.Source = schedulerSourceFunc(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
					scope := cortex.ScopeFromContext(ctx)
					scope.Levels[0].Value = "changed"
					return source.LoadSchedulingState(ctx, planID)
				})
			}
			var calls atomic.Int32
			cfg.Worker = schedulerWorkerFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, _ coordination.ReadyTaskClaim) error {
				calls.Add(1)
				if mode == "source_context_isolation" {
					require.Equal(t, "one", cortex.ScopeFromContext(ctx).Levels[0].Value)
					return nil
				}
				if mode == "panic" {
					panic("worker panic")
				}
				if mode == "parent" {
					cancel()
				}
				<-ctx.Done()
				return nil // Even a worker hiding cancellation cannot report a clean tick.
			})
			s, err := coordination.NewScheduler(cfg)
			require.NoError(t, err)
			if mode == "panic" {
				require.Panics(t, func() { _ = s.Tick(ctx, p.ID) })
			} else {
				err = s.Tick(ctx, p.ID)
				switch mode {
				case "parent":
					require.ErrorIs(t, err, context.Canceled)
				case "source_context_isolation":
					require.NoError(t, err)
					require.EqualValues(t, 1, calls.Load())
				default:
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			}
			require.Zero(t, s.Status().Active)
			require.NoError(t, s.Shutdown(context.Background()))
		})
	}
}

func TestSchedulerRejectsMissingPortsAndUnboundedConfiguration(t *testing.T) {
	for _, mode := range []string{"source", "resolver", "admitter", "worker", "clock", "concurrency", "scope_limit", "queue", "timeout", "missing_scope", "invalid_plan", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			cfg, p, ctx := schedulerFixture(t)
			switch mode {
			case "source":
				cfg.Source = schedulerSourceFunc(nil)
			case "resolver":
				cfg.Resolver = schedulerResolverFunc(nil)
			case "admitter":
				cfg.Admitter = schedulerAdmitterFunc(nil)
			case "worker":
				cfg.Worker = schedulerWorkerFunc(nil)
			case "clock":
				cfg.Clock = (*schedulerClock)(nil)
			case "concurrency":
				cfg.MaxConcurrent = 257
			case "scope_limit":
				cfg.MaxPerScope = 3
			case "queue":
				cfg.MaxQueued = 4097
			case "timeout":
				cfg.TickTimeout = 31 * time.Minute
			case "missing_scope":
				ctx = context.Background()
			case "invalid_plan":
				p.ID = id.NewAgentRunID()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			s, err := coordination.NewScheduler(cfg)
			if mode == "missing_scope" || mode == "invalid_plan" || mode == "cancelled" {
				require.NoError(t, err)
				require.Error(t, s.Tick(ctx, p.ID))
				require.NoError(t, s.Shutdown(context.Background()))
			} else {
				require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
				require.Nil(t, s)
			}
		})
	}
}

func TestSchedulerShutdownDuringAdmissionCannotStartWorker(t *testing.T) {
	cfg, p, ctx := schedulerFixture(t)
	var scheduler *coordination.TaskScheduler
	admitter := cfg.Admitter
	cfg.Admitter = schedulerAdmitterFunc(func(ctx context.Context, req coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		receipt, err := admitter.Admit(ctx, req)
		shutdownCtx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, scheduler.Shutdown(shutdownCtx), context.Canceled)
		return receipt, err
	})
	var calls atomic.Int32
	cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
		calls.Add(1)
		return nil
	})
	var err error
	scheduler, err = coordination.NewScheduler(cfg)
	require.NoError(t, err)
	require.ErrorIs(t, scheduler.Tick(ctx, p.ID), coordination.ErrSchedulerReconciliationRequired)
	require.Zero(t, calls.Load())
	require.NoError(t, scheduler.Shutdown(context.Background()))
}
