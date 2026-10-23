// Package event defines bounded, metadata-only public execution replay.
package event

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

const PrefixEvent id.Prefix = "pevt"

const (
	MaxBatchSize      = 32
	MaxPageSize       = 128
	MaxRetainedEvents = 256
)

var (
	ErrInvalid     = cortex.ErrInvalidPublicEvent
	ErrConflict    = cortex.ErrPublicEventConflict
	ErrCursorGap   = cortex.ErrPublicEventCursorGap
	ErrUnavailable = cortex.ErrPublicEventUnavailable
)

// Type identifies a public lifecycle observation, never candidate content.
type Type string

const (
	ExecutionFinished        Type = "execution_finished"
	VerificationPending      Type = "verification_pending"
	VerificationFailed       Type = "verification_failed"
	VerificationInconclusive Type = "verification_inconclusive"
	ResultCommitted          Type = "result_committed"
)

// Input contains only stable identifiers and a closed event type.
// ResultCommitted requires the event ID from its durable commit outbox.
type Input struct {
	ID       id.ID `json:"id"`
	Type     Type  `json:"type"`
	ResultID id.ID `json:"result_id,omitzero"`
}

func (i Input) validate() error {
	if i.ID.IsNil() {
		return ErrInvalid
	}
	switch i.Type {
	case ExecutionFinished, VerificationPending, VerificationFailed, VerificationInconclusive:
		if i.ID.Prefix() != PrefixEvent || !i.ResultID.IsNil() {
			return ErrInvalid
		}
	case ResultCommitted:
		if i.ID.Prefix() != verification.PrefixCommitEvent || i.ResultID.IsNil() || i.ResultID.Prefix() != verification.PrefixResult {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Event is a historical observation. It grants no continuing access to a result.
type Event struct {
	Input
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	Sequence      uint64        `json:"sequence"`
	RunRevision   int64         `json:"run_revision"`
	ObservedAt    time.Time     `json:"observed_at"`
}

// Validate checks the entire public envelope without accepting arbitrary payloads.
func (e Event) Validate() error {
	if e.SchemaVersion != 1 || !validRun(e.RunID) || e.Sequence == 0 || e.Sequence > math.MaxInt64 || e.RunRevision < 1 || e.ObservedAt.IsZero() {
		return ErrInvalid
	}
	return e.validate()
}

// Cursor binds a monotonically increasing position to one run and wire schema.
type Cursor struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	Sequence      uint64        `json:"sequence"`
}

// Validate rejects unsupported schemas and unbounded positions.
func (c Cursor) Validate() error {
	if c.SchemaVersion != 1 || !validRun(c.RunID) || c.Sequence > math.MaxInt64 {
		return ErrInvalid
	}
	return nil
}

func validRun(runID id.AgentRunID) bool { return !runID.IsNil() && runID.Prefix() == id.PrefixAgentRun }

// AppendRequest is trusted host input. Storage checks the current run transition.
// Reuse the exact request after uncertainty; IDs cannot name another batch later.
type AppendRequest struct {
	SchemaVersion       int           `json:"schema_version"`
	RunID               id.AgentRunID `json:"run_id"`
	ExpectedRunRevision int64         `json:"expected_run_revision"`
	AfterSequence       uint64        `json:"after_sequence"`
	Events              []Input       `json:"events"`
}

// Freeze validates, detaches and hashes the complete idempotency request.
func (r AppendRequest) Freeze() (AppendRequest, string, error) {
	if r.SchemaVersion != 1 || !validRun(r.RunID) || r.ExpectedRunRevision < 1 || len(r.Events) == 0 || len(r.Events) > MaxBatchSize || r.AfterSequence > math.MaxInt64-uint64(len(r.Events)) {
		return AppendRequest{}, "", ErrInvalid
	}
	seen := make(map[id.ID]bool, len(r.Events))
	for _, input := range r.Events {
		if input.validate() != nil || seen[input.ID] {
			return AppendRequest{}, "", ErrInvalid
		}
		seen[input.ID] = true
	}
	r.Events = append([]Input(nil), r.Events...)
	data, err := json.Marshal(r)
	if err != nil {
		return AppendRequest{}, "", err
	}
	return r, evidence.Digest(data), nil
}

// AppendReceipt survives replay retention and gives the original sequence range.
type AppendReceipt struct {
	FirstSequence uint64 `json:"first_sequence"`
	LastSequence  uint64 `json:"last_sequence"`
}

// ReadRequest bounds one authorized page. Sequence zero starts at the beginning.
type ReadRequest struct {
	After Cursor `json:"after"`
	Limit int    `json:"limit"`
}

// Validate checks the requested cursor and page bound.
func (r ReadRequest) Validate() error {
	if r.Limit < 1 || r.Limit > MaxPageSize {
		return ErrInvalid
	}
	return r.After.Validate()
}

// Watermark describes retention and identifies the current stored run snapshot.
// FirstSequence is LastSequence+1 when all earlier events have been pruned.
type Watermark struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	FirstSequence uint64        `json:"first_sequence"`
	LastSequence  uint64        `json:"last_sequence"`
	RunRevision   int64         `json:"run_revision"`
	ResultID      id.ID         `json:"result_id,omitzero"`
}

// Page is detached from storage. A retention gap returns only Watermark and Next.
type Page struct {
	Watermark Watermark `json:"watermark"`
	Events    []Event   `json:"events"`
	Next      Cursor    `json:"next"`
}

// Store is an optional trusted storage capability, not a subscriber authorization
// boundary. Reads must go through current host policy before public delivery.
// At most MaxRetainedEvents remain readable; ID/digest tombstones prevent reuse.
type Store interface {
	AppendPublicEvents(context.Context, AppendRequest) (AppendReceipt, error)
	ReadPublicEvents(context.Context, ReadRequest) (Page, error)
	ReadEventWatermark(context.Context, id.AgentRunID) (Watermark, error)
	PrunePublicEvents(context.Context, Cursor) error
}
