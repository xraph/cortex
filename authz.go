package cortex

import (
	"context"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// Subject is who is asking. Principal is host-defined and cortex never
// interprets it: a host puts whatever its own authorization layer needs
// there, and gets it back unchanged.
type Subject struct {
	Scope     Scope
	Principal any
	AgentID   id.AgentID
	RunID     id.AgentRunID
}

// Invocation is one tool call about to be dispatched. It embeds Subject
// so a handler receives scope and principal explicitly.
//
// The alternative is leaving handlers to pull those off the context. That
// works and is idiomatic Go, and it is also the exact pattern that let a
// tenant identifier sit available and unread until every tenant shared
// one conversation bucket. An explicit Invocation means a handler that
// ignores scope has visibly ignored it.
type Invocation struct {
	Subject
	Call llm.ToolCall
}

// ToolAuthorizer decides what the model may see and what it may call.
// It is consulted at two points because they are different questions: a
// model can name a tool it was never shown, so filtering the list is not
// a substitute for gating the call.
//
// A nil authorizer allows everything, so a host that sets none sees no
// change.
type ToolAuthorizer interface {
	// Visible filters the tool list before it reaches the model. Called
	// once per step.
	Visible(ctx context.Context, s Subject, tools []llm.Tool) []llm.Tool

	// Authorize gates one dispatch. Returning nil allows the call; any
	// error denies it, and the error's text is fed back to the model as
	// the tool result so it can react rather than silently retrying.
	Authorize(ctx context.Context, s Subject, call llm.ToolCall) error
}
