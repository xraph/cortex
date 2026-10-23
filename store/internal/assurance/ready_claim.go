package assurance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const kindReadyTaskClaim = "task_ready_claim"

var _ coordination.ReadyTaskClaimStore = (*Repository)(nil)

type readyClaimRecord struct {
	Request coordination.ReadyTaskClaimRequest `json:"request"`
	Result  coordination.ReadyTaskClaim        `json:"result"`
}

func (s *Repository) getReadyClaim(ctx context.Context, claimID id.ID) (readyClaimRecord, error) {
	row, err := s.get(ctx, Query{Kind: kindReadyTaskClaim, ID: claimID.String()})
	if err != nil {
		return readyClaimRecord{}, err
	}
	var retained readyClaimRecord
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &retained) != nil {
		return readyClaimRecord{}, coordination.ErrInvalidClaim
	}
	req, digest, err := retained.Request.Freeze()
	result := retained.Result
	if err != nil || result.RequestDigest != digest || result.Validate(req) != nil || req.Acquisition.Claim.ID != claimID || row.Kind != kindReadyTaskClaim || row.ID != claimID.String() || row.RunID != req.Acquisition.Claim.PlanID.String() || row.ScopeCanon != req.Acquisition.Reservation.Scope.Canonical() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return readyClaimRecord{}, coordination.ErrInvalidClaim
	}
	budget, err := s.getClaimBudget(ctx, claimID)
	if err != nil {
		return readyClaimRecord{}, err
	}
	_, budgetDigest, err := req.Acquisition.Freeze()
	if err != nil || budget.RequestDigest != budgetDigest {
		return readyClaimRecord{}, coordination.ErrInvalidClaim
	}
	actual, err := json.Marshal(result.Acquisition)
	if err != nil {
		return readyClaimRecord{}, err
	}
	expected, err := json.Marshal(budget.Result)
	if err != nil {
		return readyClaimRecord{}, err
	}
	if !bytes.Equal(actual, expected) {
		return readyClaimRecord{}, coordination.ErrInvalidClaim
	}
	return retained, nil
}

// ClaimReadyTask reloads prerequisite evidence and reserves ownership, capacity
// and event intent under one plan transaction. Host guards must cover current
// authorization and external revisions; storage performs no external calls.
func (s *Repository) ClaimReadyTask(ctx context.Context, request coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
	if err := require(ctx, request.Acquisition.Reservation.Scope); err != nil {
		return coordination.ReadyTaskClaim{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.ReadyTaskClaim{}, err
	}
	var result coordination.ReadyTaskClaim
	err = s.planTransaction(ctx, req.Acquisition.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.ReadyTaskClaim{}
		cached, readErr := tx.getReadyClaim(tc, req.Acquisition.Claim.ID)
		if readErr == nil {
			if cached.Result.RequestDigest != digest {
				return coordination.ErrClaimConflict
			}
			result = cached.Result
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		p, readErr := tx.GetPlan(tc, req.Acquisition.Claim.PlanID)
		if readErr != nil {
			return readErr
		}
		if p.Digest != req.Acquisition.Claim.PlanDigest || p.Revision != req.Acquisition.Claim.PlanRevision {
			return coordination.ErrClaimConflict
		}
		started := s.clock.Now()
		materials, loadErr := tx.acceptedPrerequisiteMaterials(tc, p, req.Acquisition.Claim.TaskID, req.Dependencies, 32<<20)
		if loadErr != nil {
			return loadErr
		}
		observed := s.clock.Now()
		if started.IsZero() || observed.Before(started) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, req.Acquisition.Claim.TaskID, materials, observed); checkErr != nil {
			return checkErr
		}
		_, acquisitionDigest, freezeErr := req.Acquisition.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		acquisition, acquireErr := tx.acquireBudgetedClaimInTransaction(tc, req.Acquisition, acquisitionDigest)
		if acquireErr != nil {
			return acquireErr
		}
		if !acquisition.Fresh || acquisition.Claim.At.Before(observed) {
			return coordination.ErrClaimConflict
		}
		acquisition.Fresh = false
		admitted := coordination.ReadyTaskClaim{RequestDigest: digest, Acquisition: acquisition, Outbox: coordination.TaskClaimOutbox{EventID: id.New(coordination.PrefixTaskClaimedEvent), Type: "task_claimed", ClaimID: acquisition.Claim.ID, PlanID: p.ID, TaskID: acquisition.Claim.TaskID}}
		retained := readyClaimRecord{Request: req, Result: admitted}
		data, encodeErr := json.Marshal(retained)
		if encodeErr != nil {
			return encodeErr
		}
		if len(data) > 2<<20 {
			return coordination.ErrInvalidClaim
		}
		row, buildErr := record(kindReadyTaskClaim, acquisition.Claim.ID.String(), evidence.Digest(data), p.ID.String(), p.Scope, retained)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now()
		if finished.Before(acquisition.Reservation.CreatedAt) || !finished.Before(acquisition.Claim.ExpiresAt) || !finished.Before(acquisition.Reservation.Request.Deadline) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, acquisition.Claim.TaskID, materials, finished); checkErr != nil {
			return checkErr
		}
		result = admitted
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrClaimConflict
	}
	if err != nil {
		return coordination.ReadyTaskClaim{}, err
	}
	return result, nil
}

func validateReadyClaimMaterials(p planning.Plan, taskID id.ID, materials []taskAcceptanceMaterial, at time.Time) error {
	states, err := validateTaskMaterials(materials, at)
	if err != nil {
		return err
	}
	ready, err := planning.Ready(p, states, at)
	if err != nil {
		return err
	}
	if !slices.Contains(ready, taskID) {
		return coordination.ErrClaimConflict
	}
	return nil
}

// GetReadyTaskClaim returns a historical admission for recovery and event
// discovery. Fresh is false, including after a lost commit acknowledgment.
func (s *Repository) GetReadyTaskClaim(ctx context.Context, claimID id.ID) (coordination.ReadyTaskClaim, error) {
	if claimID.IsNil() || claimID.Prefix() != coordination.PrefixClaim {
		return coordination.ReadyTaskClaim{}, coordination.ErrInvalidClaim
	}
	retained, err := s.getReadyClaim(ctx, claimID)
	if errors.Is(err, ErrAbsent) {
		err = coordination.ErrClaimNotFound
	}
	if err != nil {
		return coordination.ReadyTaskClaim{}, err
	}
	return retained.Result, nil
}
