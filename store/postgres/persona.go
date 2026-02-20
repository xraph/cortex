package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

func (s *Store) CreatePersona(ctx context.Context, p *persona.Persona) error {
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	m := personaToModel(p)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create persona: %w", err)
	}
	return nil
}

func (s *Store) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	m := new(personaModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", personaID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex: get persona: %w", err)
	}
	return personaFromModel(m), nil
}

func (s *Store) GetPersonaByName(ctx context.Context, appID, name string) (*persona.Persona, error) {
	m := new(personaModel)
	err := s.db.NewSelect().Model(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex: get persona by name: %w", err)
	}
	return personaFromModel(m), nil
}

func (s *Store) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	p.UpdatedAt = time.Now().UTC()
	m := personaToModel(p)
	res, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update persona: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	res, err := s.db.NewDelete().
		Model((*personaModel)(nil)).
		Where("id = ?", personaID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete persona: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	var models []personaModel
	q := s.db.NewSelect().Model(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list personas: %w", err)
	}
	result := make([]*persona.Persona, len(models))
	for i := range models {
		result[i] = personaFromModel(&models[i])
	}
	return result, nil
}
