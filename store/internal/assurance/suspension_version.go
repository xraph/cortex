package assurance

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

// SuspensionTransaction locks the suspension while the outer run lock is held.
type SuspensionTransaction interface {
	RunTransaction
	LockSuspension(context.Context, id.AgentRunID) (*suspension.Suspension, error)
}

// ClaimSuspensionVersion claims only the validated suspension and run revision.
func (s *Repository) ClaimSuspensionVersion(ctx context.Context, req suspension.ClaimVersion) (*suspension.Suspension, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if req.RunID.Prefix() != id.PrefixAgentRun || req.SuspensionID.Prefix() != id.PrefixSuspension || req.RunRevision < 0 {
		return nil, cortex.ErrNotSuspended
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	var result *suspension.Suspension
	err = driver.TransactRun(ctx, req.RunID, func(tc context.Context, tx RunTransaction) error {
		result = nil
		var claimErr error
		result, claimErr = s.claimVersionInTransaction(tc, tx, scope, req)
		return claimErr
	})
	if err != nil {
		if errors.Is(err, verification.ErrCommitUnavailable) {
			return nil, fmt.Errorf("%w: %w", suspension.ErrVersionClaimUnavailable, err)
		}
		return nil, err
	}
	return result, nil
}

func (s *Repository) claimVersionInTransaction(tc context.Context, tx RunTransaction, scope cortex.Scope, req suspension.ClaimVersion) (*suspension.Suspension, error) {
	locked, supported := tx.(SuspensionTransaction)
	if !supported {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	current, readErr := tx.GetRun(tc, req.RunID)
	if readErr != nil {
		return nil, readErr
	}
	if current == nil || current.ID != req.RunID || current.Scope.Canonical() != scope.Canonical() || current.State != run.StatePaused {
		return nil, cortex.ErrNotSuspended
	}
	if current.Revision != req.RunRevision {
		return nil, cortex.ErrRunConflict
	}
	paused, readErr := locked.LockSuspension(tc, req.RunID)
	if readErr != nil {
		return nil, readErr
	}
	if paused == nil || paused.ID != req.SuspensionID || paused.RunID != req.RunID || paused.Scope.Canonical() != scope.Canonical() {
		return nil, cortex.ErrNotSuspended
	}
	at := s.clock.Now().UTC()
	if paused.ExpiresAt != nil && !paused.ExpiresAt.After(at) {
		return nil, cortex.ErrSuspensionExpired
	}
	next, revisionErr := runstate.NextRevision(current.Revision)
	if revisionErr != nil {
		return nil, revisionErr
	}
	current.State = run.StateRunning
	if writeErr := tx.SetRunCandidate(tc, current, at); writeErr != nil {
		return nil, writeErr
	}
	current.Revision, current.UpdatedAt = next, at
	paused.ClaimedRun = current
	return paused, tc.Err()
}
