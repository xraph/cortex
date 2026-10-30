package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

// checkLeasedClaim refuses a claim that would leave the run running under
// no real lease, or whose first snapshot belongs to someone else.
func checkLeasedClaim(runID id.AgentRunID, lease run.Lease, first *suspension.Progress) error {
	if lease.Owner == "" || lease.ExpiresAt.IsZero() {
		return cortex.ErrRunLeaseLost
	}
	if first != nil && (first.Owner != lease.Owner || first.RunID != runID) {
		return cortex.ErrRunLeaseLost
	}
	return nil
}

// writeFirstProgress stores first as the run's snapshot inside the claim's
// transaction, and stamps it with the scope and time it was stored under.
func (s *Repository) writeFirstProgress(tc context.Context, tx RunTransaction, scope cortex.Scope, first *suspension.Progress, at time.Time) error {
	if first == nil {
		return nil
	}
	progress, supported := tx.(ProgressTransaction)
	if !supported {
		return errProgressUnavailable
	}
	first.Scope = cortex.Scope{Levels: append([]cortex.Level(nil), scope.Levels...)}
	first.UpdatedAt = at.Truncate(time.Millisecond)
	return progress.UpsertProgress(tc, first)
}

// ClaimSuspensionLeased claims a pinned pause and takes the run's lease in
// one transaction, with the first progress snapshot when one is given. The
// run is never running without an owner, so a claimer that dies leaves a
// lease that lapses and a snapshot to continue from.
func (s *Repository) ClaimSuspensionLeased(ctx context.Context, claim suspension.LeasedClaim) (*suspension.Suspension, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	req := claim.Version
	if req.RunID.Prefix() != id.PrefixAgentRun || req.SuspensionID.Prefix() != id.PrefixSuspension || req.RunRevision < 0 {
		return nil, cortex.ErrNotSuspended
	}
	if leaseErr := checkLeasedClaim(req.RunID, claim.Lease, claim.First); leaseErr != nil {
		return nil, leaseErr
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	var result *suspension.Suspension
	err = driver.TransactRun(ctx, req.RunID, func(tc context.Context, tx RunTransaction) error {
		result = nil
		var claimErr error
		result, claimErr = s.claimLeasedInTransaction(tc, tx, scope, claim)
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

func (s *Repository) claimLeasedInTransaction(tc context.Context, tx RunTransaction, scope cortex.Scope, claim suspension.LeasedClaim) (*suspension.Suspension, error) {
	locked, lockable := tx.(SuspensionTransaction)
	continuation, writable := tx.(ContinuationTransaction)
	if !lockable || !writable {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	req := claim.Version
	current, readErr := tx.GetRun(tc, req.RunID)
	if errors.Is(readErr, cortex.ErrRunNotFound) {
		// A run outside the exact scope reads as absent.
		return nil, cortex.ErrNotSuspended
	}
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
	if !claim.ExpiredBy.IsZero() {
		// The sweeper's claim: only a pause past its deadline.
		if paused.ExpiresAt == nil || paused.ExpiresAt.After(claim.ExpiredBy) {
			return nil, cortex.ErrNotSuspended
		}
	} else if paused.ExpiresAt != nil && !paused.ExpiresAt.After(at) {
		return nil, cortex.ErrSuspensionExpired
	}
	next, revisionErr := runstate.NextRevision(current.Revision)
	if revisionErr != nil {
		return nil, revisionErr
	}
	current.State = run.StateRunning
	current.Lease = heldLease(claim.Lease)
	if writeErr := continuation.SetRunContinuation(tc, current, at); writeErr != nil {
		return nil, writeErr
	}
	current.Revision, current.UpdatedAt = next, at
	if writeErr := s.writeFirstProgress(tc, tx, scope, claim.First, at); writeErr != nil {
		return nil, writeErr
	}
	paused.ClaimedRun = current
	return paused, tc.Err()
}

// ClaimContinuationLeased is ClaimContinuation under a lease that must be
// set, with the run's first progress snapshot written in the same
// transaction.
func (s *Repository) ClaimContinuationLeased(ctx context.Context, runID id.AgentRunID, lease run.Lease, first *suspension.Progress) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if leaseErr := checkLeasedClaim(runID, lease, first); leaseErr != nil {
		return nil, leaseErr
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
		if writeErr := s.writeFirstProgress(tc, tx, scope, first, at); writeErr != nil {
			return writeErr
		}
		result = current
		return tc.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// StartResumeLeased is StartResume, taking the lease of the run the
// admission claimed and writing its first progress snapshot in the
// transaction that grants execution, so the one worker allowed to execute
// the resume holds the run from that moment. The run's revision is left as
// the admission recorded it. A start recorded before is returned as it is,
// and takes nothing.
func (s *Repository) StartResumeLeased(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string, lease run.Lease, first *suspension.Progress) (suspension.ResumeStart, error) {
	if leaseErr := checkLeasedClaim(runID, lease, first); leaseErr != nil {
		return suspension.ResumeStart{}, leaseErr
	}
	admitted, err := s.GetResumeAdmission(ctx, runID, key)
	if err != nil {
		return suspension.ResumeStart{}, err
	}
	if digest != admitted.RequestDigest {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionConflict
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionUnavailable
	}
	var result suspension.ResumeStart
	err = driver.TransactRun(ctx, admitted.Request.Version.RunID, func(tc context.Context, tx RunTransaction) error {
		result = suspension.ResumeStart{}
		continuation, writable := tx.(ContinuationTransaction)
		if !writable {
			return suspension.ErrResumeAdmissionUnavailable
		}
		repo := s.inTransaction(tx)
		prior, priorErr := repo.GetResumeStart(tc, runID, key)
		if priorErr == nil {
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, suspension.ErrResumeAdmissionNotFound) {
			return priorErr
		}
		_, expiryErr := repo.GetResumeExpiry(tc, runID, key)
		if expiryErr == nil {
			return cortex.ErrSuspensionExpired
		}
		if !errors.Is(expiryErr, suspension.ErrResumeAdmissionNotFound) {
			return expiryErr
		}
		current, paused, readErr := lockAdmittedResume(tc, tx, admitted)
		if readErr != nil {
			return readErr
		}
		at := s.clock.Now().UTC()
		if paused.ExpiresAt != nil && !paused.ExpiresAt.After(at) {
			return cortex.ErrSuspensionExpired
		}
		if at.Before(admitted.AdmittedAt) {
			return suspension.ErrResumeAdmissionConflict
		}
		result = suspension.ResumeStart{SchemaVersion: 1, Version: admitted.Request.Version, Scope: admitted.Request.Scope, RequestDigest: digest, StartedAt: at}
		if insertErr := insertResumeRecord(tc, repo, kindResumeStart, result.Version, result.Scope, result); insertErr != nil {
			return insertErr
		}
		current.Lease = heldLease(lease)
		if writeErr := continuation.SetRunLease(tc, current); writeErr != nil {
			return writeErr
		}
		if writeErr := s.writeFirstProgress(tc, tx, admitted.Request.Scope, first, at); writeErr != nil {
			return writeErr
		}
		paused.ClaimedRun = current
		result.Fresh, result.Suspension = true, paused
		return tc.Err()
	})
	if err != nil {
		return suspension.ResumeStart{}, err
	}
	return result, nil
}

// AdmitResumeLeased is AdmitResume, taking the run's lease and writing its
// first progress snapshot in the transaction that claims the pause. An
// admission recorded before is returned as it was, and takes nothing.
func (s *Repository) AdmitResumeLeased(ctx context.Context, input suspension.ResumeAdmissionRequest, lease run.Lease, first *suspension.Progress) (suspension.ResumeAdmission, error) {
	req, digest, err := freezeResumeRequest(ctx, input)
	if err != nil {
		return suspension.ResumeAdmission{}, err
	}
	if leaseErr := checkLeasedClaim(req.Version.RunID, lease, first); leaseErr != nil {
		return suspension.ResumeAdmission{}, leaseErr
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionUnavailable
	}
	var result suspension.ResumeAdmission
	err = driver.TransactRun(ctx, req.Version.RunID, func(tc context.Context, tx RunTransaction) error {
		result = suspension.ResumeAdmission{}
		repo := s.inTransaction(tx)
		prior, priorErr := repo.GetResumeAdmission(tc, req.Version.RunID, req.Version.SuspensionID)
		if priorErr == nil {
			if prior.RequestDigest != digest {
				return suspension.ErrResumeAdmissionConflict
			}
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, suspension.ErrResumeAdmissionNotFound) {
			return priorErr
		}
		paused, claimErr := s.claimLeasedInTransaction(tc, tx, req.Scope, suspension.LeasedClaim{Version: req.Version, Lease: lease, First: first})
		if claimErr != nil {
			return claimErr
		}
		if paused.Reason != suspension.ReasonAgentReply {
			return suspension.ErrResumeAdmissionConflict
		}
		data, encodeErr := json.Marshal(paused)
		if encodeErr != nil {
			return encodeErr
		}
		result = suspension.ResumeAdmission{SchemaVersion: 1, Request: req, RequestDigest: digest, SuspensionDigest: evidence.Digest(data), ClaimedRevision: paused.ClaimedRun.Revision, AdmittedAt: s.clock.Now().UTC()}
		if insertErr := insertResumeRecord(tc, repo, kindResumeAdmission, req.Version, req.Scope, result); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return suspension.ResumeAdmission{}, err
	}
	return result, nil
}
