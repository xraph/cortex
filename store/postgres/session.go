package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

// CreateSession persists a new session, stamping the scope from the
// context. The partial UNIQUE (agent_id, scope_canon) WHERE is_default is
// what actually enforces one default session per agent per scope; a
// non-default session never touches that constraint.
func (s *Store) CreateSession(ctx context.Context, sess *session.Session) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.UpdatedAt = now
	sess.Scope = scope
	m := sessionToModel(sess)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create session: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, sessionID id.SessionID) (*session.Session, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(sessionModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", sessionID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrSessionNotFound
		}
		return nil, fmt.Errorf("cortex: get session: %w", err)
	}
	return sessionFromModel(m)
}

// mutableSessionColumns is every cortex_sessions column UpdateSession is
// allowed to write. A session's scope is set once at creation and never
// rewritten: scope_l0/l1/l2/extra/canon are deliberately absent here.
// Grove's NewUpdate builds SET from every model field by default, so
// without this explicit whitelist an UpdateSession issued from a broader
// (but still scope-matching) context would silently overwrite a row's
// narrower stored scope.
var mutableSessionColumns = []string{
	"agent_id",
	"title",
	"message_count",
	"last_message",
	"is_default",
	"metadata",
	"created_at",
	"updated_at",
}

// UpdateSession modifies an existing session's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the session's stored scope to touch it), and is never written back —
// mutableSessionColumns excludes the five scope columns from what gets
// written.
func (s *Store) UpdateSession(ctx context.Context, sess *session.Session) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sess.UpdatedAt = time.Now().UTC()
	m := sessionToModel(sess)
	q := s.pgdb.NewUpdate(m).Column(mutableSessionColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: update session rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrSessionNotFound
	}
	return nil
}

func (s *Store) DeleteSession(ctx context.Context, sessionID id.SessionID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.pgdb.NewDelete((*sessionModel)(nil)).
		Where("id = ?", sessionID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: delete session rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrSessionNotFound
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context, filter *session.ListFilter) ([]*session.Session, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &session.ListFilter{}
	}
	var models []sessionModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if !filter.AgentID.IsNil() {
		q = q.Where("agent_id = ?", filter.AgentID.String())
	}
	if filter.DefaultOnly {
		q = q.Where("is_default = ?", true)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(title) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list sessions: %w", err)
	}
	result := make([]*session.Session, len(models))
	for i := range models {
		sess, err := sessionFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list sessions: %w", err)
		}
		result[i] = sess
	}
	return result, nil
}

func (s *Store) CountSessions(ctx context.Context, filter *session.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &session.ListFilter{}
	}
	q := s.pgdb.NewSelect((*sessionModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if !filter.AgentID.IsNil() {
		q = q.Where("agent_id = ?", filter.AgentID.String())
	}
	if filter.DefaultOnly {
		q = q.Where("is_default = ?", true)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(title) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count sessions: %w", err)
	}
	return count, nil
}
