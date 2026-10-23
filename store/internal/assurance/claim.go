package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	kindTaskClaim          = "task_claim"
	kindTaskClaimRevision  = "task_claim_revision"
	kindTaskClaimOperation = "task_claim_operation"
	kindTaskDispatch       = "task_dispatch"
	kindTaskClaimIdentity  = "task_claim_identity"
)

var _ coordination.ClaimStore = (*Repository)(nil)

func claimKey(planID, taskID id.ID) string { return planID.String() + ":" + taskID.String() }
func claimRevisionKey(planID, taskID id.ID, revision int64) string {
	return fmt.Sprintf("%s:%020d", claimKey(planID, taskID), revision)
}
func claimIDs(planID, taskID id.ID) bool {
	return !planID.IsNil() && planID.Prefix() == planning.PrefixPlan && !taskID.IsNil() && taskID.Prefix() == planning.PrefixTask
}
func claimRecord(c coordination.Claim) (Record, error) {
	kind, key, parent := kindTaskClaim, claimKey(c.PlanID, c.TaskID), c.PlanID.String()
	if c.Revision > 1 {
		kind, key, parent = kindTaskClaimRevision, claimRevisionKey(c.PlanID, c.TaskID, c.Revision), claimKey(c.PlanID, c.TaskID)
	}
	return record(kind, key, c.Digest, parent, c.Scope, c)
}
func decodeClaim(row Record) (coordination.Claim, error) {
	var c coordination.Claim
	if row.Deleted || len(row.Body) > coordination.MaxClaimBytes || json.Unmarshal([]byte(row.Body), &c) != nil || c.Validate() != nil {
		return c, coordination.ErrInvalidClaim
	}
	expected, err := claimRecord(c)
	if err != nil || row.Kind != expected.Kind || row.ID != expected.ID || row.RunID != expected.RunID || row.ScopeCanon != expected.ScopeCanon || row.Digest != expected.Digest {
		return c, coordination.ErrInvalidClaim
	}
	return c, nil
}
func (s *Repository) claimRevision(ctx context.Context, planID, taskID id.ID, revision int64) (coordination.Claim, error) {
	if !claimIDs(planID, taskID) || revision < 1 {
		return coordination.Claim{}, coordination.ErrInvalidClaim
	}
	query := Query{Kind: kindTaskClaim, ID: claimKey(planID, taskID)}
	if revision > 1 {
		query.Kind, query.ID = kindTaskClaimRevision, claimRevisionKey(planID, taskID, revision)
	}
	row, err := s.get(ctx, query)
	if errors.Is(err, ErrAbsent) {
		return coordination.Claim{}, coordination.ErrClaimNotFound
	}
	if err != nil {
		return coordination.Claim{}, err
	}
	return decodeClaim(row)
}

// GetTaskClaim returns the latest ownership observation, even after replanning.
func (s *Repository) GetTaskClaim(ctx context.Context, planID, taskID id.ID) (coordination.Claim, error) {
	initial, err := s.claimRevision(ctx, planID, taskID, 1)
	if err != nil {
		return coordination.Claim{}, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindTaskClaimRevision, RunID: claimKey(planID, taskID), Reverse: true, Limit: 1})
	if err != nil {
		return coordination.Claim{}, err
	}
	if len(rows) == 0 {
		return initial, nil
	}
	if checkErr := validateRecord(ctx, rows[0]); checkErr != nil {
		return coordination.Claim{}, checkErr
	}
	current, err := decodeClaim(rows[0])
	if err != nil {
		return coordination.Claim{}, err
	}
	previous, err := s.claimRevision(ctx, planID, taskID, current.Revision-1)
	if err != nil {
		return coordination.Claim{}, err
	}
	if coordination.ValidateClaimSuccessor(previous, current) != nil {
		return coordination.Claim{}, coordination.ErrInvalidClaim
	}
	return current, nil
}

// ListTaskClaims includes removed tasks so their dispatches remain recoverable.
func (s *Repository) ListTaskClaims(ctx context.Context, req coordination.ClaimListRequest) ([]coordination.Claim, error) {
	if req.PlanID.IsNil() || req.PlanID.Prefix() != planning.PrefixPlan || req.Limit < 1 || req.Limit > 256 || (!req.AfterTaskID.IsNil() && req.AfterTaskID.Prefix() != planning.PrefixTask) {
		return nil, coordination.ErrInvalidClaim
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	query := Query{Kind: kindTaskClaim, RunID: req.PlanID.String(), Limit: req.Limit}
	if !req.AfterTaskID.IsNil() {
		query.After = claimKey(req.PlanID, req.AfterTaskID)
	}
	rows, err := s.driver.Select(ctx, query)
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Claim, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		initial, decodeErr := decodeClaim(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if initial.PlanID != req.PlanID {
			return nil, coordination.ErrInvalidClaim
		}
		current, readErr := s.GetTaskClaim(ctx, req.PlanID, initial.TaskID)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, current)
	}
	return result, nil
}

// TaskClaimHistory reads contiguous immutable ownership revisions.
func (s *Repository) TaskClaimHistory(ctx context.Context, req coordination.ClaimHistoryRequest) ([]coordination.Claim, error) {
	if !claimIDs(req.PlanID, req.TaskID) || req.AfterRevision < 0 || req.AfterRevision == math.MaxInt64 || req.Limit < 1 || req.Limit > 256 {
		return nil, coordination.ErrInvalidClaim
	}
	initial, err := s.claimRevision(ctx, req.PlanID, req.TaskID, 1)
	if err != nil {
		return nil, err
	}
	previous := initial
	result := make([]coordination.Claim, 0, req.Limit)
	if req.AfterRevision == 0 {
		result = append(result, initial)
	} else if req.AfterRevision > 1 {
		previous, err = s.claimRevision(ctx, req.PlanID, req.TaskID, req.AfterRevision)
		if err != nil {
			return nil, err
		}
	}
	if len(result) == req.Limit {
		return result, nil
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindTaskClaimRevision, RunID: claimKey(req.PlanID, req.TaskID), After: claimRevisionKey(req.PlanID, req.TaskID, previous.Revision), Limit: req.Limit - len(result)})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		current, decodeErr := decodeClaim(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if coordination.ValidateClaimSuccessor(previous, current) != nil {
			return nil, coordination.ErrInvalidClaim
		}
		result = append(result, current)
		previous = current
	}
	return result, nil
}

type claimOperation struct {
	Kind     string                     `json:"kind"`
	Request  *coordination.ClaimRequest `json:"request,omitempty"`
	Token    coordination.ClaimToken    `json:"token"`
	Duration time.Duration              `json:"duration,omitempty"`
	RunID    id.AgentRunID              `json:"run_id,omitempty"`
}

func (op claimOperation) digest() (string, error) {
	data, err := json.Marshal(op)
	if err != nil {
		return "", err
	}
	return evidence.Digest(data), nil
}

// AcquireTaskClaim reserves one owner. This is not the readiness-aware scheduler
// admission API: callers must not dispatch solely because reservation succeeds.
func (s *Repository) AcquireTaskClaim(ctx context.Context, req coordination.ClaimRequest) (coordination.Claim, error) {
	frozen, _, err := req.Freeze()
	if err != nil {
		return coordination.Claim{}, err
	}
	result, _, err := s.mutateClaim(ctx, req.PlanID, req.TaskID, claimOperation{Kind: "acquire", Request: &frozen})
	return result, err
}

// RenewTaskClaim extends only the current live token on the current plan.
func (s *Repository) RenewTaskClaim(ctx context.Context, token coordination.ClaimToken, duration time.Duration) (coordination.Claim, error) {
	if token.Validate() != nil || duration <= 0 || duration > coordination.MaxClaimDuration {
		return coordination.Claim{}, coordination.ErrInvalidClaim
	}
	result, _, err := s.mutateClaim(ctx, token.PlanID, token.TaskID, claimOperation{Kind: "renew", Token: token, Duration: duration})
	return result, err
}

// BeginTaskDispatch retains intent before the host invokes the reserved run.
func (s *Repository) BeginTaskDispatch(ctx context.Context, token coordination.ClaimToken, runID id.AgentRunID) (coordination.DispatchAdmission, error) {
	if token.Validate() != nil || runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return coordination.DispatchAdmission{}, coordination.ErrInvalidClaim
	}
	result, fresh, err := s.mutateClaim(ctx, token.PlanID, token.TaskID, claimOperation{Kind: "dispatch", Token: token, RunID: runID})
	if err != nil {
		return coordination.DispatchAdmission{}, err
	}
	return coordination.DispatchAdmission{Claim: result, Fresh: fresh}, nil
}

// ExpireTaskClaim releases an unused reservation or marks dispatch for recovery.
func (s *Repository) ExpireTaskClaim(ctx context.Context, planID, taskID id.ID, expected int64) (coordination.Claim, error) {
	if expected < 1 {
		return coordination.Claim{}, coordination.ErrInvalidClaim
	}
	result, _, err := s.mutateClaim(ctx, planID, taskID, claimOperation{Kind: "expire", Token: coordination.ClaimToken{PlanID: planID, TaskID: taskID, Revision: expected}})
	return result, err
}

func (s *Repository) mutateClaim(ctx context.Context, planID, taskID id.ID, op claimOperation) (coordination.Claim, bool, error) {
	if !claimIDs(planID, taskID) {
		return coordination.Claim{}, false, coordination.ErrInvalidClaim
	}
	digest, err := op.digest()
	if err != nil {
		return coordination.Claim{}, false, err
	}
	var result coordination.Claim
	fresh := false
	err = s.planTransaction(ctx, planID, func(tc context.Context, tx *Repository) error {
		result = coordination.Claim{}
		fresh = false
		cached, readErr := tx.get(tc, Query{Kind: kindTaskClaimOperation, ID: digest})
		if readErr == nil {
			if cached.Kind != kindTaskClaimOperation || cached.ID != digest || cached.Deleted || len(cached.Body) > coordination.MaxClaimBytes || json.Unmarshal([]byte(cached.Body), &result) != nil || result.Validate() != nil || cached.Digest != evidence.Digest([]byte(cached.Body)) || cached.RunID != claimKey(planID, taskID) || result.PlanID != planID || result.TaskID != taskID || result.Scope.Canonical() != cached.ScopeCanon {
				return coordination.ErrInvalidClaim
			}
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		current, readErr := tx.GetTaskClaim(tc, planID, taskID)
		if readErr != nil && !errors.Is(readErr, coordination.ErrClaimNotFound) {
			return readErr
		}
		p, planErr := tx.GetPlan(tc, planID)
		if planErr != nil {
			return planErr
		}
		at := s.clock.Now()
		var next coordination.Claim
		var changeErr error
		if op.Kind == "acquire" {
			var previous *coordination.Claim
			if readErr == nil {
				previous = &current
			}
			next, changeErr = coordination.AcquireClaim(previous, *op.Request, p, at)
		} else {
			if readErr != nil {
				return readErr
			}
			if op.Kind != "expire" && (current.PlanDigest != p.Digest || current.PlanRevision != p.Revision) {
				return coordination.ErrClaimConflict
			}
			switch op.Kind {
			case "renew":
				next, changeErr = coordination.RenewClaim(current, op.Token, op.Duration, at)
			case "dispatch":
				next, changeErr = coordination.BeginDispatch(current, op.Token, op.RunID, at)
			case "expire":
				next, changeErr = coordination.ExpireClaim(current, op.Token.Revision, at)
			default:
				return coordination.ErrInvalidClaim
			}
		}
		if changeErr != nil {
			return changeErr
		}
		if op.Kind == "acquire" {
			identity, identityErr := record(kindTaskClaimIdentity, next.ID.String(), digest, claimKey(planID, taskID), next.Scope, next)
			if identityErr != nil {
				return identityErr
			}
			if insertErr := tx.driver.Insert(tc, identity); insertErr != nil {
				return insertErr
			}
		}
		if op.Kind == "dispatch" {
			link, linkErr := record(kindTaskDispatch, op.RunID.String(), next.Digest, claimKey(planID, taskID), next.Scope, next)
			if linkErr != nil {
				return linkErr
			}
			if insertErr := tx.driver.Insert(tc, link); insertErr != nil {
				return insertErr
			}
		}
		row, buildErr := claimRecord(next)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		data, encodeErr := json.Marshal(next)
		if encodeErr != nil {
			return encodeErr
		}
		acknowledgment, buildErr := record(kindTaskClaimOperation, digest, evidence.Digest(data), claimKey(planID, taskID), next.Scope, next)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, acknowledgment); insertErr != nil {
			return insertErr
		}
		observed := s.clock.Now()
		if observed.IsZero() || observed.Before(at) || (op.Kind != "expire" && !observed.Before(next.ExpiresAt)) {
			return coordination.ErrClaimConflict
		}
		result = next
		fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrClaimConflict
	}
	if err != nil {
		return coordination.Claim{}, false, err
	}
	return result, fresh, nil
}
