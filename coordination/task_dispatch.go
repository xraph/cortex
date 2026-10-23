package coordination

import (
	"context"
	"encoding/json"
	"math"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const PrefixTaskDispatchEvent id.Prefix = "tdispatch"

// TaskDispatchRequest pins a ready admission, current ownership, an accepted
// assignment and its active parent. Dependencies contain the complete current
// prerequisite chain. Hosts must fence grants and external revisions through
// this operation and authenticate Actor and Worker; these fields are not credentials.
type TaskDispatchRequest struct {
	SchemaVersion      int                  `json:"schema_version"`
	Scope              cortex.Scope         `json:"scope"`
	Claim              ClaimToken           `json:"claim"`
	ClaimDigest        string               `json:"claim_digest"`
	AdmissionDigest    string               `json:"admission_digest"`
	AssignmentID       id.ID                `json:"assignment_id"`
	AssignmentRevision int64                `json:"assignment_revision"`
	AssignmentDigest   string               `json:"assignment_digest"`
	ParentRunRevision  int64                `json:"parent_run_revision"`
	RunID              id.AgentRunID        `json:"run_id"`
	Dependencies       []AcceptedDependency `json:"dependencies,omitempty"`
	Actor              string               `json:"actor"`
	Reason             string               `json:"reason"`
}

// Freeze detaches the bounded dispatch request before storage or host callbacks.
func (r TaskDispatchRequest) Freeze() (TaskDispatchRequest, string, error) {
	if r.SchemaVersion != 1 || cortex.ValidateScope(r.Scope) != nil || r.Claim.Validate() != nil || !evidence.ValidDigest(r.ClaimDigest) || !evidence.ValidDigest(r.AdmissionDigest) || r.AssignmentID.IsNil() || r.AssignmentID.Prefix() != PrefixAssignment || r.AssignmentRevision < 1 || r.AssignmentRevision == math.MaxInt64 || !evidence.ValidDigest(r.AssignmentDigest) || r.ParentRunRevision < 1 || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.Actor != r.Claim.Worker.Principal || !handoffText(r.Reason, 4096) || !validAcceptedDependencies(r.Dependencies, r.Scope, id.ID{}) {
		return TaskDispatchRequest{}, "", ErrInvalidClaim
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 1<<20 {
		return TaskDispatchRequest{}, "", ErrInvalidClaim
	}
	var frozen TaskDispatchRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskDispatchRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// TaskDispatchOutbox retains metadata for authorized delivery. The reserved run
// ID identifies an intent; it does not establish that execution started.
type TaskDispatchOutbox struct {
	EventID      id.ID         `json:"event_id"`
	Type         string        `json:"type"`
	ClaimID      id.ID         `json:"claim_id"`
	PlanID       id.ID         `json:"plan_id"`
	TaskID       id.ID         `json:"task_id"`
	AssignmentID id.ID         `json:"assignment_id"`
	RunID        id.AgentRunID `json:"run_id"`
}

// TaskDispatch records the original atomic transition. Historical receipts never
// authorize execution. Even Fresh requires current host authority and a runtime
// that creates the reserved child exactly once without adopting an existing run.
type TaskDispatch struct {
	RequestDigest string             `json:"request_digest"`
	Claim         Claim              `json:"claim"`
	Assignment    Assignment         `json:"assignment"`
	Outbox        TaskDispatchOutbox `json:"outbox"`
	Fresh         bool               `json:"-"`
}

// TaskDispatchStore rechecks readiness and ownership, fences the active parent,
// and retains dispatch intent and assignment progress atomically. It invokes no
// agent, creates no run and delivers no public event.
type TaskDispatchStore interface {
	DispatchReadyTask(context.Context, TaskDispatchRequest) (TaskDispatch, error)
	GetTaskDispatch(context.Context, id.ID) (TaskDispatch, error)
}

// Validate checks the original request and receipt associations, not current
// authorization. Fresh is supplied by trusted storage and is not serialized.
func (r TaskDispatch) Validate(request TaskDispatchRequest) error {
	req, digest, err := request.Freeze()
	c, a := r.Claim, r.Assignment
	h := a.Handoff
	if err != nil || r.RequestDigest != digest || c.Validate() != nil || a.Validate() != nil || c.State != ClaimDispatching || c.ID != req.Claim.ClaimID || c.PlanID != req.Claim.PlanID || c.TaskID != req.Claim.TaskID || c.Worker != req.Claim.Worker || c.Fence != req.Claim.Fence || c.Revision != req.Claim.Revision+1 || c.PreviousDigest != req.ClaimDigest || c.RunID != req.RunID || c.Scope.Canonical() != req.Scope.Canonical() || a.State != AssignmentWorking || a.Revision != req.AssignmentRevision+1 || a.PreviousDigest != req.AssignmentDigest || a.Actor != req.Actor || a.Reason != req.Reason || !a.At.Equal(c.At) || h.AssignmentID != req.AssignmentID || h.PlanID != c.PlanID || h.PlanRevision != c.PlanRevision || h.PlanDigest != c.PlanDigest || h.TaskID != c.TaskID || h.Scope.Canonical() != c.Scope.Canonical() || h.Receiver != c.Worker || h.GrantRef != c.GrantRef || h.Fence != c.Fence || h.Purpose != HandoffWork || h.ParentRunID == c.RunID || c.ExpiresAt.After(h.Deadline) {
		return ErrInvalidClaim
	}
	out := r.Outbox
	if out.EventID.IsNil() || out.EventID.Prefix() != PrefixTaskDispatchEvent || out.Type != "task_dispatched" || out.ClaimID != c.ID || out.PlanID != c.PlanID || out.TaskID != c.TaskID || out.AssignmentID != h.AssignmentID || out.RunID != c.RunID {
		return ErrInvalidClaim
	}
	return nil
}
