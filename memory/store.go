package memory

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for agent memory (conversation, working, summaries).
type Store interface {
	SaveConversation(ctx context.Context, agentID id.AgentID, tenantID string, messages []Message) error
	LoadConversation(ctx context.Context, agentID id.AgentID, tenantID string, limit int) ([]Message, error)
	ClearConversation(ctx context.Context, agentID id.AgentID, tenantID string) error

	SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error
	LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error)
	ClearWorking(ctx context.Context, runID id.AgentRunID) error

	SaveSummary(ctx context.Context, agentID id.AgentID, tenantID string, summary string) error
	LoadSummaries(ctx context.Context, agentID id.AgentID, tenantID string) ([]string, error)
}
