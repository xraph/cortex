package coordination

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const PrefixTaskRunEvent id.Prefix = "trunstart"

// TaskRunRequest binds a pristine prepared child to current dispatched ownership.
// Run has no timestamps or persisted revision; storage supplies those at commit.
// The host must authorize the prepared configuration and fence current grants and
// external prerequisite revisions. Creating this row does not invoke a provider.
type TaskRunRequest struct {
	SchemaVersion      int                        `json:"schema_version"`
	Claim              ClaimToken                 `json:"claim"`
	DispatchDigest     string                     `json:"dispatch_digest"`
	AssignmentRevision int64                      `json:"assignment_revision"`
	AssignmentDigest   string                     `json:"assignment_digest"`
	ParentRunRevision  int64                      `json:"parent_run_revision"`
	Dependencies       []AcceptedDependency       `json:"dependencies,omitempty"`
	Run                run.Run                    `json:"run"`
	Intent             verification.CaptureIntent `json:"intent"`
	Actor              string                     `json:"actor"`
	Reason             string                     `json:"reason"`
}

// Freeze validates and detaches a bounded request, including its private input.
func (r TaskRunRequest) Freeze() (TaskRunRequest, string, error) {
	child, intent := r.Run, r.Intent
	parent, parentErr := id.Parse(child.Provenance.ParentRunID)
	if r.SchemaVersion != 1 || r.Claim.Validate() != nil || !evidence.ValidDigest(r.DispatchDigest) || r.AssignmentRevision < 1 || !evidence.ValidDigest(r.AssignmentDigest) || r.ParentRunRevision < 1 || r.Actor != r.Claim.Worker.Principal || !handoffText(r.Reason, 4096) || !validAcceptedDependencies(r.Dependencies, child.Scope, id.ID{}) || cortex.ValidateScope(child.Scope) != nil || child.ID.IsNil() || child.ID.Prefix() != id.PrefixAgentRun || child.AgentID.IsNil() || child.AgentID.Prefix() != id.PrefixAgent || child.SessionID.IsNil() || child.SessionID.Prefix() != id.PrefixSession || child.Revision != 0 || !child.CreatedAt.IsZero() || !child.UpdatedAt.IsZero() || child.StartedAt != nil || child.CompletedAt != nil || child.State != run.StateRunning || child.Output != "" || child.Error != "" || child.StopReason != "" || child.FinishReason != "" || child.ResultRef != "" || child.VerificationStatus.Normalized() != verification.StatusUnchecked || child.StepCount != 0 || child.TokensUsed != 0 || child.Usage != (execution.Usage{}) || len(child.Metadata) != 0 || len(child.Input) > 1<<20 || !utf8.ValidString(child.Input) || child.Budget.Validate() != nil || child.Budget.MaxSteps <= 0 || child.Budget.MaxTokens <= 0 || child.Budget.MaxToolCalls <= 0 || child.Budget.Deadline.IsZero() || parentErr != nil || parent.IsNil() || parent.Prefix() != id.PrefixAgentRun || parent == child.ID || !evidence.ValidDigest(child.Provenance.AssignmentDigest) || child.Provenance.SchemaVersion != 1 || child.Provenance.Purpose != run.PurposeNormal || !child.Provenance.VerificationRequired || intent.Validate() != nil || intent.SchemaVersion != 2 || intent.Template.ExpectedRunRevision != 1 {
		return TaskRunRequest{}, "", ErrInvalidClaim
	}
	binding := intent.Template.Binding
	if binding.RunID != child.ID || binding.Scope.Canonical() != child.Scope.Canonical() || binding.Context.PrincipalRef != r.Claim.Worker.Principal || binding.Context.PreparedDigest != child.Provenance.PreparedDigest || binding.Context.ExecutionInputs == nil || binding.Context.ExecutionInputs.SHA256 != child.Provenance.ExecutionInputsDigest || child.Provenance.VerificationIntentID != intent.ID.String() || child.Provenance.AcceptanceRef != intent.Template.ContractID.String() || intent.Template.Deadline.After(child.Budget.Deadline) {
		return TaskRunRequest{}, "", ErrInvalidClaim
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 2<<20 {
		return TaskRunRequest{}, "", ErrInvalidClaim
	}
	var frozen TaskRunRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskRunRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// TaskRunOutbox records child creation metadata for authorized event delivery.
// It carries no input, output or proof that an external call has started.
type TaskRunOutbox struct {
	EventID  id.ID         `json:"event_id"`
	Type     string        `json:"type"`
	ClaimID  id.ID         `json:"claim_id"`
	PlanID   id.ID         `json:"plan_id"`
	TaskID   id.ID         `json:"task_id"`
	RunID    id.AgentRunID `json:"run_id"`
	IntentID id.ID         `json:"intent_id"`
}

// TaskRunStart retains creation metadata without copying private run input.
// Fresh only identifies confirmed first creation. Historical receipts never permit
// reexecution; reconcile the current run after an ambiguous acknowledgment.
type TaskRunStart struct {
	RequestDigest string        `json:"request_digest"`
	RunID         id.AgentRunID `json:"run_id"`
	RunDigest     string        `json:"run_digest"`
	CreatedAt     time.Time     `json:"created_at"`
	IntentID      id.ID         `json:"intent_id"`
	IntentDigest  string        `json:"intent_digest"`
	Outbox        TaskRunOutbox `json:"outbox"`
	Fresh         bool          `json:"-"`
}

// Validate checks the original request and initial run digest without granting authority.
func (r TaskRunStart) Validate(request TaskRunRequest) error {
	req, digest, err := request.Freeze()
	if err != nil || r.RequestDigest != digest || r.IntentDigest != req.Intent.Digest || r.IntentID != req.Intent.ID || r.RunID != req.Run.ID || !r.CreatedAt.Equal(r.CreatedAt.Truncate(time.Millisecond)) {
		return ErrInvalidClaim
	}
	child, err := req.InitialRun(r.CreatedAt)
	if err != nil {
		return err
	}
	data, err := json.Marshal(child)
	if err != nil || r.RunDigest != evidence.Digest(data) {
		return ErrInvalidClaim
	}
	out := r.Outbox
	if out.EventID.IsNil() || out.EventID.Prefix() != PrefixTaskRunEvent || out.Type != "task_run_created" || out.ClaimID != req.Claim.ClaimID || out.PlanID != req.Claim.PlanID || out.TaskID != req.Claim.TaskID || out.RunID != req.Run.ID || out.IntentID != req.Intent.ID {
		return ErrInvalidClaim
	}
	return nil
}

// InitialRun builds the pristine persisted snapshot at trusted storage time.
// Creation timestamps use milliseconds so all built-in stores round-trip them.
// Storage must still check live ownership, policy and prerequisites atomically.
func (r TaskRunRequest) InitialRun(at time.Time) (run.Run, error) {
	req, _, err := r.Freeze()
	if err != nil || at.IsZero() || !at.Before(req.Run.Budget.Deadline) {
		return run.Run{}, ErrInvalidClaim
	}
	child := req.Run
	at = at.UTC().Truncate(time.Millisecond)
	child.Revision, child.Entity, child.StartedAt = 1, cortex.Entity{CreatedAt: at, UpdatedAt: at}, &at
	return child, nil
}

// TaskRunStore atomically creates the reserved child, its capture intent and an
// immutable start receipt. An existing run is never adopted, even if identical.
// Hosts must authorize each request and guard external revisions through commit.
type TaskRunStore interface {
	CreateTaskRun(context.Context, TaskRunRequest) (TaskRunStart, error)
	GetTaskRunStart(context.Context, id.ID) (TaskRunStart, error)
}
