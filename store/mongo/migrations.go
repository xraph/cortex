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

// staleAgentAppNameIndexName is Mongo's default auto-generated name for
// the pre-scope agent unique index (app_id, name), from when it carried
// no explicit name. Store.Migrate drops it by this name before creating
// agentScopeNameUniqueIndexName below: app_id was never the isolation
// boundary for agent names, scope is, so two different scopes must each
// be able to use the same agent name, which the old index refused.
// CreateMany is additive and never drops a stale index on its own, so
// leaving the old one in place would keep the write path colliding
// cross-scope even after the new index existed too.
const staleAgentAppNameIndexName = "app_id_1_name_1"

// agentScopeNameUniqueIndexName is the fixed name for the scope-aware
// agent name unique index, so a future migration can find and drop it by
// name the same way staleAgentAppNameIndexName is used here.
const agentScopeNameUniqueIndexName = "cortex_agents_scope_name_unique"

// staleSkillAppNameIndexName, staleTraitAppNameIndexName, and
// staleBehaviorAppNameIndexName are Mongo's default auto-generated names
// for the pre-scope (app_id, name) unique indexes on cortex_skills,
// cortex_traits, and cortex_behaviors, from when they carried no explicit
// name. Store.Migrate drops each by name before creating its
// scope-keyed replacement below: app_id was never the isolation boundary
// for these names, scope is, so the old index refused two different
// scopes the same name. CreateMany is additive and never drops a stale
// index on its own, so leaving the old ones in place would keep the
// write path colliding cross-scope even after the new indexes existed
// too.
const (
	staleSkillAppNameIndexName    = "app_id_1_name_1"
	staleTraitAppNameIndexName    = "app_id_1_name_1"
	staleBehaviorAppNameIndexName = "app_id_1_name_1"
)

// skillScopeNameUniqueIndexName, traitScopeNameUniqueIndexName, and
// behaviorScopeNameUniqueIndexName are the fixed names for the
// scope-aware unique indexes on cortex_skills, cortex_traits, and
// cortex_behaviors, so a future migration can find and drop them by name
// the same way the stale names above are used here.
const (
	skillScopeNameUniqueIndexName    = "cortex_skills_scope_name_unique"
	traitScopeNameUniqueIndexName    = "cortex_traits_scope_name_unique"
	behaviorScopeNameUniqueIndexName = "cortex_behaviors_scope_name_unique"
)

// stalePersonaAppNameIndexName is Mongo's default auto-generated name
// for the pre-scope (app_id, name) unique index on cortex_personas, from
// when it carried no explicit name. Store.Migrate drops it by name
// before creating its scope-keyed replacement below: app_id was never
// the isolation boundary for persona names, scope is, so the old index
// refused two different scopes the same name. CreateMany is additive and
// never drops a stale index on its own, so leaving the old one in place
// would keep the write path colliding cross-scope even after the new
// index existed too.
const stalePersonaAppNameIndexName = "app_id_1_name_1"

// personaScopeNameUniqueIndexName is the fixed name for the scope-aware
// unique index on cortex_personas, so a future migration can find and
// drop it by name the same way stalePersonaAppNameIndexName is used
// here.
const personaScopeNameUniqueIndexName = "cortex_personas_scope_name_unique"

// staleOrchestrationAppNameIndexName is Mongo's default auto-generated
// name for the pre-scope (app_id, name) unique index on
// cortex_orchestration_configs, from when it carried no explicit name.
// Store.Migrate drops it by name before creating its scope-keyed
// replacement below: app_id was never the isolation boundary for
// orchestration names, scope is, so the old index refused two different
// scopes the same name. CreateMany is additive and never drops a stale
// index on its own, so leaving the old one in place would keep the
// write path colliding cross-scope even after the new index existed
// too.
//
// The app_id field itself is not dropped from these documents, but not
// because any lookup still needs it: GetOrchestrationByName(ctx, name)
// takes no appID parameter. It stays because rescopeLegacyRows reads it
// off pre-v1.8.0 documents to reconstruct the scope a Rescoper should
// assign them.
//
// Orchestration is the last entity converted in this phase, and the only
// one that ever had scope columns dropped once already: an earlier
// attempt populated cortex_orchestration_runs' scope fields only
// partially and it read as coverage that wasn't there, so this
// collection carried no scope-aware index at all until now.
const staleOrchestrationAppNameIndexName = "app_id_1_name_1"

// orchestrationScopeNameUniqueIndexName is the fixed name for the
// scope-aware unique index on cortex_orchestration_configs, so a future
// migration can find and drop it by name the same way
// staleOrchestrationAppNameIndexName is used here.
const orchestrationScopeNameUniqueIndexName = "cortex_orchestration_configs_scope_name_unique"

// migrationIndexes returns the index definitions for all cortex collections.
// This is what Store.Migrate actually runs on every startup (idempotent
// CreateMany).
func migrationIndexes() map[string][]mongo.IndexModel {
	return map[string][]mongo.IndexModel{
		colAgents: {
			// Partial (scope_canon $ne "") for the same reason as the
			// postgres/sqlite equivalent: Store.Migrate applies this
			// index before rescoping legacy rows, and any pre-v1.8.0
			// document is still sitting at scope_canon = "" at that
			// point. Two such documents sharing a name but never
			// colliding under the old app_id-keyed index would make a
			// non-partial unique index fail to build against existing
			// data before the rescoper ever got a chance to separate
			// them. Every document Create writes always carries a real,
			// non-empty scope_canon (Create rejects a zero scope), so
			// the partial index protects every current and future write
			// exactly as a full index would.
			{
				// $gt rather than $ne: Mongo's partial-index filter
				// language only supports a restricted operator subset
				// ($eq, $exists, $gt/$gte/$lt/$lte, $type, and top-level
				// $and) and rejects $ne outright ("Expression not
				// supported in partial index: $not"). scope_canon is
				// always a string, so "greater than the empty string"
				// is equivalent to "non-empty" under BSON string
				// ordering and lands within the supported subset.
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(agentScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
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
			// Partial (scope_canon $gt "") for the same reason as
			// colAgents above: Store.Migrate applies this index before
			// rescoping legacy rows, and any pre-v1.8.0 document is
			// still sitting at scope_canon = "" at that point.
			{
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(skillScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colTraits: {
			// Partial (scope_canon $gt "") for the same reason as
			// colAgents above: Store.Migrate applies this index before
			// rescoping legacy rows, and any pre-v1.8.0 document is
			// still sitting at scope_canon = "" at that point.
			{
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(traitScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "category", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colBehaviors: {
			// Partial (scope_canon $gt "") for the same reason as
			// colAgents above: Store.Migrate applies this index before
			// rescoping legacy rows, and any pre-v1.8.0 document is
			// still sitting at scope_canon = "" at that point.
			{
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(behaviorScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colPersonas: {
			// Partial (scope_canon $gt "") for the same reason as
			// colAgents above: Store.Migrate applies this index before
			// rescoping legacy rows, and any pre-v1.8.0 document is
			// still sitting at scope_canon = "" at that point.
			{
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(personaScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colOrchestrationConfigs: {
			// Partial (scope_canon $gt "") for the same reason as
			// colAgents above: Store.Migrate applies this index before
			// rescoping legacy rows, and any pre-v1.8.0 document is
			// still sitting at scope_canon = "" at that point.
			{
				Keys: bson.D{{Key: "scope_canon", Value: 1}, {Key: "name", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(orchestrationScopeNameUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "app_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colOrchestrationRuns: {
			{Keys: bson.D{{Key: "app_id", Value: 1}, {Key: "status", Value: 1}}},
			{Keys: bson.D{{Key: "config_id", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: -1}}},
			scopeIndex,
		},
	}
}
