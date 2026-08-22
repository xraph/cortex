package memory

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for agent memory (conversation, working, summaries).
//
// Conversation and summary methods carry no tenant or scope parameter: the
// scope comes from the context via cortex.ScopeFromContext, so there is
// nothing left for a caller to forget to pass.
type Store interface {
	SaveConversation(ctx context.Context, agentID id.AgentID, messages []Message) error
	LoadConversation(ctx context.Context, agentID id.AgentID, limit int) ([]Message, error)
	ClearConversation(ctx context.Context, agentID id.AgentID) error

	SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error
	LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error)
	ClearWorking(ctx context.Context, runID id.AgentRunID) error

	SaveSummary(ctx context.Context, agentID id.AgentID, summary string) error
	LoadSummaries(ctx context.Context, agentID id.AgentID) ([]string, error)
}
