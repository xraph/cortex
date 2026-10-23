// Package coordination defines accountable assignments around Cortex execution.
// Message integrity does not grant authority or establish task acceptance.
package coordination

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	PrefixAssignment id.Prefix = "assign"
	PrefixHandoff    id.Prefix = "hoff"
	MaxHandoffBytes            = 256 << 10
)

var (
	ErrInvalidHandoff     = cortex.ErrInvalidHandoff
	ErrHandoffUnavailable = cortex.ErrHandoffUnavailable
)

// Identity is a claimed endpoint and principal. A host resolver must bind it to
// authenticated local or remote identity before accepting the assignment.
type Identity struct {
	Agent     string `json:"agent"`
	Node      string `json:"node,omitempty"`
	Principal string `json:"principal"`
}

// HandoffPurpose selects production or a review without effect tools.
type HandoffPurpose string

const (
	HandoffWork   HandoffPurpose = "work"
	HandoffReview HandoffPurpose = "review"
)

// Handoff is an immutable proposed assignment. Fence is an asserted scheduler
// generation, not proof of ownership. The receiving host must verify it against
// its durable assignment before accepting work. No message means task success.
type Handoff struct {
	SchemaVersion  int                `json:"schema_version"`
	ID             id.ID              `json:"id"`
	AssignmentID   id.ID              `json:"assignment_id"`
	Fence          uint64             `json:"fence"`
	Scope          cortex.Scope       `json:"scope"`
	PlanID         id.ID              `json:"plan_id"`
	PlanRevision   int64              `json:"plan_revision"`
	PlanDigest     string             `json:"plan_digest"`
	TaskID         id.ID              `json:"task_id"`
	ParentRunID    id.AgentRunID      `json:"parent_run_id"`
	Sender         Identity           `json:"sender"`
	Receiver       Identity           `json:"receiver"`
	Purpose        HandoffPurpose     `json:"purpose"`
	GrantRef       string             `json:"grant_ref"`
	Authority      planning.Authority `json:"authority"`
	ContractDigest string             `json:"contract_digest"`
	Criteria       []string           `json:"criteria"`
	Inputs         []evidence.Ref     `json:"inputs,omitempty"`
	Deadline       time.Time          `json:"deadline"`
	Digest         string             `json:"digest"`
}

// SealHandoff checks structure and detaches the canonical proposed assignment.
func SealHandoff(h Handoff) (Handoff, error) {
	h.Digest = ""
	if h.validateFields() != nil {
		return Handoff{}, ErrInvalidHandoff
	}
	data, err := json.Marshal(h)
	if err != nil || len(data) > MaxHandoffBytes {
		return Handoff{}, ErrInvalidHandoff
	}
	var detached Handoff
	if json.Unmarshal(data, &detached) != nil {
		return Handoff{}, ErrInvalidHandoff
	}
	detached.Digest = evidence.Digest(data)
	encoded, err := json.Marshal(detached)
	if err != nil || len(encoded) > MaxHandoffBytes {
		return Handoff{}, ErrInvalidHandoff
	}
	return detached, nil
}

// Validate checks integrity only. Use ValidateHandoff against current host data
// before accepting work; peer-provided grants and plan digests cannot authorize it.
func (h Handoff) Validate() error {
	sealed, err := SealHandoff(h)
	if err != nil || sealed.Digest != h.Digest {
		return ErrInvalidHandoff
	}
	return nil
}
func handoffText(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func (i Identity) valid() bool {
	return handoffText(i.Agent, 256) && handoffText(i.Principal, 512) && (i.Node == "" || handoffText(i.Node, 512))
}
func (h Handoff) validateFields() error {
	if h.SchemaVersion != 1 || h.ID.IsNil() || h.ID.Prefix() != PrefixHandoff || h.AssignmentID.IsNil() || h.AssignmentID.Prefix() != PrefixAssignment || h.Fence == 0 || cortex.ValidateScope(h.Scope) != nil || h.PlanID.IsNil() || h.PlanID.Prefix() != planning.PrefixPlan || h.PlanRevision < 1 || !evidence.ValidDigest(h.PlanDigest) || h.TaskID.IsNil() || h.TaskID.Prefix() != planning.PrefixTask || h.ParentRunID.IsNil() || h.ParentRunID.Prefix() != id.PrefixAgentRun || !h.Sender.valid() || !h.Receiver.valid() || (h.Sender.Agent == h.Receiver.Agent && h.Sender.Node == h.Receiver.Node) || !handoffText(h.GrantRef, 512) || h.Authority.Validate() != nil || !evidence.ValidDigest(h.ContractDigest) || !h.Deadline.Equal(h.Authority.Budget.Deadline) || len(h.Criteria) < 1 || len(h.Criteria) > 256 || len(h.Inputs) > 256 {
		return ErrInvalidHandoff
	}
	if h.Purpose != HandoffWork && h.Purpose != HandoffReview {
		return ErrInvalidHandoff
	}
	if h.Purpose == HandoffReview {
		if len(h.Authority.Effects) != 0 {
			return ErrInvalidHandoff
		}
		for _, grant := range h.Authority.Resources {
			if grant.Access != planning.AccessRead {
				return ErrInvalidHandoff
			}
		}
	}
	criteria := make(map[string]bool, len(h.Criteria))
	for _, criterion := range h.Criteria {
		if !handoffText(criterion, 256) || criteria[criterion] {
			return ErrInvalidHandoff
		}
		criteria[criterion] = true
	}
	inputs := make(map[id.ID]bool, len(h.Inputs))
	for _, ref := range h.Inputs {
		if ref.Validate() != nil || ref.Scope.Canonical() != h.Scope.Canonical() || inputs[ref.ID] || h.Deadline.After(ref.RetainUntil) {
			return ErrInvalidHandoff
		}
		inputs[ref.ID] = true
	}
	return nil
}

// HandoffAuthority comes from the receiving host's current identity and grant
// resolver. Assignment and fence must come from durable scheduler ownership.
type HandoffAuthority struct {
	Sender, Receiver Identity
	AssignmentID     id.ID
	ParentRunID      id.AgentRunID
	Fence            uint64
	GrantRef         string
	Ceiling          planning.Authority
	// ProducerPrincipal is required for review and comes from host provenance.
	ProducerPrincipal string
}

// ValidateHandoff checks a peer proposal against current local plan and grants.
// Inputs still require authorized content reads and trusted verification before
// they can satisfy task dependencies. This method performs no I/O.
func ValidateHandoff(h Handoff, p planning.Plan, current HandoffAuthority, at time.Time) error {
	if ValidateHandoffPlan(h, p, at) != nil || h.Sender != current.Sender || h.Receiver != current.Receiver || h.AssignmentID != current.AssignmentID || h.ParentRunID != current.ParentRunID || h.Fence != current.Fence || h.GrantRef != current.GrantRef || !h.Authority.Within(current.Ceiling) {
		return ErrInvalidHandoff
	}
	if h.Purpose == HandoffReview && (!handoffText(current.ProducerPrincipal, 512) || h.Receiver.Principal == current.ProducerPrincipal) {
		return ErrInvalidHandoff
	}
	return nil
}

// ValidateHandoffPlan checks the proposal against sealed plan definitions and
// time. It does not authenticate a principal, grant or scheduler generation.
func ValidateHandoffPlan(h Handoff, p planning.Plan, at time.Time) error {
	if h.Validate() != nil || planning.Validate(p, planning.Limits{}) != nil || at.IsZero() || !at.Before(h.Deadline) || h.Scope.Canonical() != p.Scope.Canonical() || h.PlanID != p.ID || h.PlanRevision != p.Revision || h.PlanDigest != p.Digest || h.ContractDigest != p.Contract.Digest {
		return ErrInvalidHandoff
	}
	ceiling, err := planning.TaskAuthority(p, h.TaskID)
	if err != nil || !h.Authority.Within(ceiling) {
		return ErrInvalidHandoff
	}
	for _, task := range p.Tasks {
		if task.ID == h.TaskID && !slices.Equal(h.Criteria, task.Criteria) {
			return ErrInvalidHandoff
		}
	}
	for _, ref := range h.Inputs {
		if ref.CreatedAt.After(at) {
			return ErrInvalidHandoff
		}
	}
	return nil
}

// DecodeHandoff refuses unknown fields and trailing payloads before validating
// the sealed proposal. Decoding does not authenticate identities or grants.
func DecodeHandoff(data []byte) (Handoff, error) {
	if len(data) == 0 || len(data) > MaxHandoffBytes {
		return Handoff{}, ErrInvalidHandoff
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var h Handoff
	if decoder.Decode(&h) != nil {
		return Handoff{}, ErrInvalidHandoff
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || h.Validate() != nil {
		return Handoff{}, ErrInvalidHandoff
	}
	return h, nil
}
