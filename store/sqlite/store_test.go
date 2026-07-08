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
	ctx := context.Background()
	s := newTestStore(t)

	cfg := &agent.Config{ID: id.NewAgentID(), Name: "dup", AppID: "app1"}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Same (app_id, name), different ID — must collide on the unique index.
	dup := &agent.Config{ID: id.NewAgentID(), Name: "dup", AppID: "app1"}
	err := s.Create(ctx, dup)
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
}

func TestCreatePersonaDuplicateReturnsAlreadyExists(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := &persona.Persona{ID: id.NewPersonaID(), Name: "dup", AppID: "app1"}
	if err := s.CreatePersona(ctx, p); err != nil {
		t.Fatalf("first create: %v", err)
	}

	dup := &persona.Persona{ID: id.NewPersonaID(), Name: "dup", AppID: "app1"}
	err := s.CreatePersona(ctx, dup)
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
}
