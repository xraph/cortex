package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
)

const kindResumeAdmission = "resume_admission"
const kindResumeStart = "resume_start"
const maxResumeRecordBytes = 16 << 20

func freezeResumeRequest(ctx context.Context, req suspension.ResumeAdmissionRequest) (suspension.ResumeAdmissionRequest, string, error) {
	if err := require(ctx, req.Scope); err != nil {
		return suspension.ResumeAdmissionRequest{}, "", err
	}
	if req.Version.RunID.Prefix() != id.PrefixAgentRun || req.Version.SuspensionID.Prefix() != id.PrefixSuspension || req.Version.RunRevision < 0 || len(req.Input) == 0 || len(req.Input) > suspension.MaxResumeInputBytes || !utf8.Valid(req.Input) || !json.Valid(req.Input) {
		return suspension.ResumeAdmissionRequest{}, "", suspension.ErrResumeAdmissionConflict
	}
	req.Scope.Levels = slices.Clone(req.Scope.Levels)
	req.Input = slices.Clone(req.Input)
	data, err := json.Marshal(req)
	if err != nil {
		return suspension.ResumeAdmissionRequest{}, "", err
	}
	return req, evidence.Digest(data), nil
}

func insertResumeRecord(ctx context.Context, repo *Repository, kind string, version suspension.ClaimVersion, scope cortex.Scope, value any) error {
	row, err := record(kind, version.SuspensionID.String(), "", version.RunID.String(), scope, value)
	if err != nil {
		return err
	}
	if len(row.Body) > maxResumeRecordBytes {
		return suspension.ErrResumeAdmissionConflict
	}
	row.Digest = evidence.Digest([]byte(row.Body))
	return repo.insert(ctx, row)
}

func decodeResumeAdmission(ctx context.Context, row Record) (suspension.ResumeAdmission, error) {
	var result suspension.ResumeAdmission
	if err := validateRecord(ctx, row); err != nil {
		return result, err
	}
	if row.Deleted || row.Kind != kindResumeAdmission || len(row.Body) > maxResumeRecordBytes || evidence.Digest([]byte(row.Body)) != row.Digest || json.Unmarshal([]byte(row.Body), &result) != nil {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	_, digest, err := freezeResumeRequest(ctx, result.Request)
	next, revisionErr := runstate.NextRevision(result.Request.Version.RunRevision)
	if err != nil || revisionErr != nil || result.SchemaVersion != 1 || row.ID != result.Request.Version.SuspensionID.String() || row.RunID != result.Request.Version.RunID.String() || result.RequestDigest != digest || !evidence.ValidDigest(result.SuspensionDigest) || result.ClaimedRevision != next || result.AdmittedAt.IsZero() {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	return result, nil
}

// GetResumeAdmission reads immutable input and never grants execution authority.
func (s *Repository) GetResumeAdmission(ctx context.Context, runID id.AgentRunID, key id.SuspensionID) (suspension.ResumeAdmission, error) {
	if runID.Prefix() != id.PrefixAgentRun || key.Prefix() != id.PrefixSuspension {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	row, err := s.get(ctx, Query{Kind: kindResumeAdmission, ID: key.String(), RunID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionNotFound
	}
	if err != nil {
		return suspension.ResumeAdmission{}, err
	}
	result, err := decodeResumeAdmission(ctx, row)
	if err == nil && (result.Request.Version.SuspensionID != key || result.Request.Version.RunID != runID) {
		return suspension.ResumeAdmission{}, suspension.ErrResumeAdmissionConflict
	}
	return result, err
}

// AdmitResume atomically claims an agent-reply pause and retains its exact input.
func (s *Repository) AdmitResume(ctx context.Context, input suspension.ResumeAdmissionRequest) (suspension.ResumeAdmission, error) {
	req, digest, err := freezeResumeRequest(ctx, input)
	if err != nil {
		return suspension.ResumeAdmission{}, err
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
		paused, claimErr := s.claimVersionInTransaction(tc, tx, req.Scope, req.Version)
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

// GetResumeStart observes a possible execution start. It cannot authorize replay.
func (s *Repository) GetResumeStart(ctx context.Context, runID id.AgentRunID, key id.SuspensionID) (suspension.ResumeStart, error) {
	if runID.Prefix() != id.PrefixAgentRun || key.Prefix() != id.PrefixSuspension {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionConflict
	}
	row, err := s.get(ctx, Query{Kind: kindResumeStart, ID: key.String(), RunID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionNotFound
	}
	if err != nil {
		return suspension.ResumeStart{}, err
	}
	var result suspension.ResumeStart
	if row.Deleted || row.Kind != kindResumeStart || row.ID != key.String() || len(row.Body) > 64<<10 || evidence.Digest([]byte(row.Body)) != row.Digest || json.Unmarshal([]byte(row.Body), &result) != nil {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionConflict
	}
	admitted, err := s.GetResumeAdmission(ctx, runID, key)
	if err != nil {
		return suspension.ResumeStart{}, err
	}
	if result.SchemaVersion != 1 || result.Version != admitted.Request.Version || result.Scope.Canonical() != admitted.Request.Scope.Canonical() || result.RequestDigest != admitted.RequestDigest || row.RunID != result.Version.RunID.String() || result.StartedAt.IsZero() || result.StartedAt.Before(admitted.AdmittedAt) {
		return suspension.ResumeStart{}, suspension.ErrResumeAdmissionConflict
	}
	return result, nil
}

// StartResume grants execution once, after checking the unchanged claimed run.
// Ambiguous acknowledgments and existing starts return no suspension to execute.
func (s *Repository) StartResume(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string) (suspension.ResumeStart, error) {
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
		repo := s.inTransaction(tx)
		prior, priorErr := repo.GetResumeStart(tc, runID, key)
		if priorErr == nil {
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, suspension.ErrResumeAdmissionNotFound) {
			return priorErr
		}
		current, readErr := tx.GetRun(tc, admitted.Request.Version.RunID)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != admitted.Request.Version.RunID || current.Scope.Canonical() != admitted.Request.Scope.Canonical() || current.State != run.StateRunning || current.Revision != admitted.ClaimedRevision {
			return cortex.ErrRunConflict
		}
		locked, supported := tx.(SuspensionTransaction)
		if !supported {
			return suspension.ErrResumeAdmissionUnavailable
		}
		paused, readErr := locked.LockSuspension(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		data, encodeErr := json.Marshal(paused)
		if encodeErr != nil || paused == nil || evidence.Digest(data) != admitted.SuspensionDigest {
			return suspension.ErrResumeAdmissionConflict
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
		paused.ClaimedRun = current
		result.Fresh, result.Suspension = true, paused
		return tc.Err()
	})
	if err != nil {
		return suspension.ResumeStart{}, err
	}
	return result, nil
}

// ListResumeAdmissions pages exact-scope history, including already started work.
func (s *Repository) ListResumeAdmissions(ctx context.Context, after string, limit int) (suspension.ResumeAdmissionPage, error) {
	var page suspension.ResumeAdmissionPage
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return page, err
	}
	if after != "" {
		key, err := id.ParseSuspensionID(after)
		if err != nil || key.String() != after {
			return page, suspension.ErrResumeAdmissionConflict
		}
	}
	if limit == 0 {
		limit = 64
	}
	if limit < 1 || limit > 256 {
		return page, suspension.ErrResumeAdmissionConflict
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindResumeAdmission, After: after, Limit: limit + 1})
	if err != nil {
		return page, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	for _, row := range rows {
		admitted, decodeErr := decodeResumeAdmission(ctx, row)
		if decodeErr != nil {
			return suspension.ResumeAdmissionPage{}, decodeErr
		}
		page.Admissions = append(page.Admissions, admitted)
	}
	if more {
		page.Next = rows[len(rows)-1].ID
	}
	return page, ctx.Err()
}

var _ suspension.ResumeAdmissionStore = (*Repository)(nil)
