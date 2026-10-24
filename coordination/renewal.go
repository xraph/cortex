package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const PrefixTaskRenewalEvent id.Prefix = "trenew"

// TaskRenewalRequest extends dispatched ownership while fencing the current
// parent, child, assignment and prerequisites. Claims and run revisions are
// observations, not credentials. The host must authenticate the worker and hold
// current grant and external revision guards through the storage transaction.
type TaskRenewalRequest struct {
	Actor              string               `json:"actor"`
	Reason             string               `json:"reason"`
	SchemaVersion      int                  `json:"schema_version"`
	Claim              Claim                `json:"claim"`
	ParentRunID        id.AgentRunID        `json:"parent_run_id"`
	ParentRunRevision  int64                `json:"parent_run_revision"`
	RunRevision        int64                `json:"run_revision"`
	AssignmentID       id.ID                `json:"assignment_id"`
	AssignmentRevision int64                `json:"assignment_revision"`
	AssignmentDigest   string               `json:"assignment_digest"`
	Dependencies       []AcceptedDependency `json:"dependencies,omitempty"`
	Duration           time.Duration        `json:"duration"`
}

// Freeze validates and detaches a bounded renewal request.
func (r TaskRenewalRequest) Freeze() (TaskRenewalRequest, string, error) {
	if r.Actor != r.Claim.Worker.Principal || !handoffText(r.Reason, 4096) || r.SchemaVersion != 1 || r.Claim.Validate() != nil || r.Claim.State != ClaimDispatching || r.ParentRunID.IsNil() || r.ParentRunID.Prefix() != id.PrefixAgentRun || r.ParentRunID == r.Claim.RunID || r.ParentRunRevision < 1 || r.RunRevision < 1 || r.AssignmentID.IsNil() || r.AssignmentID.Prefix() != PrefixAssignment || r.AssignmentRevision < 1 || !evidence.ValidDigest(r.AssignmentDigest) || !validAcceptedDependencies(r.Dependencies, r.Claim.Scope, id.ID{}) || r.Duration <= 0 || r.Duration > MaxClaimDuration {
		return TaskRenewalRequest{}, "", ErrInvalidClaim
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 1<<20 {
		return TaskRenewalRequest{}, "", ErrInvalidClaim
	}
	var frozen TaskRenewalRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskRenewalRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// TaskRenewalOutbox retains metadata for authorized delivery. It grants no
// execution permission and does not allocate a public event sequence.
type TaskRenewalOutbox struct {
	EventID id.ID         `json:"event_id"`
	Type    string        `json:"type"`
	ClaimID id.ID         `json:"claim_id"`
	PlanID  id.ID         `json:"plan_id"`
	TaskID  id.ID         `json:"task_id"`
	RunID   id.AgentRunID `json:"run_id"`
}

// TaskRenewal retains the original renewal. Fresh is true only for a confirmed
// first write. Historical receipts may describe expired ownership; a renewal
// loop must check Fresh, current authority and expiry before extending local work.
type TaskRenewal struct {
	RequestDigest string            `json:"request_digest"`
	Claim         Claim             `json:"claim"`
	Outbox        TaskRenewalOutbox `json:"outbox"`
	Fresh         bool              `json:"-"`
}

// Validate checks immutable successor and outbox bindings, not live authority.
func (r TaskRenewal) Validate(request TaskRenewalRequest) error {
	req, digest, err := request.Freeze()
	if err != nil || r.RequestDigest != digest || r.Claim.State != ClaimDispatching || ValidateClaimSuccessor(req.Claim, r.Claim) != nil {
		return ErrInvalidClaim
	}
	expected, err := RenewClaim(req.Claim, req.Claim.Token(), req.Duration, r.Claim.At)
	if err != nil || expected.Digest != r.Claim.Digest {
		return ErrInvalidClaim
	}
	out := r.Outbox
	if out.EventID.IsNil() || out.EventID.Prefix() != PrefixTaskRenewalEvent || out.Type != "task_renewed" || out.ClaimID != req.Claim.ID || out.PlanID != req.Claim.PlanID || out.TaskID != req.Claim.TaskID || out.RunID != req.Claim.RunID {
		return ErrInvalidClaim
	}
	return nil
}

// TaskRenewalStore atomically renews dispatched ownership and retains its
// metadata outbox after current execution checks. It invokes no external work.
type TaskRenewalStore interface {
	RenewTaskExecution(context.Context, TaskRenewalRequest) (TaskRenewal, error)
}
