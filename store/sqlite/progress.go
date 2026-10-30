package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/grove"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
)

// SaveProgress comes from the shared assurance repository, which checks the
// run's state and lease owner under TransactRun and then calls
// UpsertProgress below in the same transaction.
var _ suspension.ProgressStore = (*Store)(nil)

type progressModel struct {
	grove.BaseModel `grove:"table:cortex_run_progress"`
	RunID           string    `grove:"run_id,pk"`
	Owner           string    `grove:"owner,notnull"`
	Progress        string    `grove:"progress,notnull"`
	ScopeL0         string    `grove:"scope_l0,notnull"`
	ScopeL1         string    `grove:"scope_l1,notnull"`
	ScopeL2         string    `grove:"scope_l2,notnull"`
	ScopeExtra      string    `grove:"scope_extra,notnull"`
	ScopeCanon      string    `grove:"scope_canon,notnull"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
}

// UpsertProgress writes the snapshot inside TransactRun. The conflict
// branch only replaces a row stored at the same scope.
func (d assuranceDriver) UpsertProgress(ctx context.Context, p *suspension.Progress) error {
	if d.transaction == nil {
		return suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	body, err := runstate.EncodeProgress(p)
	if err != nil {
		return err
	}
	l0, l1, l2, extra := scopeColumns(scope)
	m := &progressModel{
		RunID: p.RunID.String(), Owner: p.Owner, Progress: body,
		ScopeL0: l0, ScopeL1: l1, ScopeL2: l2, ScopeExtra: extra, ScopeCanon: scope.Canonical(),
		CreatedAt: p.UpdatedAt, UpdatedAt: p.UpdatedAt,
	}
	result, err := d.queries().NewInsert(m).
		OnConflict("(run_id) DO UPDATE SET owner = excluded.owner, progress = excluded.progress, updated_at = excluded.updated_at WHERE cortex_run_progress.scope_canon = excluded.scope_canon").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save run progress: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex/sqlite: save run progress rows affected: %w", err)
	}
	if count != 1 {
		return cortex.ErrRunLeaseLost
	}
	return nil
}

// GetProgress reads a run's snapshot at the exact context scope.
func (s *Store) GetProgress(ctx context.Context, runID id.AgentRunID) (*suspension.Progress, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	m := new(progressModel)
	q := s.sdb.NewSelect(m).Where("run_id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrProgressNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get run progress: %w", err)
	}
	return progressFromModel(m)
}

// DeleteProgress removes a run's snapshot at the exact context scope. A
// run with none is not an error.
func (s *Store) DeleteProgress(ctx context.Context, runID id.AgentRunID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := s.sdb.NewDelete((*progressModel)(nil)).Where("run_id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, err = q.Exec(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: delete run progress: %w", err)
	}
	return nil
}

func progressFromModel(m *progressModel) (*suspension.Progress, error) {
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	scope, err := decodeStoredScope(m.ScopeCanon, m.ScopeL0, m.ScopeL1, m.ScopeL2, m.ScopeExtra)
	if err != nil {
		return nil, fmt.Errorf("run progress %s: %w", runID, err)
	}
	p := &suspension.Progress{RunID: runID, Scope: scope, Owner: m.Owner, UpdatedAt: m.UpdatedAt.UTC()}
	if err = runstate.DecodeProgress(m.Progress, p); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: run progress %s: %w", runID, err)
	}
	return p, nil
}
