package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/skill"
)

// CreateSkill persists a new skill.
func (s *Store) CreateSkill(ctx context.Context, sk *skill.Skill) error {
	t := now()
	sk.CreatedAt = t
	sk.UpdatedAt = t
	m := skillToModel(sk)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create skill: %w", err)
	}

	return nil
}

// GetSkill returns a skill by ID.
func (s *Store) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	var m skillModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": skillID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrSkillNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get skill: %w", err)
	}

	return skillFromModel(&m)
}

// GetSkillByName returns a skill by app ID and name.
func (s *Store) GetSkillByName(ctx context.Context, appID, name string) (*skill.Skill, error) {
	var m skillModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrSkillNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get skill by name: %w", err)
	}

	return skillFromModel(&m)
}

// UpdateSkill modifies an existing skill.
func (s *Store) UpdateSkill(ctx context.Context, sk *skill.Skill) error {
	sk.UpdatedAt = now()
	m := skillToModel(sk)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update skill: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrSkillNotFound
	}

	return nil
}

// DeleteSkill removes a skill.
func (s *Store) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	res, err := s.mdb.NewDelete((*skillModel)(nil)).
		Filter(bson.M{"_id": skillID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete skill: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrSkillNotFound
	}

	return nil
}

// ListSkills returns skills, optionally filtered.
func (s *Store) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	var models []skillModel

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
		return nil, fmt.Errorf("cortex/mongo: list skills: %w", err)
	}

	result := make([]*skill.Skill, len(models))
	for i := range models {
		sk, convErr := skillFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = sk
	}

	return result, nil
}
