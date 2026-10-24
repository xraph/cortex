package coordination

import (
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	PrefixClaim      id.Prefix = "tclaim"
	MaxClaimBytes              = 16 << 10
	MaxClaimDuration           = 30 * time.Minute
)

var (
	ErrInvalidClaim  = cortex.ErrInvalidClaim
	ErrClaimConflict = cortex.ErrClaimConflict
	ErrClaimNotFound = cortex.ErrClaimNotFound
)

// ClaimState distinguishes a reservation from a dispatch with uncertain effects.
type ClaimState string

const (
	ClaimHeld        ClaimState = "held"
	ClaimDispatching ClaimState = "dispatching"
	ClaimRecovering  ClaimState = "recovering"
	ClaimReleased    ClaimState = "released"
	ClaimCompleted   ClaimState = "completed"
)

// ClaimRequest reserves exclusive ownership. It does not prove readiness, grant
// authority or reserve execution budget. The scheduler must establish those
// separately before atomically recording dispatch and calling the agent runtime.
type ClaimRequest struct {
	SchemaVersion    int           `json:"schema_version"`
	ID               id.ID         `json:"id"`
	PlanID           id.ID         `json:"plan_id"`
	PlanRevision     int64         `json:"plan_revision"`
	PlanDigest       string        `json:"plan_digest"`
	TaskID           id.ID         `json:"task_id"`
	ExpectedRevision int64         `json:"expected_revision"`
	Worker           Identity      `json:"worker"`
	GrantRef         string        `json:"grant_ref"`
	Duration         time.Duration `json:"duration"`
}

// Freeze detaches and fingerprints a bounded ownership request.
func (r ClaimRequest) Freeze() (ClaimRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixClaim || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.PlanRevision < 1 || !evidence.ValidDigest(r.PlanDigest) || r.TaskID.IsNil() || r.TaskID.Prefix() != planning.PrefixTask || r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 || !r.Worker.valid() || !handoffText(r.GrantRef, 512) || r.Duration <= 0 || r.Duration > MaxClaimDuration {
		return ClaimRequest{}, "", ErrInvalidClaim
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxClaimBytes {
		return ClaimRequest{}, "", ErrInvalidClaim
	}
	var frozen ClaimRequest
	if json.Unmarshal(data, &frozen) != nil {
		return ClaimRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// ClaimToken is an optimistic fence, not a bearer credential. Every mutation
// checks its full identity, generation and revision against retained ownership.
type ClaimToken struct {
	ClaimID  id.ID    `json:"claim_id"`
	PlanID   id.ID    `json:"plan_id"`
	TaskID   id.ID    `json:"task_id"`
	Worker   Identity `json:"worker"`
	Fence    uint64   `json:"fence"`
	Revision int64    `json:"revision"`
}

// Claim is an immutable ownership revision. Dispatching is recorded before an
// external call. Its expiry enters recovery, never permission to repeat work.
type Claim struct {
	SchemaVersion  int           `json:"schema_version"`
	ID             id.ID         `json:"id"`
	Scope          cortex.Scope  `json:"scope"`
	PlanID         id.ID         `json:"plan_id"`
	PlanRevision   int64         `json:"plan_revision"`
	PlanDigest     string        `json:"plan_digest"`
	TaskID         id.ID         `json:"task_id"`
	Worker         Identity      `json:"worker"`
	GrantRef       string        `json:"grant_ref"`
	Fence          uint64        `json:"fence"`
	Revision       int64         `json:"revision"`
	State          ClaimState    `json:"state"`
	RunID          id.AgentRunID `json:"run_id,omitempty"`
	AcquiredAt     time.Time     `json:"acquired_at"`
	At             time.Time     `json:"at"`
	ExpiresAt      time.Time     `json:"expires_at"`
	Deadline       time.Time     `json:"deadline"`
	PreviousDigest string        `json:"previous_digest,omitempty"`
	Digest         string        `json:"digest"`
	AcceptanceID   *id.ID        `json:"acceptance_id,omitempty"`
	RecoveryDigest string        `json:"recovery_digest,omitempty"`
}

// Token identifies this exact immutable observation of ownership.
func (c Claim) Token() ClaimToken {
	return ClaimToken{c.ID, c.PlanID, c.TaskID, c.Worker, c.Fence, c.Revision}
}

func sealClaim(c Claim) (Claim, error) {
	c.Digest = ""
	if c.SchemaVersion != 1 || c.ID.IsNil() || c.ID.Prefix() != PrefixClaim || cortex.ValidateScope(c.Scope) != nil || c.PlanID.IsNil() || c.PlanID.Prefix() != planning.PrefixPlan || c.PlanRevision < 1 || !evidence.ValidDigest(c.PlanDigest) || c.TaskID.IsNil() || c.TaskID.Prefix() != planning.PrefixTask || !c.Worker.valid() || !handoffText(c.GrantRef, 512) || c.Fence == 0 || c.Revision < 1 || c.AcquiredAt.IsZero() || c.At.Before(c.AcquiredAt) || !c.ExpiresAt.After(c.AcquiredAt) || c.ExpiresAt.After(c.Deadline) || c.ExpiresAt.After(c.At.Add(MaxClaimDuration)) {
		return Claim{}, ErrInvalidClaim
	}
	if (c.Revision == 1 && (c.PreviousDigest != "" || c.Fence != 1)) || (c.Revision > 1 && !evidence.ValidDigest(c.PreviousDigest)) {
		return Claim{}, ErrInvalidClaim
	}
	if c.State == ClaimCompleted {
		if c.AcceptanceID == nil || c.AcceptanceID.IsNil() || c.AcceptanceID.Prefix() != PrefixTaskAcceptance {
			return Claim{}, ErrInvalidClaim
		}
	} else if c.AcceptanceID != nil || c.RecoveryDigest != "" {
		return Claim{}, ErrInvalidClaim
	}
	switch c.State {
	case ClaimHeld:
		if !c.RunID.IsNil() || !c.At.Before(c.ExpiresAt) {
			return Claim{}, ErrInvalidClaim
		}
	case ClaimDispatching:
		if c.RunID.IsNil() || c.RunID.Prefix() != id.PrefixAgentRun || !c.At.Before(c.ExpiresAt) {
			return Claim{}, ErrInvalidClaim
		}
	case ClaimCompleted:
		if c.RunID.IsNil() || c.RunID.Prefix() != id.PrefixAgentRun {
			return Claim{}, ErrInvalidClaim
		}
		if c.RecoveryDigest == "" {
			if !c.At.Before(c.ExpiresAt) {
				return Claim{}, ErrInvalidClaim
			}
		} else if !evidence.ValidDigest(c.RecoveryDigest) || c.At.Before(c.ExpiresAt) || !c.At.Before(c.Deadline) {
			return Claim{}, ErrInvalidClaim
		}
	case ClaimRecovering:
		if c.RunID.IsNil() || c.RunID.Prefix() != id.PrefixAgentRun || c.At.Before(c.ExpiresAt) {
			return Claim{}, ErrInvalidClaim
		}
	case ClaimReleased:
		if !c.RunID.IsNil() || c.At.Before(c.ExpiresAt) {
			return Claim{}, ErrInvalidClaim
		}
	default:
		return Claim{}, ErrInvalidClaim
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > MaxClaimBytes-80 {
		return Claim{}, ErrInvalidClaim
	}
	var detached Claim
	if json.Unmarshal(data, &detached) != nil {
		return Claim{}, ErrInvalidClaim
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}

// Validate checks integrity; callers still need current host authorization.
func (c Claim) Validate() error {
	sealed, err := sealClaim(c)
	if err != nil || sealed.Digest != c.Digest {
		return ErrInvalidClaim
	}
	return nil
}

// AcquireClaim starts an attempt after release, or after a completed task moves
// to a newer plan revision. Success cannot be overwritten within the same plan.
// An expired dispatch remains unavailable until a separate recovery protocol
// proves its effects and run are settled. This function cannot waive recovery.
func AcquireClaim(previous *Claim, request ClaimRequest, p planning.Plan, at time.Time) (Claim, error) {
	req, _, err := request.Freeze()
	if err != nil || planning.Validate(p, planning.Limits{}) != nil || at.IsZero() || req.PlanID != p.ID || req.PlanRevision != p.Revision || req.PlanDigest != p.Digest {
		return Claim{}, ErrInvalidClaim
	}
	deadline := p.Budget.Deadline
	found := false
	for _, task := range p.Tasks {
		if task.ID == req.TaskID {
			deadline = minTime(deadline, task.Budget.Deadline)
			found = true
			break
		}
	}
	if !found || !at.Before(deadline) {
		return Claim{}, ErrInvalidClaim
	}
	c := Claim{SchemaVersion: 1, ID: req.ID, Scope: p.Scope, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: req.TaskID, Worker: req.Worker, GrantRef: req.GrantRef, Fence: 1, Revision: 1, State: ClaimHeld, AcquiredAt: at, At: at, ExpiresAt: minTime(at.Add(req.Duration), deadline), Deadline: deadline}
	if previous == nil {
		if req.ExpectedRevision != 0 {
			return Claim{}, ErrClaimConflict
		}
	} else {
		if previous.Validate() != nil {
			return Claim{}, ErrInvalidClaim
		}
		if (previous.State != ClaimReleased && (previous.State != ClaimCompleted || req.PlanRevision <= previous.PlanRevision)) || req.PlanRevision < previous.PlanRevision || previous.ID == req.ID || previous.PlanID != req.PlanID || previous.TaskID != req.TaskID || previous.Scope.Canonical() != p.Scope.Canonical() || previous.Revision != req.ExpectedRevision || previous.Revision == math.MaxInt64 || previous.Fence == math.MaxUint64 || at.Before(previous.At) {
			return Claim{}, ErrClaimConflict
		}
		c.Fence, c.Revision, c.PreviousDigest = previous.Fence+1, previous.Revision+1, previous.Digest
	}
	return sealClaim(c)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func liveClaim(c Claim, token ClaimToken, at time.Time) error {
	if c.Validate() != nil || at.IsZero() || at.Before(c.At) {
		return ErrInvalidClaim
	}
	if c.Token() != token || c.Revision == math.MaxInt64 || (c.State != ClaimHeld && c.State != ClaimDispatching) || !at.Before(c.ExpiresAt) {
		return ErrClaimConflict
	}
	return nil
}
func nextClaim(c Claim, at time.Time) Claim {
	c.Revision++
	c.PreviousDigest = c.Digest
	c.At = at
	return c
}

// RenewClaim extends a live reservation or dispatch without changing its fence.
func RenewClaim(c Claim, token ClaimToken, duration time.Duration, at time.Time) (Claim, error) {
	if duration <= 0 || duration > MaxClaimDuration {
		return Claim{}, ErrInvalidClaim
	}
	if err := liveClaim(c, token, at); err != nil {
		return Claim{}, err
	}
	expiry := minTime(at.Add(duration), c.Deadline)
	if !expiry.After(c.ExpiresAt) {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	c.ExpiresAt = expiry
	return sealClaim(c)
}

// BeginDispatch binds a reserved child run before any external execution. A lost
// acknowledgment must be reconciled by the host and is never permission to retry.
func BeginDispatch(c Claim, token ClaimToken, runID id.AgentRunID, at time.Time) (Claim, error) {
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return Claim{}, ErrInvalidClaim
	}
	if err := liveClaim(c, token, at); err != nil {
		return Claim{}, err
	}
	if c.State != ClaimHeld {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	c.State, c.RunID = ClaimDispatching, runID
	return sealClaim(c)
}

// ExpireClaim releases only reservations that never began dispatch. A dispatched
// run becomes recovering and retains its fence and identity for reconciliation.
func ExpireClaim(c Claim, expectedRevision int64, at time.Time) (Claim, error) {
	if c.Validate() != nil || at.IsZero() || at.Before(c.At) {
		return Claim{}, ErrInvalidClaim
	}
	if c.Revision != expectedRevision || c.Revision == math.MaxInt64 || at.Before(c.ExpiresAt) || (c.State != ClaimHeld && c.State != ClaimDispatching) {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	if c.State == ClaimHeld {
		c.State = ClaimReleased
	} else {
		c.State = ClaimRecovering
	}
	return sealClaim(c)
}

// Validate checks a token's bounded shape without granting its holder authority.
func (t ClaimToken) Validate() error {
	if t.ClaimID.IsNil() || t.ClaimID.Prefix() != PrefixClaim || t.PlanID.IsNil() || t.PlanID.Prefix() != planning.PrefixPlan || t.TaskID.IsNil() || t.TaskID.Prefix() != planning.PrefixTask || !t.Worker.valid() || t.Fence == 0 || t.Revision < 1 || t.Revision == math.MaxInt64 {
		return ErrInvalidClaim
	}
	return nil
}

// CompleteClaim closes a live dispatched claim after storage validates evidence.
// The pure transition alone does not establish verifier trust or task acceptance.
func CompleteClaim(c Claim, token ClaimToken, acceptanceID id.ID, at time.Time) (Claim, error) {
	if acceptanceID.IsNil() || acceptanceID.Prefix() != PrefixTaskAcceptance {
		return Claim{}, ErrInvalidClaim
	}
	if err := liveClaim(c, token, at); err != nil {
		return Claim{}, err
	}
	if c.State != ClaimDispatching {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	c.State, c.AcceptanceID = ClaimCompleted, &acceptanceID
	return sealClaim(c)
}
