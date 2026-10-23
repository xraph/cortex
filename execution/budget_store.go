package execution

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// SettlementReceipt retains the final accounting request and observation time.
// Its digest proves integrity only; the host authenticates usage evidence.
type SettlementReceipt struct {
	Request Settlement `json:"request"`
	Digest  string     `json:"digest"`
	At      time.Time  `json:"at"`
}

// Reservation retains immutable allocation and optional final accounting.
type Reservation struct {
	Request       ReservationRequest `json:"request"`
	RequestDigest string             `json:"request_digest"`
	CreatedAt     time.Time          `json:"created_at"`
	Settlement    *SettlementReceipt `json:"settlement,omitempty"`
}

// Validate checks identity and timestamps without authenticating receipts.
func (r Reservation) Validate() error {
	_, digest, err := r.Request.Freeze()
	if err != nil || digest != r.RequestDigest || r.CreatedAt.IsZero() || !r.CreatedAt.Before(r.Request.Deadline) {
		return ErrInvalidReservation
	}
	if r.Settlement != nil {
		_, digest, err = r.Settlement.Request.Freeze()
		if err != nil || r.Settlement.Digest != digest || r.Settlement.Request.ReservationID != r.Request.ID || r.Settlement.Request.RequestDigest != r.RequestDigest || r.Settlement.At.Before(r.CreatedAt) {
			return ErrInvalidReservation
		}
	}
	return nil
}

// BudgetSnapshot reports conservative charges. Overflow means exact aggregate
// arithmetic exceeded int64; raw settlement amounts remain retained separately.
type BudgetSnapshot struct {
	Policy       ReservationPolicy `json:"policy"`
	Charged      Resources         `json:"charged"`
	Overflow     bool              `json:"overflow"`
	Reservations int               `json:"reservations"`
}

// BudgetStore is optional shared accounting. Callers provide authenticated usage
// and bounded provider estimates; neither reservations nor receipts grant access.
// Branches settle only after every child has final accounting. Unknown usage is
// charged pessimistically and never refunded merely because its lease expired.
type BudgetStore interface {
	CreateReservationPolicy(context.Context, ReservationPolicy) error
	GetReservationPolicy(context.Context, id.ID) (ReservationPolicy, error)
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	Settle(context.Context, Settlement) error
	GetReservation(context.Context, id.ID) (Reservation, error)
	ListReservations(context.Context, ReservationListRequest) ([]Reservation, error)
	GetBudgetSnapshot(context.Context, id.ID) (BudgetSnapshot, error)
}

// ReservationListRequest bounds discovery for an exact-scope plan account.
type ReservationListRequest struct {
	PlanID id.ID
	After  id.ID
	Limit  int
}
