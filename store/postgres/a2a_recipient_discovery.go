package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/recipientdiscovery"
)

// ListDeferredRecipients projects bound pending delivery IDs in the exact scope.
func (s *Store) ListDeferredRecipients(ctx context.Context, after id.DeliveryID, limit int) ([]id.DeliveryID, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if validationErr := recipientdiscovery.Validate(after, limit); validationErr != nil {
		return nil, validationErr
	}
	var models []a2aDeliveryModel
	q := s.pgdb.NewSelect(&models).Column("id").Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx)).Where("state = ?", a2a.DeliveryDelivering).Where("run_id <> ?", "").Where("claim_token <> ?", "").OrderExpr("id ASC").Limit(limit)
	for _, predicate := range scopePredicates(scope, true) {
		q = q.Where(predicate.Column+" = ?", predicate.Value)
	}
	if !after.IsNil() {
		q = q.Where("id > ?", after.String())
	}
	if scanErr := q.Scan(ctx); scanErr != nil {
		return nil, scanErr
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		rows = append(rows, model.ID)
	}
	return recipientdiscovery.Parse(rows, after, limit)
}
