package mongo

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex/id"
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

// sessionDefaultUniqueIndexName is the fixed name for the partial unique
// index on cortex_sessions that enforces one default session per agent
// per scope, so a future migration can find and drop it by name the same
// way the stale *AppNameIndexName constants above are used.
//
// Unlike every other scope-keyed unique index in this file, this one
// carries no stale predecessor to drop: cortex_sessions is new this
// phase, so there is no pre-scope index competing with it.
const sessionDefaultUniqueIndexName = "cortex_sessions_agent_scope_default_unique"

// suspensionRunUniqueIndexName and suspensionExpiryIndexName are the
// fixed names for the two cortex_suspensions indexes, so a future
// migration can find and drop them by name the same way the stale
// *AppNameIndexName constants above are used. Like cortex_sessions,
// cortex_suspensions is new and carries no stale predecessor to drop.
const (
	suspensionRunUniqueIndexName = "cortex_suspensions_run_unique"
	suspensionExpiryIndexName    = "cortex_suspensions_expiry"
)

// overlayAgentScopeUniqueIndexName is the fixed name for the partial
// unique index on cortex_overlays that enforces one overlay per agent
// per scope, so a future migration can find and drop it by name the same
// way the stale *AppNameIndexName constants above are used. Like
// cortex_sessions and cortex_suspensions, cortex_overlays is new and
// carries no stale predecessor to drop.
const overlayAgentScopeUniqueIndexName = "cortex_overlays_agent_scope_unique"

// The postgres and sqlite backends carry a second migration this release
// (20260826000002) that adds a sections column to cortex_agents. Mongo
// has no counterpart because it has no DDL: a document simply gains the
// field the first time something writes it, and a document without it
// decodes to an empty section list, which is exactly what an agent that
// only ever set system_prompt is supposed to have.

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
			// Mirrors the postgres/sqlite 20260824000002 migration:
			// conversation memory is keyed on a session now, and every
			// SaveConversation/LoadConversation/ClearConversation call
			// filters on session_id alongside scope.
			{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "scope_canon", Value: 1}}},
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
		colSessions: {
			// Both predicates are load-bearing. is_default keeps
			// non-default sessions out of the index, so an agent can hold
			// many threads at once. scope_canon $gt "" keeps this in step
			// with every other partial unique index in this file
			// (colAgents above documents why), even though cortex_sessions
			// carries no legacy unscoped documents of its own: it is new
			// this phase, and CreateSession always stamps a real scope
			// before the first write.
			//
			// $gt rather than $ne for the same reason as colAgents:
			// Mongo's partial-index filter language rejects $ne outright,
			// and "greater than the empty string" is equivalent to
			// "non-empty" under BSON string ordering.
			{
				Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "scope_canon", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(sessionDefaultUniqueIndexName).
					SetPartialFilterExpression(bson.M{"is_default": true, "scope_canon": bson.M{"$gt": ""}}),
			},
			{Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "scope_canon", Value: 1}}},
			{Keys: bson.D{{Key: "created_at", Value: 1}}},
			scopeIndex,
		},
		colSuspensions: {
			// One suspension per run, which is what makes
			// ClaimSuspension a meaningful primitive: the claim reads
			// "the" suspension for a run, not one of several.
			//
			// scope_canon $gt "" keeps this in step with every other
			// partial unique index in this file (colAgents above
			// documents why), even though cortex_suspensions carries no
			// legacy unscoped documents of its own: it is new this
			// release, and CreateSuspension always stamps a real scope
			// before the first write.
			//
			// $gt rather than $ne for the same reason as colAgents:
			// Mongo's partial-index filter language rejects $ne outright,
			// and "greater than the empty string" is equivalent to
			// "non-empty" under BSON string ordering.
			{
				Keys: bson.D{{Key: "run_id", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(suspensionRunUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
			// The expiry sweep (ListExpired) reads only documents that
			// carry a deadline at all, so the index carries only those
			// documents too -- $exists true is inside the operator subset
			// partial filters accept, unlike $ne.
			{
				Keys: bson.D{{Key: "expires_at", Value: 1}},
				Options: options.Index().SetName(suspensionExpiryIndexName).
					SetPartialFilterExpression(bson.M{"expires_at": bson.M{"$exists": true}}),
			},
			scopeIndex,
		},
		colOverlays: {
			// One overlay per agent per scope. Prompt assembly reads
			// "the" overlay for an agent at a scope, so two documents
			// competing for that slot would make which one applies a
			// matter of document order.
			//
			// scope_canon $gt "" keeps this in step with every other
			// partial unique index in this file (colAgents above
			// documents why), even though cortex_overlays carries no
			// legacy unscoped documents of its own: it is new this
			// release, and CreateOverlay always stamps a real scope
			// before the first write.
			//
			// $gt rather than $ne for the same reason as colAgents:
			// Mongo's partial-index filter language rejects $ne outright,
			// and "greater than the empty string" is equivalent to
			// "non-empty" under BSON string ordering.
			{
				Keys: bson.D{{Key: "agent_id", Value: 1}, {Key: "scope_canon", Value: 1}},
				Options: options.Index().SetUnique(true).SetName(overlayAgentScopeUniqueIndexName).
					SetPartialFilterExpression(bson.M{"scope_canon": bson.M{"$gt": ""}}),
			},
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

// backfillSessionMarker tags every cortex_sessions document this backfill
// creates, in the cortex-owned backfilled_by field, for the same
// audit-trail reason as the postgres/sqlite migrations of the same
// version -- an organically-created default session
// (engine.resolveSession) has the identical IsDefault=true, Title
// "Default" shape but never sets backfilled_by. This used to live in
// Metadata, which session.Session documents as belonging to the host: a
// host that overwrote its own metadata on a backfilled document would
// silently destroy the marker. Mongo carries no versioned migration
// list to give this a Down to reverse, so nothing currently reads the
// marker back, but it costs nothing to leave every backfilled document
// distinguishable from an organic one, in a field a host can't collide
// with, and it keeps this data shape identical across all three
// backends.
const backfillSessionMarker = "20260824000004"

// legacyConversationGroup is one distinct (agent_id, scope) pairing this
// backfill found sitting on unsessioned conversation documents.
type legacyConversationGroup struct {
	AgentID                               string
	ScopeL0, ScopeL1, ScopeL2, ScopeCanon string
}

func (g legacyConversationGroup) key() string { return g.AgentID + "|" + g.ScopeCanon }

// legacyConversationScopeDoc is the minimal projection
// findLegacyConversationGroups reads: agent_id and the four scope
// fields, no content. Bounding the projection this way means the size
// of the in-memory group set tracks the number of distinct
// (agent_id, scope) pairs waiting to be backfilled, not the number of
// orphaned documents -- a large legacy database can hold many documents
// per pair, and this pass no longer holds any of their content.
type legacyConversationScopeDoc struct {
	AgentID    string `bson:"agent_id"`
	ScopeL0    string `bson:"scope_l0"`
	ScopeL1    string `bson:"scope_l1"`
	ScopeL2    string `bson:"scope_l2"`
	ScopeCanon string `bson:"scope_canon"`
}

// backfillLegacyMessage is the subset of memory.Message this backfill
// needs off a stored conversation document's JSON content. Role and
// Content are the only two fields that survive engine.llmToMemory's
// reload-then-resave round trip unchanged (see the postgres migration
// 20260824000004's comment on the pre-daa7e44 duplication bug) --
// everything else in memory.Message is deliberately not decoded here.
type backfillLegacyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// backfillDefaultSessions creates one default session per distinct
// (agent_id, scope) pairing found on pre-v1.9.0 conversation documents
// and points those documents at it.
//
// Unlike the postgres/sqlite migration of the same version, this has no
// versioned-migration-table to make it run exactly once: Store.Migrate
// calls it on every startup, the same as rescopeLegacyRows, and it stays
// cheap on repeat runs because its own filter (session_id: "") shrinks
// to nothing the moment a document has been pointed at a session --
// there is nothing left for a later call to find.
//
// See the postgres migration 20260824000004 for the full reasoning
// behind skipping documents whose scope_canon is still "" (a host that
// never ran a Rescoper) and computing message_count as a
// distinct-(role,content) count rather than a raw document count; this
// is the same logic against mongo documents.
func (s *Store) backfillDefaultSessions(ctx context.Context) error {
	groups, err := s.findLegacyConversationGroups(ctx)
	if err != nil {
		return fmt.Errorf("find legacy conversation groups: %w", err)
	}
	for _, g := range groups {
		if err := s.backfillOneGroup(ctx, g); err != nil {
			return fmt.Errorf("backfill default session for agent %s: %w", g.AgentID, err)
		}
	}
	return nil
}

// findLegacyConversationGroups discovers every distinct (agent_id,
// scope) pairing sitting on orphaned conversation documents, without
// reading any document content -- see legacyConversationScopeDoc for
// why. backfillOneGroup issues its own, separate query per group to
// read that group's content, so this pass never buffers more than one
// group's identity at a time per document scanned.
func (s *Store) findLegacyConversationGroups(ctx context.Context) ([]legacyConversationGroup, error) {
	filter := bson.M{
		"kind":        "conversation",
		"session_id":  "",
		"scope_canon": bson.M{"$ne": ""},
	}
	projection := bson.M{"agent_id": 1, "scope_l0": 1, "scope_l1": 1, "scope_l2": 1, "scope_canon": 1}
	cur, err := s.mdb.Collection(colMemories).Find(ctx, filter, options.Find().SetProjection(projection))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()

	seen := make(map[string]struct{})
	var groups []legacyConversationGroup
	for cur.Next(ctx) {
		var doc legacyConversationScopeDoc
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return nil, fmt.Errorf("decode legacy conversation scope: %w", decodeErr)
		}
		g := legacyConversationGroup(doc)
		k := g.key()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		groups = append(groups, g)
	}
	return groups, cur.Err()
}

// reduceLegacyMessagesFor streams one group's orphaned conversation
// documents -- content only, sorted by created_at -- and reduces them to
// the two values its default session needs: a distinct-(role,content)
// message count and the content of the chronologically-last message
// with non-empty content. See backfillDefaultSessions/the postgres
// migration for why messageCount is a distinct-pair count rather than a
// raw document count, and why lastMessage skips empty-content messages.
//
// This is a cursor, not a slice load: memory use is bounded by one
// group's document count at a time, not by the total number of orphaned
// documents across every group findLegacyConversationGroups found.
func (s *Store) reduceLegacyMessagesFor(ctx context.Context, g legacyConversationGroup) (messageCount int, lastMessage string, err error) {
	filter := bson.M{
		"kind": "conversation", "session_id": "",
		"agent_id": g.AgentID, "scope_canon": g.ScopeCanon,
	}
	cur, findErr := s.mdb.Collection(colMemories).Find(ctx, filter,
		options.Find().SetProjection(bson.M{"content": 1}).SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if findErr != nil {
		return 0, "", findErr
	}
	defer func() { _ = cur.Close(ctx) }()

	seen := make(map[[2]string]struct{})
	for cur.Next(ctx) {
		var doc struct {
			Content string `bson:"content"`
		}
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return 0, "", fmt.Errorf("decode conversation content: %w", decodeErr)
		}
		var msg backfillLegacyMessage
		if jsonErr := json.Unmarshal([]byte(doc.Content), &msg); jsonErr != nil {
			// A document that isn't valid JSON can't have been written
			// by SaveConversation; skip it from the count rather than
			// aborting the whole backfill over one corrupt document.
			continue
		}
		seen[[2]string{msg.Role, msg.Content}] = struct{}{}
		if msg.Content != "" {
			lastMessage = msg.Content
		}
	}
	if cursorErr := cur.Err(); cursorErr != nil {
		return 0, "", cursorErr
	}
	return len(seen), lastMessage, nil
}

// backfillOneGroup creates (or reuses) the default session for one
// (agent_id, scope_canon) group and points that group's orphaned
// conversation documents at it, inside one multi-document transaction so
// a mid-write failure rolls back whole rather than leaving some
// documents pointed at a session that itself failed to commit.
func (s *Store) backfillOneGroup(ctx context.Context, g legacyConversationGroup) error {
	messageCount, lastMessage, err := s.reduceLegacyMessagesFor(ctx, g)
	if err != nil {
		return fmt.Errorf("read conversation documents: %w", err)
	}

	txSess, err := s.mdb.Client().StartSession()
	if err != nil {
		return fmt.Errorf("start backfill session: %w", err)
	}
	defer txSess.EndSession(ctx)

	_, err = txSess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		sid, sessErr := s.insertOrReuseDefaultSession(sc, g, messageCount, lastMessage)
		if sessErr != nil {
			return nil, sessErr
		}

		filter := bson.M{
			"kind":        "conversation",
			"session_id":  "",
			"agent_id":    g.AgentID,
			"scope_canon": g.ScopeCanon,
		}
		if _, updErr := s.mdb.Collection(colMemories).UpdateMany(sc, filter, bson.M{"$set": bson.M{"session_id": sid}}); updErr != nil {
			return nil, fmt.Errorf("point conversation documents at default session: %w", updErr)
		}
		return nil, nil //nolint:nilnil // WithTransaction's callback contract: no result value to return
	})
	if err != nil {
		return fmt.Errorf("commit backfill transaction: %w", err)
	}
	return nil
}

// insertOrReuseDefaultSession inserts the default session for
// (agent_id, scope_canon), or -- if the partial unique index
// (sessionDefaultUniqueIndexName) already has one, because a concurrent
// instance's backfill or an ordinary engine.resolveSession call won the
// race -- reads back whichever document actually holds that slot. This
// mirrors the postgres/sqlite migrations' identical insert-then-select
// pattern: trusting the id this call minted, without confirming it's
// the one that actually landed, would risk pointing the memory
// documents below at a session that was never created.
func (s *Store) insertOrReuseDefaultSession(ctx context.Context, g legacyConversationGroup, messageCount int, lastMessage string) (string, error) {
	t := now()
	doc := sessionModel{
		ID:           id.NewSessionID().String(),
		AgentID:      g.AgentID,
		Title:        "Default",
		MessageCount: messageCount,
		LastMessage:  lastMessage,
		IsDefault:    true,
		BackfilledBy: backfillSessionMarker,
		ScopeL0:      g.ScopeL0,
		ScopeL1:      g.ScopeL1,
		ScopeL2:      g.ScopeL2,
		ScopeExtra:   map[string]string{},
		ScopeCanon:   g.ScopeCanon,
		CreatedAt:    t,
		UpdatedAt:    t,
	}
	if _, err := s.mdb.NewInsert(&doc).Exec(ctx); err != nil && !isUniqueViolation(err) {
		return "", fmt.Errorf("insert default session: %w", err)
	}

	var existing sessionModel
	if err := s.mdb.NewFind(&existing).
		Filter(bson.M{"agent_id": g.AgentID, "scope_canon": g.ScopeCanon, "is_default": true}).
		Scan(ctx); err != nil {
		return "", fmt.Errorf("resolve default session: %w", err)
	}
	return existing.ID, nil
}
