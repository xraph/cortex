package orchestration

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// PrefixCoordinator identifies one host-owned coordinator attempt.
const PrefixCoordinator id.Prefix = "oclaim"

// CoordinatorRequest claims one parent generation. Initial is valid only for a
// newly created running parent without prior ownership. Repeated requests never
// grant another execution, even when an acknowledgment was lost.
type CoordinatorRequest struct {
	TeamID             id.OrchestrationID
	Owner              id.ID
	ExpectedGeneration int64
	Duration           time.Duration
	Initial            bool
}

// CoordinatorLease retains immutable ownership history. Renewal advances
// Revision without changing Generation. A new owner advances both. A read is
// evidence only; dispatch also needs an acknowledged claim and current checks.
type CoordinatorLease struct {
	SchemaVersion  int                `json:"schema_version"`
	TeamID         id.OrchestrationID `json:"team_id"`
	Scope          cortex.Scope       `json:"scope"`
	Owner          id.ID              `json:"owner"`
	Generation     int64              `json:"generation"`
	Revision       int64              `json:"revision"`
	AcquiredAt     time.Time          `json:"acquired_at"`
	ExpiresAt      time.Time          `json:"expires_at"`
	Released       bool               `json:"released"`
	PreviousDigest string             `json:"previous_digest,omitempty"`
}

// Freeze validates and detaches a bounded ownership observation.
func (l CoordinatorLease) Freeze() (CoordinatorLease, string, error) {
	if l.SchemaVersion != 1 || l.TeamID.IsNil() || l.TeamID.Prefix() != id.PrefixOrchestration || cortex.ValidateScope(l.Scope) != nil || l.Owner.IsNil() || l.Owner.Prefix() != PrefixCoordinator || l.Generation < 2 || l.Revision < 1 || l.AcquiredAt.IsZero() || !l.ExpiresAt.After(l.AcquiredAt) || l.Revision == 1 && l.PreviousDigest != "" || l.Revision > 1 && !evidence.ValidDigest(l.PreviousDigest) {
		return CoordinatorLease{}, "", cortex.ErrInvalidContinuation
	}
	data, err := json.Marshal(l)
	if err != nil || len(data) > 16<<10 {
		return CoordinatorLease{}, "", cortex.ErrInvalidContinuation
	}
	var frozen CoordinatorLease
	if json.Unmarshal(data, &frozen) != nil {
		return CoordinatorLease{}, "", cortex.ErrInvalidContinuation
	}
	return frozen, evidence.Digest(data), nil
}

// CoordinatorClaim contains the parent snapshot acknowledged with ownership.
type CoordinatorClaim struct {
	Lease CoordinatorLease
	Run   *Run
}

// CoordinatorStore serializes ownership, parent fences and admission under one
// account lock. Lost acknowledgments grant no execution. Finish releases a
// waiting pass or atomically closes a terminal parent's account. The ordinary
// unowned mutation paths cannot modify a leased parent or admit its children.
type CoordinatorStore interface {
	AcquireOrchestrationCoordinator(context.Context, CoordinatorRequest) (CoordinatorClaim, error)
	GetOrchestrationCoordinator(context.Context, id.OrchestrationID) (CoordinatorLease, error)
	CheckOrchestrationCoordinator(context.Context, CoordinatorLease) (CoordinatorLease, error)
	RenewOrchestrationCoordinator(context.Context, CoordinatorLease, time.Duration) (CoordinatorLease, error)
	FinishOrchestrationCoordinator(context.Context, CoordinatorLease, *Run) error
	BeginCoordinatedInvocation(context.Context, CoordinatorLease, InvocationIntent) error
	ReserveCoordinatedChild(context.Context, CoordinatorLease, execution.ReservationRequest) (execution.Reservation, error)
}
