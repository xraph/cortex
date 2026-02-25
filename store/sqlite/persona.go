package sqlite

import (
	"context"
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
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create persona: %w", err)
	}
	return nil
}

func (s *Store) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	m := new(personaModel)
	err := s.sdb.NewSelect(m).Where("id = ?", personaID.String()).Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get persona: %w", err)
	}
	return personaFromModel(m)
}

func (s *Store) GetPersonaByName(ctx context.Context, appID, name string) (*persona.Persona, error) {
	m := new(personaModel)
	err := s.sdb.NewSelect(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get persona by name: %w", err)
	}
	return personaFromModel(m)
}

func (s *Store) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	p.UpdatedAt = time.Now().UTC()
	m := personaToModel(p)
	res, err := s.sdb.NewUpdate(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update persona: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update persona rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	res, err := s.sdb.NewDelete((*personaModel)(nil)).
		Where("id = ?", personaID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete persona: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete persona rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	var models []personaModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
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
		return nil, fmt.Errorf("cortex/sqlite: list personas: %w", err)
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
