package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/xraph/cortex/internal/hostport"
)

// TaskRenewalSource reads current parent, child, assignment and prerequisite
// revisions for the exact claim. It invokes no external work. Duration is the
// scheduler ceiling; the source may attenuate it to a tighter child deadline.
// Both this port and the renewer must honor cancellation.
type TaskRenewalSource interface {
	ResolveTaskRenewal(context.Context, Claim, time.Duration) (TaskRenewalRequest, error)
}

// TaskRenewalConfig enables renewal after confirmed child creation. Interval
// must be at least a millisecond and at most half Duration. Configure guarded
// renewal against the same store used for admission and dispatch.
type TaskRenewalConfig struct {
	Source   TaskRenewalSource
	Renewer  TaskClaimRenewer
	Duration time.Duration
	Interval time.Duration
}

// TaskRenewalStart activates renewal for the immediate dispatched successor of
// an admitted claim. Call exactly once, synchronously after confirmed fresh
// child creation and before external execution. It never authorizes a call.
type TaskRenewalStart func(Claim) error

// RenewingClaimedTaskWorker supports the child-creation activation boundary.
// The context's deadline is the overall execution ceiling. A watchdog cancels
// it at the last confirmed claim expiry. Individual calls retain their own
// authorization leases, which task renewal does not extend.
type RenewingClaimedTaskWorker interface {
	ClaimedTaskWorker
	RunClaimedTaskWithRenewal(context.Context, ReadyTaskClaimRequest, ReadyTaskClaim, TaskRenewalStart) error
}

func (c TaskRenewalConfig) validate(worker ClaimedTaskWorker) error {
	if hostport.IsNil(c.Source) || hostport.IsNil(c.Renewer) || c.Duration <= 0 || c.Duration > MaxClaimDuration || c.Interval < time.Millisecond || c.Interval > c.Duration/2 {
		return ErrInvalidScheduler
	}
	if _, ok := worker.(RenewingClaimedTaskWorker); !ok {
		return ErrInvalidScheduler
	}
	return nil
}

func detachClaim(c Claim) (Claim, error) {
	data, err := json.Marshal(c)
	if err != nil || len(data) > MaxClaimBytes {
		return Claim{}, ErrInvalidClaim
	}
	var result Claim
	if json.Unmarshal(data, &result) != nil || result.Validate() != nil {
		return Claim{}, ErrInvalidClaim
	}
	return result, nil
}

func (s *TaskScheduler) runRenewing(ctx context.Context, req ReadyTaskClaimRequest, receipt ReadyTaskClaim, ceiling time.Time) (err error) {
	initial, err := detachClaim(receipt.Acquisition.Claim)
	if err != nil {
		return err
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	workCtx = schedulerContext{Context: workCtx, stopped: s.ctx}
	expire := func() { cancel(errors.Join(ErrSchedulerReconciliationRequired, context.DeadlineExceeded)) }
	observed := s.config.Clock.Now()
	remaining := initial.ExpiresAt.Sub(observed)
	if observed.Before(initial.At) || remaining <= 0 {
		cancel(nil)
		return ErrSchedulerReconciliationRequired
	}
	watchdog := time.AfterFunc(remaining, expire)
	activated := make(chan Claim, 1)
	var started atomic.Bool
	start := func(c Claim) error {
		now := s.config.Clock.Now()
		frozen, freezeErr := detachClaim(c)
		if freezeErr != nil || frozen.State != ClaimDispatching || ValidateClaimSuccessor(initial, frozen) != nil || now.Before(frozen.At) || !now.Before(initial.ExpiresAt) || workCtx.Err() != nil || !started.CompareAndSwap(false, true) {
			cancel(ErrSchedulerReconciliationRequired)
			return ErrSchedulerReconciliationRequired
		}
		activated <- frozen
		return nil
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var renewalErr error
		defer func() {
			if recover() != nil {
				renewalErr = ErrSchedulerReconciliationRequired
				cancel(renewalErr)
			}
			done <- renewalErr
		}()
		renewalErr = s.renewTaskWhileRunning(workCtx, cancel, activated, watchdog, ceiling, stop)
	}()
	defer func() {
		close(stop)
		if !watchdog.Stop() {
			expire()
		}
		cause := context.Cause(workCtx)
		cancel(nil) // Cancel in-flight reads/writes before waiting for their return.
		renewalErr := <-done
		watchdog.Stop() // A returning renewal may have raced the first stop.
		err = errors.Join(err, renewalErr, cause)
	}()
	worker, ok := s.config.Worker.(RenewingClaimedTaskWorker)
	if !ok {
		return ErrInvalidScheduler
	}
	return worker.RunClaimedTaskWithRenewal(workCtx, req, receipt, start)
}

func (s *TaskScheduler) renewTaskWhileRunning(ctx context.Context, cancel context.CancelCauseFunc, activated <-chan Claim, watchdog *time.Timer, ceiling time.Time, stop <-chan struct{}) error {
	var current Claim
	select {
	case current = <-activated:
	case <-ctx.Done():
		return nil
	}
	policy := s.config.Renewal
	lastObserved := current.At
	fail := func(err error) error {
		err = errors.Join(ErrSchedulerReconciliationRequired, err)
		cancel(err)
		return err
	}
	for {
		now := s.config.Clock.Now()
		if now.Before(lastObserved) || !now.Before(current.ExpiresAt) {
			return fail(context.DeadlineExceeded)
		}
		lastObserved = now
		// At the final ceiling, let the watchdog end ownership without a futile write.
		if !current.ExpiresAt.Before(ceiling) {
			<-ctx.Done()
			return nil
		}
		delay := current.ExpiresAt.Add(-policy.Duration).Add(policy.Interval).Sub(now)
		if delay < policy.Interval {
			delay = policy.Interval
		}
		if remaining := current.ExpiresAt.Sub(now); delay >= remaining {
			delay = remaining / 2
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		now = s.config.Clock.Now()
		if now.Before(lastObserved) || !now.Before(current.ExpiresAt) {
			return fail(context.DeadlineExceeded)
		}
		lastObserved = now
		duration := min(policy.Duration, ceiling.Sub(now))
		offered, err := detachClaim(current)
		if err != nil {
			return fail(err)
		}
		request, err := policy.Source.ResolveTaskRenewal(ctx, offered, duration)
		if err == nil {
			request, _, err = request.Freeze()
			if err == nil && (request.Claim.Digest != current.Digest || request.Duration > duration) {
				err = ErrInvalidClaim
			}
		}
		var receipt TaskRenewal
		if err == nil && ctx.Err() == nil {
			// The resolver receives its own copy; renewal cannot mutate validation input.
			input, _, freezeErr := request.Freeze()
			if freezeErr != nil {
				err = freezeErr
			} else {
				receipt, err = policy.Renewer.Renew(ctx, input)
			}
		}
		select {
		case <-stop:
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
		default:
		}
		if err != nil {
			return fail(err)
		}
		finished := s.config.Clock.Now()
		if ctx.Err() != nil || finished.Before(lastObserved) || !finished.Before(current.ExpiresAt) || !receipt.Fresh || receipt.Validate(request) != nil || receipt.Claim.At.Before(now) || receipt.Claim.At.After(finished) || receipt.Claim.ExpiresAt.After(ceiling) {
			return fail(ErrInvalidClaim)
		}
		next, err := detachClaim(receipt.Claim)
		if err != nil {
			return fail(err)
		}
		if !watchdog.Stop() {
			return fail(context.DeadlineExceeded)
		}
		watchdog.Reset(next.ExpiresAt.Sub(finished))
		current, lastObserved = next, finished
	}
}
