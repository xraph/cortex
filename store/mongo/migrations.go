package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/grove/drivers/mongodriver/mongomigrate"
	"github.com/xraph/grove/migrate"
)

// Migrations is the grove migration group for the Cortex mongo store.
var Migrations = migrate.NewGroup("cortex")

func init() {
	Migrations.MustRegister(
		&migrate.Migration{
			Name:    "create_cortex_agents",
			Version: "20240101000001",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*agentModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colAgents, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*agentModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_runs",
			Version: "20240101000002",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*runModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colRuns, []mongo.IndexModel{
					{Keys: bson.D{{Key: "agent_id", Value: 1}}},
					{Keys: bson.D{{Key: "state", Value: 1}}},
					{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}},
					{Keys: bson.D{{Key: "created_at", Value: -1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*runModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_steps",
			Version: "20240101000003",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*stepModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colSteps, []mongo.IndexModel{
					{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "index", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*stepModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_tool_calls",
			Version: "20240101000004",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*toolCallModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colToolCalls, []mongo.IndexModel{
					{Keys: bson.D{{Key: "step_id", Value: 1}}},
					{Keys: bson.D{{Key: "run_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*toolCallModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_memories",
			Version: "20240101000005",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*memoryModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colMemories, []mongo.IndexModel{
					{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}}},
					{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "tenant_id", Value: 1}, {Key: "kind", Value: 1}}},
					{
						Keys:    bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "key", Value: 1}},
						Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"kind": "working"}),
					},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*memoryModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_checkpoints",
			Version: "20240101000006",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*checkpointModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colCheckpoints, []mongo.IndexModel{
					{Keys: bson.D{{Key: "run_id", Value: 1}}},
					{Keys: bson.D{{Key: "state", Value: 1}, {Key: "created_at", Value: 1}}},
					{Keys: bson.D{{Key: "tenant_id", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*checkpointModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_skills",
			Version: "20240101000007",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*skillModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colSkills, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*skillModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_traits",
			Version: "20240101000008",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*traitModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colTraits, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "category", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*traitModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_behaviors",
			Version: "20240101000009",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*behaviorModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colBehaviors, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*behaviorModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_personas",
			Version: "20240101000010",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*personaModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colPersonas, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				return mexec.DropCollection(ctx, (*personaModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "create_cortex_orchestrations",
			Version: "20240101000011",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				if err := mexec.CreateCollection(ctx, (*orchestrationConfigModel)(nil)); err != nil {
					return err
				}

				if err := mexec.CreateIndexes(ctx, colOrchestrationConfigs, []mongo.IndexModel{
					{
						Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
						Options: options.Index().SetUnique(true),
					},
					{Keys: bson.D{{Key: "app_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: 1}}},
				}); err != nil {
					return err
				}

				if err := mexec.CreateCollection(ctx, (*orchestrationRunModel)(nil)); err != nil {
					return err
				}

				return mexec.CreateIndexes(ctx, colOrchestrationRuns, []mongo.IndexModel{
					{Keys: bson.D{{Key: "app_id", Value: 1}, {Key: "status", Value: 1}}},
					{Keys: bson.D{{Key: "config_id", Value: 1}}},
					{Keys: bson.D{{Key: "created_at", Value: -1}}},
				})
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				if err := mexec.DropCollection(ctx, (*orchestrationRunModel)(nil)); err != nil {
					return err
				}
				return mexec.DropCollection(ctx, (*orchestrationConfigModel)(nil))
			},
		},
		&migrate.Migration{
			Name:    "add_scope_indexes",
			Version: "20260821000001",
			Comment: "Add compound scope_l0/l1/l2 index to every scope-carrying collection",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}

				// Every scoped query builds an equality filter over these
				// three fields via scopeFilter, so without this index every
				// scoped read is a full collection scan. tenant_id indexes
				// on colRuns/colMemories/colCheckpoints are left alone: the
				// tenant_id columns survive this release as read-only.
				for _, coll := range []string{colAgents, colRuns, colMemories, colCheckpoints} {
					if err := mexec.CreateIndexes(ctx, coll, []mongo.IndexModel{
						{
							Keys:    bson.D{{Key: "scope_l0", Value: 1}, {Key: "scope_l1", Value: 1}, {Key: "scope_l2", Value: 1}},
							Options: options.Index().SetName(scopeIndexName),
						},
					}); err != nil {
						return fmt.Errorf("index scope on %s: %w", coll, err)
					}
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				for _, coll := range []string{colAgents, colRuns, colMemories, colCheckpoints} {
					if err := mexec.DB().Collection(coll).Indexes().DropOne(ctx, scopeIndexName); err != nil {
						return fmt.Errorf("drop scope index on %s: %w", coll, err)
					}
				}
				return nil
			},
		},
	)
}

// scopeIndexName is the fixed name for the scope_l0/l1/l2 compound index,
// given explicitly so the Down migration can drop it by name rather than
// reconstructing Mongo's default key-based naming convention.
const scopeIndexName = "scope_l0_l1_l2"

// scopeIndex is the compound scope_l0/l1/l2 index shared by every
// scope-carrying collection. Every scoped query builds an equality filter
// over these three fields via scopeFilter, so without this index every
// scoped read is a full collection scan. The stale tenant_id indexes below
// are left alone: per the phase's ruling, the tenant_id columns survive
// this release as read-only, so their indexes stay too even though
// nothing writes tenant_id anymore.
var scopeIndex = mongo.IndexModel{
	Keys:    bson.D{{Key: "scope_l0", Value: 1}, {Key: "scope_l1", Value: 1}, {Key: "scope_l2", Value: 1}},
	Options: options.Index().SetName(scopeIndexName),
}

// migrationIndexes returns the index definitions for all cortex collections.
// This is what Store.Migrate actually runs on every startup (idempotent
// CreateMany); the grove migrate.Group above mirrors the same definitions
// in the Up-migration format the postgres/sqlite backends use.
func migrationIndexes() map[string][]mongo.IndexModel {
	return map[string][]mongo.IndexModel{
		colAgents: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colRuns: {
			{Keys: bson.D{{Key: "agent_id", Value: 1}}},
			{Keys: bson.D{{Key: "state", Value: 1}}},
			{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Keys: bson.D{{Key: "created_at", Value: -1}}},
			scopeIndex,
		},
		colSteps: {
			{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "index", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colToolCalls: {
			{Keys: bson.D{{Key: "step_id", Value: 1}}},
			{Keys: bson.D{{Key: "run_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colMemories: {
			{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}}},
			{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "tenant_id", Value: 1}, {Key: "kind", Value: 1}}},
			{
				Keys:    bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "key", Value: 1}},
				Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"kind": "working"}),
			},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colCheckpoints: {
			{Keys: bson.D{{Key: "run_id", Value: 1}}},
			{Keys: bson.D{{Key: "state", Value: 1}, {Key: "created_at", Value: 1}}},
			{Keys: bson.D{{Key: "tenant_id", Value: 1}}},
			scopeIndex,
		},
		colSkills: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colTraits: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "category", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colBehaviors: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colPersonas: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colOrchestrationConfigs: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colOrchestrationRuns: {
			{Keys: bson.D{{Key: "app_id", Value: 1}, {Key: "status", Value: 1}}},
			{Keys: bson.D{{Key: "config_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: -1}}},
		},
	}
}
