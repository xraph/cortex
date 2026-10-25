package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

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
	filter := a2aRoutingFilter(ctx, bson.M{"_id": d.ID.String(), "state": a2a.DeliveryDelivering, "claim_token": d.ClaimToken, "$and": bson.A{a2aOptionalString("run_id", "")}})
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}
	result, err := s.mdb.Collection(colA2ADeliveries).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"run_id": runID.String(), "updated_at": now()}})
	if err != nil {
		return fmt.Errorf("cortex: bind a2a delivery run: %w", err)
	}
	if result.MatchedCount == 0 {
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

func a2aOptionalString(field, value string) bson.M {
	if value != "" {
		return bson.M{field: value}
	}
	return bson.M{"$or": bson.A{bson.M{field: ""}, bson.M{field: bson.M{"$exists": false}}}}
}
