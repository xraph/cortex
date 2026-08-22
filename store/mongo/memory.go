package mongo

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

// scopeColumns flattens a scope into the three indexed field values plus an
// overflow map, mirroring the postgres store's column layout so scope data
// reads the same shape across backends.
func scopeColumns(s cortex.Scope) (l0, l1, l2 string, extra map[string]string) {
	extra = make(map[string]string)
	for i, lvl := range s.Levels {
		encoded := lvl.Key + "=" + lvl.Value
		switch i {
		case 0:
			l0 = encoded
		case 1:
			l1 = encoded
		case 2:
			l2 = encoded
		default:
			extra[lvl.Key] = lvl.Value
		}
	}
	return l0, l1, l2, extra
}

// scopeFilter builds the bson filter fragment for a scope match. Prefix
// matching (exact = false) is the default: one equality per level the
// caller actually supplied, and nothing for the levels they left off, so a
// workspace-only filter matches every project inside it.
func scopeFilter(s cortex.Scope, exact bool) bson.M {
	l0, l1, l2, _ := scopeColumns(s)
	cols := []struct {
		key string
		val string
	}{
		{"scope_l0", l0},
		{"scope_l1", l1},
		{"scope_l2", l2},
	}

	f := bson.M{}
	for _, c := range cols {
		if c.val == "" && !exact {
			continue
		}
		f[c.key] = c.val
	}
	return f
}

// SaveConversation appends messages to conversation memory.
func (s *Store) SaveConversation(ctx context.Context, agentID id.AgentID, messages []memory.Message) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	if len(messages) == 0 {
		return nil
	}
	l0, l1, l2, extra := scopeColumns(scope)
	canon := scope.Canonical()

	models := make([]memoryModel, len(messages))
	for i, msg := range messages {
		content, err := json.Marshal(&msg)
		if err != nil {
			return fmt.Errorf("cortex/mongo: marshal message: %w", err)
		}
		models[i] = memoryModel{
			AgentID:    agentID.String(),
			Kind:       "conversation",
			Content:    string(content),
			Metadata:   msg.Metadata,
			ScopeL0:    l0,
			ScopeL1:    l1,
			ScopeL2:    l2,
			ScopeExtra: extra,
			ScopeCanon: canon,
			CreatedAt:  now(),
		}
	}

	_, err := s.mdb.NewInsert(&models).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save conversation: %w", err)
	}

	return nil
}

// LoadConversation returns conversation messages for an agent within scope.
func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, limit int) ([]memory.Message, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	filter := bson.M{
		"agent_id": agentID.String(),
		"kind":     "conversation",
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []memoryModel

	q := s.mdb.NewFind(&models).
		Filter(filter).
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

// ClearConversation removes all conversation messages for an agent within scope.
func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}

	filter := bson.M{
		"agent_id": agentID.String(),
		"kind":     "conversation",
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	_, err := s.mdb.NewDelete((*memoryModel)(nil)).
		Many().
		Filter(filter).
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
		CreatedAt:  now(),
	}

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save summary: %w", err)
	}

	return nil
}

// LoadSummaries returns all summaries for an agent within scope.
func (s *Store) LoadSummaries(ctx context.Context, agentID id.AgentID) ([]string, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	filter := bson.M{
		"agent_id": agentID.String(),
		"kind":     "summary",
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []memoryModel

	err := s.mdb.NewFind(&models).
		Filter(filter).
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
