package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/internal/orchestrationstate"
)

// LinkOrchestrationChild atomically admits one child while its parent is running.
func (s *Store) LinkOrchestrationChild(ctx context.Context, team id.OrchestrationID, child id.AgentRunID) error {
	return orchestrationstate.Link(ctx, orchestrationRunDriver{store: s}, team, child)
}

type orchestrationRunDriver struct{ store *Store }

func (d orchestrationRunDriver) Read(ctx context.Context, key id.OrchestrationID) (orchestrationstate.Snapshot, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return orchestrationstate.Snapshot{}, err
	}
	var model orchestrationRunModel
	filter := bson.M{"_id": key.String()}
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}
	if err = d.store.mdb.NewFind(&model).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return orchestrationstate.Snapshot{}, cortex.ErrOrchestrationRunNotFound
		}
		return orchestrationstate.Snapshot{}, fmt.Errorf("cortex: read orchestration run revision: %w", err)
	}
	record, err := orchestrationRunFromModel(&model)
	return orchestrationstate.Snapshot{Run: record, Revision: model.Revision}, err
}

func (d orchestrationRunDriver) CompareAndSwap(ctx context.Context, before orchestrationstate.Snapshot, after *orchestration.Run) (bool, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return false, err
	}
	model := orchestrationRunToModel(after)
	filter := bson.M{"_id": before.Run.ID.String(), "revision": before.Revision}
	if before.Revision == 0 {
		delete(filter, "revision")
		filter["$or"] = bson.A{bson.M{"revision": int64(0)}, bson.M{"revision": bson.M{"$exists": false}}}
	}
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}
	set := bson.M{"revision": before.Revision + 1, "config_id": model.ConfigID, "strategy": model.Strategy, "status": model.Status, "input": model.Input, "output": model.Output, "error": model.Error, "agent_run_ids": model.AgentRunIDs, "started_at": model.StartedAt, "completed_at": model.CompletedAt, "created_at": model.CreatedAt, "updated_at": model.UpdatedAt}
	result, err := d.store.mdb.NewUpdate((*orchestrationRunModel)(nil)).Filter(filter).SetUpdate(bson.M{"$set": set}).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("cortex: update orchestration run revision: %w", err)
	}
	return result.MatchedCount() == 1, nil
}
