package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

const kindTaskUsageClosure = "task_usage_closure"

var _ coordination.TaskUsageClosureStore = (*Repository)(nil)

// GetTaskUsageClosure reads historical closure, without granting access or funds.
func (s *Repository) GetTaskUsageClosure(ctx context.Context, runID id.AgentRunID) (*coordination.TaskUsageClosure, error) {
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return nil, coordination.ErrInvalidTaskUsage
	}
	row, err := s.get(ctx, Query{Kind: kindTaskUsageClosure, ID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var closed coordination.TaskUsageClosure
	if row.Deleted || len(row.Body) > 64<<10 || json.Unmarshal([]byte(row.Body), &closed) != nil || closed.Validate() != nil || closed.Request.Usage.Seal.Request.RunID != runID || row.Kind != kindTaskUsageClosure || row.ID != runID.String() || row.RunID != runID.String() || row.ScopeCanon != closed.Request.Usage.Seal.Request.Scope.Canonical() || row.Digest != closed.Digest {
		return nil, coordination.ErrInvalidTaskUsage
	}
	return &closed, nil
}

// CloseTaskUsage closes local verification admission after task acceptance or
// explicit termination or rejection, with terminal verifier jobs. Its caller must hold
// current host authority. Closure preserves unknown usage and does not prove
// external quiescence or refund funds.
func (s *Repository) CloseTaskUsage(ctx context.Context, request coordination.TaskUsageClosureRequest) (coordination.TaskUsageClosure, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskUsageClosure{}, err
	}
	if scopeErr := require(ctx, req.Usage.Seal.Request.Scope); scopeErr != nil {
		return coordination.TaskUsageClosure{}, scopeErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result coordination.TaskUsageClosure
	err = s.planTransaction(bounded, req.Usage.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskUsageClosure{}
		retained, readErr := tx.GetTaskUsageClosure(tc, req.Usage.Seal.Request.RunID)
		if readErr != nil {
			return readErr
		}
		if retained != nil {
			if retained.RequestDigest != digest {
				return coordination.ErrTaskUsageConflict
			}
			result = *retained
			return nil
		}
		snapshot, readErr := tx.readTaskUsageSnapshot(tc, req.Usage)
		if readErr != nil {
			return readErr
		}
		if !req.MatchesClaim(snapshot.Claim) || snapshot.Digest != req.UsageDigest {
			return coordination.ErrTaskUsageConflict
		}
		if terminalErr := tx.checkTaskUsageDisposition(tc, snapshot.Claim, req); terminalErr != nil {
			return terminalErr
		}
		for _, job := range snapshot.Jobs {
			if job.State == verification.JobQueued || job.State == verification.JobRunning || job.State == verification.JobReady {
				return coordination.ErrTaskUsageConflict
			}
		}
		closed, sealErr := coordination.SealTaskUsageClosure(coordination.TaskUsageClosure{SchemaVersion: 1, Request: req, RequestDigest: digest, At: s.clock.Now().UTC()})
		if sealErr != nil {
			return sealErr
		}
		runID := req.Usage.Seal.Request.RunID.String()
		row, encodeErr := record(kindTaskUsageClosure, runID, closed.Digest, runID, req.Usage.Seal.Request.Scope, closed)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		result = closed
		result.Fresh = true
		return nil
	})
	if err != nil {
		return coordination.TaskUsageClosure{}, err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return coordination.TaskUsageClosure{}, contextErr
	}
	return result, nil
}

// checkTaskUsageAdmission must run while holding the run transaction guard.
func (s *Repository) checkTaskUsageAdmission(ctx context.Context, runID id.AgentRunID) error {
	closed, err := s.GetTaskUsageClosure(ctx, runID)
	if err != nil {
		return err
	}
	if closed != nil {
		return coordination.ErrTaskUsageClosed
	}
	return nil
}
