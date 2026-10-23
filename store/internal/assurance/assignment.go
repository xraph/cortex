package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	kindAssignment         = "assignment"
	kindAssignmentRevision = "assignment_revision"
)

var _ coordination.AssignmentStore = (*Repository)(nil)

func assignmentKey(identifier id.ID, revision int64) string {
	return fmt.Sprintf("%s:%020d", identifier.String(), revision)
}
func assignmentRecord(a coordination.Assignment) (Record, error) {
	kind, key, parent := kindAssignment, a.Handoff.AssignmentID.String(), a.Handoff.PlanID.String()
	if a.Revision > 1 {
		kind, key, parent = kindAssignmentRevision, assignmentKey(a.Handoff.AssignmentID, a.Revision), a.Handoff.AssignmentID.String()
	}
	return record(kind, key, a.Digest, parent, a.Handoff.Scope, a)
}
func decodeAssignment(row Record) (coordination.Assignment, error) {
	var a coordination.Assignment
	if row.Deleted || len(row.Body) > coordination.MaxAssignmentBytes || json.Unmarshal([]byte(row.Body), &a) != nil || a.Validate() != nil {
		return a, coordination.ErrInvalidAssignment
	}
	expected, err := assignmentRecord(a)
	if err != nil || row.Kind != expected.Kind || row.ID != expected.ID || row.RunID != expected.RunID || row.Digest != expected.Digest || row.ScopeCanon != expected.ScopeCanon {
		return a, coordination.ErrInvalidAssignment
	}
	return a, nil
}
func (s *Repository) assignmentRevision(ctx context.Context, identifier id.ID, revision int64) (coordination.Assignment, error) {
	if identifier.IsNil() || identifier.Prefix() != coordination.PrefixAssignment || revision < 1 {
		return coordination.Assignment{}, coordination.ErrInvalidAssignment
	}
	query := Query{Kind: kindAssignment, ID: identifier.String()}
	if revision > 1 {
		query.Kind, query.ID = kindAssignmentRevision, assignmentKey(identifier, revision)
	}
	row, err := s.get(ctx, query)
	if errors.Is(err, ErrAbsent) {
		return coordination.Assignment{}, coordination.ErrAssignmentNotFound
	}
	if err != nil {
		return coordination.Assignment{}, err
	}
	return decodeAssignment(row)
}

// ProposeAssignment retains a plan-bound proposal, without accepting its grants.
func (s *Repository) ProposeAssignment(ctx context.Context, h coordination.Handoff) (coordination.Assignment, error) {
	if err := require(ctx, h.Scope); err != nil {
		return coordination.Assignment{}, err
	}
	if h.Validate() != nil {
		return coordination.Assignment{}, coordination.ErrInvalidHandoff
	}
	frozen, err := coordination.SealHandoff(h)
	if err != nil {
		return coordination.Assignment{}, err
	}
	var accepted coordination.Assignment
	err = s.planTransaction(ctx, frozen.PlanID, func(tc context.Context, tx *Repository) error {
		existing, readErr := tx.assignmentRevision(tc, frozen.AssignmentID, 1)
		if readErr == nil {
			if existing.Handoff.Digest != frozen.Digest {
				return coordination.ErrAssignmentConflict
			}
			accepted = existing
			return nil
		}
		if !errors.Is(readErr, coordination.ErrAssignmentNotFound) {
			return readErr
		}
		p, readErr := tx.GetPlan(tc, frozen.PlanID)
		if readErr != nil {
			return readErr
		}
		at := s.clock.Now()
		if checkErr := coordination.ValidateHandoffPlan(frozen, p, at); checkErr != nil {
			return checkErr
		}
		next, buildErr := coordination.Propose(frozen, at)
		if buildErr != nil {
			return buildErr
		}
		row, buildErr := assignmentRecord(next)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		accepted = next
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrAssignmentConflict
	}
	if err != nil {
		return coordination.Assignment{}, err
	}
	return accepted, nil
}

// AdvanceAssignment appends progress and its audit fields in a single row.
func (s *Repository) AdvanceAssignment(ctx context.Context, change coordination.AssignmentChange) (coordination.Assignment, error) {
	frozen, digest, err := change.Freeze()
	change = frozen
	if err != nil {
		return coordination.Assignment{}, err
	}
	initial, err := s.assignmentRevision(ctx, change.AssignmentID, 1)
	if err != nil {
		return coordination.Assignment{}, err
	}
	var accepted coordination.Assignment
	err = s.planTransaction(ctx, initial.Handoff.PlanID, func(tc context.Context, tx *Repository) error {
		existing, readErr := tx.assignmentRevision(tc, change.AssignmentID, change.ExpectedRevision+1)
		if readErr == nil {
			if existing.RequestDigest != digest {
				return coordination.ErrAssignmentConflict
			}
			accepted = existing
			return nil
		}
		if !errors.Is(readErr, coordination.ErrAssignmentNotFound) {
			return readErr
		}
		previous, readErr := tx.GetAssignment(tc, change.AssignmentID)
		if readErr != nil {
			return readErr
		}
		at := s.clock.Now()
		next, advanceErr := coordination.Advance(previous, change, at)
		if advanceErr != nil {
			return advanceErr
		}
		if !next.State.Terminal() {
			p, planErr := tx.GetPlan(tc, initial.Handoff.PlanID)
			if planErr != nil {
				return planErr
			}
			if checkErr := coordination.ValidateHandoffPlan(next.Handoff, p, at); checkErr != nil {
				return checkErr
			}
		}
		if next.State == coordination.AssignmentSubmitted {
			if submissionErr := tx.validateAssignmentSubmission(tc, next, at); submissionErr != nil {
				return submissionErr
			}
		}
		row, buildErr := assignmentRecord(next)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		accepted = next
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrAssignmentConflict
	}
	if err != nil {
		return coordination.Assignment{}, err
	}
	return accepted, nil
}

// GetAssignment returns the latest committed observation at the exact scope.
func (s *Repository) GetAssignment(ctx context.Context, identifier id.ID) (coordination.Assignment, error) {
	initial, err := s.assignmentRevision(ctx, identifier, 1)
	if err != nil {
		return coordination.Assignment{}, err
	}
	row, err := s.get(ctx, Query{Kind: kindAssignmentRevision, RunID: identifier.String(), Reverse: true})
	if errors.Is(err, ErrAbsent) {
		return initial, nil
	}
	if err != nil {
		return coordination.Assignment{}, err
	}
	latest, err := decodeAssignment(row)
	if err != nil {
		return coordination.Assignment{}, err
	}
	if latest.Handoff.Digest != initial.Handoff.Digest {
		return coordination.Assignment{}, coordination.ErrInvalidAssignment
	}
	// Verify the local chain edge without reading an unbounded history.
	previous, err := s.assignmentRevision(ctx, identifier, latest.Revision-1)
	if err != nil {
		return coordination.Assignment{}, err
	}
	if edgeErr := validateAssignmentEdge(previous, latest); edgeErr != nil {
		return coordination.Assignment{}, edgeErr
	}
	return latest, nil
}
func validateAssignmentEdge(previous, next coordination.Assignment) error {
	change := coordination.AssignmentChange{AssignmentID: next.Handoff.AssignmentID, ExpectedRevision: next.Revision - 1, HandoffDigest: next.Handoff.Digest, Fence: next.Handoff.Fence, State: next.State, Actor: next.Actor, Reason: next.Reason}
	if next.State == coordination.AssignmentSubmitted {
		change.Submission = next.Submission
	}
	rebuilt, err := coordination.Advance(previous, change, next.At)
	if err != nil || rebuilt.Digest != next.Digest {
		return coordination.ErrInvalidAssignment
	}
	return nil
}

// ListAssignments pages plan assignments. Each current observation may advance
// independently during this non-snapshot read.
func (s *Repository) ListAssignments(ctx context.Context, req coordination.AssignmentListRequest) ([]coordination.Assignment, error) {
	if req.PlanID.IsNil() || req.PlanID.Prefix() != planning.PrefixPlan || req.Limit < 1 || req.Limit > 256 || (!req.After.IsNil() && req.After.Prefix() != coordination.PrefixAssignment) {
		return nil, coordination.ErrInvalidAssignment
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindAssignment, RunID: req.PlanID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Assignment, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		initial, decodeErr := decodeAssignment(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		current, readErr := s.GetAssignment(ctx, initial.Handoff.AssignmentID)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, current)
	}
	return result, nil
}

// AssignmentHistory returns a bounded, contiguous page of immutable observations.
func (s *Repository) AssignmentHistory(ctx context.Context, req coordination.AssignmentHistoryRequest) ([]coordination.Assignment, error) {
	if req.AfterRevision < 0 || req.Limit < 1 || req.Limit > 256 {
		return nil, coordination.ErrInvalidAssignment
	}
	initial, err := s.assignmentRevision(ctx, req.AssignmentID, 1)
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Assignment, 0, req.Limit)
	previous := initial
	if req.AfterRevision == 0 {
		result = append(result, initial)
	} else {
		previous, err = s.assignmentRevision(ctx, req.AssignmentID, req.AfterRevision)
		if err != nil {
			return nil, err
		}
	}
	if len(result) == req.Limit {
		return result, nil
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindAssignmentRevision, RunID: req.AssignmentID.String(), After: assignmentKey(req.AssignmentID, previous.Revision), Limit: req.Limit - len(result)})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		next, decodeErr := decodeAssignment(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if next.Handoff.Digest != initial.Handoff.Digest || validateAssignmentEdge(previous, next) != nil {
			return nil, coordination.ErrInvalidAssignment
		}
		result = append(result, next)
		previous = next
	}
	return result, nil
}
