package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

// CreateSession persists a new session, stamping the scope from the
// context. The partial (agent_id, scope_canon) unique index (WHERE
// is_default) is what actually enforces one default session per agent per
// scope; a non-default session never touches that index.
func (s *Store) CreateSession(ctx context.Context, sess *session.Session) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	sess.CreatedAt = t
	sess.UpdatedAt = t
	sess.Scope = scope
	m := sessionToModel(sess)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create session: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create session: %w", err)
	}

	return nil
}

// GetSession returns a session by ID within the caller's scope.
func (s *Store) GetSession(ctx context.Context, sessionID id.SessionID) (*session.Session, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m sessionModel

	filter := bson.M{"_id": sessionID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrSessionNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get session: %w", err)
	}

	return sessionFromModel(&m)
}

// UpdateSession modifies an existing session's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the session's stored scope to touch it), and is never written back —
// scope_l0/l1/l2/extra/canon are deliberately absent from set below.
func (s *Store) UpdateSession(ctx context.Context, sess *session.Session) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sess.UpdatedAt = now()
	m := sessionToModel(sess)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"agent_id":      m.AgentID,
		"title":         m.Title,
		"message_count": m.MessageCount,
		"last_message":  m.LastMessage,
		"is_default":    m.IsDefault,
		"metadata":      m.Metadata,
		"updated_at":    m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*sessionModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update session: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrSessionNotFound
	}

	return nil
}

// DeleteSession removes a session within the caller's scope, along with
// every conversation message it owns, in the same multi-document
// transaction. Mongo has no FOREIGN KEY to do this for it -- and unlike
// postgres/sqlite (see their DeleteSession comments for why a FK there
// isn't viable either, FK or no FK), so the delete below is what keeps
// all three backends observably identical: deleting a session leaves no
// orphaned messages on any of them.
func (s *Store) DeleteSession(ctx context.Context, sessionID id.SessionID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sid := sessionID.String()
	filter := bson.M{"_id": sid}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	txSess, err := s.mdb.Client().StartSession()
	if err != nil {
		return fmt.Errorf("cortex/mongo: start delete session: %w", err)
	}
	defer txSess.EndSession(ctx)

	_, err = txSess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		res, delErr := s.mdb.NewDelete((*sessionModel)(nil)).
			Filter(filter).
			Exec(sc)
		if delErr != nil {
			return nil, fmt.Errorf("cortex/mongo: delete session: %w", delErr)
		}
		if res.DeletedCount() == 0 {
			return nil, fmt.Errorf("cortex/mongo: delete session: %w", cortex.ErrSessionNotFound)
		}

		msgFilter := bson.M{"session_id": sid, "kind": "conversation"}
		for k, v := range scopeFilter(scope, false) {
			msgFilter[k] = v
		}
		if _, msgErr := s.mdb.NewDelete((*memoryModel)(nil)).Many().Filter(msgFilter).Exec(sc); msgErr != nil {
			return nil, fmt.Errorf("cortex/mongo: delete session messages: %w", msgErr)
		}
		return nil, nil //nolint:nilnil // WithTransaction's callback contract: no result value to return
	})
	if err != nil {
		return fmt.Errorf("cortex/mongo: commit delete session: %w", err)
	}

	return nil
}

// ListSessions returns sessions within the caller's scope, optionally
// filtered.
func (s *Store) ListSessions(ctx context.Context, filter *session.ListFilter) ([]*session.Session, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &session.ListFilter{}
	}
	var models []sessionModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if !filter.AgentID.IsNil() {
		f["agent_id"] = filter.AgentID.String()
	}
	if filter.DefaultOnly {
		f["is_default"] = true
	}
	if filter.Search != "" {
		f["title"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter.Limit > 0 {
		q = q.Limit(int64(filter.Limit))
	}

	if filter.Offset > 0 {
		q = q.Skip(int64(filter.Offset))
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list sessions: %w", err)
	}

	result := make([]*session.Session, len(models))
	for i := range models {
		sess, convErr := sessionFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = sess
	}

	return result, nil
}

// CountSessions returns the total number of sessions matching the filter
// within the caller's scope.
func (s *Store) CountSessions(ctx context.Context, filter *session.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &session.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if !filter.AgentID.IsNil() {
		f["agent_id"] = filter.AgentID.String()
	}
	if filter.DefaultOnly {
		f["is_default"] = true
	}
	if filter.Search != "" {
		f["title"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*sessionModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count sessions: %w", err)
	}

	return count, nil
}
