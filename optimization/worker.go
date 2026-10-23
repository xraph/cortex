package optimization

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/id"
)

// WorkerConfig bounds a serial worker's lease, renewal and idle polling.
type WorkerConfig struct{ LeaseDuration, HeartbeatInterval, PollInterval time.Duration }

// DefaultWorkerConfig uses a thirty-second lease with ten-second renewal.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{LeaseDuration: 30 * time.Second, HeartbeatInterval: 10 * time.Second, PollInterval: time.Second}
}
func (c WorkerConfig) validate() error {
	if c.LeaseDuration <= 0 || c.LeaseDuration > 5*time.Minute || c.HeartbeatInterval <= 0 || c.HeartbeatInterval >= c.LeaseDuration/2 || c.PollInterval <= 0 || c.PollInterval > time.Minute {
		return ErrInvalidSpec
	}
	return nil
}

// WorkItem contains a frozen experiment snapshot and current persistence fence.
type WorkItem struct {
	Experiment Experiment
	Lease      WorkLease
}

// PhaseHandler executes one bounded phase step. It must honor cancellation and
// reconcile prior attempts before effects. The worker supplies transition authority.
type PhaseHandler interface {
	Execute(context.Context, WorkItem) (Transition, error)
}

// Work polls bounded pages in the caller's exact scope. It owns one active phase
// and one heartbeat goroutine; the host owns scope enumeration and supervision.
func (o *Optimizer) Work(ctx context.Context) error {
	if _, err := o.caller(ctx); err != nil {
		return err
	}
	for {
		for _, state := range []State{StateQueued, StateRunning} {
			query := ListQuery{Limit: 100, State: state}
			for {
				page, err := o.List(ctx, query)
				if err != nil {
					return err
				}
				for _, experiment := range page.Items {
					if err = o.WorkOnce(ctx, experiment.ID); err != nil && !errors.Is(err, ErrConflict) {
						return err
					}
				}
				if page.NextCursor == "" {
					break
				}
				query.Cursor = page.NextCursor
			}
		}
		timer := time.NewTimer(o.config.Worker.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// WorkOnce claims one experiment, renews authority while executing one phase,
// and commits its transition. Failed or interrupted phases retain durable state.
func (o *Optimizer) WorkOnce(ctx context.Context, experimentID id.ID) error {
	if !o.work.TryLock() {
		return ErrConflict
	}
	defer o.work.Unlock()
	resolved, cleanup, err := o.meter.authorizeAction(ctx, experimentID, ActionStart)
	if err != nil {
		return err
	}
	defer cleanup()
	prior, err := o.config.Repository.GetExperiment(resolved, experimentID)
	if err != nil {
		return err
	}
	if prior.State == StateRunning && time.Now().UTC().Before(prior.Lease.ExpiresAt) && prior.Lease.Owner != o.owner {
		return ErrConflict
	}
	if prior.State == StateRunning && (prior.Lease.Owner != o.owner || !time.Now().UTC().Before(prior.Lease.ExpiresAt)) || prior.State == StateQueued && prior.Lease.Generation > 0 {
		if recoveryErr := o.reconcileUsage(ctx, prior); recoveryErr != nil {
			return recoveryErr
		}
	}
	var lease WorkLease
	if transactionErr := o.config.Repository.Transact(resolved, func(tx Transaction) error {
		var claimErr error
		lease, claimErr = tx.ClaimWork(ClaimRequest{ExperimentID: experimentID, Owner: o.owner, Duration: o.config.Worker.LeaseDuration})
		return claimErr
	}); transactionErr != nil {
		return transactionErr
	}
	experiment, err := o.config.Repository.GetExperiment(resolved, experimentID)
	if err != nil {
		return err
	}
	// Keep the caller identity for repeated checks. A requester-only context
	// would hide revocation of the calling worker during execution.
	phaseCtx, cancel := context.WithDeadline(ctx, experiment.Deadline)
	defer cancel()
	heartbeat := make(chan error, 1)
	go func() { heartbeat <- o.heartbeat(phaseCtx, cancel, experiment, lease) }()
	change, runErr := o.config.Handler.Execute(phaseCtx, WorkItem{Experiment: experiment, Lease: lease})
	phaseErr := phaseCtx.Err()
	cancel()
	renewErr := <-heartbeat
	if phaseErr != nil || renewErr != nil || (runErr != nil && !errors.Is(runErr, ErrBudgetExceeded)) {
		return errors.Join(runErr, phaseErr, renewErr)
	}
	current, done, err := o.meter.authorizeAction(ctx, experimentID, ActionStart)
	if err != nil {
		return err
	}
	defer done()
	if errors.Is(runErr, ErrBudgetExceeded) {
		change = Transition{NextState: StateBudgetExhausted}
	}
	change.ExperimentID, change.ExpectedRevision, change.Lease, change.Actor = experimentID, experiment.Revision, lease, experiment.Spec.Requester
	change.IdempotencyKey = fmt.Sprintf("worker/%d/%d", lease.Generation, experiment.Revision)
	commitErr := o.config.Repository.Transact(current, func(tx Transaction) error { return tx.CommitTransition(change) })
	return errors.Join(runErr, commitErr)
}

func (o *Optimizer) reconcileUsage(ctx context.Context, experiment Experiment) error {
	actor, err := o.caller(ctx)
	if err != nil {
		return err
	}
	query := PageQuery{Limit: 100}
	for {
		page, listErr := o.config.Repository.ListReservations(ctx, experiment.ID, query)
		if listErr != nil {
			return listErr
		}
		for _, reservation := range page.Items {
			if reservation.Call == nil || reservation.State == ReservationSettled {
				continue
			}
			if o.config.UsageRecovery == nil {
				return ErrUnknownAttempt
			}
			recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = o.config.UsageRecovery.Recover(recoveryCtx, experiment.ID, reservation.Request.CallKey, actor)
			cancel()
			if err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		query.Cursor = page.NextCursor
	}
}

func (o *Optimizer) heartbeat(ctx context.Context, cancel context.CancelFunc, experiment Experiment, lease WorkLease) error {
	ticker := time.NewTicker(o.config.Worker.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resolved, done, err := o.meter.authorizeAction(ctx, experiment.ID, ActionStart)
			if err != nil {
				cancel()
				return err
			}
			lease.ExpiresAt = time.Now().UTC().Add(o.config.Worker.LeaseDuration)
			if lease.ExpiresAt.After(experiment.Deadline) {
				lease.ExpiresAt = experiment.Deadline
			}
			err = o.config.Repository.Transact(resolved, func(tx Transaction) error { return tx.RenewLease(lease) })
			done()
			if err != nil {
				cancel()
				return err
			}
		}
	}
}
