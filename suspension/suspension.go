// Package suspension defines a paused run and everything needed to
// continue it.
package suspension

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// SuspendReason says why a run stopped. Both reasons share one mechanism
// because they are the same problem: the loop cannot proceed until
// something outside it returns.
type SuspendReason string

const (
	// ReasonApproval means a human must grant or deny before the run
	// continues.
	ReasonApproval SuspendReason = "approval"
	// ReasonExternalTool means the caller, not the engine, executes the
	// pending tool call and reports the result back.
	ReasonExternalTool SuspendReason = "external_tool"
)

// PendingCall is one tool call awaiting a result from outside the engine.
type PendingCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Continuation is everything the loop needs to pick up where it stopped.
// It is stored in typed columns rather than an untyped metadata map, so a
// malformed continuation is a scan error at the boundary instead of a
// half-restored run several steps later.
//
// SystemPrompt holds the ASSEMBLED string, not the sections it came from.
// That is deliberate and it matters once v1.12.0 turns prompt assembly
// into a pipeline over sections and overlays: a run suspended before an
// overlay changed, and resumed after, continues with the prompt it began
// with. Consistency within one run beats picking up a mid-run change to
// the agent's identity.
type Continuation struct {
	Messages     []llm.Message `json:"messages"`
	SystemPrompt string        `json:"system_prompt"`
	StepIndex    int           `json:"step_index"`
	TokensUsed   int           `json:"tokens_used"`
}

// Suspension is a paused run.
type Suspension struct {
	cortex.Entity
	ID    id.SuspensionID `json:"id"`
	RunID id.AgentRunID   `json:"run_id"`

	// Scope is stored so Resume re-derives authorization from the scope
	// the run STARTED under, rather than trusting the scope of whoever
	// happens to call Resume. A resume arriving with different authority
	// must not quietly continue with it.
	Scope cortex.Scope `json:"scope"`

	Reason    SuspendReason `json:"reason"`
	Pending   []PendingCall `json:"pending,omitempty"`
	Cont      Continuation  `json:"continuation"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
}

// Store persists suspensions and provides the concurrency primitive that
// resuming a run depends on.
type Store interface {
	// CreateSuspension writes a new suspension row for a run that just
	// paused.
	CreateSuspension(ctx context.Context, s *Suspension) error

	// GetSuspension reads the suspension for a run without changing
	// anything. Callers that intend to resume the run must use
	// ClaimSuspension instead: a Get followed by a separate write is the
	// race ClaimSuspension exists to close.
	GetSuspension(ctx context.Context, runID id.AgentRunID) (*Suspension, error)

	// ClaimSuspension is the concurrency primitive for resume.
	// Implementations must perform the run's paused-to-running
	// transition and the suspension read as ONE atomic operation, not a
	// read followed by a separate write: that gap is exactly the race
	// this method exists to close, since two concurrent resume calls
	// must not both observe a paused run and proceed. The caller that
	// loses the race gets cortex.ErrNotSuspended, not the suspension it
	// lost the race for.
	ClaimSuspension(ctx context.Context, runID id.AgentRunID) (*Suspension, error)

	// DeleteSuspension removes a suspension row. Callers use this once a
	// resume has fully completed, or as part of the expiry sweep once an
	// expired suspension has been handled.
	DeleteSuspension(ctx context.Context, runID id.AgentRunID) error

	// ListExpired returns suspensions whose ExpiresAt is at or before
	// now, bounded to at most limit rows, for the expiry sweep to pick
	// up and resolve.
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*Suspension, error)
}
