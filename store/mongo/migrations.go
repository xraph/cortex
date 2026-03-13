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
	)
}

// migrationIndexes returns the index definitions for all cortex collections.
func migrationIndexes() map[string][]mongo.IndexModel {
	return map[string][]mongo.IndexModel{
		colAgents: {
			{
				Keys:    bson.D{{Key: "app_id", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colRuns: {
			{Keys: bson.D{{Key: "agent_id", Value: 1}}},
			{Keys: bson.D{{Key: "state", Value: 1}}},
			{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Keys: bson.D{{Key: "created_at", Value: -1}}},
		},
		colSteps: {
			{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "index", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colToolCalls: {
			{Keys: bson.D{{Key: "step_id", Value: 1}}},
			{Keys: bson.D{{Key: "run_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colMemories: {
			{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}}},
			{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "tenant_id", Value: 1}, {Key: "kind", Value: 1}}},
			{
				Keys:    bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "key", Value: 1}},
				Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"kind": "working"}),
			},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
		},
		colCheckpoints: {
			{Keys: bson.D{{Key: "run_id", Value: 1}}},
			{Keys: bson.D{{Key: "state", Value: 1}, {Key: "created_at", Value: 1}}},
			{Keys: bson.D{{Key: "tenant_id", Value: 1}}},
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
	}
}
