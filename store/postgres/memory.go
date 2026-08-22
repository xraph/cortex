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
		// Metadata is jsonb; the Go zero value for the field is "", which
		// postgres rejects as invalid JSON syntax rather than falling
		// back to the column's DEFAULT '{}' the way an omitted column
		// would. Every other model in this package runs its Metadata
		// field through mustJSON for the same reason — this one just got
		// missed when memory rows picked up the same jsonb typing.
		m := &memoryModel{
			AgentID:    agentID.String(),
			Kind:       "conversation",
			Content:    string(content),
			Metadata:   mustJSON(nil),
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
		AgentID: runID.String(),
		Kind:    "working",
		Key:     key,
		Content: mustJSON(value),
		// Metadata is jsonb; see SaveConversation's comment on why this
		// can't be left at the Go zero value.
		Metadata:   mustJSON(nil),
		ScopeL0:    l0,
		ScopeL1:    l1,
		ScopeL2:    l2,
		ScopeExtra: extra,
		ScopeCanon: scope.Canonical(),
	}
	// The conflict target has to restate the partial index's WHERE
	// clause verbatim (idx_cortex_memories_working is WHERE kind =
	// 'working') — postgres only accepts a bare "(agent_id, kind, key)"
	// target for a full unique index, and rejects it here at plan time
	// for a partial one ("there is no unique or exclusion constraint
	// matching the ON CONFLICT specification"), which made every upsert
	// on an existing key fail outright.
	//
	// scope_canon is part of the conflict target (and of the index
	// itself, via the 20260822000002 migration) because without it, two
	// different scopes saving the same (agent_id, kind, key) — which
	// happens whenever a run ID is known cross-scope, since it's a
	// bearer capability rather than an isolation boundary — collide on
	// the same row: the second save's DO UPDATE overwrites the first
	// scope's content while leaving its scope columns untouched, and the
	// first scope's next LoadWorking then returns the second scope's
	// value. The trailing WHERE on the UPDATE is redundant given the
	// index now includes scope_canon, but kept as a second line of
	// defense against exactly that class of bug.
	_, err := s.pgdb.NewInsert(m).
		OnConflict("(agent_id, kind, key, scope_canon) WHERE kind = 'working' DO UPDATE SET content = EXCLUDED.content WHERE cortex_memories.scope_canon = EXCLUDED.scope_canon").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: save working memory: %w", err)
	}
	return nil
}

func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(memoryModel)
	q := s.pgdb.NewSelect(m).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working").
		Where(`"key" = ?`, key)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: load working memory: %w", err)
	}
	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex: unmarshal working memory: %w", err)
	}
	return v, nil
}

func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.pgdb.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", runID.String()).
		Where("kind = ?", "working")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, err := q.Exec(ctx); err != nil {
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
		AgentID: agentID.String(),
		Kind:    "summary",
		Content: summary,
		// Metadata is jsonb; see SaveConversation's comment on why this
		// can't be left at the Go zero value.
		Metadata:   mustJSON(nil),
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
