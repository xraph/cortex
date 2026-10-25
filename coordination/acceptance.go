package coordination

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

const (
	PrefixTaskAcceptance    id.Prefix = "taccept"
	PrefixTaskAcceptedEvent id.Prefix = "taccepted"
	MaxTaskAcceptanceBytes            = 2 << 20
)

var (
	ErrInvalidTaskAcceptance  = cortex.ErrInvalidTaskAcceptance
	ErrTaskAcceptanceConflict = cortex.ErrTaskAcceptanceConflict
	ErrTaskAcceptanceNotFound = cortex.ErrTaskAcceptanceNotFound
)

// AcceptedDependency names a retained acceptance and freshly resolved context.
// Hosts must fence current grants and external revisions during acceptance.
type AcceptedDependency struct {
	AcceptanceID id.ID                `json:"acceptance_id"`
	Digest       string               `json:"digest"`
	Current      verification.Binding `json:"current"`
}

// TaskAcceptanceRequest pins ownership, assignment and a committed result.
// Dependencies must cover the complete prerequisite chain. No peer verdict or
// caller-supplied report list can replace retained verifier evidence.
type TaskAcceptanceRequest struct {
	SchemaVersion      int                            `json:"schema_version"`
	ID                 id.ID                          `json:"id"`
	Claim              ClaimToken                     `json:"claim"`
	AssignmentID       id.ID                          `json:"assignment_id"`
	AssignmentRevision int64                          `json:"assignment_revision"`
	AssignmentDigest   string                         `json:"assignment_digest"`
	Result             verification.ResultReadRequest `json:"result"`
	Current            verification.Binding           `json:"current"`
	Dependencies       []AcceptedDependency           `json:"dependencies,omitempty"`
	Actor              string                         `json:"actor"`
	Reason             string                         `json:"reason"`
	Recovery           *TaskRecoveryProof             `json:"recovery,omitempty"`
}

// Freeze detaches bounded trusted host input. Digests are not credentials.
func (r TaskAcceptanceRequest) Freeze() (TaskAcceptanceRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixTaskAcceptance || r.Claim.Validate() != nil || r.AssignmentID.IsNil() || r.AssignmentID.Prefix() != PrefixAssignment || r.AssignmentRevision < 1 || r.AssignmentRevision == math.MaxInt64 || !evidence.ValidDigest(r.AssignmentDigest) || r.Result.Validate() != nil || r.Current.Validate() != nil || r.Current.RunID != r.Result.RunID || len(r.Dependencies) > 255 || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskAcceptanceRequest{}, "", ErrInvalidTaskAcceptance
	}
	if !validAcceptedDependencies(r.Dependencies, r.Current.Scope, r.ID) {
		return TaskAcceptanceRequest{}, "", ErrInvalidTaskAcceptance
	}
	if r.Recovery != nil {
		proof, _, proofErr := r.Recovery.Freeze()
		if proofErr != nil || proof.Seal.Request.RunID != r.Result.RunID || proof.Seal.Request.Scope.Canonical() != r.Current.Scope.Canonical() {
			return TaskAcceptanceRequest{}, "", ErrInvalidTaskAcceptance
		}
		r.Recovery = &proof
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 1<<20 {
		return TaskAcceptanceRequest{}, "", ErrInvalidTaskAcceptance
	}
	var frozen TaskAcceptanceRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskAcceptanceRequest{}, "", ErrInvalidTaskAcceptance
	}
	return frozen, evidence.Digest(data), nil
}

// TaskAcceptanceOutbox is a retained metadata-only event. Delivery requires host
// authorization; the event is not a public stream sequence or content grant.
type TaskAcceptanceOutbox struct {
	EventID      id.ID         `json:"event_id"`
	Type         string        `json:"type"`
	AcceptanceID id.ID         `json:"acceptance_id"`
	PlanID       id.ID         `json:"plan_id"`
	TaskID       id.ID         `json:"task_id"`
	RunID        id.AgentRunID `json:"run_id"`
}

// TaskAcceptance is a historical verified task transition and its outbox intent.
// Fresh is true only for a confirmed first write. Revalidate current evidence and
// authority before using the result as a dependency or publishing any content.
type TaskAcceptance struct {
	SchemaVersion int                   `json:"schema_version"`
	Request       TaskAcceptanceRequest `json:"request"`
	RequestDigest string                `json:"request_digest"`
	Claim         Claim                 `json:"claim"`
	ObservedAt    time.Time             `json:"observed_at"`
	Outbox        TaskAcceptanceOutbox  `json:"outbox"`
	Digest        string                `json:"digest"`
	Fresh         bool                  `json:"-"`
}

// SealTaskAcceptance validates and detaches a storage-produced transition.
func SealTaskAcceptance(a TaskAcceptance) (TaskAcceptance, error) {
	req, digest, err := a.Request.Freeze()
	if err != nil || a.SchemaVersion != 1 || digest != a.RequestDigest || a.Claim.Validate() != nil || a.Claim.State != ClaimCompleted || (a.Claim.AcceptanceID == nil || *a.Claim.AcceptanceID != req.ID) || a.Claim.ID != req.Claim.ClaimID || a.Claim.PlanID != req.Claim.PlanID || a.Claim.TaskID != req.Claim.TaskID || a.Claim.Worker != req.Claim.Worker || a.Claim.Fence != req.Claim.Fence || a.Claim.Revision != req.Claim.Revision+1 || a.Claim.RunID != req.Result.RunID || a.Claim.Scope.Canonical() != req.Current.Scope.Canonical() || !a.ObservedAt.Equal(a.Claim.At) || a.Outbox.EventID.IsNil() || a.Outbox.EventID.Prefix() != PrefixTaskAcceptedEvent || a.Outbox.Type != "task_result_accepted" || a.Outbox.AcceptanceID != req.ID || a.Outbox.PlanID != a.Claim.PlanID || a.Outbox.TaskID != a.Claim.TaskID || a.Outbox.RunID != a.Claim.RunID {
		return TaskAcceptance{}, ErrInvalidTaskAcceptance
	}
	if req.Recovery == nil {
		if a.Claim.RecoveryDigest != "" {
			return TaskAcceptance{}, ErrInvalidTaskAcceptance
		}
	} else {
		_, proofDigest, proofErr := req.Recovery.Freeze()
		if proofErr != nil || a.Claim.RecoveryDigest != proofDigest || a.Claim.PreviousDigest != req.Recovery.ClaimDigest || a.ObservedAt.Before(req.Recovery.ObservedAt) || !a.ObservedAt.Before(req.Recovery.ExpiresAt) {
			return TaskAcceptance{}, ErrInvalidTaskAcceptance
		}
	}
	a.Request, a.Digest, a.Fresh = req, "", false
	data, err := json.Marshal(a)
	if err != nil || len(data) > MaxTaskAcceptanceBytes {
		return TaskAcceptance{}, ErrInvalidTaskAcceptance
	}
	var detached TaskAcceptance
	if json.Unmarshal(data, &detached) != nil {
		return TaskAcceptance{}, ErrInvalidTaskAcceptance
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}

// Validate checks integrity without claiming continuing freshness or access.
func (a TaskAcceptance) Validate() error {
	sealed, err := SealTaskAcceptance(a)
	if err != nil || sealed.Digest != a.Digest {
		return ErrInvalidTaskAcceptance
	}
	return nil
}

// TaskAcceptanceStore persists verified success and completes its claim atomically.
// Callers authenticate Actor and fence external grants/revisions. The store never
// invokes an agent, settles unknown usage or grants publication permission.
type TaskAcceptanceStore interface {
	AcceptTaskResult(context.Context, TaskAcceptanceRequest) (TaskAcceptance, error)
	GetTaskAcceptance(context.Context, id.ID) (TaskAcceptance, error)
	ListTaskAcceptances(context.Context, TaskAcceptanceListRequest) ([]TaskAcceptance, error)
}

// TaskAcceptanceListRequest bounds exact-scope recovery and outbox discovery.
type TaskAcceptanceListRequest struct {
	PlanID id.ID
	After  id.ID
	Limit  int
}

// Validate checks bounded acceptance pagination without granting read access.
func (r TaskAcceptanceListRequest) Validate() error {
	if r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.Limit < 1 || r.Limit > 256 || (!r.After.IsNil() && r.After.Prefix() != PrefixTaskAcceptance) {
		return ErrInvalidTaskAcceptance
	}
	return nil
}

// AcceptedClaimStore pages immutable accepted ownership by acceptance ID,
// including claims superseded by later plan revisions. Every returned claim must
// retain its matching acceptance and ownership history. Reads grant no authority.
type AcceptedClaimStore interface {
	ListAcceptedTaskClaims(context.Context, TaskAcceptanceListRequest) ([]Claim, error)
}
