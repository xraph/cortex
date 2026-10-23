package coordination

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

var (
	ErrInvalidAssignment     = cortex.ErrInvalidAssignment
	ErrAssignmentConflict    = cortex.ErrAssignmentConflict
	ErrAssignmentNotFound    = cortex.ErrAssignmentNotFound
	ErrAssignmentUnavailable = cortex.ErrAssignmentUnavailable
)

// AssignmentState records host-observed progress. None of these states grants
// dispatch permission or establishes verified task success.
type AssignmentState string

const (
	AssignmentProposed  AssignmentState = "proposed"
	AssignmentAccepted  AssignmentState = "accepted"
	AssignmentWorking   AssignmentState = "working"
	AssignmentBlocked   AssignmentState = "blocked"
	AssignmentSubmitted AssignmentState = "submitted"
	AssignmentRejected  AssignmentState = "rejected"
	AssignmentCancelled AssignmentState = "cancelled"
	MaxAssignmentBytes                  = MaxHandoffBytes + 16<<10
)

// Assignment is one immutable journal revision, including its original proposal.
// Submissions require retained evidence admission. Cancellation does not prove
// external effects stopped, and a submission never establishes task success.
type Assignment struct {
	SchemaVersion  int             `json:"schema_version"`
	Handoff        Handoff         `json:"handoff"`
	Revision       int64           `json:"revision"`
	State          AssignmentState `json:"state"`
	Actor          string          `json:"actor"`
	Reason         string          `json:"reason"`
	At             time.Time       `json:"at"`
	PreviousDigest string          `json:"previous_digest,omitempty"`
	RequestDigest  string          `json:"request_digest"`
	Submission     *Submission     `json:"submission,omitempty"`
	Digest         string          `json:"digest"`
}

// AssignmentChange binds an observation to the original assignment generation.
// Actor must come from authenticated host context, never an unchecked peer field.
type AssignmentChange struct {
	AssignmentID     id.ID           `json:"assignment_id"`
	ExpectedRevision int64           `json:"expected_revision"`
	HandoffDigest    string          `json:"handoff_digest"`
	Fence            uint64          `json:"fence"`
	State            AssignmentState `json:"state"`
	Actor            string          `json:"actor"`
	Reason           string          `json:"reason"`
	Submission       *Submission     `json:"submission,omitempty"`
}

// BindingDigest validates and hashes every caller-controlled transition field.
func (c AssignmentChange) BindingDigest() (string, error) {
	_, digest, err := c.Freeze()
	return digest, err
}

// Freeze detaches every transition field before host storage callbacks.
func (c AssignmentChange) Freeze() (AssignmentChange, string, error) {
	if c.AssignmentID.IsNil() || c.AssignmentID.Prefix() != PrefixAssignment || c.ExpectedRevision < 1 || c.ExpectedRevision == math.MaxInt64 || !evidence.ValidDigest(c.HandoffDigest) || c.Fence == 0 || !c.State.valid() || c.State == AssignmentProposed || !handoffText(c.Actor, 512) || !handoffText(c.Reason, 4096) {
		return AssignmentChange{}, "", ErrInvalidAssignment
	}
	if c.State == AssignmentSubmitted {
		if c.Submission == nil || c.Submission.Validate() != nil {
			return AssignmentChange{}, "", ErrInvalidAssignment
		}
	} else if c.Submission != nil {
		return AssignmentChange{}, "", ErrInvalidAssignment
	}
	data, err := json.Marshal(c)
	if err != nil {
		return AssignmentChange{}, "", ErrInvalidAssignment
	}
	var frozen AssignmentChange
	if json.Unmarshal(data, &frozen) != nil {
		return AssignmentChange{}, "", ErrInvalidAssignment
	}
	return frozen, evidence.Digest(data), nil
}

func (s AssignmentState) valid() bool {
	switch s {
	case AssignmentProposed, AssignmentAccepted, AssignmentWorking, AssignmentBlocked, AssignmentSubmitted, AssignmentRejected, AssignmentCancelled:
		return true
	}
	return false
}

// Terminal reports whether the progress journal can accept another observation.
func (s AssignmentState) Terminal() bool { return s == AssignmentRejected || s == AssignmentCancelled }

func sealAssignment(a Assignment) (Assignment, error) {
	a.Digest = ""
	if a.SchemaVersion != 1 || a.Handoff.Validate() != nil || a.Revision < 1 || !a.State.valid() || !handoffText(a.Actor, 512) || !handoffText(a.Reason, 4096) || a.At.IsZero() || !evidence.ValidDigest(a.RequestDigest) {
		return Assignment{}, ErrInvalidAssignment
	}
	if a.Submission != nil {
		if a.Submission.Validate() != nil || a.Handoff.Purpose != HandoffWork || a.Submission.RunID == a.Handoff.ParentRunID || (a.State != AssignmentSubmitted && !a.State.Terminal()) {
			return Assignment{}, ErrInvalidAssignment
		}
	} else if a.State == AssignmentSubmitted {
		return Assignment{}, ErrInvalidAssignment
	}
	switch {
	case a.Revision == 1:
		if a.State != AssignmentProposed || a.PreviousDigest != "" || a.RequestDigest != a.Handoff.Digest || a.Actor != a.Handoff.Sender.Principal || a.Reason != "proposed" || !a.At.Before(a.Handoff.Deadline) {
			return Assignment{}, ErrInvalidAssignment
		}
	case a.State == AssignmentProposed || !evidence.ValidDigest(a.PreviousDigest):
		return Assignment{}, ErrInvalidAssignment
	default:
		c := AssignmentChange{AssignmentID: a.Handoff.AssignmentID, ExpectedRevision: a.Revision - 1, HandoffDigest: a.Handoff.Digest, Fence: a.Handoff.Fence, State: a.State, Actor: a.Actor, Reason: a.Reason}
		if a.State == AssignmentSubmitted {
			c.Submission = a.Submission
		}
		digest, err := c.BindingDigest()
		if err != nil || digest != a.RequestDigest || !a.actorAllowed() || (!a.State.Terminal() && !a.At.Before(a.Handoff.Deadline)) {
			return Assignment{}, ErrInvalidAssignment
		}
	}
	data, err := json.Marshal(a)
	if err != nil || len(data) > MaxAssignmentBytes-80 {
		return Assignment{}, ErrInvalidAssignment
	}
	var detached Assignment
	if json.Unmarshal(data, &detached) != nil {
		return Assignment{}, ErrInvalidAssignment
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}
func (a Assignment) actorAllowed() bool {
	return a.Actor == a.Handoff.Receiver.Principal || a.State == AssignmentCancelled && a.Actor == a.Handoff.Sender.Principal
}

// Validate checks a snapshot's integrity; the store checks its predecessor.
func (a Assignment) Validate() error {
	sealed, err := sealAssignment(a)
	if err != nil || sealed.Digest != a.Digest {
		return ErrInvalidAssignment
	}
	return nil
}

// Propose creates a detached initial snapshot. The store must validate the
// proposal against the current plan under the same lock used by plan revisions.
func Propose(h Handoff, at time.Time) (Assignment, error) {
	return sealAssignment(Assignment{SchemaVersion: 1, Handoff: h, Revision: 1, State: AssignmentProposed, Actor: h.Sender.Principal, Reason: "proposed", At: at, RequestDigest: h.Digest})
}

// Advance records one legal observation without changing authority or ownership.
func Advance(previous Assignment, change AssignmentChange, at time.Time) (Assignment, error) {
	frozen, requestDigest, err := change.Freeze()
	change = frozen
	if err != nil || previous.Validate() != nil || at.IsZero() || at.Before(previous.At) {
		return Assignment{}, ErrInvalidAssignment
	}
	if change.AssignmentID != previous.Handoff.AssignmentID || change.ExpectedRevision != previous.Revision || change.HandoffDigest != previous.Handoff.Digest || change.Fence != previous.Handoff.Fence || !transitionAllowed(previous.State, change.State) {
		return Assignment{}, ErrAssignmentConflict
	}
	next := previous
	next.Revision++
	next.State, next.Actor, next.Reason, next.At = change.State, change.Actor, change.Reason, at
	next.PreviousDigest, next.RequestDigest = previous.Digest, requestDigest
	if change.Submission != nil {
		next.Submission = change.Submission
	}
	return sealAssignment(next)
}
func transitionAllowed(from, to AssignmentState) bool {
	if from.Terminal() {
		return false
	}
	if to == AssignmentCancelled {
		return true
	}
	switch from {
	case AssignmentProposed:
		return to == AssignmentAccepted || to == AssignmentRejected
	case AssignmentAccepted:
		return to == AssignmentWorking || to == AssignmentBlocked || to == AssignmentRejected
	case AssignmentWorking:
		return to == AssignmentBlocked || to == AssignmentSubmitted
	case AssignmentSubmitted:
		return to == AssignmentRejected
	case AssignmentBlocked:
		return to == AssignmentWorking || to == AssignmentRejected
	}
	return false
}

// AssignmentListRequest pages stable identities for one plan, in lexical order.
type AssignmentListRequest struct {
	PlanID id.ID
	After  id.ID
	Limit  int
}

// AssignmentHistoryRequest pages immutable revisions in ascending order.
type AssignmentHistoryRequest struct {
	AssignmentID  id.ID
	AfterRevision int64
	Limit         int
}

// AssignmentStore is an optional exact-scope progress journal, not an authority
// boundary. Hosts must authorize every call and fence grants and worker ownership
// separately. It is not sufficient on its own for HandoffReceiverConfig.Accept.
// Identical retries return the original observation even after expiry or replan;
// that historical acknowledgement must never be interpreted as permission to run.
// New progress serializes with plan revisions; terminal observations remain legal
// after expiry or replan. No method dispatches work or marks a task successful.
type AssignmentStore interface {
	ProposeAssignment(context.Context, Handoff) (Assignment, error)
	AdvanceAssignment(context.Context, AssignmentChange) (Assignment, error)
	GetAssignment(context.Context, id.ID) (Assignment, error)
	ListAssignments(context.Context, AssignmentListRequest) ([]Assignment, error)
	AssignmentHistory(context.Context, AssignmentHistoryRequest) ([]Assignment, error)
}
