// Package assurance shares immutable evidence and report persistence rules.
package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/grove"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/verification"
)

const (
	KindEvidence     = "evidence"
	KindContract     = "contract"
	KindReport       = "report"
	MaxEvidenceBytes = 1 << 20
)

var (
	ErrAbsent = errors.New("cortex: assurance record absent")
	ErrExists = errors.New("cortex: assurance record exists")
)

// Record preserves canonical JSON as text, avoiding database JSON rewrites.
// The same indexed scope codec is used by every Cortex backend.
type Record struct {
	grove.BaseModel `grove:"table:cortex_assurance_records" bson:"-"`
	ID              string `grove:"id" bson:"id"`
	Kind            string `grove:"kind" bson:"kind"`
	ScopeL0         string `grove:"scope_l0" bson:"scope_l0"`
	ScopeL1         string `grove:"scope_l1" bson:"scope_l1"`
	ScopeL2         string `grove:"scope_l2" bson:"scope_l2"`
	ScopeExtra      string `grove:"scope_extra" bson:"-"`
	ScopeCanon      string `grove:"scope_canon" bson:"scope_canon"`
	Digest          string `grove:"digest" bson:"digest"`
	RunID           string `grove:"run_id" bson:"run_id"`
	Body            string `grove:"body" bson:"body"`
	Deleted         bool   `grove:"deleted" bson:"deleted"`
}

// Query is always constrained by exact context scope in the backend.
type Query struct {
	Kind, ID, Digest, RunID, After string
	Limit                          int
	Reverse                        bool
}

// Driver contains only database-specific operations. Insert must never update
// an existing row. ReplaceEvidence is a compare-and-swap on the complete body.
type Driver interface {
	Insert(context.Context, Record) error
	Select(context.Context, Query) ([]Record, error)
	ReplaceEvidence(context.Context, Record, string) error
}

// Repository implements optional store capabilities without widening Store.
type Repository struct {
	driver Driver
	clock  verification.Clock
}

// New creates a shared persistence adapter for a backend driver.
func New(driver Driver) *Repository { return &Repository{driver: driver, clock: wallClock{}} }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// WithClock returns an adapter with trusted host time, for deterministic lease tests.
// Nil clocks leave the default clock in place.
func (s *Repository) WithClock(clock verification.Clock) *Repository {
	if clock == nil {
		return s
	}
	return &Repository{driver: s.driver, clock: clock}
}
func (s *Repository) inTransaction(tx RunTransaction) *Repository {
	return &Repository{driver: tx, clock: s.clock}
}

// Columns returns the exact indexed identity, including empty trailing levels.
func Columns(scope cortex.Scope) [3]string {
	var columns [3]string
	for i, level := range scope.Levels {
		if i < 3 {
			columns[i] = level.Key + "=" + level.Value
		}
	}
	return columns
}

func require(ctx context.Context, scope cortex.Scope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if checkErr := cortex.ValidateScope(scope); checkErr != nil {
		return checkErr
	}
	if current.Canonical() != scope.Canonical() {
		return cortex.ErrInvalidScope
	}
	return nil
}

func record(kind, identifier, digest, runID string, scope cortex.Scope, value any) (Record, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Record{}, err
	}
	columns := Columns(scope)
	return Record{ID: identifier, Kind: kind, Digest: digest, RunID: runID, Body: string(data), ScopeCanon: scope.Canonical(), ScopeL0: columns[0], ScopeL1: columns[1], ScopeL2: columns[2], ScopeExtra: "{}"}, nil
}

func validateRecord(ctx context.Context, r Record) error {
	scope, err := cortex.DecodeStoredScope(r.ScopeCanon, [3]string{r.ScopeL0, r.ScopeL1, r.ScopeL2}, r.ScopeExtra != "{}")
	if err != nil {
		return err
	}
	return require(ctx, scope)
}

func (s *Repository) get(ctx context.Context, query Query) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return Record{}, err
	}
	query.Limit = 1
	rows, err := s.driver.Select(ctx, query)
	if err != nil {
		return Record{}, err
	}
	if len(rows) == 0 {
		return Record{}, ErrAbsent
	}
	r := rows[0]
	if checkErr := validateRecord(ctx, r); checkErr != nil {
		return Record{}, checkErr
	}
	return r, nil
}

func (s *Repository) insert(ctx context.Context, r Record) error {
	err := s.driver.Insert(ctx, r)
	if !errors.Is(err, ErrExists) {
		return err
	}
	existing, readErr := s.get(ctx, Query{Kind: r.Kind, ID: r.ID})
	if readErr != nil {
		return readErr
	}
	if existing.Deleted {
		return ErrExists
	}
	if existing.Digest == r.Digest && existing.Body == r.Body {
		return nil
	}
	return ErrExists
}

// SQLMigration is shared by SQLite and PostgreSQL; only portable SQL is used.
const SQLMigration = `CREATE TABLE IF NOT EXISTS cortex_assurance_records (
 id TEXT NOT NULL, kind TEXT NOT NULL,
 scope_l0 TEXT NOT NULL, scope_l1 TEXT NOT NULL, scope_l2 TEXT NOT NULL,
 scope_extra TEXT NOT NULL DEFAULT '{}', scope_canon TEXT NOT NULL,
 digest TEXT NOT NULL, run_id TEXT NOT NULL, body TEXT NOT NULL,
 deleted BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (kind, scope_canon, id)
);
CREATE INDEX IF NOT EXISTS idx_cortex_assurance_run ON cortex_assurance_records (kind, scope_canon, run_id, id);
CREATE INDEX IF NOT EXISTS idx_cortex_assurance_digest ON cortex_assurance_records (kind, scope_canon, digest);
CREATE INDEX IF NOT EXISTS idx_cortex_assurance_scope ON cortex_assurance_records (scope_l0, scope_l1, scope_l2);`
