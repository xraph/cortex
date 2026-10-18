package mongo

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/scopeaudit"
)

// InspectScopes streams invalid stored identities to an operator callback.
// This administrative scan crosses all scopes and never changes data.
func (s *Store) InspectScopes(ctx context.Context, visit func(store.ScopeIssue) error) error {
	if visit == nil {
		return fmt.Errorf("cortex: scope inventory callback is required")
	}
	collections := []string{colAgents, colRuns, colSteps, colToolCalls, colMemories, colCheckpoints, colSkills, colTraits, colBehaviors, colPersonas, colSessions, colSuspensions, colOverlays, colOrchestrationConfigs, colOrchestrationRuns, colA2AMessages, colA2AConversations, colA2ADeliveries, colA2APendingAsks}
	for _, collection := range collections {
		if err := s.inspectCollectionScopes(ctx, collection, visit); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Store) inspectCollectionScopes(ctx context.Context, collection string, visit func(store.ScopeIssue) error) error {
	cursor, err := s.mdb.Collection(collection).Find(ctx, bson.M{}, options.Find().SetBatchSize(100).SetProjection(bson.M{"_id": 1, "scope_canon": 1, "scope_l0": 1, "scope_l1": 1, "scope_l2": 1, "scope_extra": 1}))
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		var row bson.M
		if err := cursor.Decode(&row); err != nil {
			return err
		}
		canon := inventoryString(row["scope_canon"])
		l0 := inventoryString(row["scope_l0"])
		l1 := inventoryString(row["scope_l1"])
		l2 := inventoryString(row["scope_l2"])
		extra, marshalErr := json.Marshal(row["scope_extra"])
		if marshalErr != nil {
			return cortex.ErrInvalidScope
		}
		if issue := scopeaudit.Issue(collection, fmt.Sprint(row["_id"]), canon, [3]string{l0, l1, l2}, string(extra)); issue != nil {
			if err := visit(*issue); err != nil {
				return err
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func inventoryString(value any) string {
	if value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return "\x00invalid-scope-value"
	}
	return text
}
