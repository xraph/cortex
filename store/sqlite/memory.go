package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

func (s *Store) SaveConversation(ctx context.Context, agentID id.AgentID, tenantID string, messages []memory.Message) error {
	if len(messages) == 0 {
		return nil
	}
	models := make([]memoryModel, len(messages))
	for i, msg := range messages {
		models[i] = *messageToModel(agentID.String(), tenantID, msg)
	}
	_, err := s.sdb.NewInsert(&models).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save conversation: %w", err)
	}
	return nil
}

func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, tenantID string, limit int) ([]memory.Message, error) {
	var models []memoryModel
	q := s.sdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("tenant_id = ?", tenantID).
		Where("kind = ?", "conversation").
		OrderExpr("created_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: load conversation: %w", err)
	}
	messages := make([]memory.Message, 0, len(models))
	for _, m := range models {
		var msg memory.Message
		if err := json.Unmarshal([]byte(m.Content), &msg); err == nil {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID, tenantID string) error {
	_, err := s.sdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", agentID.String()).
		Where("tenant_id = ?", tenantID).
		Where("kind = ?", "conversation").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: clear conversation: %w", err)
	}
	return nil
}

func (s *Store) SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error {
	m := &memoryModel{
		AgentID: runID.String(),
		Kind:    "working",
		Key:     key,
		Content: mustJSON(value),
	}
	_, err := s.sdb.NewInsert(m).
		OnConflict("(agent_id, kind, key) DO UPDATE").
		Set("content = EXCLUDED.content").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save working memory: %w", err)
	}
	return nil
}

func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	m := new(memoryModel)
	err := s.sdb.NewSelect(m).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Where("\"key\" = ?", key).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: load working memory: %w", err)
	}
	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: unmarshal working memory: %w", err)
	}
	return v, nil
}

func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	_, err := s.sdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: clear working memory: %w", err)
	}
	return nil
}

func (s *Store) SaveSummary(ctx context.Context, agentID id.AgentID, tenantID, summary string) error {
	m := &memoryModel{
		AgentID:  agentID.String(),
		TenantID: tenantID,
		Kind:     "summary",
		Content:  summary,
	}
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save summary: %w", err)
	}
	return nil
}

func (s *Store) LoadSummaries(ctx context.Context, agentID id.AgentID, tenantID string) ([]string, error) {
	var models []memoryModel
	err := s.sdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("tenant_id = ?", tenantID).
		Where("kind = ?", "summary").
		OrderExpr("created_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: load summaries: %w", err)
	}
	summaries := make([]string, len(models))
	for i, m := range models {
		summaries[i] = m.Content
	}
	return summaries, nil
}
