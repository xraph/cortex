package verification

import (
	"context"
	"errors"
	"time"
)

// LeaseRenewalPolicy keeps evaluator callbacks owned within the job deadline.
// Interval must be at most half Duration. Host ports must honor cancellation
// and support concurrent authorization, reads and renewal during callbacks.
type LeaseRenewalPolicy struct {
	Duration time.Duration
	Interval time.Duration
}

// LeaseRenewer is optional unless automatic evaluator renewal is configured.
// Renewal must atomically preserve the live claim and ownership generation.
type LeaseRenewer interface {
	RenewVerificationJob(context.Context, JobLease, time.Duration) (Job, error)
}

func (p LeaseRenewalPolicy) validate() error {
	if p.Duration <= 0 || p.Duration > MaxJobLease || p.Interval <= 0 || p.Interval > p.Duration/2 {
		return ErrInvalid
	}
	return nil
}

// withLeaseRenewal supervises only the host callback. Storage transitions retain
// their own live-lease fences. No renewal goroutine survives the callback return.
func (e *Executor) withLeaseRenewal(ctx context.Context, lease JobLease, job Job, callback func(context.Context) error) (err error) {
	if e.config.LeaseRenewal == nil {
		return callback(ctx)
	}
	current, err := e.load(ctx, lease, job.RequestDigest)
	if err != nil {
		return err
	}
	initialCtx, initialCancel := context.WithDeadline(ctx, current.Lease.ExpiresAt)
	current, err = e.renewLease(initialCtx, lease, current)
	initialCancel()
	if err != nil {
		return err
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	watchdog := time.AfterFunc(time.Until(current.Lease.ExpiresAt), func() { cancel(ErrJobExpired) })
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- e.renewWhileRunning(workCtx, cancel, lease, current, watchdog, stop) }()
	defer func() {
		close(stop)
		renewalErr := <-done
		if !watchdog.Stop() {
			cancel(ErrJobExpired)
		}
		cause := context.Cause(workCtx)
		cancel(nil)
		err = errors.Join(err, renewalErr, cause)
	}()
	return callback(workCtx)
}

func (e *Executor) renewWhileRunning(ctx context.Context, cancel context.CancelCauseFunc, lease JobLease, current Job, watchdog *time.Timer, stop <-chan struct{}) error {
	ticker := time.NewTicker(e.config.LeaseRenewal.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
		select {
		case <-stop:
			return nil
		default:
		}
		renewalCtx, renewalCancel := context.WithDeadline(ctx, current.Lease.ExpiresAt)
		next, err := e.renewLease(renewalCtx, lease, current)
		renewalCancel()
		if err != nil {
			cancel(err)
			return err
		}
		if !watchdog.Stop() {
			cancel(ErrJobExpired)
			return ErrJobExpired
		}
		watchdog.Reset(time.Until(next.Lease.ExpiresAt))
		current = next
	}
}

func (e *Executor) renewLease(ctx context.Context, lease JobLease, current Job) (Job, error) {
	if err := e.authorizeLease(ctx, current, lease); err != nil {
		return Job{}, err
	}
	storage, ok := e.config.Store.(LeaseRenewer)
	if !ok {
		return Job{}, ErrInvalid
	}
	renewed, err := storage.RenewVerificationJob(ctx, lease, e.config.LeaseRenewal.Duration)
	if err != nil {
		return Job{}, err
	}
	if renewed.Validate() != nil || renewed.RequestDigest != current.RequestDigest || renewed.Request.ID != current.Request.ID || renewed.Lease == nil || renewed.Lease.ExpiresAt.Before(current.Lease.ExpiresAt) {
		return Job{}, ErrInvalid
	}
	if leaseErr := renewed.CheckLease(lease, e.config.Clock.Now().UTC()); leaseErr != nil {
		return Job{}, leaseErr
	}
	if authErr := e.authorize(ctx, renewed.Request.Binding); authErr != nil {
		return Job{}, authErr
	}
	return SealJob(renewed)
}
