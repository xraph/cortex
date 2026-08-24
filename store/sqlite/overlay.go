package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
)

// CreateOverlay persists a new overlay, stamping the scope from the
// context. The partial UNIQUE (agent_id, scope_canon) is what enforces
// one overlay per agent per scope, so a second create for the same pair
// comes back as cortex.ErrAlreadyExists rather than quietly shadowing
// the first.
func (s *Store) CreateOverlay(ctx context.Context, o *prompt.Overlay) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	o.Scope = scope
	m := overlayToModel(o)
	if _, err := s.sdb.NewInsert(m).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create overlay: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create overlay: %w", err)
	}
	return nil
}

// GetOverlay reads one overlay by id within the caller's scope.
func (s *Store) GetOverlay(ctx context.Context, overlayID id.OverlayID) (*prompt.Overlay, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(overlayModel)
	q := s.sdb.NewSelect(m).Where("id = ?", overlayID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOverlayNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get overlay: %w", err)
	}
	return overlayFromModel(m)
}

// GetOverlayForAgent reads the overlay an agent has at the caller's
// exact scope. See prompt.Store for why this one read matches exactly
// rather than by prefix.
func (s *Store) GetOverlayForAgent(ctx context.Context, agentID id.AgentID) (*prompt.Overlay, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(overlayModel)
	q := s.sdb.NewSelect(m).Where("agent_id = ?", agentID.String())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOverlayNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get overlay for agent: %w", err)
	}
	return overlayFromModel(m)
}

// mutableOverlayColumns is every cortex_overlays column UpdateOverlay is
// allowed to write. An overlay's scope is set once at creation and never
// rewritten: scope_l0/l1/l2/extra/canon are deliberately absent here.
// Grove's NewUpdate builds SET from every model field by default, so
// without this explicit whitelist UpdateOverlay would blank the scope
// columns on every call (overlayToModel always derives them from
// o.Scope, and nothing populates o.Scope on the update path).
//
// agent_id and created_at are absent for the same reason in a different
// direction: repointing an overlay at a different agent is a create, not
// an edit, and it would move the row into a uniqueness slot another
// overlay may already hold.
var mutableOverlayColumns = []string{
	"patches",
	"tools_added",
	"tools_removed",
	"model",
	"temperature",
	"max_tokens",
	"updated_at",
}

// UpdateOverlay rewrites an overlay's mutable fields within the caller's
// scope. Scope is immutable after creation: the context scope is used
// only as an authorization predicate (the caller must be at or above the
// overlay's stored scope to touch it), and is never written back.
func (s *Store) UpdateOverlay(ctx context.Context, o *prompt.Overlay) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	o.UpdatedAt = time.Now().UTC()
	m := overlayToModel(o)
	q := s.sdb.NewUpdate(m).Column(mutableOverlayColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update overlay: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update overlay rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrOverlayNotFound
	}
	return nil
}

// DeleteOverlay removes an overlay within the caller's scope.
func (s *Store) DeleteOverlay(ctx context.Context, overlayID id.OverlayID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*overlayModel)(nil)).Where("id = ?", overlayID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete overlay: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete overlay rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrOverlayNotFound
	}
	return nil
}

// ListOverlays returns overlays within the caller's scope, oldest first.
func (s *Store) ListOverlays(ctx context.Context, filter *prompt.ListFilter) ([]*prompt.Overlay, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &prompt.ListFilter{}
	}
	var models []overlayModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if !filter.AgentID.IsNil() {
		q = q.Where("agent_id = ?", filter.AgentID.String())
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list overlays: %w", err)
	}
	result := make([]*prompt.Overlay, len(models))
	for i := range models {
		o, err := overlayFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex/sqlite: list overlays: %w", err)
		}
		result[i] = o
	}
	return result, nil
}
