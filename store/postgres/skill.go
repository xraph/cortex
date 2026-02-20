package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/skill"
)

func (s *Store) CreateSkill(ctx context.Context, sk *skill.Skill) error {
	now := time.Now().UTC()
	sk.CreatedAt = now
	sk.UpdatedAt = now
	m := skillToModel(sk)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create skill: %w", err)
	}
	return nil
}

func (s *Store) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	m := new(skillModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", skillID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrSkillNotFound
		}
		return nil, fmt.Errorf("cortex: get skill: %w", err)
	}
	return skillFromModel(m), nil
}

func (s *Store) GetSkillByName(ctx context.Context, appID, name string) (*skill.Skill, error) {
	m := new(skillModel)
	err := s.db.NewSelect().Model(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrSkillNotFound
		}
		return nil, fmt.Errorf("cortex: get skill by name: %w", err)
	}
	return skillFromModel(m), nil
}

func (s *Store) UpdateSkill(ctx context.Context, sk *skill.Skill) error {
	sk.UpdatedAt = time.Now().UTC()
	m := skillToModel(sk)
	res, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update skill: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrSkillNotFound
	}
	return nil
}

func (s *Store) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	res, err := s.db.NewDelete().
		Model((*skillModel)(nil)).
		Where("id = ?", skillID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete skill: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrSkillNotFound
	}
	return nil
}

func (s *Store) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	var models []skillModel
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
		return nil, fmt.Errorf("cortex: list skills: %w", err)
	}
	result := make([]*skill.Skill, len(models))
	for i := range models {
		result[i] = skillFromModel(&models[i])
	}
	return result, nil
}
