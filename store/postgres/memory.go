package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
)

// SaveConversation inserts messages and updates the owning session's
// message_count/last_message inside one transaction. The counter is
// derived from the rows this call actually writes (len(messages), not a
// flat +1 per call), so it always matches the physical row count even
// when a caller's batch size varies from call to call. If the session
// row can't be found — a session id that doesn't correspond to a real
// session — the whole write rolls back rather than leaving orphaned
// message rows a reader can never reach through GetSession/ListSessions.
func (s *Store) SaveConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, messages []memory.Message) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	if len(messages) == 0 {
		return nil
	}
	l0, l1, l2, extra := scopeColumns(scope)
	sid := sessionID.String()

	tx, err := s.pgdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return fmt.Errorf("cortex: begin save conversation: %w", err)
	}
	// After a successful Commit, Rollback is a documented no-op; before
	// one, its error can't be acted on any further than the failure that
	// triggered this defer already is.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	// lastMessage tracks the most recent message with non-empty content,
	// not simply the last message in the batch: a run that hits MaxSteps
	// mid tool-call exits with an assistant message that carries
	// ToolCalls but no Content, and writing that as last_message would
	// blank a field message_count says just grew.
	var lastMessage string
	for _, msg := range messages {
		content, marshalErr := json.Marshal(&msg)
		if marshalErr != nil {
			return fmt.Errorf("cortex: marshal message: %w", marshalErr)
		}
		// Metadata is jsonb; the Go zero value for the field is "", which
		// postgres rejects as invalid JSON syntax rather than falling
		// back to the column's DEFAULT '{}' the way an omitted column
		// would. Every other model in this package runs its Metadata
		// field through mustJSON for the same reason — this one just got
		// missed when memory rows picked up the same jsonb typing.
		m := &memoryModel{
			AgentID:    agentID.String(),
			SessionID:  sid,
			Kind:       "conversation",
			Content:    string(content),
			Metadata:   mustJSON(nil),
			ScopeL0:    l0,
			ScopeL1:    l1,
			ScopeL2:    l2,
			ScopeExtra: extra,
			ScopeCanon: scope.Canonical(),
		}
		if _, insErr := tx.NewInsert(m).Exec(ctx); insErr != nil {
			return fmt.Errorf("cortex: save conversation: %w", insErr)
		}
		if msg.Content != "" {
			lastMessage = msg.Content
		}
	}

	// id + scope_canon alone identify a row, but not which agent it
	// belongs to: a caller-supplied session id from another agent in the
	// same scope would otherwise match here too. agent_id closes that --
	// see resolveConversationSession in api/memory_handler.go for the
	// ownership check on the read side of the same hole.
	res, err := tx.NewRaw(
		`UPDATE cortex_sessions SET message_count = message_count + $1, last_message = $2, updated_at = $3 WHERE id = $4 AND agent_id = $5 AND scope_canon = $6`,
		len(messages), lastMessage, time.Now().UTC(), sid, agentID.String(), scope.Canonical(),
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update session counters: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: save conversation rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("cortex: save conversation: %w", cortex.ErrSessionNotFound)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cortex: commit save conversation: %w", err)
	}
	return nil
}

// LoadConversation returns the NEWEST `limit` messages in chronological
// order, not the oldest. The query orders created_at/id DESC and takes
// LIMIT off the front -- the recent end of the conversation -- then the
// loop below walks the result backwards to hand callers chronological
// order again. Ordering ASC and limiting (the old behavior) always
// returns the SAME oldest prefix once a conversation grows past the
// limit: every later turn is written but never read back into context,
// and an install that had already accumulated duplicate rows from the
// pre-fix re-save bug stayed pinned on that stale prefix even after the
// duplication itself was fixed upstream. id DESC breaks ties within a
// batch that shares one created_at timestamp, so the reversal is a
// stable chronological order rather than an arbitrary one.
func (s *Store) LoadConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, limit int) ([]memory.Message, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	var models []memoryModel
	q := s.pgdb.NewSelect(&models).
		Where("agent_id = ?", agentID.String()).
		Where("session_id = ?", sessionID.String()).
		Where("kind = ?", "conversation").
		OrderExpr("created_at DESC, id DESC")
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
	for i := len(models) - 1; i >= 0; i-- {
		var msg memory.Message
		if err := json.Unmarshal([]byte(models[i].Content), &msg); err == nil {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// ClearConversation deletes the session's message rows and resets its
// counters in the same transaction, for the same drift reason as
// SaveConversation.
func (s *Store) ClearConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sid := sessionID.String()

	tx, err := s.pgdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return fmt.Errorf("cortex: begin clear conversation: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	q := tx.NewDelete((*memoryModel)(nil)).
		Where("agent_id = ?", agentID.String()).
		Where("session_id = ?", sid).
		Where("kind = ?", "conversation")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, delErr := q.Exec(ctx); delErr != nil {
		return fmt.Errorf("cortex: clear conversation: %w", delErr)
	}

	// Same agent_id guard as SaveConversation's counter update above.
	res, err := tx.NewRaw(
		`UPDATE cortex_sessions SET message_count = 0, last_message = '', updated_at = $1 WHERE id = $2 AND agent_id = $3 AND scope_canon = $4`,
		time.Now().UTC(), sid, agentID.String(), scope.Canonical(),
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: reset session counters: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: clear conversation rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("cortex: clear conversation: %w", cortex.ErrSessionNotFound)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cortex: commit clear conversation: %w", err)
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrWorkingMemoryNotFound
		}
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
