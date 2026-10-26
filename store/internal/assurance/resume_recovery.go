package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

const kindResumeExpiry = "resume_expiry"

// LatestResumeAdmission reads the most recent retained pause identity for a run.
func (s *Repository) LatestResumeAdmission(ctx context.Context, runID id.AgentRunID) (suspension.ResumeAdmission, error) {
	if runID.Prefix() != id.PrefixAgentRun {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindResumeAdmission, RunID: runID.String(), Limit: 1, Reverse: true})
	if err != nil {
		return suspension.ResumeAdmission{}, err
	}
	if len(rows) == 0 {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionNotFound
	}
	if len(rows) != 1 || rows[0].RunID != runID.String() {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	return decodeResumeAdmission(ctx, rows[0])
}

// GetResumeExpiry reads an immutable terminal decision without granting replay.
func (s *Repository) GetResumeExpiry(ctx context.Context, runID id.AgentRunID, key id.SuspensionID) (suspension.ResumeExpiry, error) {
	admitted, err := s.GetResumeAdmission(ctx, runID, key)
	if err != nil {
		return suspension.ResumeExpiry{}, err
	}
	row, err := s.get(ctx, Query{Kind: kindResumeExpiry, ID: key.String(), RunID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return suspension.ResumeExpiry{}, suspension.ErrResumeAdmissionNotFound
	}
	if err != nil {
		return suspension.ResumeExpiry{}, err
	}
	var result suspension.ResumeExpiry
	next, revisionErr := runstate.NextRevision(admitted.ClaimedRevision)
	if revisionErr != nil || row.Deleted || row.Kind != kindResumeExpiry || row.ID != key.String() || row.RunID != runID.String() || len(row.Body) > 64<<10 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &result) != nil || result.SchemaVersion != 1 || result.Version != admitted.Request.Version || result.Scope.Canonical() != admitted.Request.Scope.Canonical() || result.RequestDigest != admitted.RequestDigest || result.EventID.Prefix() != event.PrefixEvent || result.RunRevision != next || result.ExpiredAt.IsZero() || result.ExpiredAt.Before(admitted.AdmittedAt) {
		return suspension.ResumeExpiry{}, suspension.ErrResumeAdmissionConflict
	}
	return result, nil
}

// ExpireResumeAdmission atomically ends only an expired, never-started admission.
func (s *Repository) ExpireResumeAdmission(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string) (suspension.ResumeExpiry, error) {
	admitted, err := s.GetResumeAdmission(ctx, runID, key)
	if err != nil {
		return suspension.ResumeExpiry{}, err
	}
	if digest != admitted.RequestDigest {
		return suspension.ResumeExpiry{}, suspension.ErrResumeAdmissionConflict
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return suspension.ResumeExpiry{}, suspension.ErrResumeAdmissionUnavailable
	}
	var result suspension.ResumeExpiry
	err = driver.TransactRun(ctx, runID, func(tc context.Context, tx RunTransaction) error {
		result = suspension.ResumeExpiry{}
		repo := s.inTransaction(tx)
		prior, priorErr := repo.GetResumeExpiry(tc, runID, key)
		if priorErr == nil {
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, suspension.ErrResumeAdmissionNotFound) {
			return priorErr
		}
		_, startErr := repo.GetResumeStart(tc, runID, key)
		if startErr == nil {
			return suspension.ErrResumeAlreadyStarted
		}
		if !errors.Is(startErr, suspension.ErrResumeAdmissionNotFound) {
			return startErr
		}
		current, paused, lockErr := lockAdmittedResume(tc, tx, admitted)
		if lockErr != nil {
			return lockErr
		}
		at := s.clock.Now().UTC()
		if paused.ExpiresAt == nil || at.Before(*paused.ExpiresAt) || at.Before(admitted.AdmittedAt) {
			return suspension.ErrResumeAdmissionConflict
		}
		next, revisionErr := runstate.NextRevision(current.Revision)
		if revisionErr != nil {
			return revisionErr
		}
		current.State, current.StopReason = run.StateIncomplete, run.StopDeadline
		current.Output, current.FinishReason, current.ResultRef = "", "", ""
		current.VerificationStatus, current.CompletedAt = verification.StatusUnchecked, &at
		if saveErr := tx.SetRunCandidate(tc, current, at); saveErr != nil {
			return saveErr
		}
		result = suspension.ResumeExpiry{SchemaVersion: 1, Version: admitted.Request.Version, Scope: admitted.Request.Scope, RequestDigest: digest, RunRevision: next, ExpiredAt: at, EventID: id.New(event.PrefixEvent)}
		if eventErr := repo.appendRunEvents(tc, tx, runID, event.Input{ID: result.EventID, Type: event.ExecutionFinished}); eventErr != nil {
			return eventErr
		}
		if insertErr := insertResumeRecord(tc, repo, kindResumeExpiry, result.Version, result.Scope, result); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return suspension.ResumeExpiry{}, err
	}
	return result, nil
}

func lockAdmittedResume(ctx context.Context, tx RunTransaction, admitted suspension.ResumeAdmission) (*run.Run, *suspension.Suspension, error) {
	current, err := tx.GetRun(ctx, admitted.Request.Version.RunID)
	if err != nil {
		return nil, nil, err
	}
	if current == nil || current.ID != admitted.Request.Version.RunID || current.Scope.Canonical() != admitted.Request.Scope.Canonical() || current.State != run.StateRunning || current.Revision != admitted.ClaimedRevision {
		return nil, nil, cortex.ErrRunConflict
	}
	locked, ok := tx.(SuspensionTransaction)
	if !ok {
		return nil, nil, suspension.ErrResumeAdmissionUnavailable
	}
	paused, err := locked.LockSuspension(ctx, current.ID)
	if err != nil {
		return nil, nil, err
	}
	data, encodeErr := json.Marshal(paused)
	if encodeErr != nil || paused == nil || evidence.Digest(data) != admitted.SuspensionDigest {
		return nil, nil, suspension.ErrResumeAdmissionConflict
	}
	return current, paused, nil
}

var _ suspension.ResumeRecoveryStore = (*Repository)(nil)
