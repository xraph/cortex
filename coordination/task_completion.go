package coordination

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

// TaskCompletionRequest reserves one independent review for a submitted result.
// PreparedDigest and InputDigest bind the private runtime material supplied by
// the host. No private input bytes or provider credentials enter the journal.
type TaskCompletionRequest struct {
	SchemaVersion      int                          `json:"schema_version"`
	AssignmentID       id.ID                        `json:"assignment_id"`
	AssignmentRevision int64                        `json:"assignment_revision"`
	AssignmentDigest   string                       `json:"assignment_digest"`
	BindingDigest      string                       `json:"binding_digest"`
	Handoff            Handoff                      `json:"handoff"`
	SessionID          id.SessionID                 `json:"session_id"`
	RunID              id.AgentRunID                `json:"run_id"`
	ReviewID           id.ID                        `json:"review_id"`
	AdjudicationID     id.ID                        `json:"adjudication_id"`
	AcceptanceID       id.ID                        `json:"acceptance_id"`
	PreparedDigest     string                       `json:"prepared_digest"`
	InputDigest        string                       `json:"input_digest"`
	Reservation        execution.ReservationRequest `json:"reservation"`
	Actor              string                       `json:"actor"`
	Reason             string                       `json:"reason"`
}

// Freeze bounds and detaches the full immutable review dispatch request.
func (r TaskCompletionRequest) Freeze() (TaskCompletionRequest, string, error) {
	h := r.Handoff
	if r.SchemaVersion != 1 || r.AssignmentID.IsNil() || r.AssignmentID.Prefix() != PrefixAssignment || r.AssignmentRevision < 1 || !evidence.ValidDigest(r.AssignmentDigest) || !evidence.ValidDigest(r.BindingDigest) || h.Validate() != nil || h.Purpose != HandoffReview || r.AssignmentID == h.AssignmentID || r.SessionID.IsNil() || r.SessionID.Prefix() != id.PrefixSession || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.RunID == h.ParentRunID || r.ReviewID.IsNil() || r.ReviewID.Prefix() != PrefixReview || r.AdjudicationID.IsNil() || r.AdjudicationID.Prefix() != PrefixTaskAdjudication || r.AcceptanceID.IsNil() || r.AcceptanceID.Prefix() != PrefixTaskAcceptance || !evidence.ValidDigest(r.PreparedDigest) || !evidence.ValidDigest(r.InputDigest) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskCompletionRequest{}, "", ErrInvalidReview
	}
	reservation, _, err := r.Reservation.Freeze()
	if err != nil {
		return TaskCompletionRequest{}, "", err
	}
	b := h.Authority.Budget
	if reservation.PlanID != h.PlanID || reservation.PlanDigest != h.PlanDigest || reservation.TaskID != h.TaskID || reservation.Scope.Canonical() != h.Scope.Canonical() || reservation.OwnerRef != h.AssignmentID.String() || reservation.Kind != execution.ReservationLeaf || !reservation.Deadline.Equal(h.Deadline) || reservation.Allocation.Steps < int64(b.MaxSteps) || reservation.Allocation.Tokens < int64(b.MaxTokens) || reservation.Allocation.ToolCalls < int64(b.MaxToolCalls) || b.MaxSteps < 1 || b.MaxTokens < 1 {
		return TaskCompletionRequest{}, "", ErrInvalidReview
	}
	r.Reservation = reservation
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxHandoffBytes+32<<10 {
		return TaskCompletionRequest{}, "", ErrInvalidReview
	}
	var frozen TaskCompletionRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskCompletionRequest{}, "", ErrInvalidReview
	}
	return frozen, evidence.Digest(data), nil
}

// TaskCompletion is a durable review dispatch intent and conservative allocation.
// Only a confirmed Fresh response permits the initial dispatch. Every subsequent
// observation is reconciliation-only, including a lost initial acknowledgment.
// Unknown usage remains charged until separately authenticated settlement.
type TaskCompletion struct {
	SchemaVersion int                   `json:"schema_version"`
	Request       TaskCompletionRequest `json:"request"`
	RequestDigest string                `json:"request_digest"`
	Reservation   execution.Reservation `json:"reservation"`
	ObservedAt    time.Time             `json:"observed_at"`
	Digest        string                `json:"digest"`
	Fresh         bool                  `json:"-"`
}

// SealTaskCompletion detaches a storage-produced dispatch receipt.
func SealTaskCompletion(c TaskCompletion) (TaskCompletion, error) {
	req, digest, err := c.Request.Freeze()
	if err != nil {
		return TaskCompletion{}, err
	}
	_, reservationDigest, err := req.Reservation.Freeze()
	if err != nil || c.SchemaVersion != 1 || digest != c.RequestDigest || c.Reservation.Validate() != nil || c.Reservation.RequestDigest != reservationDigest || c.Reservation.Settlement != nil || c.ObservedAt.IsZero() || c.Reservation.CreatedAt.After(c.ObservedAt) || !c.ObservedAt.Before(req.Handoff.Deadline) {
		return TaskCompletion{}, ErrInvalidReview
	}
	c.Request, c.Digest, c.Fresh = req, "", false
	data, err := json.Marshal(c)
	if err != nil || len(data) > MaxHandoffBytes+64<<10 {
		return TaskCompletion{}, ErrInvalidReview
	}
	c.Digest = evidence.Digest(data)
	return c, nil
}

// Validate checks immutable identity and allocation, not current authority.
func (c TaskCompletion) Validate() error {
	sealed, err := SealTaskCompletion(c)
	if err != nil || sealed.Digest != c.Digest {
		return ErrInvalidReview
	}
	return nil
}

// ValidateTaskCompletion checks the exact producer and independent review input
// set before any reviewer dispatch. The store also validates retained execution.
func ValidateTaskCompletion(request TaskCompletionRequest, subject Assignment, binding verification.Binding, p planning.Plan, at time.Time) error {
	req, _, err := request.Freeze()
	if err != nil || subject.Validate() != nil || subject.State != AssignmentSubmitted || subject.Submission == nil || ValidateHandoffPlan(subject.Handoff, p, at) != nil || ValidateHandoffPlan(req.Handoff, p, at) != nil || req.AssignmentID != subject.Handoff.AssignmentID || req.AssignmentRevision != subject.Revision || req.AssignmentDigest != subject.Digest || req.BindingDigest != subject.Submission.BindingDigest || req.Handoff.TaskID != subject.Handoff.TaskID || req.Handoff.ParentRunID != subject.Handoff.ParentRunID || req.Handoff.Sender != subject.Handoff.Sender || req.Actor != subject.Handoff.Sender.Principal || at.Before(subject.At) {
		return ErrInvalidReview
	}
	digest, err := binding.Digest()
	if err != nil || digest != req.BindingDigest || binding.RunID != subject.Submission.RunID || binding.Context.PrincipalRef != subject.Handoff.Receiver.Principal || req.RunID == binding.RunID || req.PreparedDigest == binding.Context.PreparedDigest || req.Handoff.Receiver.Principal == subject.Handoff.Receiver.Principal || (req.Handoff.Receiver.Agent == subject.Handoff.Receiver.Agent && req.Handoff.Receiver.Node == subject.Handoff.Receiver.Node) {
		return ErrInvalidReview
	}
	var policy *planning.ReviewRequirement
	for _, task := range p.Tasks {
		if task.ID == req.Handoff.TaskID {
			policy = task.Review
			break
		}
	}
	if policy == nil || policy.ReviewerAgent != req.Handoff.Receiver.Agent || policy.ReviewerPrincipal != req.Handoff.Receiver.Principal {
		return ErrInvalidReview
	}
	refs := append([]evidence.Ref{binding.Candidate}, binding.Evidence...)
	if len(refs) != len(req.Handoff.Inputs) {
		return ErrInvalidReview
	}
	for _, ref := range refs {
		found := false
		for _, input := range req.Handoff.Inputs {
			if reflect.DeepEqual(ref, input) {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalidReview
		}
	}
	return nil
}

// TaskCompletionStore atomically reserves review identity and shared capacity.
// It does not execute or retry reviewers. Get returns nil when no intent exists.
// Hosts must authorize both methods; stored metadata does not grant access.
type TaskCompletionStore interface {
	ReserveTaskCompletion(context.Context, TaskCompletionRequest) (TaskCompletion, error)
	GetTaskCompletion(context.Context, id.ID) (*TaskCompletion, error)
}

// TaskCompletionReviewer executes only a fresh reserved independent reviewer.
// Execute must authenticate current grants, fence execution, create the reserved
// RunID once and reject existing or uncertain creation. It must enforce prepared
// and input digests, no effect tools and the reserved budget. Observe resolves
// only retained output for that run with current read authority; nil means pending.
// Neither method may redispatch an uncertain run. Both honor cancellation.
type TaskCompletionReviewer interface {
	Execute(context.Context, TaskCompletion) error
	Observe(context.Context, TaskCompletion) (*Review, error)
}
