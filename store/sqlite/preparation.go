package sqlite

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/preparation"
)

var _ store.PreparationSnapshotter = (*Store)(nil)

type preparationReader interface {
	NewSelect(...any) *sqlitedriver.SelectQuery
}

func (s *Store) preparationQuery() preparationReader {
	if s.preparationReader != nil {
		return s.preparationReader
	}
	return s.sdb
}

// CapturePreparation collects sources under one read-only serializable transaction.
func (s *Store) CapturePreparation(ctx context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if collect == nil {
		return nil, cortex.ErrPreparationUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	tx, err := s.sdb.BeginTxQuery(ctx, &driver.TxOptions{IsolationLevel: driver.LevelSerializable, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: begin preparation: %w: %w", cortex.ErrPreparationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // read-only cleanup; no-op after commit, no receipt on failure
	snapshot := &Store{db: s.db, sdb: s.sdb, preparationReader: tx}
	view := preparation.New(ctx, snapshot, nil)
	defer view.Close()
	if collectErr := collect(view); collectErr != nil {
		return nil, collectErr
	}
	view.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: commit preparation: %w: %w", cortex.ErrPreparationUnavailable, err)
	}
	return &run.SourceSnapshot{ID: rand.Text(), Provider: "sqlite", Isolation: "serializable", Scope: scope.Canonical()}, nil
}
