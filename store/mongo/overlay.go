package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
)

// CreateOverlay persists a new overlay, stamping the scope from the
// context. The partial unique index on (agent_id, scope_canon) is what
// enforces one overlay per agent per scope, so a second create for the
// same pair comes back as cortex.ErrAlreadyExists rather than quietly
// shadowing the first.
func (s *Store) CreateOverlay(ctx context.Context, o *prompt.Overlay) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	o.CreatedAt = t
	o.UpdatedAt = t
	o.Scope = scope
	m := overlayToModel(o)

	if _, err := s.mdb.NewInsert(m).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create overlay: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create overlay: %w", err)
	}
	return nil
}

// GetOverlay reads one overlay by id within the caller's scope.
func (s *Store) GetOverlay(ctx context.Context, overlayID id.OverlayID) (*prompt.Overlay, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m overlayModel

	filter := bson.M{"_id": overlayID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOverlayNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get overlay: %w", err)
	}
	return overlayFromModel(&m)
}

// GetOverlayForAgent reads the overlay an agent has at the caller's
// exact scope. See prompt.Store for why this one read matches exactly
// rather than by prefix.
func (s *Store) GetOverlayForAgent(ctx context.Context, agentID id.AgentID) (*prompt.Overlay, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m overlayModel

	filter := bson.M{"agent_id": agentID.String()}
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}

	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOverlayNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get overlay for agent: %w", err)
	}
	return overlayFromModel(&m)
}

// GetOverlayForAgentAt reads the overlay an agent has at exactly the
// given scope. See prompt.Store for why inheritance is an ancestor walk
// rather than a prefix match, and why the scope argument is bounded to
// the caller's own ancestry.
func (s *Store) GetOverlayForAgentAt(ctx context.Context, agentID id.AgentID, scope cortex.Scope) (*prompt.Overlay, error) {
	caller := cortex.ScopeFromContext(ctx)
	if caller.IsZero() {
		return nil, cortex.ErrNoScope
	}
	// A scope outside the caller's own ancestry is refused as absent
	// rather than as an error: from where the caller stands, that
	// overlay does not exist.
	if scope.IsZero() || !scope.Covers(caller) {
		return nil, cortex.ErrOverlayNotFound
	}
	var m overlayModel

	filter := bson.M{"agent_id": agentID.String()}
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}

	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOverlayNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get overlay for agent at scope: %w", err)
	}
	return overlayFromModel(&m)
}

// UpdateOverlay rewrites an overlay's mutable fields within the caller's
// scope. Scope is immutable after creation: the context scope is used
// only as an authorization predicate (the caller must be at or above the
// overlay's stored scope to touch it), and is never written back. grove's
// NewUpdate(model).Exec defaults to a full-field $set built from the
// model struct when no explicit update document is given, which would
// otherwise blank scope_l0/l1/l2/extra/canon on every call.
//
// agent_id and created_at are absent from set for a different reason:
// repointing an overlay at a different agent is a create, not an edit,
// and it would move the document into a uniqueness slot another overlay
// may already hold.
//
// temperature and max_tokens are $set unconditionally, including when
// they are nil. Skipping a nil would make "clear this override"
// impossible to express, since the stored value would survive every
// update that tried to remove it.
func (s *Store) UpdateOverlay(ctx context.Context, o *prompt.Overlay) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	o.UpdatedAt = now()
	m := overlayToModel(o)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"patches":       m.Patches,
		"tools_added":   m.ToolsAdded,
		"tools_removed": m.ToolsRemoved,
		"model":         m.Model,
		"temperature":   m.Temperature,
		"max_tokens":    m.MaxTokens,
		"updated_at":    m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*overlayModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update overlay: %w", err)
	}
	if res.MatchedCount() == 0 {
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
	filter := bson.M{"_id": overlayID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*overlayModel)(nil)).Filter(filter).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete overlay: %w", err)
	}
	if res.DeletedCount() == 0 {
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

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if !filter.AgentID.IsNil() {
		f["agent_id"] = filter.AgentID.String()
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
		return nil, fmt.Errorf("cortex/mongo: list overlays: %w", err)
	}

	result := make([]*prompt.Overlay, len(models))
	for i := range models {
		o, convErr := overlayFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = o
	}
	return result, nil
}
