package assurance

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

var (
	errProgressUnavailable = errors.New("cortex: run progress transaction unavailable")
	errNoProgress          = errors.New("cortex: save run progress: no progress given")
)

// ProgressTransaction writes a run's progress snapshot while the run lock
// is held.
type ProgressTransaction interface {
	RunTransaction
	// UpsertProgress inserts or replaces the snapshot of p.RunID at the
	// exact context scope. p.Scope and p.UpdatedAt are already set.
	UpsertProgress(context.Context, *suspension.Progress) error
}

// SaveProgress writes a running run's snapshot for the worker that holds
// its lease. The run's state and lease owner are read under the run lock,
// in the same transaction as the write, so a worker whose run was handed
// off, reclaimed or finished cannot land one more snapshot after it lost
// the run: the write either commits before the change that took the run,
// or it reads that change and is refused.
func (s *Repository) SaveProgress(ctx context.Context, p *suspension.Progress) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if p == nil {
		return errNoProgress
	}
	// A run with no lease has lease_owner '', so an empty owner would match
	// it. Nobody holds such a run, and nobody may write its progress.
	if p.Owner == "" || p.RunID.Prefix() != id.PrefixAgentRun {
		return cortex.ErrRunLeaseLost
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return errProgressUnavailable
	}
	saved := *p
	saved.Scope = cortex.Scope{Levels: append([]cortex.Level(nil), scope.Levels...)}
	err = driver.TransactRun(ctx, p.RunID, func(tc context.Context, tx RunTransaction) error {
		progress, supported := tx.(ProgressTransaction)
		if !supported {
			return errProgressUnavailable
		}
		current, readErr := tx.GetRun(tc, p.RunID)
		if errors.Is(readErr, cortex.ErrRunNotFound) {
			return cortex.ErrRunLeaseLost
		}
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != p.RunID || current.Scope.Canonical() != scope.Canonical() || current.State != run.StateRunning || current.Lease == nil || current.Lease.Owner != p.Owner {
			return cortex.ErrRunLeaseLost
		}
		// Milliseconds are the finest time every backend keeps, so the time
		// handed back is the time a later read returns.
		saved.UpdatedAt = s.clock.Now().UTC().Truncate(time.Millisecond)
		if writeErr := progress.UpsertProgress(tc, &saved); writeErr != nil {
			return writeErr
		}
		return tc.Err()
	})
	if err != nil {
		return err
	}
	p.Scope, p.UpdatedAt = saved.Scope, saved.UpdatedAt
	return nil
}
