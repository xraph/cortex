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
//
// This only ever matches on scope_l0/l1/l2 — it ignores the extra overflow
// map entirely, on both read and write. That's harmless today only
// because cortex.WithScope (scope.go's maxScopeLevels, currently 3)
// refuses to construct a Scope deep enough to populate extra in the first
// place. If maxScopeLevels ever rises, this needs to start matching on
// extra too, or mongo's isolation guarantee silently diverges from
// postgres/sqlite for any level past the third — see the comment on
// maxScopeLevels for the other half of this.
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
			// ID has to be assigned explicitly: memoryModel's bson tag is
			// "_id,omitempty", and leaving the Go zero value (an empty
			// string) omits the field entirely rather than writing it as
			// "". Mongo then generates its own ObjectID for _id, which
			// the read path can't decode back into this string-typed
			// field ("decoding an object ID into a string is not
			// supported by default") — every LoadConversation call after
			// a SaveConversation failed outright until this was set.
			ID:         id.NewMemoryID().String(),
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

// SaveWorking stores a working memory key-value pair, upserting if the key
// already exists, within the caller's scope. A run ID is a bearer
// capability, not an isolation boundary — anyone who learns it could
// otherwise read or clobber another tenant's scratch state — so this is
// guarded the same as SaveConversation.
func (s *Store) SaveWorking(ctx context.Context, runID id.AgentRunID, key string, value any) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	l0, l1, l2, extra := scopeColumns(scope)
	t := now()

	filter := bson.M{
		"agent_id": runID.String(),
		"kind":     "working",
		"key":      key,
	}
	// exact=true, not the usual prefix match: this filter also decides
	// which existing row an upsert overwrites. Prefix matching pins only
	// the levels the caller supplied and leaves deeper ones unconstrained,
	// so a broader (ancestor) scope's save would match a narrower
	// (descendant) scope's existing row and the $set below would
	// silently reparent it, blanking scope_l1/scope_l2 down to the
	// ancestor's shallower scope. An upsert has to match its own scope
	// precisely or fall through to inserting a new row.
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}

	_, err := s.mdb.NewUpdate((*memoryModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{
			"$set": bson.M{
				"agent_id":    runID.String(),
				"kind":        "working",
				"key":         key,
				"content":     mustJSON(value),
				"scope_l0":    l0,
				"scope_l1":    l1,
				"scope_l2":    l2,
				"scope_extra": extra,
				"scope_canon": scope.Canonical(),
				"created_at":  t,
			},
			// _id belongs in $setOnInsert, not $set: Mongo rejects any
			// attempt to modify _id on an existing document ("Performing
			// an update on the path '_id' would modify the immutable
			// field '_id'"), which every re-save of an existing key
			// would trigger if this were in $set instead. On insert,
			// leaving _id unset at all falls back to Mongo's own
			// ObjectID generation, which LoadWorking then can't decode
			// back into memoryModel's string-typed ID field.
			"$setOnInsert": bson.M{"_id": id.NewMemoryID().String()},
		}).
		Upsert().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: save working memory: %w", err)
	}

	return nil
}

// LoadWorking returns a working memory value by key within the caller's
// scope.
func (s *Store) LoadWorking(ctx context.Context, runID id.AgentRunID, key string) (any, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	filter := bson.M{
		"agent_id": runID.String(),
		"kind":     "working",
		"key":      key,
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var m memoryModel

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrWorkingMemoryNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: load working memory: %w", err)
	}

	var v any
	if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
		return nil, fmt.Errorf("cortex/mongo: unmarshal working memory: %w", err)
	}

	return v, nil
}

// ClearWorking removes all working memory for a run within the caller's
// scope.
func (s *Store) ClearWorking(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}

	filter := bson.M{
		"agent_id": runID.String(),
		"kind":     "working",
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	_, err := s.mdb.NewDelete((*memoryModel)(nil)).
		Many().
		Filter(filter).
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
		// See SaveConversation's comment on why ID must be set explicitly.
		ID:         id.NewMemoryID().String(),
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
