package state

import (
	"math"
	"strings"

	"github.com/xraph/cortex/optimization"
)

func (tx *transaction) ClaimWork(request optimization.ClaimRequest) (optimization.WorkLease, error) {
	var empty optimization.WorkLease
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	if request.Duration <= 0 || strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.Owner) != request.Owner {
		return empty, optimization.ErrInvalidSpec
	}
	key := request.ExperimentID.String()
	experiment, exists := tx.data.Experiments[key]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if lessonErr := checkLessonBindings(tx.data, experiment, tx.now); lessonErr != nil {
		return empty, lessonErr
	}
	if experiment.State != optimization.StateQueued && experiment.State != optimization.StateRunning {
		return empty, optimization.ErrConflict
	}
	if experiment.Deadline.IsZero() || !tx.now.Before(experiment.Deadline) {
		return empty, optimization.ErrBudgetExceeded
	}
	parent, err := tx.liveParent(experiment)
	if err != nil {
		return empty, err
	}
	if experiment.State == optimization.StateRunning && tx.validLease(experiment, experiment.Lease) {
		if experiment.Lease.Owner != request.Owner {
			return empty, optimization.ErrConflict
		}
		return experiment.Lease, nil
	}
	if experiment.Lease.Generation == math.MaxInt64 {
		return empty, optimization.ErrConflict
	}
	expires := tx.now.Add(request.Duration)
	if expires.After(experiment.Deadline) {
		expires = experiment.Deadline
	}
	lease := optimization.WorkLease{ExperimentID: experiment.ID, Owner: request.Owner, Generation: experiment.Lease.Generation + 1, ExpiresAt: expires, ParentGeneration: parent.Lease.Generation}
	if experiment.State == optimization.StateQueued && experiment.Phase == "" {
		experiment.Phase = optimization.PhaseBaseline
	}
	experiment.State = optimization.StateRunning
	experiment.Lease = lease
	experiment.Revision++
	experiment.UpdatedAt = tx.now
	tx.data.Experiments[key] = experiment
	tx.appendEvent(experiment, "claimed", optimization.IdentityRef{Subject: request.Owner})
	return lease, nil
}

func (tx *transaction) RenewLease(lease optimization.WorkLease) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	key := lease.ExperimentID.String()
	experiment, exists := tx.data.Experiments[key]
	if !exists {
		return optimization.ErrNotFound
	}
	if lessonErr := checkLessonBindings(tx.data, experiment, tx.now); lessonErr != nil {
		return lessonErr
	}
	if !tx.validLease(experiment, lease) || !lease.ExpiresAt.After(tx.now) || lease.ExpiresAt.Before(experiment.Lease.ExpiresAt) || lease.ExpiresAt.After(experiment.Deadline) {
		return optimization.ErrConflict
	}
	experiment.Lease.ExpiresAt = lease.ExpiresAt
	experiment.UpdatedAt = tx.now
	tx.data.Experiments[key] = experiment
	return nil
}

func (tx *transaction) validLease(experiment optimization.Experiment, lease optimization.WorkLease) bool {
	parent, err := tx.liveParent(experiment)
	if err != nil || lease.ParentGeneration != parent.Lease.Generation || experiment.Lease.ParentGeneration != lease.ParentGeneration {
		return false
	}
	return experiment.State == optimization.StateRunning && lease.ExperimentID == experiment.ID && lease.Owner != "" && lease.Owner == experiment.Lease.Owner && lease.Generation == experiment.Lease.Generation && lease.Generation > 0 && tx.now.Before(experiment.Lease.ExpiresAt) && tx.now.Before(experiment.Deadline)
}
