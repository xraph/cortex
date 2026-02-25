package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

// CreatePersona persists a new persona.
func (s *Store) CreatePersona(ctx context.Context, p *persona.Persona) error {
	t := now()
	p.CreatedAt = t
	p.UpdatedAt = t
	m := personaToModel(p)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create persona: %w", err)
	}

	return nil
}

// GetPersona returns a persona by ID.
func (s *Store) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	var m personaModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": personaID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrPersonaNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get persona: %w", err)
	}

	return personaFromModel(&m)
}

// GetPersonaByName returns a persona by app ID and name.
func (s *Store) GetPersonaByName(ctx context.Context, appID, name string) (*persona.Persona, error) {
	var m personaModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrPersonaNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get persona by name: %w", err)
	}

	return personaFromModel(&m)
}

// UpdatePersona modifies an existing persona.
func (s *Store) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	p.UpdatedAt = now()
	m := personaToModel(p)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update persona: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrPersonaNotFound
	}

	return nil
}

// DeletePersona removes a persona.
func (s *Store) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	res, err := s.mdb.NewDelete((*personaModel)(nil)).
		Filter(bson.M{"_id": personaID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete persona: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrPersonaNotFound
	}

	return nil
}

// ListPersonas returns personas, optionally filtered.
func (s *Store) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	var models []personaModel

	f := bson.M{}
	if filter != nil && filter.AppID != "" {
		f["app_id"] = filter.AppID
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}

		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
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
