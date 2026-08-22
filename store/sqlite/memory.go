package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

// indexedScopeLevels is how many scope levels get their own indexed column.
// Levels past this land in scope_extra (JSON-encoded, since sqlite has no
// native jsonb type). Mirrors the postgres store's convention so scope data
// reads the same shape across backends.
const indexedScopeLevels = 3

// scopePredicate is one equality clause against a scope column.
type scopePredicate struct {
	Column string
	Value  string
}

// scopeColumns flattens a scope into the three indexed column values plus a
// JSON-encoded overflow map. Absent levels are the empty string, never NULL,
// so they stay comparable.
func scopeColumns(s cortex.Scope) (l0, l1, l2, extraJSON string) {
	extra := make(map[string]string)
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
	return l0, l1, l2, mustJSON(extra)
}

// scopePredicates builds the WHERE clauses for a scope filter. Prefix
// matching (exact = false) is the default: one equality per level the
// caller actually supplied, and nothing for the levels they left off, so a
// workspace-only filter matches every project inside it.
func scopePredicates(s cortex.Scope, exact bool) []scopePredicate {
	cols := []string{"scope_l0", "scope_l1", "scope_l2"}
	l0, l1, l2, _ := scopeColumns(s)
	vals := []string{l0, l1, l2}

	preds := make([]scopePredicate, 0, indexedScopeLevels)
	for i := range cols {
		if vals[i] == "" && !exact {
			continue
		}
		preds = append(preds, scopePredicate{Column: cols[i], Value: vals[i]})
	}
	return preds
}

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
			return fmt.Errorf("cortex/sqlite: marshal message: %w", err)
		}
		models[i] = memoryModel{
			AgentID:    agentID.String(),
			Kind:       "conversation",
			Content:    string(content),
			ScopeL0:    l0,
			ScopeL1:    l1,
			ScopeL2:    l2,
			ScopeExtra: extra,
			ScopeCanon: canon,
		}
	}
	_, err := s.sdb.NewInsert(&models).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save conversation: %w", err)
	}
	return nil
}

func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, limit int) ([]memory.Message, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	var models []memoryModel
	q := s.sdb.NewSelect(&models).
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

func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}

	q := s.sdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", agentID.String()).
		Where("kind = ?", "conversation")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	_, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: clear conversation: %w", err)
	}
	return nil
}

// SaveWorking upserts a working-memory value for a run within the
// caller's scope. A run ID is a bearer capability, not an isolation
// boundary — anyone who learns it could otherwise read or clobber another
// tenant's scratch state — so this is guarded the same as
// SaveConversation.
func (s *Store) SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	l0, l1, l2, extra := scopeColumns(scope)

	m := &memoryModel{
		AgentID:    runID.String(),
		Kind:       "working",
		Key:        key,
		Content:    mustJSON(value),
		ScopeL0:    l0,
		ScopeL1:    l1,
		ScopeL2:    l2,
		ScopeExtra: extra,
		ScopeCanon: scope.Canonical(),
	}
	// The conflict target has to restate the partial index's WHERE
	// clause verbatim (idx_cortex_memories_working is WHERE kind =
	// 'working') — SQLite only accepts a bare "(agent_id, kind, key)"
	// target for a full unique index, and rejects it here at prepare
	// time with "ON CONFLICT clause does not match any PRIMARY KEY or
	// UNIQUE constraint" for a partial one, which made every upsert on
	// an existing key fail outright.
	_, err := s.sdb.NewInsert(m).
		OnConflict("(agent_id, kind, key) WHERE kind = 'working' DO UPDATE").
		Set("content = EXCLUDED.content").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save working memory: %w", err)
	}
	return nil
}

func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(memoryModel)
	q := s.sdb.NewSelect(m).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Where("\"key\" = ?", key)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: load working memory: %w", err)
	}
	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: unmarshal working memory: %w", err)
	}
	return v, nil
}

func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: clear working memory: %w", err)
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
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save summary: %w", err)
	}
	return nil
}

func (s *Store) LoadSummaries(ctx context.Context, agentID id.AgentID) ([]string, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	var models []memoryModel
	q := s.sdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("kind = ?", "summary").
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: load summaries: %w", err)
	}
	summaries := make([]string, len(models))
	for i, m := range models {
		summaries[i] = m.Content
	}
	return summaries, nil
}
