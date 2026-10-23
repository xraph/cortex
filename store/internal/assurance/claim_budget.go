package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

const kindTaskClaimBudget = "task_claim_budget"

var _ coordination.BudgetedClaimStore = (*Repository)(nil)

type claimBudgetRecord struct {
	Request       coordination.BudgetedClaimRequest `json:"request"`
	RequestDigest string                            `json:"request_digest"`
	Result        coordination.BudgetedClaim        `json:"result"`
}

func (s *Repository) getClaimBudget(ctx context.Context, claimID id.ID) (claimBudgetRecord, error) {
	row, err := s.get(ctx, Query{Kind: kindTaskClaimBudget, ID: claimID.String()})
	if err != nil {
		return claimBudgetRecord{}, err
	}
	var binding claimBudgetRecord
	if row.Deleted || len(row.Body) > 64<<10 || json.Unmarshal([]byte(row.Body), &binding) != nil {
		return claimBudgetRecord{}, coordination.ErrInvalidClaim
	}
	req, digest, err := binding.Request.Freeze()
	c, r := binding.Result.Claim, binding.Result.Reservation
	_, reservationDigest, reservationErr := req.Reservation.Freeze()
	if err != nil || reservationErr != nil || digest != binding.RequestDigest || c.Validate() != nil || r.Validate() != nil || c.ID != claimID || c.ID != req.Claim.ID || c.PlanID != req.Claim.PlanID || c.PlanRevision != req.Claim.PlanRevision || c.PlanDigest != req.Claim.PlanDigest || c.TaskID != req.Claim.TaskID || c.Worker != req.Claim.Worker || c.GrantRef != req.Claim.GrantRef || c.Revision != req.Claim.ExpectedRevision+1 || c.State != coordination.ClaimHeld || !c.At.Equal(c.AcquiredAt) || r.RequestDigest != reservationDigest || r.Settlement != nil || r.CreatedAt.Before(c.At) || !r.CreatedAt.Before(c.ExpiresAt) || c.ExpiresAt.After(r.Request.Deadline) || c.Scope.Canonical() != r.Request.Scope.Canonical() || row.Kind != kindTaskClaimBudget || row.ID != claimID.String() || row.RunID != claimKey(c.PlanID, c.TaskID) || row.ScopeCanon != c.Scope.Canonical() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return claimBudgetRecord{}, coordination.ErrInvalidClaim
	}
	return binding, nil
}

// AcquireBudgetedTaskClaim reserves a task owner and capacity atomically. It
// never adopts independently created records or returns fresh authority on retry.
func (s *Repository) AcquireBudgetedTaskClaim(ctx context.Context, request coordination.BudgetedClaimRequest) (coordination.BudgetedClaim, error) {
	if err := require(ctx, request.Reservation.Scope); err != nil {
		return coordination.BudgetedClaim{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.BudgetedClaim{}, err
	}
	var result coordination.BudgetedClaim
	err = s.planTransaction(ctx, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.BudgetedClaim{}
		cached, readErr := tx.getClaimBudget(tc, req.Claim.ID)
		if readErr == nil {
			if cached.RequestDigest != digest {
				return coordination.ErrClaimConflict
			}
			result = cached.Result
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		op := claimOperation{Kind: "acquire", Request: &req.Claim}
		opDigest, digestErr := op.digest()
		if digestErr != nil {
			return digestErr
		}
		claim, fresh, claimErr := tx.mutateClaimInTransaction(tc, req.Claim.PlanID, req.Claim.TaskID, op, opDigest)
		if claimErr != nil {
			return claimErr
		}
		if !fresh {
			return coordination.ErrClaimConflict
		}
		if claim.ExpiresAt.After(req.Reservation.Deadline) {
			return execution.ErrBudgetExceeded
		}
		_, reservationDigest, freezeErr := req.Reservation.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		reservation, fresh, reserveErr := tx.reserveInTransaction(tc, req.Reservation, reservationDigest)
		if reserveErr != nil {
			return reserveErr
		}
		if !fresh {
			return execution.ErrReservationConflict
		}
		if reservation.CreatedAt.Before(claim.At) {
			return coordination.ErrClaimConflict
		}
		retained := coordination.BudgetedClaim{Claim: claim, Reservation: reservation}
		binding := claimBudgetRecord{Request: req, RequestDigest: digest, Result: retained}
		data, encodeErr := json.Marshal(binding)
		if encodeErr != nil {
			return encodeErr
		}
		if len(data) > 64<<10 {
			return coordination.ErrInvalidClaim
		}
		row, buildErr := record(kindTaskClaimBudget, claim.ID.String(), evidence.Digest(data), claimKey(claim.PlanID, claim.TaskID), claim.Scope, binding)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		observed := s.clock.Now()
		if observed.IsZero() || observed.Before(reservation.CreatedAt) || !observed.Before(claim.ExpiresAt) || !observed.Before(reservation.Request.Deadline) {
			return coordination.ErrClaimConflict
		}
		result = retained
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrClaimConflict
	}
	if err != nil {
		return coordination.BudgetedClaim{}, err
	}
	return result, nil
}

// checkClaimBudget fences future renewal/dispatch through the ordinary claim
// API too. Historical operation acknowledgments still return Fresh=false.
func (s *Repository) checkClaimBudget(ctx context.Context, next coordination.Claim, at time.Time) error {
	binding, err := s.getClaimBudget(ctx, next.ID)
	if errors.Is(err, ErrAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	original := binding.Result.Claim
	if next.PlanID != original.PlanID || next.PlanDigest != original.PlanDigest || next.TaskID != original.TaskID || next.Worker != original.Worker || next.Fence != original.Fence {
		return coordination.ErrClaimConflict
	}
	reservation, err := s.GetReservation(ctx, binding.Request.Reservation.ID)
	if err != nil {
		return err
	}
	if reservation.RequestDigest != binding.Result.Reservation.RequestDigest {
		return coordination.ErrClaimConflict
	}
	if reservation.Settlement != nil || !at.Before(reservation.Request.Deadline) || next.ExpiresAt.After(reservation.Request.Deadline) {
		return execution.ErrBudgetExceeded
	}
	policy, _, err := s.effectiveReservationPolicy(ctx, next.PlanID)
	if err != nil {
		return err
	}
	reservations, err := s.accountReservations(ctx, next.PlanID)
	if err != nil {
		return err
	}
	return execution.CheckReservationBudget(policy, reservations, reservation.Request.ID, at)
}

// GetBudgetedTaskClaim returns the retained acquisition for recovery. Fresh is
// always false; callers read current claims and reservations before new work.
func (s *Repository) GetBudgetedTaskClaim(ctx context.Context, claimID id.ID) (coordination.BudgetedClaim, error) {
	if claimID.IsNil() || claimID.Prefix() != coordination.PrefixClaim {
		return coordination.BudgetedClaim{}, coordination.ErrInvalidClaim
	}
	binding, err := s.getClaimBudget(ctx, claimID)
	if errors.Is(err, ErrAbsent) {
		err = coordination.ErrClaimNotFound
	}
	if err != nil {
		return coordination.BudgetedClaim{}, err
	}
	return binding.Result, nil
}
