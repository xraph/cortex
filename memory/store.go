package memory

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for agent memory (conversation, working, summaries).
//
// None of these methods carry a tenant or scope parameter: the scope
// comes from the context via cortex.ScopeFromContext, so there is nothing
// left for a caller to forget to pass. This includes the working-memory
// methods, which are keyed by run ID — a bearer capability, not an
// isolation boundary on its own — so they need the same guard as
// everything else here.
type Store interface {
	SaveConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, messages []Message) error
	LoadConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, limit int) ([]Message, error)
	ClearConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID) error

	SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error
	LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error)
	ClearWorking(ctx context.Context, runID id.AgentRunID) error

	SaveSummary(ctx context.Context, agentID id.AgentID, summary string) error
	LoadSummaries(ctx context.Context, agentID id.AgentID) ([]string, error)
}
