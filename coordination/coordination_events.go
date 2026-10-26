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
)

// Event is private immutable transition metadata. Sequence orders
// index materialization only, not historical occurrence across source journals.
// Source revisions and predecessor digests retain their original causal order.
// EventID reuses an existing outbox ID or a stable source identity hash.
type Event struct {
	SchemaVersion     int           `json:"schema_version"`
	Scope             cortex.Scope  `json:"scope"`
	PlanID            id.ID         `json:"plan_id"`
	Sequence          int64         `json:"sequence"`
	EventID           string        `json:"event_id"`
	Type              string        `json:"type"`
	SourceKind        string        `json:"source_kind"`
	SourceID          string        `json:"source_id"`
	SourceDigest      string        `json:"source_digest"`
	PlanRevision      int64         `json:"plan_revision,omitempty"`
	ClaimRevision     int64         `json:"claim_revision,omitempty"`
	PredecessorDigest string        `json:"predecessor_digest,omitempty"`
	TaskID            id.ID         `json:"task_id,omitempty"`
	RunID             id.AgentRunID `json:"run_id,omitempty"`
	Artifact          *evidence.Ref `json:"artifact,omitempty"`
	ObservedAt        time.Time     `json:"observed_at,omitempty"`
	Digest            string        `json:"digest"`
}

// SealEvent detaches bounded metadata. Sequence zero is a source
// projection awaiting atomic index insertion, never a deliverable event.
func SealEvent(e Event) (Event, error) {
	if e.SchemaVersion != 1 || cortex.ValidateScope(e.Scope) != nil || e.PlanID.IsNil() || e.PlanID.Prefix() != planning.PrefixPlan || e.Sequence < 0 || e.Sequence == math.MaxInt64 || !handoffText(e.EventID, 512) || !handoffText(e.Type, 128) || !handoffText(e.SourceKind, 128) || !handoffText(e.SourceID, 1024) || !evidence.ValidDigest(e.SourceDigest) || e.PlanRevision < 0 || e.ClaimRevision < 0 || (e.PredecessorDigest != "" && !evidence.ValidDigest(e.PredecessorDigest)) || (!e.TaskID.IsNil() && e.TaskID.Prefix() != planning.PrefixTask) || (!e.RunID.IsNil() && e.RunID.Prefix() != id.PrefixAgentRun) || (e.Artifact != nil && (e.Artifact.Validate() != nil || e.Artifact.Scope.Canonical() != e.Scope.Canonical())) {
		return Event{}, planning.ErrInvalid
	}
	e.Digest = ""
	data, err := json.Marshal(e)
	if err != nil || len(data) > 32<<10 {
		return Event{}, planning.ErrInvalid
	}
	var frozen Event
	if json.Unmarshal(data, &frozen) != nil {
		return Event{}, planning.ErrInvalid
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate checks indexed metadata integrity without authorizing delivery.
func (e Event) Validate() error {
	frozen, err := SealEvent(e)
	if err != nil || e.Sequence < 1 || frozen.Digest != e.Digest {
		return planning.ErrInvalid
	}
	return nil
}

// EventSources returns the finite importer categories. Claim and
// effect history categories include superseded ownership incarnations.
func EventSources() []string {
	return []string{"task_ready_claim", "task_ready_dispatch", "task_run_start", "task_execution_renewal", "task_acceptance", "plan_replan", "plan_replan_outcome", "plan_replan_commit", "artifact_board_identity", "task_claim_history", "effect_continuation_history"}
}

// EventImport bounds one source-journal page. After is an opaque
// storage cursor. An empty Next in the response completes that source pass.
type EventImport struct {
	Scope         cortex.Scope
	PlanID        id.ID
	Source, After string
	Limit         int
}

// Validate rejects unbounded or unsupported source scans.
func (r EventImport) Validate() error {
	valid := false
	for _, source := range EventSources() {
		if source == r.Source {
			valid = true
		}
	}
	if !valid || cortex.ValidateScope(r.Scope) != nil || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || len(r.After) > 4096 || r.Limit < 1 || r.Limit > 256 {
		return planning.ErrInvalid
	}
	return nil
}

// EventImportPage reports bounded materialization progress. Imports
// are idempotent across restarts; callers may safely restart a pass from zero.
type EventImportPage struct {
	Next     string
	Imported int
}

// EventPage selects an exact plan and finite metadata sequence page.
type EventPage struct {
	Scope  cortex.Scope
	PlanID id.ID
	After  int64
	Limit  int
}

// Validate checks scope, identity and page bounds before access.
func (r EventPage) Validate() error {
	if cortex.ValidateScope(r.Scope) != nil || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.After < 0 || r.After == math.MaxInt64 || r.Limit < 1 || r.Limit > 256 {
		return planning.ErrInvalid
	}
	return nil
}

// Delivery binds an authenticated sink acknowledgment to one exact
// immutable event. Reference is a private receipt identifier, never event content.
type Delivery struct {
	Event     Event  `json:"event"`
	SinkID    string `json:"sink_id"`
	Reference string `json:"reference"`
}

// Validate checks receipt bindings. The trusted sink must authenticate transport
// and durable receipt identity; a string or hash alone is not external proof.
func (r Delivery) Validate() error {
	if r.Event.Validate() != nil || !handoffText(r.SinkID, 512) || !handoffText(r.Reference, 1024) {
		return planning.ErrInvalid
	}
	return nil
}

// EventStore indexes retained source intents and acknowledges exact
// sink receipts. It never calls a provider or publishes run content.
type EventStore interface {
	ImportCoordinationEvents(context.Context, EventImport) (EventImportPage, error)
	ListCoordinationEvents(context.Context, EventPage) ([]Event, error)
	GetCoordinationDelivery(context.Context, Event, string) (*Delivery, error)
	AcknowledgeCoordinationEvent(context.Context, Delivery) error
}
