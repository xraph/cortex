package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

// CreatePersona persists a new persona, stamping the scope from the
// context. The (scope_canon, name) unique index is what actually
// enforces per-scope name uniqueness; app_id survives on the document as
// a vestigial field no longer read or filtered on.
func (s *Store) CreatePersona(ctx context.Context, p *persona.Persona) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	p.CreatedAt = t
	p.UpdatedAt = t
	p.Scope = scope
	m := personaToModel(p)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create persona: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create persona: %w", err)
	}

	return nil
}

// GetPersona returns a persona by ID within the caller's scope.
func (s *Store) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m personaModel

	filter := bson.M{"_id": personaID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrPersonaNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get persona: %w", err)
	}

	return personaFromModel(&m)
}

// GetPersonaByName returns a persona by name within the caller's scope.
func (s *Store) GetPersonaByName(ctx context.Context, name string) (*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m personaModel

	filter := bson.M{"name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrPersonaNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get persona by name: %w", err)
	}

	return personaFromModel(&m)
}

// UpdatePersona modifies an existing persona's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope
// is used only as an authorization predicate (the caller must be at or
// above the persona's stored scope to touch it), and is never written
// back. grove's NewUpdate(model).Exec defaults to a full-field $set built
// from the model struct when no explicit update document is given, which
// would otherwise blank scope_l0/l1/l2/extra/canon on every call.
//
// app_id is deliberately absent from set too, for the same reason:
// personaToModel always writes AppID as "" now (Persona carries no such
// field to draw from), so including it here would blank whatever a
// pre-v1.8.0 document's app_id field still holds on its very first
// Update. The field itself is vestigial but intentionally left in place;
// erasing its content is not this task's call to make.
func (s *Store) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	p.UpdatedAt = now()
	m := personaToModel(p)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":                m.Name,
		"description":         m.Description,
		"identity":            m.Identity,
		"skills":              m.Skills,
		"traits":              m.Traits,
		"behaviors":           m.Behaviors,
		"cognitive_style":     m.CognitiveStyle,
		"communication_style": m.CommunicationStyle,
		"perception":          m.Perception,
		"metadata":            m.Metadata,
		"updated_at":          m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*personaModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update persona: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrPersonaNotFound
	}

	return nil
}

// DeletePersona removes a persona within the caller's scope.
func (s *Store) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": personaID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*personaModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete persona: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrPersonaNotFound
	}

	return nil
}

// ListPersonas returns personas within the caller's scope, optionally filtered.
func (s *Store) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &persona.ListFilter{}
	}
	var models []personaModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
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
		return nil, fmt.Errorf("cortex/mongo: list personas: %w", err)
	}

	result := make([]*persona.Persona, len(models))
	for i := range models {
		p, convErr := personaFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = p
	}

	return result, nil
}

// CountPersonas returns the total number of personas matching the
// filter within the caller's scope.
func (s *Store) CountPersonas(ctx context.Context, filter *persona.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &persona.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*personaModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count personas: %w", err)
	}

	return count, nil
}
