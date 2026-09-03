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
	// ReasonAgentReply means the run is waiting on another agent's answer.
	//
	// It is not ReasonExternalTool even though both wait on something
	// outside the loop, because the two say different things about who
	// acts next. External says the CALLER executes the call and reports
	// back. Agent-reply says cortex itself is waiting on a peer, and a
	// caller answering it would be forging a message that peer never
	// sent. The messaging bus resumes these, off its own correlation
	// ledger; nobody else may.
	ReasonAgentReply SuspendReason = "agent_reply"
)

// PendingCall is one tool call awaiting a result from outside the engine.
//
// Arguments is carried here rather than left to be recovered from the
// continuation's last assistant message. A caller resuming an external
// tool has to actually execute the call, and making it cross-reference
// the message history by call id to find out with what would be a
// needless puzzle for the one consumer this type exists to serve.
type PendingCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the JSON-encoded argument object, verbatim from the
	// model's tool call.
	Arguments string `json:"arguments"`
}

// RunConfig is the resolved configuration a run was executing under: the
// merge of engine defaults, agent config and that run's overrides,
// already flattened.
//
// It is stored for the same reason SystemPrompt is stored assembled
// rather than as the sections it came from. A run's overrides live only
// in the call that started it, so a resume that rebuilt the config from
// the agent would silently drop them: a run deliberately started with a
// narrowed Tools list would come back with the agent's full tool set,
// which is authority widening across a pause, and a run started with a
// raised MaxSteps would come back on the agent's smaller budget and
// refuse to continue at all.
//
// Storing the resolved values also pins what the run was actually
// executing, so an agent-config edit landing during the pause cannot
// shift a conversation already in flight.
type RunConfig struct {
	Model         string   `json:"model,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	MaxSteps      int      `json:"max_steps,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	ReasoningLoop string   `json:"reasoning_loop,omitempty"`
	Tools         []string `json:"tools,omitempty"`
	PersonaRef    string   `json:"persona_ref,omitempty"`

	// ToolsRestricted says Tools is an explicit allowlist even when it
	// is empty, which is what an overlay that withdrew the agent's last
	// tool produces. It travels with the continuation because a resumed
	// run rebuilds its config from here and from nowhere else: without
	// it, an empty list would read as "every registered tool" and the
	// resume would hand back exactly what the overlay took away.
	//
	// It is absent from rows written before this field existed, and
	// decodes there as false, which is the behavior those runs had.
	// The flag can therefore be LOST across an upgrade, but it can
	// never be spuriously SET. That is the safe direction here: losing
	// it resumes an old run under the tool rules it was suspended
	// under, where inventing it would strip tools from a run nobody
	// restricted.
	ToolsRestricted bool `json:"tools_restricted,omitempty"`
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

	// NewMessagesFrom is the index in Messages where this run's own
	// messages begin. Everything before it is conversation history the
	// run loaded at startup and must never save again.
	//
	// Without it a resume has no way to tell the two apart, and its
	// first SaveConversation writes the whole history back as new rows.
	// That is not hypothetical: it shipped in v1.8.0, and agents went
	// deaf after about six runs because the duplicated rows filled the
	// fixed read window until it held nothing recent. The engine tracks
	// this boundary as newMessagesFrom in both react loops; this field
	// is the same number, carried across the pause.
	NewMessagesFrom int `json:"new_messages_from"`

	// SessionID is the session the run was loading from and saving to. A
	// resume cannot re-derive it: resolveSession lazily creates a
	// default session under a create race, so asking it again after a
	// pause can land on a different session than the one the messages
	// above came from.
	SessionID id.SessionID `json:"session_id,omitempty"`

	// Config is the resolved configuration the run was executing under.
	// See RunConfig: a resume builds its config from this and never
	// re-merges the agent's, so the run's own overrides survive the
	// pause.
	Config RunConfig `json:"config"`
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
	// now, bounded to at most limit rows, within the caller's scope.
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*Suspension, error)

	// ListExpiredAcrossScopes is ListExpired with the scope filter
	// DELIBERATELY BYPASSED: it returns expired suspensions from every
	// scope in the database, and it is the one read in this interface
	// that does.
	//
	// It exists for the engine's expiry sweeper and nothing else. A
	// sweeper is process infrastructure started by Engine.Start, so
	// there is no request scope on its context and no way to enumerate
	// the scopes that have rows; ListExpired would sweep whichever scope
	// the sweeper's context happened to carry, which in practice is
	// none. Hence a second method rather than a flag or a sentinel
	// scope: a cross-scope read has to be visible as one at the call
	// site, and no handler can reach this by passing a zero value.
	//
	// Crossing scopes to FIND the work does not license doing the work
	// unscoped. Callers must rebind their context to each returned
	// suspension's own Scope before touching that run, exactly as Resume
	// does.
	ListExpiredAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*Suspension, error)

	// ClaimExpiredSuspension is ClaimSuspension's mirror for the expiry
	// sweeper: the same atomic paused-to-running transition, conditioned
	// on the suspension being PAST its deadline rather than short of it.
	//
	// The two predicates partition the same row, which is what makes a
	// resume and a sweep exclusive rather than merely unlikely to
	// collide. A resume that beats the deadline claims the run and the
	// sweeper can no longer take it; a sweep that finds the deadline
	// passed takes the run and no resume can claim it afterwards. Both
	// answer against the same stored ExpiresAt, so there is no window
	// where the two agree the row is theirs.
	//
	// Zero rows matched means somebody else got there first, and it
	// returns cortex.ErrNotSuspended. The sweeper skips such a run
	// rather than failing it: it is either gone, already resumed, or no
	// longer paused.
	ClaimExpiredSuspension(ctx context.Context, runID id.AgentRunID, now time.Time) (*Suspension, error)
}
