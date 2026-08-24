package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
)

// newTestStore opens a migrated SQLite store backed by a temporary file.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_test.db")
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	s := New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAgentDuplicateReturnsAlreadyExists(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})

	cfg := &agent.Config{ID: id.NewAgentID(), Name: "dup"}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Same (scope_canon, name), different ID — must collide on the unique index.
	dup := &agent.Config{ID: id.NewAgentID(), Name: "dup"}
	err := s.Create(ctx, dup)
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
}

func TestCreatePersonaDuplicateReturnsAlreadyExists(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})

	p := &persona.Persona{ID: id.NewPersonaID(), Name: "dup"}
	if err := s.CreatePersona(ctx, p); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Same (scope_canon, name), different ID — must collide on the unique index.
	dup := &persona.Persona{ID: id.NewPersonaID(), Name: "dup"}
	err := s.CreatePersona(ctx, dup)
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
}

// TestAgentWithoutSectionsRoundTripsEmpty pins the compatibility promise
// the sections column has to keep. cortex_agents is created without that
// column and gains it by ALTER TABLE, so an agent that only ever set
// SystemPrompt has to come back with its prompt untouched and no
// sections at all. Assembly falls back to SystemPrompt when the section
// list is empty, so this is what keeps an upgraded agent producing the
// exact prompt it produced before.
func TestAgentWithoutSectionsRoundTripsEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})

	cfg := &agent.Config{ID: id.NewAgentID(), Name: "legacy", SystemPrompt: "you are helpful"}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	got, err := s.Get(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.SystemPrompt != "you are helpful" {
		t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, "you are helpful")
	}
	if len(got.Sections) != 0 {
		t.Errorf("Sections = %+v, want empty; an agent that never set sections must not gain any", got.Sections)
	}
}

// TestAgentSectionsRoundTrip is the other half: an agent that DOES carry
// sections has to get them back intact, or the column would be a
// write-only field nothing could read.
func TestAgentSectionsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})

	want := []prompt.Section{{ID: "identity", Title: "Identity", Body: "you are helpful", Order: 10, Locked: true}}
	cfg := &agent.Config{ID: id.NewAgentID(), Name: "sectioned", Sections: want}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	got, err := s.Get(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(got.Sections) != 1 || got.Sections[0] != want[0] {
		t.Errorf("Sections round-tripped as %+v, want %+v", got.Sections, want)
	}
}
