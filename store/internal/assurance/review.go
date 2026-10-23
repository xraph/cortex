package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const kindAssignmentReview = "assignment_review"

var _ coordination.ReviewStore = (*Repository)(nil)

func reviewRecord(r coordination.Review) (Record, error) {
	return record(kindAssignmentReview, r.ID.String(), r.Digest, r.SubjectAssignmentID.String(), r.Handoff.Scope, r)
}
func decodeReview(row Record) (coordination.Review, error) {
	var r coordination.Review
	if row.Deleted || len(row.Body) > coordination.MaxReviewBytes || json.Unmarshal([]byte(row.Body), &r) != nil || r.Validate() != nil {
		return r, coordination.ErrInvalidReview
	}
	expected, err := reviewRecord(r)
	if err != nil || row.Kind != expected.Kind || row.ID != expected.ID || row.RunID != expected.RunID || row.Digest != expected.Digest || row.ScopeCanon != expected.ScopeCanon {
		return r, coordination.ErrInvalidReview
	}
	return r, nil
}

// GetAssignmentReview reads a retained observation without reauthorizing bytes.
func (s *Repository) GetAssignmentReview(ctx context.Context, identifier id.ID) (coordination.Review, error) {
	if identifier.IsNil() || identifier.Prefix() != coordination.PrefixReview {
		return coordination.Review{}, coordination.ErrInvalidReview
	}
	row, err := s.get(ctx, Query{Kind: kindAssignmentReview, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return coordination.Review{}, coordination.ErrReviewNotFound
	}
	if err != nil {
		return coordination.Review{}, err
	}
	return decodeReview(row)
}

// ListAssignmentReviews preserves every contradiction instead of voting on it.
func (s *Repository) ListAssignmentReviews(ctx context.Context, req coordination.ReviewListRequest) ([]coordination.Review, error) {
	if req.AssignmentID.IsNil() || req.AssignmentID.Prefix() != coordination.PrefixAssignment || req.Limit < 1 || req.Limit > 256 || (!req.After.IsNil() && req.After.Prefix() != coordination.PrefixReview) {
		return nil, coordination.ErrInvalidReview
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindAssignmentReview, RunID: req.AssignmentID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Review, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		r, decodeErr := decodeReview(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if r.SubjectAssignmentID != req.AssignmentID {
			return nil, coordination.ErrInvalidReview
		}
		result = append(result, r)
	}
	return result, nil
}

// RecordAssignmentReview validates retained producer/reviewer execution under
// the plan fence. It never changes either run or the producer assignment state.
// Hosts must authenticate reviewers and enforce current scheduler/grant policy.
func (s *Repository) RecordAssignmentReview(ctx context.Context, r coordination.Review) (coordination.Review, error) {
	if err := require(ctx, r.Handoff.Scope); err != nil {
		return coordination.Review{}, err
	}
	if r.Validate() != nil {
		return coordination.Review{}, coordination.ErrInvalidReview
	}
	frozen, err := coordination.SealReview(r)
	if err != nil {
		return coordination.Review{}, err
	}
	var saved coordination.Review
	err = s.planTransaction(ctx, frozen.Handoff.PlanID, func(tc context.Context, tx *Repository) error {
		previous, readErr := tx.GetAssignmentReview(tc, frozen.ID)
		if readErr == nil {
			if previous.Digest != frozen.Digest {
				return coordination.ErrReviewConflict
			}
			saved = previous
			return nil
		}
		if !errors.Is(readErr, coordination.ErrReviewNotFound) {
			return readErr
		}
		if checkErr := tx.validateReviewExecution(tc, frozen); checkErr != nil {
			return checkErr
		}
		row, buildErr := reviewRecord(frozen)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		saved = frozen
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrReviewConflict
	}
	if err != nil {
		return coordination.Review{}, err
	}
	return saved, nil
}

func (s *Repository) validateReviewExecution(ctx context.Context, r coordination.Review) error {
	subject, err := s.GetAssignment(ctx, r.SubjectAssignmentID)
	if err != nil {
		return err
	}
	reviewer, err := s.GetAssignment(ctx, r.Handoff.AssignmentID)
	if err != nil {
		return err
	}
	if reviewer.Handoff.Digest != r.Handoff.Digest || reviewer.State != coordination.AssignmentWorking || subject.State != coordination.AssignmentSubmitted || subject.Handoff.PlanID != r.Handoff.PlanID || subject.Submission == nil {
		return coordination.ErrInvalidReview
	}
	p, err := s.GetPlan(ctx, r.Handoff.PlanID)
	if err != nil {
		return err
	}
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	// Acquire both run fences before reading retained evidence. The plan lock
	// serializes reviews of this plan; other run writers never acquire plan locks.
	current, err := reader.LockSubmissionRun(ctx, r.RunID)
	if err != nil {
		return err
	}
	if current.Revision != r.RunRevision || current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.Scope.Canonical() != r.Handoff.Scope.Canonical() || current.Provenance.ParentRunID != r.Handoff.ParentRunID.String() || current.Provenance.AssignmentDigest != r.Handoff.Digest || current.Provenance.PreparedDigest != r.PreparedDigest || current.Provenance.AcceptanceRef != p.Contract.ID.String() {
		return coordination.ErrInvalidReview
	}
	producer, err := reader.LockSubmissionRun(ctx, subject.Submission.RunID)
	if err != nil {
		return err
	}
	job, err := s.GetVerificationJob(ctx, subject.Submission.JobID)
	if err != nil {
		return err
	}
	at := s.clock.Now()
	if coordination.ValidateReview(r, subject, job.Request.Binding, p, at) != nil {
		return coordination.ErrInvalidReview
	}
	// Verification may legitimately advance the producer revision after submission.
	// Recheck its current state against the same immutable job and binding.
	observedSubject := subject
	pins := *subject.Submission
	pins.RunRevision = producer.Revision
	observedSubject.Submission = &pins
	if checkErr := s.validateAssignmentSubmission(ctx, observedSubject, at); checkErr != nil {
		return checkErr
	}
	outputExpiry, checkErr := s.validateReviewOutput(ctx, current, r, at)
	if checkErr != nil {
		return checkErr
	}
	var total int64
	if len(r.Handoff.Inputs) > 64 {
		return evidence.ErrTooLarge
	}
	for _, ref := range r.Handoff.Inputs {
		if ref.Bytes > MaxEvidenceBytes || ref.Bytes > (8<<20)-total {
			return evidence.ErrTooLarge
		}
		total += ref.Bytes
	}
	for _, ref := range r.Handoff.Inputs {
		if _, readErr := s.LoadEvidence(ctx, ref); readErr != nil {
			return readErr
		}
	}
	observed := s.clock.Now()
	if (!outputExpiry.IsZero() && !outputExpiry.After(observed)) || observed.Before(at) || coordination.ValidateReview(r, subject, job.Request.Binding, p, observed) != nil {
		return coordination.ErrInvalidReview
	}
	return ctx.Err()
}

func (s *Repository) validateReviewOutput(ctx context.Context, current *run.Run, r coordination.Review, at time.Time) (time.Time, error) {
	candidate, private, err := s.loadPrivateCandidate(ctx, current.ID)
	if err != nil {
		return time.Time{}, err
	}
	if !private {
		if current.Provenance.VerificationRequired {
			return time.Time{}, coordination.ErrInvalidReview
		}
		return time.Time{}, r.ValidateOutput([]byte(current.Output))
	}
	ref := candidate.Candidate
	if ref.Bytes > coordination.MaxReviewBytes || ref.CreatedAt.After(at) || !ref.RetainUntil.After(at) {
		return time.Time{}, coordination.ErrInvalidReview
	}
	if checkErr := s.validateCapturedOutput(ctx, current, verification.Binding{Candidate: ref, Context: verification.ContextSnapshot{ExecutionTranscript: candidate.Transcript}}); checkErr != nil {
		return time.Time{}, checkErr
	}
	artifact, err := s.LoadEvidence(ctx, ref)
	if err != nil {
		return time.Time{}, err
	}
	return ref.RetainUntil, r.ValidateOutput(artifact.Data)
}
