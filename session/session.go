// Package session defines the Session entity and its store interface. A
// session is one conversation thread against one agent in one scope.
package session

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Session groups conversation messages into a thread. An agent has many
// per scope, and exactly one of them is the default: the thread a run
// lands in when its caller names no session.
type Session struct {
	cortex.Entity
	ID      id.SessionID `json:"id"`
	AgentID id.AgentID   `json:"agent_id"`
	Scope   cortex.Scope `json:"scope"`

	// Title and Metadata belong to the host. Cortex stores them and
	// never interprets them.
	Title    string         `json:"title,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`

	// MessageCount and LastMessage belong to cortex and are maintained
	// on write, inside the same transaction as the messages they
	// describe. A counter that can drift from its rows is worse than no
	// counter, because a reader trusts it.
	MessageCount int    `json:"message_count"`
	LastMessage  string `json:"last_message,omitempty"`

	// IsDefault marks the thread a run uses when it names no session.
	// It is a real row rather than an empty-string sentinel, because an
	// empty identifier that silently means "the shared one" is the shape
	// that leaked history across tenants.
	IsDefault bool `json:"is_default"`

	// BackfilledBy belongs to cortex, not the host: it is set once, at
	// creation, by the v1.9.0 migration that gives a pre-existing
	// unsessioned conversation its first default session, to the
	// migration version that created it (e.g. "20260824000004"). It is
	// empty for every session a run creates organically through
	// engine.resolveSession. This lives in its own column rather than
	// Metadata specifically so a host PUTting its own metadata can never
	// destroy it -- Metadata is documented above as host-owned and
	// cortex never reads it back for anything, which used to include
	// this marker before it moved here. No store write path other than
	// the backfill migrations sets it, and UpdateSession never carries
	// it in its mutable-column list, so it can't be overwritten after
	// creation either.
	BackfilledBy string `json:"backfilled_by,omitempty"`
}

// Store defines persistence for sessions. Scope arrives on the context
// via cortex.ScopeFromContext, so no method carries it.
type Store interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, sessionID id.SessionID) (*Session, error)
	UpdateSession(ctx context.Context, s *Session) error
	DeleteSession(ctx context.Context, sessionID id.SessionID) error
	ListSessions(ctx context.Context, filter *ListFilter) ([]*Session, error)
	CountSessions(ctx context.Context, filter *ListFilter) (int64, error)
}

// ListFilter controls session listing. AgentID narrows to one agent;
// Exact narrows the scope match to rows stored at precisely that depth
// instead of everything beneath it; DefaultOnly narrows to the agent's
// default session, which is how a run resolves the thread to use when
// its caller names none.
type ListFilter struct {
	AgentID     id.AgentID
	Exact       bool
	DefaultOnly bool
	Search      string
	Limit       int
	Offset      int
}
