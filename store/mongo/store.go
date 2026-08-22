package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

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
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.dropStaleWorkingMemoryIndex(ctx); err != nil {
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

	return nil
}

// mongoIndexNotFound is the error code Mongo returns for an index name
// that doesn't exist, which DropOne otherwise reports as a fatal error.
const mongoIndexNotFound = 27

// dropStaleWorkingMemoryIndex removes the pre-scope working-memory
// unique index by its Mongo-generated default name
// (staleWorkingMemoryIndexName). It carried no scope column, so a caller
// in one scope could upsert over another scope's row using nothing but a
// run ID — a bearer capability, not an isolation boundary.
// migrationIndexes() replaces it with workingMemoryUniqueIndexName, which
// also indexes scope_canon, but CreateMany only creates indexes, it
// never drops a stale one on its own — so this runs first, on every
// startup, and tolerates the index already being gone (a fresh database,
// or a Migrate call that already dropped it once).
func (s *Store) dropStaleWorkingMemoryIndex(ctx context.Context) error {
	err := s.mdb.Collection(colMemories).Indexes().DropOne(ctx, staleWorkingMemoryIndexName)
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && cmdErr.Code == mongoIndexNotFound {
		return nil
	}
	return fmt.Errorf("cortex/mongo: drop stale working-memory index: %w", err)
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
