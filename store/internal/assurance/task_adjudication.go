package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const kindTaskAdjudication = "task_adjudication"

var _ coordination.TaskAdjudicationStore = (*Repository)(nil)

// GetTaskAdjudication reads a historical decision under exact caller scope.
func (s *Repository) GetTaskAdjudication(ctx context.Context, identifier id.ID) (coordination.TaskAdjudication, error) {
	if identifier.IsNil() || identifier.Prefix() != coordination.PrefixTaskAdjudication {
		return coordination.TaskAdjudication{}, coordination.ErrInvalidReview
	}
	row, err := s.get(ctx, Query{Kind: kindTaskAdjudication, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return coordination.TaskAdjudication{}, coordination.ErrReviewNotFound
	}
	if err != nil {
		return coordination.TaskAdjudication{}, err
	}
	var a coordination.TaskAdjudication
	if row.Deleted || len(row.Body) > coordination.MaxTaskAdjudicationBytes || json.Unmarshal([]byte(row.Body), &a) != nil || a.Validate() != nil || row.Kind != kindTaskAdjudication || row.ID != a.Request.ID.String() || row.RunID != a.Request.PlanID.String() || row.Digest != a.Digest {
		return coordination.TaskAdjudication{}, coordination.ErrInvalidReview
	}
	return a, nil
}

// RecordTaskAdjudication derives policy from retained independent execution.
// Current host authority must surround this call, including historical retries.
func (s *Repository) RecordTaskAdjudication(ctx context.Context, request coordination.TaskAdjudicationRequest) (coordination.TaskAdjudication, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskAdjudication{}, err
	}
	var result coordination.TaskAdjudication
	err = s.planTransaction(ctx, req.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskAdjudication{}
		cached, readErr := tx.GetTaskAdjudication(tc, req.ID)
		if readErr == nil {
			if cached.RequestDigest != digest {
				return coordination.ErrReviewConflict
			}
			result = cached
			return nil
		}
		if !errors.Is(readErr, coordination.ErrReviewNotFound) {
			return readErr
		}
		p, readErr := tx.GetPlan(tc, req.PlanID)
		if readErr != nil {
			return readErr
		}
		review, readErr := tx.GetAssignmentReview(tc, req.ReviewID)
		if readErr != nil {
			return readErr
		}
		if checkErr := tx.validateReviewExecution(tc, review); checkErr != nil {
			return checkErr
		}
		saved, checkErr := coordination.PrepareTaskAdjudication(req, review, p, s.clock.Now())
		if checkErr != nil {
			return checkErr
		}
		row, buildErr := record(kindTaskAdjudication, req.ID.String(), saved.Digest, p.ID.String(), p.Scope, saved)
		if buildErr != nil {
			return buildErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		if _, checkErr = coordination.PrepareTaskAdjudication(req, review, p, s.clock.Now()); checkErr != nil {
			return checkErr
		}
		result = saved
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrReviewConflict
	}
	if err != nil {
		return coordination.TaskAdjudication{}, err
	}
	if err := ctx.Err(); err != nil {
		return coordination.TaskAdjudication{}, err
	}
	return result, nil
}

func (s *Repository) validateTaskAdjudication(ctx context.Context, p planning.Plan, req coordination.TaskAcceptanceRequest) (time.Time, error) {
	var policy *planning.ReviewRequirement
	for _, task := range p.Tasks {
		if task.ID == req.Claim.TaskID {
			policy = task.Review
			break
		}
	}
	if policy == nil {
		if req.Adjudication != nil {
			return time.Time{}, coordination.ErrInvalidTaskAcceptance
		}
		return time.Time{}, nil
	}
	closed, closureErr := s.GetTaskCompletionSettlement(ctx, req.AssignmentID)
	if closureErr != nil {
		return time.Time{}, closureErr
	}
	if closed != nil && closed.Abandonment != nil {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	if req.SchemaVersion != 2 || req.Adjudication == nil {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	a, err := s.GetTaskAdjudication(ctx, req.Adjudication.ID)
	if err != nil {
		return time.Time{}, err
	}
	bindingDigest, err := req.Current.Digest()
	if err != nil {
		return time.Time{}, err
	}
	ar := a.Request
	if a.Digest != req.Adjudication.Digest || a.Decision != coordination.AdjudicationSupported || ar.PlanID != p.ID || ar.PlanDigest != p.Digest || ar.TaskID != req.Claim.TaskID || ar.AssignmentID != req.AssignmentID || ar.AssignmentRevision != req.AssignmentRevision || ar.AssignmentDigest != req.AssignmentDigest || ar.BindingDigest != bindingDigest || ar.PolicyRevision != policy.PolicyRevision {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	review, err := s.GetAssignmentReview(ctx, ar.ReviewID)
	if err != nil {
		return time.Time{}, err
	}
	if checkErr := s.validateReviewExecution(ctx, review); checkErr != nil {
		return time.Time{}, checkErr
	}
	observed, err := coordination.PrepareTaskAdjudication(ar, review, p, s.clock.Now())
	if err != nil || observed.Decision != coordination.AdjudicationSupported {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	// A supported review cannot hide another retained contradiction. Bound the
	// total scan and fail closed if more observations need explicit resolution.
	reviews, err := s.ListAssignmentReviews(ctx, coordination.ReviewListRequest{AssignmentID: req.AssignmentID, Limit: 256})
	if err != nil {
		return time.Time{}, err
	}
	if len(reviews) == 256 {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	for _, other := range reviews {
		if other.SubjectDigest != req.AssignmentDigest {
			continue
		}
		for _, finding := range other.Findings {
			if finding.Assessment != coordination.ReviewSupported {
				return time.Time{}, coordination.ErrInvalidTaskAcceptance
			}
		}
	}
	deadline := review.Handoff.Deadline
	candidate, private, err := s.loadPrivateCandidate(ctx, review.RunID)
	if err != nil {
		return time.Time{}, err
	}
	if private && candidate.Candidate.RetainUntil.Before(deadline) {
		deadline = candidate.Candidate.RetainUntil
	}
	if !s.clock.Now().Before(deadline) {
		return time.Time{}, coordination.ErrInvalidTaskAcceptance
	}
	return deadline, ctx.Err()
}
