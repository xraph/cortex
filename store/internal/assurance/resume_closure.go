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

const kindResumeClosure = "resume_closure"

// GetResumeClosure returns retained incomplete closure evidence in exact scope.
func (s *Repository) GetResumeClosure(ctx context.Context, runID id.AgentRunID, pauseID id.SuspensionID) (suspension.ResumeClosure, error) {
	if runID.Prefix() != id.PrefixAgentRun || pauseID.Prefix() != id.PrefixSuspension {
		return suspension.ResumeClosure{}, suspension.ErrResumeAdmissionConflict
	}
	row, err := s.get(ctx, Query{Kind: kindResumeClosure, ID: pauseID.String(), RunID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return suspension.ResumeClosure{}, suspension.ErrResumeAdmissionNotFound
	}
	if err != nil {
		return suspension.ResumeClosure{}, err
	}
	var result suspension.ResumeClosure
	if row.Deleted || row.Kind != kindResumeClosure || row.ID != pauseID.String() || row.RunID != runID.String() || len(row.Body) > 32<<10 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &result) != nil || result.Validate() != nil || result.Request.Version.RunID != runID || result.Request.Version.SuspensionID != pauseID || result.Request.Scope.Canonical() != row.ScopeCanon {
		return suspension.ResumeClosure{}, suspension.ErrResumeAdmissionConflict
	}
	if startErr := s.checkResumeClosureStart(ctx, result.Request); startErr != nil {
		return suspension.ResumeClosure{}, startErr
	}
	return result, nil
}

func (s *Repository) checkResumeClosureStart(ctx context.Context, req suspension.ResumeClosureRequest) error {
	admitted, err := s.GetResumeAdmission(ctx, req.Version.RunID, req.Version.SuspensionID)
	if err != nil {
		return err
	}
	if admitted.Request.Version != req.Version || admitted.Request.Scope.Canonical() != req.Scope.Canonical() || admitted.RequestDigest != req.AdmissionDigest || admitted.ClaimedRevision > req.RunRevision {
		return suspension.ErrResumeAdmissionConflict
	}
	started, err := s.GetResumeStart(ctx, req.Version.RunID, req.Version.SuspensionID)
	if err != nil {
		return err
	}
	digest, err := started.Digest()
	if err != nil || digest != req.StartDigest || req.ObservedAt.Before(started.StartedAt) {
		return suspension.ErrResumeAdmissionConflict
	}
	return nil
}

// CloseResume records incomplete termination after host-authenticated quiescence.
// This low-level store method does not authenticate host proof. Use ResumeCloser
// for current authorization and fencing. No private result is accepted here.
func (s *Repository) CloseResume(ctx context.Context, request suspension.ResumeClosureRequest) (suspension.ResumeClosure, error) {
	if err := require(ctx, request.Scope); err != nil {
		return suspension.ResumeClosure{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return suspension.ResumeClosure{}, err
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return suspension.ResumeClosure{}, suspension.ErrResumeAdmissionUnavailable
	}
	var result suspension.ResumeClosure
	err = driver.TransactRun(ctx, req.Version.RunID, func(tc context.Context, tx RunTransaction) error {
		result = suspension.ResumeClosure{}
		repo := s.inTransaction(tx)
		prior, priorErr := repo.GetResumeClosure(tc, req.Version.RunID, req.Version.SuspensionID)
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
		if readErr := repo.checkResumeClosureStart(tc, req); readErr != nil {
			return readErr
		}
		latest, readErr := repo.LatestResumeAdmission(tc, req.Version.RunID)
		if readErr != nil {
			return readErr
		}
		if latest.Request.Version != req.Version || latest.RequestDigest != req.AdmissionDigest {
			return suspension.ErrResumeAdmissionConflict
		}
		current, readErr := tx.GetRun(tc, req.Version.RunID)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != req.Version.RunID || current.Scope.Canonical() != req.Scope.Canonical() || current.Revision != req.RunRevision || current.State != run.StateRunning || current.ResultRef != "" || current.VerificationStatus == verification.StatusPassed {
			return cortex.ErrRunConflict
		}
		at := s.clock.Now().UTC()
		if at.Before(req.ObservedAt) || !at.Before(req.ExpiresAt) {
			return suspension.ErrResumeAdmissionConflict
		}
		next, revisionErr := runstate.NextRevision(current.Revision)
		if revisionErr != nil {
			return revisionErr
		}
		current.State, current.StopReason = run.StateIncomplete, run.StopExecutionError
		current.Output, current.FinishReason, current.ResultRef = "", "", ""
		current.VerificationStatus, current.CompletedAt = verification.StatusUnchecked, &at
		if saveErr := tx.SetRunCandidate(tc, current, at); saveErr != nil {
			return saveErr
		}
		result = suspension.ResumeClosure{Request: req, RequestDigest: digest, EventID: id.New(event.PrefixEvent), RunRevision: next, ClosedAt: at}
		if eventErr := repo.appendRunEvents(tc, tx, current.ID, event.Input{ID: result.EventID, Type: event.ExecutionFinished}); eventErr != nil {
			return eventErr
		}
		if insertErr := insertResumeRecord(tc, repo, kindResumeClosure, req.Version, req.Scope, result); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now().UTC()
		if finished.Before(at) || !finished.Before(req.ExpiresAt) {
			return suspension.ErrResumeAdmissionConflict
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return suspension.ResumeClosure{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return suspension.ResumeClosure{}, contextErr
	}
	return result, nil
}

var _ suspension.ResumeClosureStore = (*Repository)(nil)
