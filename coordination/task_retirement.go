package coordination

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskRetirementRequest releases unsuccessful ownership after its calls and
// accounting are closed. Recovery must authenticate permanent worker/tool
// quiescence and all external outcomes, including delayed deliveries.
type TaskRetirementRequest struct {
	SchemaVersion    int               `json:"schema_version"`
	Claim            ClaimToken        `json:"claim"`
	Recovery         TaskRecoveryProof `json:"recovery"`
	ClosureDigest    string            `json:"closure_digest"`
	SettlementDigest string            `json:"settlement_digest"`
	Actor            string            `json:"actor"`
	Reason           string            `json:"reason"`
}

// Freeze detaches the full request. Proof references do not authenticate it.
func (r TaskRetirementRequest) Freeze() (TaskRetirementRequest, string, error) {
	proof, _, err := r.Recovery.Freeze()
	if err != nil || r.SchemaVersion != 1 || r.Claim.Validate() != nil || !evidence.ValidDigest(r.ClosureDigest) || !evidence.ValidDigest(r.SettlementDigest) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskRetirementRequest{}, "", ErrInvalidClaim
	}
	r.Recovery = proof
	data, err := json.Marshal(r)
	if err != nil || len(data) > 64<<10 {
		return TaskRetirementRequest{}, "", ErrInvalidClaim
	}
	return r, evidence.Digest(data), nil
}

// RetireRecoveredClaim constructs the ownership successor. Storage must first
// compare the complete sealed journal, unsuccessful closure and guarded settlement.
// Retirement preserves the expired lease and original run; it grants no retry.
func RetireRecoveredClaim(c Claim, request TaskRetirementRequest, at time.Time) (Claim, error) {
	r, digest, err := request.Freeze()
	if err != nil {
		return Claim{}, err
	}
	if r.Recovery.validateClaim(c, at) != nil || c.Token() != r.Claim {
		return Claim{}, ErrClaimConflict
	}
	return retireRecoveredClaim(c, digest, at)
}

func retireRecoveredClaim(c Claim, digest string, at time.Time) (Claim, error) {
	if c.Validate() != nil || !evidence.ValidDigest(digest) || at.IsZero() || at.Before(c.At) {
		return Claim{}, ErrInvalidClaim
	}
	if c.State != ClaimRecovering || c.Revision == math.MaxInt64 {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	c.SchemaVersion, c.State, c.RetirementDigest = 2, ClaimRetired, digest
	return sealClaim(c)
}

// TaskRetirement retains the request and immutable retired ownership. Fresh is
// true only for a confirmed initial commit. Historical receipts grant no work.
type TaskRetirement struct {
	SchemaVersion int                   `json:"schema_version"`
	Request       TaskRetirementRequest `json:"request"`
	RequestDigest string                `json:"request_digest"`
	Claim         Claim                 `json:"claim"`
	Digest        string                `json:"digest"`
	Fresh         bool                  `json:"-"`
}

// SealTaskRetirement validates the receipt and detaches host-owned slices.
func SealTaskRetirement(r TaskRetirement) (TaskRetirement, error) {
	req, digest, err := r.Request.Freeze()
	c := r.Claim
	if err != nil || r.SchemaVersion != 1 || r.RequestDigest != digest || c.Validate() != nil || c.State != ClaimRetired || c.RetirementDigest != digest || c.ID != req.Claim.ClaimID || c.PlanID != req.Claim.PlanID || c.TaskID != req.Claim.TaskID || c.Worker != req.Claim.Worker || c.Fence != req.Claim.Fence || req.Claim.Revision == math.MaxInt64 || c.Revision != req.Claim.Revision+1 || c.PreviousDigest != req.Recovery.ClaimDigest || c.RunID != req.Recovery.Seal.Request.RunID || c.Scope.Canonical() != req.Recovery.Seal.Request.Scope.Canonical() || c.At.Before(req.Recovery.ObservedAt) || !c.At.Before(req.Recovery.ExpiresAt) {
		return TaskRetirement{}, ErrInvalidClaim
	}
	r.Request, r.Digest = req, ""
	data, err := json.Marshal(r)
	if err != nil || len(data) > 128<<10 {
		return TaskRetirement{}, ErrInvalidClaim
	}
	var frozen TaskRetirement
	if json.Unmarshal(data, &frozen) != nil {
		return TaskRetirement{}, ErrInvalidClaim
	}
	frozen.Digest, frozen.Fresh = evidence.Digest(data), r.Fresh
	return frozen, nil
}

// Validate checks integrity without authenticating proof or granting authority.
func (r TaskRetirement) Validate() error {
	sealed, err := SealTaskRetirement(r)
	if err != nil || r.Digest != sealed.Digest {
		return ErrInvalidClaim
	}
	return nil
}

// TaskRetirementStore retires one exact recovering claim atomically. Get returns
// nil when no receipt exists. Callers must authenticate proof and hold host fences.
type TaskRetirementStore interface {
	RetireTaskClaim(context.Context, TaskRetirementRequest) (TaskRetirement, error)
	GetTaskRetirement(context.Context, id.ID) (*TaskRetirement, error)
}

// TaskRetirementFence holds current grants and permanent worker/tool quiescence
// through one synchronous callback. Every relevant writer must participate.
// Resolve external proof before entering the storage transaction.
type TaskRetirementFence interface {
	WithTaskRetirementFence(context.Context, TaskRetirementRequest, func(TaskRetirementRequest) error) error
}

// TaskRetirementAuthorizer authenticates Actor and the Recovery attestation for
// the exact claim, seal and snapshot. Authenticate all provider/effect receipts
// and prove that delayed delivery cannot change outcomes or create more effects.
// Authorize the bound unsuccessful closure and settlement. Historical retries
// still require current access and authentic original proof.
type TaskRetirementAuthorizer interface {
	AuthorizeTaskRetirement(context.Context, TaskRetirementRequest) error
}

// TaskRetirer releases reconciled ownership under current host authority.
// Retry the identical request after an uncertain commit acknowledgment.
type TaskRetirer interface {
	Retire(context.Context, TaskRetirementRequest) (TaskRetirement, error)
}

type guardedTaskRetirer struct {
	store      TaskRetirementStore
	fence      TaskRetirementFence
	authorizer TaskRetirementAuthorizer
}

// NewTaskRetirer requires explicit storage, proof authentication and fencing.
func NewTaskRetirer(store TaskRetirementStore, fence TaskRetirementFence, authorizer TaskRetirementAuthorizer) (TaskRetirer, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidClaim
	}
	return &guardedTaskRetirer{store: store, fence: fence, authorizer: authorizer}, nil
}

func (r *guardedTaskRetirer) Retire(ctx context.Context, request TaskRetirementRequest) (TaskRetirement, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskRetirementRequest]{operation: "task retirement", invalid: ErrInvalidClaim, conflict: ErrClaimConflict, scope: func(r TaskRetirementRequest) cortex.Scope { return r.Recovery.Seal.Request.Scope }, fence: r.fence.WithTaskRetirementFence, authorize: r.authorizer.AuthorizeTaskRetirement}
	return withTaskAuthority(bounded, request, guard, func(tc context.Context, frozen TaskRetirementRequest, digest string) (TaskRetirement, error) {
		receipt, err := r.store.RetireTaskClaim(tc, frozen)
		if err != nil {
			return TaskRetirement{}, err
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return TaskRetirement{}, ErrClaimConflict
		}
		return SealTaskRetirement(receipt)
	})
}
