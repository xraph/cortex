package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// BindDeliveryRun records a reserved child only while this claim still owns it.
func (s *Store) BindDeliveryRun(ctx context.Context, d *a2a.Delivery, runID id.AgentRunID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if d == nil || d.ClaimToken == "" || !d.RunID.IsNil() || runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return a2a.ErrDeliveryClaimLost
	}
	q := s.sdb.NewUpdate((*a2aDeliveryModel)(nil)).
		Set("run_id = ?", runID.String()).Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", d.ID.String()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx)).
		Where("state = ?", a2a.DeliveryDelivering).Where("claim_token = ?", d.ClaimToken).Where("run_id = ?", "")
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: bind a2a delivery run: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: bind a2a delivery rows affected: %w", err)
	}
	if n == 0 {
		return s.deliveryMutationMiss(ctx, scope, d.ID)
	}
	return nil
}

func (s *Store) deliveryMutationMiss(ctx context.Context, scope cortex.Scope, key id.DeliveryID) error {
	row, err := s.getDelivery(ctx, scope, key)
	if err != nil {
		return err
	}
	if row.Scope.Canonical() != scope.Canonical() {
		return a2a.ErrDeliveryNotFound
	}
	return a2a.ErrDeliveryClaimLost
}
