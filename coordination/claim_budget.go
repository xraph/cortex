package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// BudgetedClaimRequest reserves ownership and capacity in one transaction. The
// reservation's OwnerRef must be the claim ID. Neither record grants authority
// or proves task readiness; the host must establish those before dispatch.
type BudgetedClaimRequest struct {
	Claim       ClaimRequest                 `json:"claim"`
	Reservation execution.ReservationRequest `json:"reservation"`
}

// Freeze detaches the request and checks that both reservations name one attempt.
func (r BudgetedClaimRequest) Freeze() (BudgetedClaimRequest, string, error) {
	claim, _, err := r.Claim.Freeze()
	if err != nil {
		return BudgetedClaimRequest{}, "", err
	}
	reservation, _, err := r.Reservation.Freeze()
	if err != nil {
		return BudgetedClaimRequest{}, "", err
	}
	if reservation.PlanID != claim.PlanID || reservation.PlanDigest != claim.PlanDigest || reservation.TaskID != claim.TaskID || reservation.OwnerRef != claim.ID.String() || reservation.Kind != execution.ReservationLeaf {
		return BudgetedClaimRequest{}, "", ErrInvalidClaim
	}
	frozen := BudgetedClaimRequest{Claim: claim, Reservation: reservation}
	data, err := json.Marshal(frozen)
	if err != nil {
		return BudgetedClaimRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// BudgetedClaim retains the original acquisition, including its allocation.
// Fresh is false on every retry, even if the original response was lost. These
// historical snapshots do not reflect later renewals or settlements.
type BudgetedClaim struct {
	Claim       Claim                 `json:"claim"`
	Reservation execution.Reservation `json:"reservation"`
	Fresh       bool                  `json:"-"`
}

// BudgetedClaimStore atomically reserves one task owner and its capacity. It is
// an accounting primitive, not ClaimReadyTask or a runtime dispatch API.
type BudgetedClaimStore interface {
	AcquireBudgetedTaskClaim(context.Context, BudgetedClaimRequest) (BudgetedClaim, error)
	GetBudgetedTaskClaim(context.Context, id.ID) (BudgetedClaim, error)
}

// Validate checks that the retained acquisition matches its original request.
// This proves neither current ownership nor permission to spend the allocation.
func (a BudgetedClaim) Validate(request BudgetedClaimRequest) error {
	req, _, err := request.Freeze()
	if err != nil {
		return ErrInvalidClaim
	}
	c, r := a.Claim, a.Reservation
	_, reservationDigest, err := req.Reservation.Freeze()
	if err != nil || c.Validate() != nil || r.Validate() != nil || c.ID != req.Claim.ID || c.PlanID != req.Claim.PlanID || c.PlanRevision != req.Claim.PlanRevision || c.PlanDigest != req.Claim.PlanDigest || c.TaskID != req.Claim.TaskID || c.Worker != req.Claim.Worker || c.GrantRef != req.Claim.GrantRef || c.Revision != req.Claim.ExpectedRevision+1 || c.State != ClaimHeld || !c.At.Equal(c.AcquiredAt) || !c.ExpiresAt.Equal(minTime(c.At.Add(req.Claim.Duration), c.Deadline)) || r.RequestDigest != reservationDigest || r.Settlement != nil || r.CreatedAt.Before(c.At) || !r.CreatedAt.Before(c.ExpiresAt) || c.ExpiresAt.After(r.Request.Deadline) || c.Scope.Canonical() != r.Request.Scope.Canonical() {
		return ErrInvalidClaim
	}
	return nil
}
