package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

// DispatchAdmission identifies whether this call first recorded dispatch intent.
// Fresh is false for historical retries, including after a lost acknowledgment.
// Even a fresh admission requires current authority, readiness and budget checks
// in the host scheduler. The store itself never invokes the runtime.
type DispatchAdmission struct {
	Claim Claim
	Fresh bool
}

// ClaimStore serializes task ownership separately from readiness and execution.
// A host must resolve current identity/grants. Tokens are not credentials and
// historical acknowledgments do not renew permission. All reads use exact scope.
type ClaimStore interface {
	AcquireTaskClaim(context.Context, ClaimRequest) (Claim, error)
	RenewTaskClaim(context.Context, ClaimToken, time.Duration) (Claim, error)
	BeginTaskDispatch(context.Context, ClaimToken, id.AgentRunID) (DispatchAdmission, error)
	ExpireTaskClaim(context.Context, id.ID, id.ID, int64) (Claim, error)
	GetTaskClaim(context.Context, id.ID, id.ID) (Claim, error)
	ListTaskClaims(context.Context, ClaimListRequest) ([]Claim, error)
	TaskClaimHistory(context.Context, ClaimHistoryRequest) ([]Claim, error)
}

// ClaimListRequest pages every task ever claimed in the plan, including tasks
// removed by replanning. A complete sweep starts again after reaching the end.
type ClaimListRequest struct {
	PlanID      id.ID
	AfterTaskID id.ID
	Limit       int
}

// Validate checks the shared pagination bounds without granting scope access.
func (r ClaimListRequest) Validate() error {
	if r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.Limit < 1 || r.Limit > 256 || (!r.AfterTaskID.IsNil() && r.AfterTaskID.Prefix() != planning.PrefixTask) {
		return ErrInvalidClaim
	}
	return nil
}

// ClaimHistoryRequest pages immutable task ownership revisions.
type ClaimHistoryRequest struct {
	PlanID        id.ID
	TaskID        id.ID
	AfterRevision int64
	Limit         int
}

// ValidateClaimSuccessor checks the ownership chain without authorizing it.
func ValidateClaimSuccessor(previous, next Claim) error {
	if previous.Validate() != nil || next.Validate() != nil || next.Revision != previous.Revision+1 || next.PreviousDigest != previous.Digest || next.PlanID != previous.PlanID || next.TaskID != previous.TaskID || next.Scope.Canonical() != previous.Scope.Canonical() || next.At.Before(previous.At) {
		return ErrInvalidClaim
	}
	if next.ID != previous.ID {
		if (previous.State != ClaimReleased && (previous.State != ClaimCompleted || next.PlanRevision <= previous.PlanRevision)) || next.State != ClaimHeld || next.Fence != previous.Fence+1 || !next.AcquiredAt.Equal(next.At) || next.PlanRevision < previous.PlanRevision {
			return ErrInvalidClaim
		}
		return nil
	}
	var rebuilt Claim
	var err error
	switch next.State {
	case previous.State:
		rebuilt, err = RenewClaim(previous, previous.Token(), next.ExpiresAt.Sub(next.At), next.At)
	case ClaimDispatching:
		rebuilt, err = BeginDispatch(previous, previous.Token(), next.RunID, next.At)
	case ClaimCompleted:
		rebuilt, err = CompleteClaim(previous, previous.Token(), *next.AcceptanceID, next.At)
	case ClaimReleased, ClaimRecovering:
		rebuilt, err = ExpireClaim(previous, previous.Revision, next.At)
	default:
		return ErrInvalidClaim
	}
	if err != nil || rebuilt.Digest != next.Digest {
		return ErrInvalidClaim
	}
	return nil
}
