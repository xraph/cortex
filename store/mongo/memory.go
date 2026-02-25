package mongo

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

// SaveConversation appends messages to conversation memory.
func (s *Store) SaveConversation(ctx context.Context, agentID id.AgentID, tenantID string, messages []memory.Message) error {
	if len(messages) == 0 {
		return nil
	}

	models := make([]memoryModel, len(messages))
	for i, msg := range messages {
		models[i] = *messageToModel(agentID.String(), tenantID, msg)
		models[i].CreatedAt = now()
	}

	_, err := s.mdb.NewInsert(&models).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save conversation: %w", err)
	}

	return nil
}

// LoadConversation returns conversation messages for an agent and tenant.
func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, tenantID string, limit int) ([]memory.Message, error) {
	var models []memoryModel

	q := s.mdb.NewFind(&models).
		Filter(bson.M{
			"agent_id":  agentID.String(),
			"tenant_id": tenantID,
			"kind":      "conversation",
		}).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if limit > 0 {
		q = q.Limit(int64(limit))
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: load conversation: %w", err)
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

// ClearConversation removes all conversation messages for an agent and tenant.
func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID, tenantID string) error {
	_, err := s.mdb.NewDelete((*memoryModel)(nil)).
		Many().
		Filter(bson.M{
			"agent_id":  agentID.String(),
			"tenant_id": tenantID,
			"kind":      "conversation",
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: clear conversation: %w", err)
	}

	return nil
}

// SaveWorking stores a working memory key-value pair, upserting if the key already exists.
func (s *Store) SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error {
	t := now()

	_, err := s.mdb.NewUpdate((*memoryModel)(nil)).
		Filter(bson.M{
			"agent_id": runID.String(),
			"kind":     "working",
			"key":      key,
		}).
		SetUpdate(bson.M{"$set": bson.M{
			"agent_id":   runID.String(),
			"kind":       "working",
			"key":        key,
			"content":    mustJSON(value),
			"created_at": t,
		}}).
		Upsert().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save working memory: %w", err)
	}

	return nil
}

// LoadWorking returns a working memory value by key.
func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	var m memoryModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{
			"agent_id": runID.String(),
			"kind":     "working",
			"key":      key,
		}).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: load working memory: %w", err)
	}

	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex/mongo: unmarshal working memory: %w", err)
	}

	return v, nil
}

// ClearWorking removes all working memory for a run.
func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	_, err := s.mdb.NewDelete((*memoryModel)(nil)).
		Many().
		Filter(bson.M{
			"agent_id": runID.String(),
			"kind":     "working",
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: clear working memory: %w", err)
	}

	return nil
}

// SaveSummary appends a summary to the agent's memory.
func (s *Store) SaveSummary(ctx context.Context, agentID id.AgentID, tenantID, summary string) error {
	m := &memoryModel{
		AgentID:   agentID.String(),
		TenantID:  tenantID,
		Kind:      "summary",
		Content:   summary,
		CreatedAt: now(),
	}

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save summary: %w", err)
	}

	return nil
}

// LoadSummaries returns all summaries for an agent and tenant.
func (s *Store) LoadSummaries(ctx context.Context, agentID id.AgentID, tenantID string) ([]string, error) {
	var models []memoryModel

	err := s.mdb.NewFind(&models).
		Filter(bson.M{
			"agent_id":  agentID.String(),
			"tenant_id": tenantID,
			"kind":      "summary",
		}).
		Sort(bson.D{{Key: "created_at", Value: 1}}).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: load summaries: %w", err)
	}

	summaries := make([]string, len(models))
	for i, m := range models {
		summaries[i] = m.Content
	}

	return summaries, nil
}
