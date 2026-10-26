package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	// PrefixTaskAdjudication identifies an immutable review policy decision.
	PrefixTaskAdjudication id.Prefix = "tadj"
	// MaxTaskAdjudicationBytes bounds stored decisions without copying artifacts.
	MaxTaskAdjudicationBytes = 32 << 10
)

// TaskAdjudicationRef pins a stored decision, never a caller-supplied verdict.
type TaskAdjudicationRef struct {
	ID     id.ID  `json:"id"`
	Digest string `json:"digest"`
}

// Validate checks identity and content binding without granting authority.
func (r TaskAdjudicationRef) Validate() error {
	if r.ID.IsNil() || r.ID.Prefix() != PrefixTaskAdjudication || !evidence.ValidDigest(r.Digest) {
		return ErrInvalidReview
	}
	return nil
}

// TaskAdjudicationRequest binds host policy to one immutable submitted result and
// retained independent review. Actor must be authenticated by the host. A human
// actor cannot provide an override verdict through this request.
type TaskAdjudicationRequest struct {
	SchemaVersion      int    `json:"schema_version"`
	ID                 id.ID  `json:"id"`
	PlanID             id.ID  `json:"plan_id"`
	PlanDigest         string `json:"plan_digest"`
	TaskID             id.ID  `json:"task_id"`
	AssignmentID       id.ID  `json:"assignment_id"`
	AssignmentRevision int64  `json:"assignment_revision"`
	AssignmentDigest   string `json:"assignment_digest"`
	BindingDigest      string `json:"binding_digest"`
	ReviewID           id.ID  `json:"review_id"`
	ReviewDigest       string `json:"review_digest"`
	PolicyRevision     string `json:"policy_revision"`
	Actor              string `json:"actor"`
	Reason             string `json:"reason"`
}

// Freeze detaches request metadata; the store resolves its evidence itself.
func (r TaskAdjudicationRequest) Freeze() (TaskAdjudicationRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixTaskAdjudication || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.TaskID.IsNil() || r.TaskID.Prefix() != planning.PrefixTask || r.AssignmentID.IsNil() || r.AssignmentID.Prefix() != PrefixAssignment || r.AssignmentRevision < 1 || r.ReviewID.IsNil() || r.ReviewID.Prefix() != PrefixReview || !evidence.ValidDigest(r.PlanDigest) || !evidence.ValidDigest(r.AssignmentDigest) || !evidence.ValidDigest(r.BindingDigest) || !evidence.ValidDigest(r.ReviewDigest) || !handoffText(r.PolicyRevision, 256) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskAdjudicationRequest{}, "", ErrInvalidReview
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxTaskAdjudicationBytes-1024 {
		return TaskAdjudicationRequest{}, "", ErrInvalidReview
	}
	return r, evidence.Digest(data), nil
}

// AdjudicationDecision is review policy only, not verification or acceptance.
type AdjudicationDecision string

const (
	AdjudicationSupported AdjudicationDecision = "supported"
	AdjudicationBlocked   AdjudicationDecision = "blocked"
)

// TaskAdjudication retains a derived policy decision. Contradictions and missing
// evidence stay blocked. Independent review never overrides failed verification.
type TaskAdjudication struct {
	SchemaVersion int                     `json:"schema_version"`
	Request       TaskAdjudicationRequest `json:"request"`
	RequestDigest string                  `json:"request_digest"`
	Decision      AdjudicationDecision    `json:"decision"`
	ObservedAt    time.Time               `json:"observed_at"`
	Digest        string                  `json:"digest"`
}

// SealTaskAdjudication validates the stored decision's integrity only.
func SealTaskAdjudication(a TaskAdjudication) (TaskAdjudication, error) {
	req, digest, err := a.Request.Freeze()
	if err != nil || a.SchemaVersion != 1 || a.RequestDigest != digest || a.ObservedAt.IsZero() || (a.Decision != AdjudicationSupported && a.Decision != AdjudicationBlocked) {
		return TaskAdjudication{}, ErrInvalidReview
	}
	a.Request, a.Digest = req, ""
	data, err := json.Marshal(a)
	if err != nil || len(data) > MaxTaskAdjudicationBytes-80 {
		return TaskAdjudication{}, ErrInvalidReview
	}
	a.Digest = evidence.Digest(data)
	return a, nil
}

// Validate checks integrity without treating the record as current evidence.
func (a TaskAdjudication) Validate() error {
	sealed, err := SealTaskAdjudication(a)
	if err != nil || sealed.Digest != a.Digest {
		return ErrInvalidReview
	}
	return nil
}

// PrepareTaskAdjudication derives a decision from retained review and plan pins.
// The store must independently validate both runs, submission and evidence bytes.
func PrepareTaskAdjudication(request TaskAdjudicationRequest, r Review, p planning.Plan, at time.Time) (TaskAdjudication, error) {
	req, digest, err := request.Freeze()
	if err != nil || r.Validate() != nil || planning.Validate(p, planning.Limits{}) != nil || at.IsZero() || r.ReviewedAt.After(at) || ValidateHandoffPlan(r.Handoff, p, at) != nil || req.PlanID != p.ID || req.PlanDigest != p.Digest || req.TaskID != r.Handoff.TaskID || req.AssignmentID != r.SubjectAssignmentID || req.AssignmentRevision != r.SubjectRevision || req.AssignmentDigest != r.SubjectDigest || req.BindingDigest != r.BindingDigest || req.ReviewID != r.ID || req.ReviewDigest != r.Digest {
		return TaskAdjudication{}, ErrInvalidReview
	}
	var policy *planning.ReviewRequirement
	for _, task := range p.Tasks {
		if task.ID == req.TaskID {
			policy = task.Review
			break
		}
	}
	if policy == nil || policy.PolicyRevision != req.PolicyRevision || policy.ReviewerAgent != r.Handoff.Receiver.Agent || policy.ReviewerPrincipal != r.Handoff.Receiver.Principal {
		return TaskAdjudication{}, ErrInvalidReview
	}
	decision := AdjudicationSupported
	for _, finding := range r.Findings {
		if finding.Assessment != ReviewSupported {
			decision = AdjudicationBlocked
		}
	}
	return SealTaskAdjudication(TaskAdjudication{SchemaVersion: 1, Request: req, RequestDigest: digest, Decision: decision, ObservedAt: at})
}

// TaskAdjudicationStore retains decisions atomically with the plan fence. Hosts
// authorize every read and write, including retries, against current policy.
type TaskAdjudicationStore interface {
	RecordTaskAdjudication(context.Context, TaskAdjudicationRequest) (TaskAdjudication, error)
	GetTaskAdjudication(context.Context, id.ID) (TaskAdjudication, error)
}
