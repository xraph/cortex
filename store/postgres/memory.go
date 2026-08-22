package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

func (s *Store) SaveConversation(ctx context.Context, agentID id.AgentID, messages []memory.Message) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	if len(messages) == 0 {
		return nil
	}
	l0, l1, l2, extra := scopeColumns(scope)

	for _, msg := range messages {
		content, err := json.Marshal(&msg)
		if err != nil {
			return fmt.Errorf("cortex: marshal message: %w", err)
		}
		m := &memoryModel{
			AgentID:    agentID.String(),
			Kind:       "conversation",
			Content:    string(content),
			ScopeL0:    l0,
			ScopeL1:    l1,
			ScopeL2:    l2,
			ScopeExtra: extra,
			ScopeCanon: scope.Canonical(),
		}
		if _, err := s.pgdb.NewInsert(m).Exec(ctx); err != nil {
			return fmt.Errorf("cortex: save conversation: %w", err)
		}
	}
	return nil
}

func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, limit int) ([]memory.Message, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	var models []memoryModel
	q := s.pgdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("kind = ?", "conversation").
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: load conversation: %w", err)
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

func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}

	q := s.pgdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", agentID.String()).
		Where("kind = ?", "conversation")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("cortex: clear conversation: %w", err)
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
	_, err := s.pgdb.NewInsert(m).
		OnConflict("(agent_id, kind, key) DO UPDATE").
		Set("content = EXCLUDED.content").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: save working memory: %w", err)
	}
	return nil
}

func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	m := new(memoryModel)
	err := s.pgdb.NewSelect(m).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Where(`"key" = ?`, key).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: load working memory: %w", err)
	}
	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex: unmarshal working memory: %w", err)
	}
	return v, nil
}

func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	_, err := s.pgdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: clear working memory: %w", err)
	}
	return nil
}

func (s *Store) SaveSummary(ctx context.Context, agentID id.AgentID, summary string) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	l0, l1, l2, extra := scopeColumns(scope)

	m := &memoryModel{
		AgentID:    agentID.String(),
		Kind:       "summary",
		Content:    summary,
		ScopeL0:    l0,
		ScopeL1:    l1,
		ScopeL2:    l2,
		ScopeExtra: extra,
		ScopeCanon: scope.Canonical(),
	}
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: save summary: %w", err)
	}
	return nil
}

func (s *Store) LoadSummaries(ctx context.Context, agentID id.AgentID) ([]string, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	var models []memoryModel
	q := s.pgdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("kind = ?", "summary").
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: load summaries: %w", err)
	}
	summaries := make([]string, len(models))
	for i, m := range models {
		summaries[i] = m.Content
	}
	return summaries, nil
}
