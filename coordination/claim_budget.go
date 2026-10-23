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
