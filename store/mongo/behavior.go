package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
)

// CreateBehavior persists a new behavior.
func (s *Store) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	t := now()
	b.CreatedAt = t
	b.UpdatedAt = t
	m := behaviorToModel(b)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create behavior: %w", err)
	}

	return nil
}

// GetBehavior returns a behavior by ID.
func (s *Store) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	var m behaviorModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": behaviorID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrBehaviorNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get behavior: %w", err)
	}

	return behaviorFromModel(&m)
}

// GetBehaviorByName returns a behavior by app ID and name.
func (s *Store) GetBehaviorByName(ctx context.Context, appID, name string) (*behavior.Behavior, error) {
	var m behaviorModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrBehaviorNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get behavior by name: %w", err)
	}

	return behaviorFromModel(&m)
}

// UpdateBehavior modifies an existing behavior.
func (s *Store) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	b.UpdatedAt = now()
	m := behaviorToModel(b)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update behavior: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrBehaviorNotFound
	}

	return nil
}

// DeleteBehavior removes a behavior.
func (s *Store) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	res, err := s.mdb.NewDelete((*behaviorModel)(nil)).
		Filter(bson.M{"_id": behaviorID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete behavior: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrBehaviorNotFound
	}

	return nil
}

// ListBehaviors returns behaviors, optionally filtered.
func (s *Store) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	var models []behaviorModel

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
		return nil, fmt.Errorf("cortex/mongo: list behaviors: %w", err)
	}

	result := make([]*behavior.Behavior, len(models))
	for i := range models {
		b, convErr := behaviorFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = b
	}

	return result, nil
}
