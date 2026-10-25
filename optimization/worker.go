package optimization

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex/id"
)

// WorkerConfig bounds an experiment worker's lease, renewal and idle polling.
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

// Work polls top-level experiments in the caller's exact scope. Parent handlers
// schedule their children through WorkOnce. It owns one active phase
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
					if experiment.Spec.Parent != nil {
						continue
					}
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
// and commits its transition. A failed parallel batch can commit successful
// siblings while returning its error; interrupted phases retain durable markers.
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
	parentChanged := false
	if prior.Spec.Parent != nil {
		parent, parentErr := o.config.Repository.GetExperiment(resolved, prior.Spec.Parent.ExperimentID)
		if parentErr != nil {
			return parentErr
		}
		parentChanged = parent.Lease.Generation != prior.Lease.ParentGeneration
	}
	if !parentChanged && prior.State == StateRunning && time.Now().UTC().Before(prior.Lease.ExpiresAt) && prior.Lease.Owner != o.owner {
		return ErrConflict
	}
	if parentChanged || prior.State == StateRunning && (prior.Lease.Owner != o.owner || !time.Now().UTC().Before(prior.Lease.ExpiresAt)) || prior.State == StateQueued && prior.Lease.Generation > 0 {
		if recoveryErr := o.reconcileUsage(ctx, prior); recoveryErr != nil {
			return recoveryErr
		}
	}
	var lease WorkLease
	if transactionErr := o.config.Repository.Transact(resolved, func(tx Transaction) error {
		var claimErr error
		lease, claimErr = tx.ClaimWork(ClaimRequest{ExperimentID: experimentID, Owner: o.owner, Duration: o.config.Worker.LeaseDuration})
		if claimErr != nil {
			return claimErr
		}
		// Repeated short steps may never reach a heartbeat tick. Refresh the
		// existing generation before every dispatch, while the lease is valid.
		expires := time.Now().UTC().Add(o.config.Worker.LeaseDuration)
		if expires.After(prior.Deadline) {
			expires = prior.Deadline
		}
		if expires.After(lease.ExpiresAt) {
			lease.ExpiresAt = expires
		}
		return tx.RenewLease(lease)
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
	var batchErr *trialBatchError
	batchFailed := errors.As(runErr, &batchErr)
	partial := batchFailed && len(change.TrialUpdates)+len(change.Evaluations) > 0
	if phaseErr != nil || renewErr != nil || (runErr != nil && !partial && (batchFailed || !errors.Is(runErr, ErrBudgetExceeded))) {
		return errors.Join(runErr, phaseErr, renewErr)
	}
	current, done, err := o.meter.authorizeAction(ctx, experimentID, ActionStart)
	if err != nil {
		return err
	}
	defer done()
	if errors.Is(runErr, ErrBudgetExceeded) && !batchFailed {
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
			bound, bindingErr := o.recoverableTrialCall(ctx, experiment, reservation)
			if bindingErr != nil {
				return bindingErr
			}
			if bound {
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
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return nil
				}
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
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return nil
				}
				cancel()
				return err
			}
		}
	}
}

// A trial's persisted dispatch marker must survive takeover so the environment
// can be inspected. Its full reservation stays charged until captured settlement.
func (o *Optimizer) recoverableTrialCall(ctx context.Context, experiment Experiment, r Reservation) (bool, error) {
	switch r.Request.Operation {
	case OperationBaseline, OperationProbe, OperationValidation:
	default:
		return false, nil
	}
	parts := strings.Split(r.Request.CallKey, "/")
	if len(parts) != 3 || parts[0] != "trial" || parts[2] != r.Request.AttemptID.String() {
		return false, nil
	}
	trialID, err := ParseID(parts[1], PrefixTrial)
	if err != nil {
		return false, nil
	}
	trial, err := o.config.Repository.GetTrial(ctx, trialID)
	if err != nil {
		return false, err
	}
	if trial.ExperimentID != experiment.ID || len(trial.Attempts) == 0 {
		return false, ErrIncompleteEvidence
	}
	attempt := trial.Attempts[len(trial.Attempts)-1]
	phase := PhaseProbe
	if trial.Split == SplitValidation {
		phase = PhaseValidate
	} else if trial.CandidateID.IsNil() {
		phase = PhaseBaseline
	}
	return attempt.ID == r.Request.AttemptID && attempt.Receipt != nil && r.Request.Operation == trialOperation(phase), nil
}
