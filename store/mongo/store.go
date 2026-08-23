package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

// Collection name constants.
const (
	colAgents               = "cortex_agents"
	colRuns                 = "cortex_runs"
	colSteps                = "cortex_steps"
	colToolCalls            = "cortex_tool_calls"
	colMemories             = "cortex_memories"
	colCheckpoints          = "cortex_checkpoints"
	colSkills               = "cortex_skills"
	colTraits               = "cortex_traits"
	colBehaviors            = "cortex_behaviors"
	colPersonas             = "cortex_personas"
	colSessions             = "cortex_sessions"
	colOrchestrationConfigs = "cortex_orchestration_configs"
	colOrchestrationRuns    = "cortex_orchestration_runs"
)

// Compile-time interface check.
var _ store.Store = (*Store)(nil)

// Store implements store.Store using MongoDB via Grove ORM.
type Store struct {
	db  *grove.DB
	mdb *mongodriver.MongoDB
}

// New creates a new MongoDB store backed by Grove ORM.
func New(db *grove.DB) *Store {
	return &Store{
		db:  db,
		mdb: mongodriver.Unwrap(db),
	}
}

// Migrate creates indexes for all cortex collections.
func (s *Store) Migrate(ctx context.Context, opts ...cortex.MigrateOption) error {
	o := cortex.ApplyMigrateOptions(opts...)

	if err := s.dropStaleWorkingMemoryIndex(ctx); err != nil {
		return err
	}
	if err := s.dropStaleAgentAppNameIndex(ctx); err != nil {
		return err
	}
	if err := s.dropStaleAppNameIndex(ctx, colSkills, staleSkillAppNameIndexName); err != nil {
		return err
	}
	if err := s.dropStaleAppNameIndex(ctx, colTraits, staleTraitAppNameIndexName); err != nil {
		return err
	}
	if err := s.dropStaleAppNameIndex(ctx, colBehaviors, staleBehaviorAppNameIndexName); err != nil {
		return err
	}
	if err := s.dropStaleAppNameIndex(ctx, colPersonas, stalePersonaAppNameIndexName); err != nil {
		return err
	}
	if err := s.dropStaleAppNameIndex(ctx, colOrchestrationConfigs, staleOrchestrationAppNameIndexName); err != nil {
		return err
	}

	indexes := migrationIndexes()

	for col, models := range indexes {
		if len(models) == 0 {
			continue
		}

		_, err := s.mdb.Collection(col).Indexes().CreateMany(ctx, models)
		if err != nil {
			return fmt.Errorf("cortex/mongo: migrate %s indexes: %w", col, err)
		}
	}

	if err := s.rescopeLegacyRows(ctx, o); err != nil {
		return fmt.Errorf("cortex/mongo: rescope legacy rows: %w", err)
	}

	// Runs after rescopeLegacyRows so a pre-v1.8.0 database jumping
	// straight to this version in one Migrate() call gets its
	// conversation rows scoped first, in the same call, and so is
	// eligible for the session backfill immediately rather than only on
	// a later restart -- see backfillDefaultSessions' comment for why
	// the postgres/sqlite migrations of the same version can't offer
	// that same-call guarantee.
	if err := s.backfillDefaultSessions(ctx); err != nil {
		return fmt.Errorf("cortex/mongo: backfill default sessions: %w", err)
	}

	return nil
}

// Mongo error codes DropOne can return that this helper treats as
// "there was nothing to drop" rather than a fatal error.
const (
	// mongoIndexNotFound is returned when the named index doesn't exist
	// on a collection that does exist — a Migrate call that already
	// dropped it once, or a database that was scoped from the start.
	mongoIndexNotFound = 27
	// mongoNamespaceNotFound is returned when the collection itself
	// doesn't exist yet, which is the normal case on a fresh database:
	// nothing in this repo pre-creates collections (the mongo migration
	// group that used to call CreateCollection was deleted as dead code
	// in an earlier round of this same phase), so cortex_memories is
	// created implicitly on its first write, not by Migrate.
	mongoNamespaceNotFound = 26
)

// dropStaleWorkingMemoryIndex removes the pre-scope working-memory
// unique index by its Mongo-generated default name
// (staleWorkingMemoryIndexName). It carried no scope column, so a caller
// in one scope could upsert over another scope's row using nothing but a
// run ID — a bearer capability, not an isolation boundary.
// migrationIndexes() replaces it with workingMemoryUniqueIndexName, which
// also indexes scope_canon, but CreateMany only creates indexes, it
// never drops a stale one on its own — so this runs first, on every
// startup, and tolerates both "the index is already gone"
// (mongoIndexNotFound, e.g. a Migrate call that already dropped it once)
// and "the collection doesn't exist yet" (mongoNamespaceNotFound, e.g. a
// genuinely fresh database, where dropIndexes fails before it ever gets
// to look for the index by name). Running the drop before CreateMany —
// rather than after, once CreateMany has implicitly brought the
// collection into existence — keeps the drop-then-create ordering
// unambiguous rather than depending on collection creation as a side
// effect of an unrelated call.
func (s *Store) dropStaleWorkingMemoryIndex(ctx context.Context) error {
	err := s.mdb.Collection(colMemories).Indexes().DropOne(ctx, staleWorkingMemoryIndexName)
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && (cmdErr.Code == mongoIndexNotFound || cmdErr.Code == mongoNamespaceNotFound) {
		return nil
	}
	return fmt.Errorf("cortex/mongo: drop stale working-memory index: %w", err)
}

// dropStaleAgentAppNameIndex removes the pre-scope agent unique index by
// its Mongo-generated default name (staleAgentAppNameIndexName). app_id
// was never the isolation boundary for agent names — scope is — so the
// old (app_id, name) index refused two different scopes the same name.
// migrationIndexes() replaces it with agentScopeNameUniqueIndexName, but
// CreateMany only creates indexes, it never drops a stale one on its own,
// so this runs first on every startup, tolerating the same "already gone"
// and "collection doesn't exist yet" cases as
// dropStaleWorkingMemoryIndex.
func (s *Store) dropStaleAgentAppNameIndex(ctx context.Context) error {
	err := s.mdb.Collection(colAgents).Indexes().DropOne(ctx, staleAgentAppNameIndexName)
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && (cmdErr.Code == mongoIndexNotFound || cmdErr.Code == mongoNamespaceNotFound) {
		return nil
	}
	return fmt.Errorf("cortex/mongo: drop stale agent app_id+name index: %w", err)
}

// dropStaleAppNameIndex removes a pre-scope (app_id, name) unique index
// by its Mongo-generated default name from the given collection. Shared
// by cortex_skills, cortex_traits, cortex_behaviors, and cortex_personas,
// which all converted to a scope-keyed unique index in their own
// migration: app_id was never the isolation boundary for any of their
// names — scope is — so the old index refused two different scopes the
// same name. CreateMany only creates indexes, it never drops a stale one
// on its own, so this runs first on every startup, tolerating the same
// "already gone" and "collection doesn't exist yet" cases as
// dropStaleWorkingMemoryIndex/dropStaleAgentAppNameIndex.
//
// indexName is passed explicitly per collection even though every
// (app_id, name) compound index Mongo has auto-named so far happens to
// collide on "app_id_1_name_1": that's a property of Mongo's
// default-naming scheme for this exact field-pair shape, not a
// guarantee, and a future stale index with a different shape (or an
// explicit name) must not silently start reusing this same literal
// because the parameter got dropped.
//
//nolint:unparam // see above: the shared literal is coincidental, not structural
func (s *Store) dropStaleAppNameIndex(ctx context.Context, col, indexName string) error {
	err := s.mdb.Collection(col).Indexes().DropOne(ctx, indexName)
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && (cmdErr.Code == mongoIndexNotFound || cmdErr.Code == mongoNamespaceNotFound) {
		return nil
	}
	return fmt.Errorf("cortex/mongo: drop stale %s app_id+name index: %w", col, err)
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.Ping(ctx)
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// now returns the current UTC time.
func now() time.Time {
	return time.Now().UTC()
}

// isNoDocuments checks if an error wraps mongo.ErrNoDocuments.
func isNoDocuments(err error) bool {
	return errors.Is(err, mongo.ErrNoDocuments)
}

// isUniqueViolation reports whether err is a MongoDB duplicate-key error
// (code 11000), so callers can translate it into cortex.ErrAlreadyExists.
func isUniqueViolation(err error) bool {
	return mongo.IsDuplicateKeyError(err)
}
