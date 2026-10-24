package coordination

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/verification"
)

// ClaimExpiryStore is the scoped storage capability needed for expiry. It cannot
// dispatch work, accept results, settle usage or grant a replacement worker.
// Implementations must honor cancellation and atomically compare revisions.
type ClaimExpiryStore interface {
	ListTaskClaims(context.Context, ClaimListRequest) ([]Claim, error)
	ExpireTaskClaim(context.Context, id.ID, id.ID, int64) (Claim, error)
}

// ClaimSweepPage contains acknowledged observations, not execution permission.
// NextTaskID is nonzero only for a full page. Continue with that cursor, then
// restart from the beginning after an empty cursor to revisit live or conflicting
// claims. Counts are per call and do not imply newly committed transitions.
type ClaimSweepPage struct {
	NextTaskID                               id.ID
	Scanned, Released, Recovering, Conflicts int
}

// ClaimSweeper expires at most one bounded page in an explicitly supplied scope.
// It releases unused claims and fences dispatched work into recovery. The caller
// owns discovery, periodic scheduling and cancellation. Recovering work requires
// provider and effect reconciliation before any retry or acceptance.
type ClaimSweeper struct {
	store ClaimExpiryStore
	clock verification.Clock
}

// NewClaimSweeper requires explicit storage and a trusted observation clock.
// Storage checks expiry against its own clock inside the revision transaction.
func NewClaimSweeper(store ClaimExpiryStore, clock verification.Clock) (*ClaimSweeper, error) {
	if hostport.IsNil(store) || hostport.IsNil(clock) {
		return nil, ErrInvalidClaim
	}
	return &ClaimSweeper{store: store, clock: clock}, nil
}

// Sweep validates the entire page before its first write. A concurrent renewal or
// expiry is a conflict to revisit on the next pass. Other errors return no cursor:
// some writes may have committed, so repeat the same page using fresh storage
// observations. An error or a historical acknowledgment never permits execution.
func (s *ClaimSweeper) Sweep(ctx context.Context, request ClaimListRequest) (ClaimSweepPage, error) {
	if err := ctx.Err(); err != nil {
		return ClaimSweepPage{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ClaimSweepPage{}, err
	}
	if request.Validate() != nil {
		return ClaimSweepPage{}, ErrInvalidClaim
	}
	at := s.clock.Now()
	if at.IsZero() {
		return ClaimSweepPage{}, ErrInvalidClaim
	}
	claims, err := s.store.ListTaskClaims(ctx, request)
	if err != nil {
		return ClaimSweepPage{}, err
	}
	if len(claims) > request.Limit {
		return ClaimSweepPage{}, ErrInvalidClaim
	}
	previous := request.AfterTaskID
	for _, claim := range claims {
		if claim.Validate() != nil || claim.Scope.Canonical() != scope.Canonical() || claim.PlanID != request.PlanID || (!previous.IsNil() && claim.TaskID.String() <= previous.String()) || claim.At.After(at) {
			return ClaimSweepPage{}, ErrInvalidClaim
		}
		previous = claim.TaskID
	}
	result := ClaimSweepPage{Scanned: len(claims)}
	for _, claim := range claims {
		if contextErr := ctx.Err(); contextErr != nil {
			return ClaimSweepPage{}, contextErr
		}
		if (claim.State != ClaimHeld && claim.State != ClaimDispatching) || at.Before(claim.ExpiresAt) {
			continue
		}
		next, expireErr := s.store.ExpireTaskClaim(ctx, request.PlanID, claim.TaskID, claim.Revision)
		if errors.Is(expireErr, ErrClaimConflict) {
			result.Conflicts++
			continue
		}
		if expireErr != nil {
			return ClaimSweepPage{}, expireErr
		}
		if (next.State != ClaimReleased && next.State != ClaimRecovering) || ValidateClaimSuccessor(claim, next) != nil {
			return ClaimSweepPage{}, ErrInvalidClaim
		}
		if next.State == ClaimReleased {
			result.Released++
		} else {
			result.Recovering++
		}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return ClaimSweepPage{}, contextErr
	}
	if len(claims) == request.Limit {
		result.NextTaskID = previous
	}
	return result, nil
}
