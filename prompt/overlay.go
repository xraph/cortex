package prompt

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Overlay is one host's per-scope customization of one agent. It carries
// the patches that reshape the agent's prompt sections, plus the handful
// of run settings a host is allowed to move without editing the agent
// itself.
//
// An overlay never replaces the agent it points at. The agent stays the
// single definition every scope shares, and the overlay is the delta a
// tenant applies on top of it, so removing the overlay restores the
// agent's own behavior exactly.
//
// One overlay per agent per scope, enforced by a unique index on
// (agent_id, scope_canon). A host that wants two different sets of
// patches for the same agent expresses that as two scopes, not two
// overlays, because "which of these applies" has no answer otherwise.
type Overlay struct {
	cortex.Entity
	ID      id.OverlayID `json:"id"`
	AgentID id.AgentID   `json:"agent_id"`

	// Scope is stamped from the caller's context on create and is
	// immutable afterwards. It is what decides which runs this overlay
	// applies to, so letting an update move it would silently retarget
	// an overlay a tenant already approved.
	Scope cortex.Scope `json:"scope"`

	// Patches reshape the agent's assembled sections. Order matters:
	// they apply in slice order, so two patches against the same section
	// compose the way a reader of the list would expect.
	Patches []Patch `json:"patches,omitempty"`

	// ToolsAdded and ToolsRemoved adjust the agent's tool list for runs
	// in this scope. They are two lists rather than one replacement list
	// because a host that only wants to withdraw one tool should not
	// have to restate the agent's whole surface, and would silently
	// re-grant anything the agent gained later if it did.
	ToolsAdded   []string `json:"tools_added,omitempty"`
	ToolsRemoved []string `json:"tools_removed,omitempty"`

	// Model overrides the agent's model when non-empty.
	Model string `json:"model,omitempty"`

	// Temperature and MaxTokens are pointers so that "leave the agent's
	// value alone" is distinguishable from "set this to zero". A plain
	// float64 would make an unset temperature read as a request for 0,
	// which is a real and very different setting.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
}

// Store persists overlays. Scope arrives on the context via
// cortex.ScopeFromContext, so no method carries it.
type Store interface {
	// CreateOverlay writes a new overlay, stamping the scope from the
	// context. A second overlay for the same agent in the same scope
	// returns cortex.ErrAlreadyExists.
	CreateOverlay(ctx context.Context, o *Overlay) error

	// GetOverlay reads one overlay by id within the caller's scope.
	GetOverlay(ctx context.Context, overlayID id.OverlayID) (*Overlay, error)

	// GetOverlayForAgent reads the overlay an agent has at the caller's
	// EXACT scope, which is the lookup prompt assembly runs. It matches
	// exactly rather than by prefix on purpose: a prefix match would
	// return whichever descendant scope's overlay happened to sort
	// first, and "the overlay that applies here" has to be one specific
	// row, not an arbitrary one. The unique index on
	// (agent_id, scope_canon) is what makes at most one row match.
	//
	// Returns cortex.ErrOverlayNotFound when the agent has no overlay at
	// this scope, which is the ordinary case for most agents.
	GetOverlayForAgent(ctx context.Context, agentID id.AgentID) (*Overlay, error)

	// GetOverlayForAgentAt reads the overlay an agent has at exactly the
	// given scope, rather than at the caller's own. It is how assembly
	// inherits: a run walks its scope's ancestor chain from broadest to
	// narrowest, asking for each prefix in turn and applying what it
	// finds in that order, so a run at [ws=A, proj=B] picks up the
	// overlay written at [ws=A] and then the one written at
	// [ws=A, proj=B].
	//
	// It matches the given scope EXACTLY, and inheritance is that walk
	// rather than a prefix match. Reaching for ListOverlays instead is
	// the mistake this method exists to prevent: prefix matching in this
	// codebase widens DOWNWARD, so listing from [ws=A] returns every
	// project's overlay inside that workspace. Feeding those into one
	// run's prompt would mix a sibling project's instructions into a
	// prompt that must never see them, and the call site would look
	// perfectly reasonable while it happened.
	//
	// The scope argument must be the caller's own scope or an ancestor
	// of it. Anything else returns cortex.ErrOverlayNotFound, because
	// naming a scope is not a way to read outside your own. That upward
	// reach is a deliberate widening: no other read here returns a row
	// stored above the caller, and this one does only within the
	// caller's own ancestry.
	GetOverlayForAgentAt(ctx context.Context, agentID id.AgentID, scope cortex.Scope) (*Overlay, error)

	// UpdateOverlay rewrites an overlay's mutable fields within the
	// caller's scope. Scope itself is never rewritten.
	UpdateOverlay(ctx context.Context, o *Overlay) error

	// DeleteOverlay removes an overlay within the caller's scope.
	DeleteOverlay(ctx context.Context, overlayID id.OverlayID) error

	// ListOverlays returns overlays visible from the caller's scope,
	// oldest first. Unlike GetOverlayForAgent this matches by prefix
	// unless the filter asks for Exact, so a workspace-level caller sees
	// every project's overlay beneath it.
	ListOverlays(ctx context.Context, filter *ListFilter) ([]*Overlay, error)
}

// ListFilter controls overlay listing. AgentID narrows to one agent;
// Exact narrows the scope match to rows stored at precisely that depth
// instead of everything beneath it.
type ListFilter struct {
	AgentID id.AgentID
	Exact   bool
	Limit   int
	Offset  int
}
