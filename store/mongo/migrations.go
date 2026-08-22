package mongo

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Mongo is schemaless, so unlike the postgres/sqlite backends there is no
// grove migrate.Group here — there is no DDL to version. Indexes are the
// only structural thing this store manages, and Store.Migrate (store.go)
// applies migrationIndexes() directly with idempotent CreateMany on every
// startup. An earlier version of this file also carried a 12-migration
// grove.Group mirroring these same definitions in the postgres/sqlite
// Up-migration format, but nothing ever executed it — mongo.Store.Migrate
// never constructed a migrate.Orchestrator for it, the way the other two
// backends do — so it was pure duplication with drift risk (confirmed
// still in parity with migrationIndexes() collection-by-collection at
// deletion time) and was removed.

// scopeIndexName is the fixed name for the scope_l0/l1/l2 compound index,
// so a future migration path can drop or rebuild it by name rather than
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

// staleWorkingMemoryIndexName is Mongo's default auto-generated name for
// the pre-scope working-memory unique index (agent_id, kind, key), from
// when it carried no explicit name. Store.Migrate drops it by this name
// before creating workingMemoryUniqueIndexName below: without a scope
// column, that index let a caller in one scope upsert over another
// scope's row using nothing but a run ID, which is a bearer capability,
// not an isolation boundary. CreateMany is additive and never drops a
// stale index on its own, so leaving the old one in place would have
// kept the write path scope-blind even after this index existed too.
const staleWorkingMemoryIndexName = "agent_id_1_kind_1_key_1"

// workingMemoryUniqueIndexName is the fixed name for the scope-aware
// working-memory unique index, so a future migration can find and drop
// it by name the same way staleWorkingMemoryIndexName is used here.
const workingMemoryUniqueIndexName = "cortex_memories_working_scope_unique"

// migrationIndexes returns the index definitions for all cortex collections.
// This is what Store.Migrate actually runs on every startup (idempotent
// CreateMany).
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
				Keys:    bson.D{{Key: "agent_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "key", Value: 1}, {Key: "scope_canon", Value: 1}},
				Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"kind": "working"}).SetName(workingMemoryUniqueIndexName),
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
