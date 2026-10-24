package coordination_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/planning"
)

type movingSchedulerClock struct{ base, started time.Time }

func (c movingSchedulerClock) Now() time.Time { return c.base.Add(time.Since(c.started)) }

type schedulerRenewalSource func(context.Context, coordination.Claim, time.Duration) (coordination.TaskRenewalRequest, error)

func (f schedulerRenewalSource) ResolveTaskRenewal(ctx context.Context, c coordination.Claim, d time.Duration) (coordination.TaskRenewalRequest, error) {
	return f(ctx, c, d)
}

type schedulerRenewer func(context.Context, coordination.TaskRenewalRequest) (coordination.TaskRenewal, error)

func (f schedulerRenewer) Renew(ctx context.Context, r coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
	return f(ctx, r)
}

type renewingSchedulerWorker func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim, coordination.TaskRenewalStart) error

func (f renewingSchedulerWorker) RunClaimedTask(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
	return errors.New("renewal entry point required")
}
func (f renewingSchedulerWorker) RunClaimedTaskWithRenewal(ctx context.Context, r coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim, start coordination.TaskRenewalStart) error {
	return f(ctx, r, receipt, start)
}

func schedulerRenewalFixture(t *testing.T) (coordination.SchedulerConfig, planning.Plan, context.Context, *atomic.Int32) {
	t.Helper()
	cfg, p, ctx := schedulerFixture(t)
	base := cfg.Clock.Now()
	cfg.Clock = movingSchedulerClock{base: base, started: time.Now()}
	cfg.TickTimeout = 5 * time.Minute
	template, receipt := renewalFixture(t)
	calls := new(atomic.Int32)
	cfg.Renewal = &coordination.TaskRenewalConfig{Duration: time.Minute, Interval: 20 * time.Second,
		Source: schedulerRenewalSource(func(_ context.Context, c coordination.Claim, duration time.Duration) (coordination.TaskRenewalRequest, error) {
			req, _, err := template.Freeze()
			req.Claim, req.Actor, req.Duration = c, c.Worker.Principal, duration
			return req, err
		}),
		Renewer: schedulerRenewer(func(_ context.Context, req coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
			calls.Add(1)
			next, err := coordination.RenewClaim(req.Claim, req.Claim.Token(), req.Duration, cfg.Clock.Now())
			if err != nil {
				return coordination.TaskRenewal{}, err
			}
			result := receipt
			_, result.RequestDigest, err = req.Freeze()
			result.Claim = next
			result.Outbox.ClaimID, result.Outbox.PlanID, result.Outbox.TaskID, result.Outbox.RunID = next.ID, next.PlanID, next.TaskID, next.RunID
			return result, err
		}),
	}
	cfg.Worker = renewingSchedulerWorker(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim, start coordination.TaskRenewalStart) error {
		next, err := coordination.BeginDispatch(receipt.Acquisition.Claim, receipt.Acquisition.Claim.Token(), template.Claim.RunID, cfg.Clock.Now())
		if err != nil {
			return err
		}
		if err = start(next); err != nil {
			return err
		}
		select {
		case <-time.After(150 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return cfg, p, ctx, calls
}

func TestSchedulerRenewsOnlyConfirmedExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx, calls := schedulerRenewalFixture(t)
		s, err := coordination.NewScheduler(cfg)
		require.NoError(t, err)
		require.NoError(t, s.Tick(ctx, p.ID))
		require.GreaterOrEqual(t, calls.Load(), int32(5))
		before := calls.Load()
		time.Sleep(time.Minute)
		require.Equal(t, before, calls.Load())
		require.Zero(t, s.Status().Active)
	})
}

func TestSchedulerRenewalStopsOnUncertainOwnership(t *testing.T) {
	for _, mode := range []string{"history", "lost_ack", "invalid", "denied", "changed_claim", "late", "stalled", "port_panic", "clock_backwards", "missing_activation", "double_activation", "wrong_activation"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, p, ctx, calls := schedulerRenewalFixture(t)
				source, renewer, worker := cfg.Renewal.Source, cfg.Renewal.Renewer, cfg.Worker.(renewingSchedulerWorker)
				cfg.Renewal.Source = schedulerRenewalSource(func(ctx context.Context, c coordination.Claim, d time.Duration) (coordination.TaskRenewalRequest, error) {
					req, err := source.ResolveTaskRenewal(ctx, c, d)
					if mode == "changed_claim" {
						req.Claim.Digest = "changed"
					}
					if mode == "denied" {
						return req, errors.New("revoked")
					}
					return req, err
				})
				cfg.Renewal.Renewer = schedulerRenewer(func(ctx context.Context, req coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
					if mode == "port_panic" {
						panic("host renewal panic")
					}
					if mode == "stalled" {
						<-ctx.Done()
						return coordination.TaskRenewal{}, ctx.Err()
					}
					result, err := renewer.Renew(ctx, req)
					switch mode {
					case "history":
						result.Fresh = false
					case "lost_ack":
						err = errors.New("lost acknowledgement")
					case "invalid":
						result.RequestDigest = "changed"
					case "late":
						time.Sleep(time.Minute)
					}
					return result, err
				})
				// The clock port is captured by value; change its observation through a wrapper.
				originalClock := cfg.Clock
				if mode == "clock_backwards" {
					cfg.Clock = renewalClockFunc(func() time.Time {
						if calls.Load() > 0 {
							return originalClock.Now().Add(-time.Hour)
						}
						return originalClock.Now()
					})
				}
				cfg.Worker = renewingSchedulerWorker(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, r coordination.ReadyTaskClaim, start coordination.TaskRenewalStart) error {
					if mode == "missing_activation" {
						<-ctx.Done()
						return ctx.Err()
					}
					return worker(ctx, req, r, func(c coordination.Claim) error {
						if mode == "wrong_activation" {
							c.Digest = "changed"
						}
						err := start(c)
						if mode == "double_activation" {
							return errors.Join(err, start(c))
						}
						return err
					})
				})
				s, err := coordination.NewScheduler(cfg)
				require.NoError(t, err)
				before := time.Now()
				require.Error(t, s.Tick(ctx, p.ID))
				require.LessOrEqual(t, time.Since(before), 80*time.Second)
				require.LessOrEqual(t, calls.Load(), int32(1))
				require.Zero(t, s.Status().Active)
			})
		})
	}
}

type renewalClockFunc func() time.Time

func (f renewalClockFunc) Now() time.Time { return f() }

func TestSchedulerRenewalRequiresExplicitPortsAndBounds(t *testing.T) {
	for _, mode := range []string{"source", "renewer", "worker", "duration", "interval", "too_frequent"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _, _ := schedulerRenewalFixture(t)
			switch mode {
			case "source":
				cfg.Renewal.Source = schedulerRenewalSource(nil)
			case "renewer":
				cfg.Renewal.Renewer = schedulerRenewer(nil)
			case "worker":
				cfg.Worker = schedulerWorkerFunc(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
					return nil
				})
			case "duration":
				cfg.Renewal.Duration = coordination.MaxClaimDuration + time.Second
			case "interval":
				cfg.Renewal.Interval = cfg.Renewal.Duration
			case "too_frequent":
				cfg.Renewal.Interval = time.Nanosecond
			}
			_, err := coordination.NewScheduler(cfg)
			require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
		})
	}
}

func TestSchedulerRenewalDrainsOnWorkerReturnAndShutdown(t *testing.T) {
	for _, mode := range []string{"return", "panic", "shutdown", "caller_cancel", "stalled_source"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, p, ctx, _ := schedulerRenewalFixture(t)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				entered, returned := make(chan struct{}), make(chan struct{})
				source := cfg.Renewal.Source
				cfg.Renewal.Source = schedulerRenewalSource(func(ctx context.Context, c coordination.Claim, d time.Duration) (coordination.TaskRenewalRequest, error) {
					close(entered)
					defer close(returned)
					<-ctx.Done()
					return source.ResolveTaskRenewal(ctx, c, d)
				})
				original := cfg.Worker.(renewingSchedulerWorker)
				cfg.Worker = renewingSchedulerWorker(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, r coordination.ReadyTaskClaim, start coordination.TaskRenewalStart) error {
					return original(ctx, req, r, func(c coordination.Claim) error {
						if startErr := start(c); startErr != nil {
							return startErr
						}
						<-entered
						switch mode {
						case "return":
							return errors.New("worker finished")
						case "panic":
							panic("worker panic")
						default:
							<-ctx.Done()
							return ctx.Err()
						}
					})
				})
				s, err := coordination.NewScheduler(cfg)
				require.NoError(t, err)
				done := make(chan error, 1)
				go func() {
					defer func() {
						if recover() != nil {
							done <- errors.New("recovered worker panic")
						}
					}()
					done <- s.Tick(ctx, p.ID)
				}()
				<-entered
				if mode == "caller_cancel" {
					cancel()
				}
				if mode == "shutdown" {
					require.NoError(t, s.Shutdown(context.Background()))
				}
				require.Error(t, <-done)
				select {
				case <-returned:
				default:
					t.Fatal("renewal still running after tick returned")
				}
				require.Zero(t, s.Status().Active)
			})
		})
	}
}

func TestSchedulerRenewalWaitsForActivationAndRespectsCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, p, ctx, calls := schedulerRenewalFixture(t)
		cfg.TickTimeout = 90 * time.Second
		original := cfg.Worker.(renewingSchedulerWorker)
		cfg.Worker = renewingSchedulerWorker(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, r coordination.ReadyTaskClaim, start coordination.TaskRenewalStart) error {
			time.Sleep(30 * time.Second)
			require.Zero(t, calls.Load())
			return original(ctx, req, r, start)
		})
		s, err := coordination.NewScheduler(cfg)
		require.NoError(t, err)
		before := time.Now()
		require.Error(t, s.Tick(ctx, p.ID))
		require.Equal(t, 90*time.Second, time.Since(before))
		require.Positive(t, calls.Load())
	})
}

func TestSchedulerRenewalRejectsClockRollbackBeforeWorker(t *testing.T) {
	cfg, p, ctx, _ := schedulerRenewalFixture(t)
	initial := cfg.Clock.Now()
	var admitted atomic.Bool
	cfg.Clock = renewalClockFunc(func() time.Time {
		if admitted.Load() {
			return initial.Add(-time.Second)
		}
		return initial
	})
	admitter := cfg.Admitter
	cfg.Admitter = schedulerAdmitterFunc(func(ctx context.Context, req coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		receipt, err := admitter.Admit(ctx, req)
		admitted.Store(true)
		return receipt, err
	})
	calls := 0
	cfg.Worker = renewingSchedulerWorker(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim, coordination.TaskRenewalStart) error {
		calls++
		return nil
	})
	s, err := coordination.NewScheduler(cfg)
	require.NoError(t, err)
	require.Error(t, s.Tick(ctx, p.ID))
	require.Zero(t, calls)
}
