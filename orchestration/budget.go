package orchestration

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// BudgetPolicy pins one team's shared capacity and per-invocation ceilings.
// Each participant may run several times, consuming another fan-out slot each
// time. This account is independent of task plans and grants no execution access.
type BudgetPolicy struct {
	SchemaVersion   int                            `json:"schema_version"`
	ExecutionID     id.OrchestrationID             `json:"execution_id"`
	PreparedDigest  string                         `json:"prepared_digest"`
	Scope           cortex.Scope                   `json:"scope"`
	Limits          execution.Resources            `json:"limits"`
	Participants    map[string]execution.Resources `json:"participants"`
	Deadline        time.Time                      `json:"deadline"`
	TokenOnly       bool                           `json:"token_only"`
	PricingRevision string                         `json:"pricing_revision,omitempty"`
}

// Freeze validates and detaches the immutable policy.
func (p BudgetPolicy) Freeze() (BudgetPolicy, string, error) {
	if p.ExecutionID.IsNil() || p.ExecutionID.Prefix() != id.PrefixOrchestration || !evidence.ValidDigest(p.PreparedDigest) || len(p.Participants) < 1 || len(p.Participants) > 257 || p.Limits.Replans != 0 {
		return BudgetPolicy{}, "", execution.ErrInvalidReservation
	}
	if _, _, err := p.AccountingPolicy().Freeze(); err != nil {
		return BudgetPolicy{}, "", err
	}
	for name, limit := range p.Participants {
		if !localRosterName(name) || !utf8.ValidString(name) || limit.FanOut != 1 || !p.Limits.Covers(limit) {
			return BudgetPolicy{}, "", execution.ErrInvalidReservation
		}
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 128<<10 {
		return BudgetPolicy{}, "", execution.ErrInvalidReservation
	}
	var frozen BudgetPolicy
	if json.Unmarshal(data, &frozen) != nil {
		return BudgetPolicy{}, "", execution.ErrInvalidReservation
	}
	return frozen, evidence.Digest(data), nil
}

// AccountingPolicy adapts the team to shared reservation arithmetic. PlanID is
// the legacy accounting key; here it contains an orchestration ID, not a plan.
func (p BudgetPolicy) AccountingPolicy() execution.ReservationPolicy {
	return execution.ReservationPolicy{SchemaVersion: p.SchemaVersion, PlanID: p.ExecutionID, Scope: p.Scope, Limits: p.Limits, Deadline: p.Deadline, TokenOnly: p.TokenOnly, PricingRevision: p.PricingRevision}
}

// ValidateReservation binds a leaf to this team, prepared artifact, participant
// and reserved agent run. PlanID identifies the team; TaskID identifies the run.
func (p BudgetPolicy) ValidateReservation(r execution.ReservationRequest) error {
	if _, _, err := p.Freeze(); err != nil {
		return err
	}
	if _, _, err := r.Freeze(); err != nil {
		return err
	}
	limit, ok := p.Participants[r.OwnerRef]
	if !ok || r.PlanID != p.ExecutionID || r.PlanDigest != p.PreparedDigest || r.Scope.Canonical() != p.Scope.Canonical() || r.TaskID.Prefix() != id.PrefixAgentRun || r.Kind != execution.ReservationLeaf || !r.ParentID.IsNil() || !limit.Covers(r.Allocation) || r.Deadline.After(p.Deadline) || r.MoneyBound == p.TokenOnly || r.PricingRevision != p.PricingRevision {
		return execution.ErrInvalidReservation
	}
	return nil
}

// BudgetSnapshot includes the admission seal and conservative retained charges.
type BudgetSnapshot struct {
	Policy       BudgetPolicy        `json:"policy"`
	Charged      execution.Resources `json:"charged"`
	Overflow     bool                `json:"overflow"`
	Reservations int                 `json:"reservations"`
	Closed       bool                `json:"closed"`
}

// BudgetStore persists team allocations independently of plan/claim accounting.
// Settlement requires trusted host usage. Unknown usage remains allocated, even
// after closure or expiry. A historical reservation is never dispatch authority.
type BudgetStore interface {
	CreateOrchestrationBudget(context.Context, BudgetPolicy) error
	GetOrchestrationBudget(context.Context, id.OrchestrationID) (BudgetPolicy, error)
	ReserveOrchestrationChild(context.Context, execution.ReservationRequest) (execution.Reservation, error)
	SettleOrchestrationChild(context.Context, execution.Settlement) error
	CloseOrchestrationBudget(context.Context, id.OrchestrationID) error
	GetOrchestrationBudgetSnapshot(context.Context, id.OrchestrationID) (BudgetSnapshot, error)
	GetReservation(context.Context, id.ID) (execution.Reservation, error)
	ListOrchestrationReservations(context.Context, execution.ReservationListRequest) ([]execution.Reservation, error)
}
