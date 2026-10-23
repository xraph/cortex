package assurance

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

// KindPlan identifies the immutable initial plan row used to serialize writes.
const KindPlan = kindPlan

// PlanDriver serializes the callback with every writer of this exact plan.
// Mongo may replay the callback. It must contain only transactional storage work.
type PlanDriver interface {
	TransactPlan(context.Context, id.ID, func(context.Context, Driver) error) error
}

func (s *Repository) planTransaction(ctx context.Context, identifier id.ID, callback func(context.Context, *Repository) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if identifier.IsNil() || identifier.Prefix() != planning.PrefixPlan {
		return planning.ErrInvalid
	}
	driver, ok := s.driver.(PlanDriver)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	return driver.TransactPlan(ctx, identifier, func(tc context.Context, tx Driver) error {
		repo := &Repository{driver: tx, clock: s.clock}
		if _, err := repo.GetPlanRevision(tc, identifier, 1); err != nil {
			return err
		}
		if err := callback(tc, repo); err != nil {
			return err
		}
		return tc.Err()
	})
}
