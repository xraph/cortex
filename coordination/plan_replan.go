package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

// PrefixPlanReplan identifies one permanently charged proposal attempt.
const PrefixPlanReplan id.Prefix = "rplan"

// PlanReplanRequest binds trusted progress and a frozen, tool-free model run to
// one shared account allocation. Admission never grants execution authority.
type PlanReplanRequest struct {
	SchemaVersion  int                          `json:"schema_version"`
	ID             id.ID                        `json:"id"`
	Progress       planning.ProgressSnapshot    `json:"progress"`
	Policy         planning.ReplanPolicy        `json:"policy"`
	Reservation    execution.ReservationRequest `json:"reservation"`
	RunID          id.AgentRunID                `json:"run_id"`
	SessionID      id.SessionID                 `json:"session_id"`
	AgentID        id.AgentID                   `json:"agent_id"`
	PreparedDigest string                       `json:"prepared_digest"`
	InputDigest    string                       `json:"input_digest"`
	Actor          string                       `json:"actor"`
	Reason         string                       `json:"reason"`
}

// Freeze validates deterministic proposal eligibility and immutable bindings.
func (r PlanReplanRequest) Freeze() (PlanReplanRequest, string, error) {
	p := r.Progress.Plan
	policy, err := r.Policy.Normalize()
	if err != nil {
		return PlanReplanRequest{}, "", err
	}
	r.Policy = policy
	decision := planning.ProposeReplan(r.Progress, r.Policy)
	a := r.Reservation.Allocation
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixPlanReplan || decision.Action != planning.ReplanPropose || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.SessionID.IsNil() || r.SessionID.Prefix() != id.PrefixSession || r.AgentID.IsNil() || r.AgentID.Prefix() != id.PrefixAgent || !evidence.ValidDigest(r.PreparedDigest) || !evidence.ValidDigest(r.InputDigest) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return PlanReplanRequest{}, "", planning.ErrInvalid
	}
	if _, _, err = r.Reservation.Freeze(); err != nil {
		return PlanReplanRequest{}, "", err
	}
	if r.Reservation.PlanID != p.ID || r.Reservation.PlanDigest != p.Digest || r.Reservation.Scope.Canonical() != p.Scope.Canonical() || r.Reservation.OwnerRef != r.ID.String() || !r.Reservation.ParentID.IsNil() || r.Reservation.Kind != execution.ReservationLeaf || a.Replans != 1 || a.FanOut != 1 || a.ToolCalls != 0 || a.Steps < 1 || a.Tokens < 1 || r.Reservation.Deadline.After(p.Budget.Deadline) || !r.Progress.At.Before(r.Reservation.Deadline) {
		return PlanReplanRequest{}, "", planning.ErrInvalid
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 2<<20 {
		return PlanReplanRequest{}, "", planning.ErrInvalid
	}
	var frozen PlanReplanRequest
	if json.Unmarshal(data, &frozen) != nil {
		return PlanReplanRequest{}, "", planning.ErrInvalid
	}
	return frozen, evidence.Digest(data), nil
}

// PlanReplanAdmission is the retained attempt and charged allocation. Fresh is
// returned only by the transaction that first admitted it, never by a read.
type PlanReplanAdmission struct {
	SchemaVersion int                   `json:"schema_version"`
	Request       PlanReplanRequest     `json:"request"`
	RequestDigest string                `json:"request_digest"`
	Reservation   execution.Reservation `json:"reservation"`
	At            time.Time             `json:"at"`
	Fresh         bool                  `json:"-"`
}

// Validate checks integrity without authenticating the caller or authorizing dispatch.
func (a PlanReplanAdmission) Validate() error {
	_, digest, err := a.Request.Freeze()
	if err != nil || a.SchemaVersion != 1 || digest != a.RequestDigest || a.Reservation.Validate() != nil || a.Reservation.Settlement != nil || a.At.IsZero() || !a.At.Equal(a.Reservation.CreatedAt) || a.At.Before(a.Request.Progress.At) || !a.At.Before(a.Request.Reservation.Deadline) {
		return planning.ErrInvalid
	}
	_, reservationDigest, err := a.Request.Reservation.Freeze()
	if err != nil || reservationDigest != a.Reservation.RequestDigest {
		return planning.ErrInvalid
	}
	return nil
}

// PlanReplanOutcome retains terminal model observations, including malformed or
// failed proposals. Accounting remains charged even when Proposed is nil.
type PlanReplanOutcome struct {
	SchemaVersion   int                         `json:"schema_version"`
	AttemptID       id.ID                       `json:"attempt_id"`
	AdmissionDigest string                      `json:"admission_digest"`
	RunRevision     int64                       `json:"run_revision"`
	RunDigest       string                      `json:"run_digest"`
	Proposed        *planning.Plan              `json:"proposed,omitempty"`
	Abandonment     *PlanReplanAbandonment      `json:"abandonment,omitempty"`
	Settlement      execution.SettlementReceipt `json:"settlement"`
	At              time.Time                   `json:"at"`
	Digest          string                      `json:"digest"`
}

// SealPlanReplanOutcome detaches and fingerprints retained model disposition.
func SealPlanReplanOutcome(o PlanReplanOutcome) (PlanReplanOutcome, error) {
	if o.SchemaVersion != 1 || o.AttemptID.IsNil() || o.AttemptID.Prefix() != PrefixPlanReplan || !evidence.ValidDigest(o.AdmissionDigest) || o.At.IsZero() || o.Settlement.At.IsZero() || o.At.Before(o.Settlement.At) {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	if o.Abandonment == nil {
		if o.RunRevision < 0 || !evidence.ValidDigest(o.RunDigest) {
			return PlanReplanOutcome{}, planning.ErrInvalid
		}
	} else if o.Abandonment.validate() != nil || o.Abandonment.AdmissionDigest != o.AdmissionDigest || o.RunDigest != "" || o.RunRevision != 0 || o.Proposed != nil {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	if o.Proposed != nil && planning.Validate(*o.Proposed, planning.Limits{}) != nil {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	_, digest, err := o.Settlement.Request.Freeze()
	if err != nil || digest != o.Settlement.Digest || o.Settlement.Request.Usage.Known&execution.KnownReplans == 0 || o.Settlement.Request.Usage.Actual.Replans != 1 {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	o.Digest = ""
	data, err := json.Marshal(o)
	if err != nil || len(data) > 2<<20 {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	var frozen PlanReplanOutcome
	if json.Unmarshal(data, &frozen) != nil {
		return PlanReplanOutcome{}, planning.ErrInvalid
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate checks outcome integrity; hashes do not authenticate provider work.
func (o PlanReplanOutcome) Validate() error {
	f, err := SealPlanReplanOutcome(o)
	if err != nil || f.Digest != o.Digest {
		return planning.ErrInvalid
	}
	return nil
}

// PlanReplanAbandonment preserves the replan proof name for prepared-run closure.
type PlanReplanAbandonment = PreparedRunAbandonment

// PlanReplanControl binds current authority to one admitted operation. Dispatch
// spans admission and the entire runtime callback. Commit proofs must cover all
// current dispatched ownership, including tasks removed from the proposed graph.
type PlanReplanControl struct {
	Request     PlanReplanRequest      `json:"request"`
	Operation   string                 `json:"operation"`
	Abandonment *PlanReplanAbandonment `json:"abandonment,omitempty"`
	Claims      []TaskRecoveryProof    `json:"claims,omitempty"`
}

// Freeze bounds and detaches current authority inputs.
func (c PlanReplanControl) Freeze() (PlanReplanControl, string, error) {
	req, digest, err := c.Request.Freeze()
	if err != nil {
		return PlanReplanControl{}, "", err
	}
	c.Request = req
	if c.Operation != "dispatch" && c.Operation != "observe" && c.Operation != "commit" {
		return PlanReplanControl{}, "", planning.ErrInvalid
	}
	if c.Abandonment != nil && (c.Operation != "observe" || c.Abandonment.validate() != nil || c.Abandonment.AdmissionDigest != digest) {
		return PlanReplanControl{}, "", planning.ErrInvalid
	}
	if len(c.Claims) > planning.MaxProgressAttempts || (c.Operation != "commit" && len(c.Claims) != 0) {
		return PlanReplanControl{}, "", planning.ErrInvalid
	}
	seen := map[string]bool{}
	for _, proof := range c.Claims {
		if _, _, err = proof.Freeze(); err != nil || seen[proof.ClaimDigest] {
			return PlanReplanControl{}, "", planning.ErrInvalid
		}
		seen[proof.ClaimDigest] = true
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > 10<<20 {
		return PlanReplanControl{}, "", planning.ErrInvalid
	}
	var frozen PlanReplanControl
	if json.Unmarshal(data, &frozen) != nil {
		return PlanReplanControl{}, "", planning.ErrInvalid
	}
	return frozen, evidence.Digest(data), nil
}

// PlanReplanStore persists bounded proposals independently of plan definitions.
// Implementations serialize admission and commit with all plan ownership writes.
// These trusted storage primitives must be called through current host guards.
type PlanReplanStore interface {
	GetPlan(context.Context, id.ID) (planning.Plan, error)
	AdmitPlanReplan(context.Context, PlanReplanRequest) (PlanReplanAdmission, error)
	GetPlanReplan(context.Context, id.ID) (*PlanReplanAdmission, error)
	ListPlanReplans(context.Context, id.ID) ([]PlanReplanAdmission, error)
	ObservePlanReplan(context.Context, PlanReplanControl) (*PlanReplanOutcome, error)
	GetPlanReplanOutcome(context.Context, id.ID) (*PlanReplanOutcome, error)
	GetPlanReplanCommit(context.Context, id.ID) (*planning.Plan, error)
	CommitPlanReplan(context.Context, PlanReplanControl) (planning.Plan, error)
}
