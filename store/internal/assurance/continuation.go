package assurance

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
)

var errContinuationUnavailable = errors.New("cortex: run continuation transaction unavailable")

// ContinuationTransaction writes the continuation columns of a run row and
// the suspension deadline while the run lock is held.
type ContinuationTransaction interface {
	SuspensionTransaction
	// SetRunContinuation saves r's state, lease, readiness, reclaim count and
	// notification time. It compares r.Revision and advances it, and returns
	// cortex.ErrRunConflict when the stored revision moved.
	SetRunContinuation(context.Context, *run.Run, time.Time) error
	// ClearSuspensionDeadline removes the expiry of the run's suspension at
	// the exact context scope.
	ClearSuspensionDeadline(context.Context, id.AgentRunID) error
}

// transactContinuation runs fn under the backend's run lock. The read, the
// state check and the conditional write all happen inside it, so two
// callers racing the same transition serialize, and the second one reads
// the state the first one wrote.
func (s *Repository) transactContinuation(ctx context.Context, runID id.AgentRunID, fn func(context.Context, ContinuationTransaction) error) error {
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return errContinuationUnavailable
	}
	return driver.TransactRun(ctx, runID, func(tc context.Context, tx RunTransaction) error {
		continuation, supported := tx.(ContinuationTransaction)
		if !supported {
			return errContinuationUnavailable
		}
		return fn(tc, continuation)
	})
}

// MarkReady moves a paused run whose suspension has not expired to ready.
// The run and its suspension are locked together, so an approval cannot
// slip past a deadline the sweeper is acting on, and two approvals cannot
// both succeed: the second one reads a run that is no longer paused.
func (s *Repository) MarkReady(ctx context.Context, runID id.AgentRunID, ready run.Readiness) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if runID.Prefix() != id.PrefixAgentRun {
		return nil, cortex.ErrNotSuspended
	}
	var result *run.Run
	err = s.transactContinuation(ctx, runID, func(tc context.Context, tx ContinuationTransaction) error {
		result = nil
		current, readErr := tx.GetRun(tc, runID)
		if errors.Is(readErr, cortex.ErrRunNotFound) {
			// A run outside the exact scope reads as absent. ClaimSuspension
			// answers that case with ErrNotSuspended, and so does this.
			return cortex.ErrNotSuspended
		}
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != runID || current.Scope.Canonical() != scope.Canonical() || current.State != run.StatePaused {
			return cortex.ErrNotSuspended
		}
		paused, lockErr := tx.LockSuspension(tc, runID)
		if lockErr != nil {
			return lockErr
		}
		if paused == nil || paused.RunID != runID || paused.Scope.Canonical() != scope.Canonical() {
			return cortex.ErrNotSuspended
		}
		at := s.clock.Now().UTC()
		if paused.ExpiresAt != nil && !paused.ExpiresAt.After(at) {
			return cortex.ErrSuspensionExpired
		}
		next, revisionErr := runstate.NextRevision(current.Revision)
		if revisionErr != nil {
			return revisionErr
		}
		saved := ready
		saved.Results = append([]byte(nil), ready.Results...)
		saved.At = ready.At.UTC()
		current.State, current.Ready = run.StateReady, &saved
		if writeErr := tx.SetRunContinuation(tc, current, at); writeErr != nil {
			return writeErr
		}
		if clearErr := tx.ClearSuspensionDeadline(tc, runID); clearErr != nil {
			return clearErr
		}
		current.Revision, current.UpdatedAt = next, at
		result = current
		return tc.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ClaimContinuation moves a ready or reclaimable run to running under
// lease. Of two concurrent callers, the second reads the running state the
// first one committed and gets ErrRunNotContinuable. A half lease, an owner
// with no expiry or an expiry with no owner, is refused with
// ErrRunLeaseLost, as HoldLease refuses it: the first would never lapse and
// the second could be held by anyone.
func (s *Repository) ClaimContinuation(ctx context.Context, runID id.AgentRunID, lease run.Lease) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if (lease.Owner == "") != lease.ExpiresAt.IsZero() {
		return nil, cortex.ErrRunLeaseLost
	}
	if runID.Prefix() != id.PrefixAgentRun {
		return nil, cortex.ErrRunNotContinuable
	}
	var result *run.Run
	err = s.transactContinuation(ctx, runID, func(tc context.Context, tx ContinuationTransaction) error {
		result = nil
		current, readErr := tx.GetRun(tc, runID)
		if errors.Is(readErr, cortex.ErrRunNotFound) {
			return cortex.ErrRunNotContinuable
		}
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != runID || current.Scope.Canonical() != scope.Canonical() || (current.State != run.StateReady && current.State != run.StateReclaimable) {
			return cortex.ErrRunNotContinuable
		}
		next, revisionErr := runstate.NextRevision(current.Revision)
		if revisionErr != nil {
			return revisionErr
		}
		at := s.clock.Now().UTC()
		current.State, current.Ready, current.NotifiedAt = run.StateRunning, nil, nil
		current.Lease = heldLease(lease)
		if writeErr := tx.SetRunContinuation(tc, current, at); writeErr != nil {
			return writeErr
		}
		current.Revision, current.UpdatedAt = next, at
		result = current
		return tc.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// MarkReclaimable takes a running run away from its worker. The sweeper
// passes no owner and may only take a lease that has lapsed; a worker
// handing its own run off passes its owner name.
func (s *Repository) MarkReclaimable(ctx context.Context, runID id.AgentRunID, now time.Time, owner string) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if runID.Prefix() != id.PrefixAgentRun {
		return nil, cortex.ErrRunNotFound
	}
	var result *run.Run
	err = s.transactContinuation(ctx, runID, func(tc context.Context, tx ContinuationTransaction) error {
		result = nil
		current, readErr := tx.GetRun(tc, runID)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != runID || current.Scope.Canonical() != scope.Canonical() {
			return cortex.ErrRunNotFound
		}
		if current.State != run.StateRunning {
			return cortex.ErrRunConflict
		}
		if owner == "" {
			if current.Lease == nil || current.Lease.ExpiresAt.IsZero() || current.Lease.ExpiresAt.After(now) {
				return cortex.ErrRunLeased
			}
			current.Reclaims++
		} else if current.Lease == nil || current.Lease.Owner != owner {
			return cortex.ErrRunLeaseLost
		}
		next, revisionErr := runstate.NextRevision(current.Revision)
		if revisionErr != nil {
			return revisionErr
		}
		at := s.clock.Now().UTC()
		current.State, current.Lease = run.StateReclaimable, nil
		if writeErr := tx.SetRunContinuation(tc, current, at); writeErr != nil {
			return writeErr
		}
		current.Revision, current.UpdatedAt = next, at
		result = current
		return tc.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// heldLease is the lease a claim stores. A zero lease stores none.
func heldLease(lease run.Lease) *run.Lease {
	if lease.Owner == "" && lease.ExpiresAt.IsZero() {
		return nil
	}
	held := run.Lease{Owner: lease.Owner, ExpiresAt: lease.ExpiresAt.UTC()}
	return &held
}
