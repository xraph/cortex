package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// CreateSuspension writes a new suspension row for a run that just
// paused, stamping the scope from the context. The partial
// UNIQUE (run_id) WHERE the scope is non-empty is what enforces one
// suspension per run, which is what makes ClaimSuspension a meaningful
// primitive.
func (s *Store) CreateSuspension(ctx context.Context, susp *suspension.Suspension) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	susp.CreatedAt = now
	susp.UpdatedAt = now
	susp.Scope = scope
	m := suspensionToModel(susp)
	if _, err := s.pgdb.NewInsert(m).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create suspension: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create suspension: %w", err)
	}
	return nil
}

// GetSuspension reads a run's suspension within the caller's scope
// without changing anything. Callers that intend to resume must use
// ClaimSuspension instead.
func (s *Store) GetSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m, err := selectSuspensionRow(ctx, s.pgdb, scope, runID)
	if err != nil {
		return nil, err
	}
	return suspensionFromModel(m)
}

// suspensionSelector is the sliver of grove's query surface both a
// connection (*pgdriver.PgDB) and a transaction (*pgdriver.PgTx) expose,
// so selectSuspensionRow can serve GetSuspension from the pool and
// ClaimSuspension from inside its transaction without either one
// restating the scope predicates.
type suspensionSelector interface {
	NewSelect(model ...any) *pgdriver.SelectQuery
}

// ClaimSuspension is the concurrency primitive for resume: it performs
// the run's paused-to-running transition and the suspension read as ONE
// operation, so two concurrent resumes cannot both proceed.
//
// The suspension read and the conditional UPDATE share a transaction.
// The UPDATE carries its own state = 'paused' predicate, so under READ
// COMMITTED a second caller blocks on the row lock the first holds, then
// re-evaluates that predicate against the committed row and matches
// nothing. Zero rows affected means someone else won the race, and the
// loser gets cortex.ErrNotSuspended rather than the suspension it lost.
//
// A read followed by a separate write is the exact race this method
// exists to close, so the rowcount check below is load-bearing: without
// it both callers would return the same suspension and both would go on
// to resume the same run.
func (s *Store) ClaimSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	tx, err := s.pgdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cortex: begin claim suspension: %w", err)
	}
	// After a successful Commit, Rollback is a documented no-op; before
	// one, its error can't be acted on any further than the failure that
	// triggered this defer already is.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	// Read first so a run with no suspension at all fails before the
	// state transition, rather than leaving a run flipped to running with
	// nothing to continue from.
	m, err := selectSuspensionRow(ctx, tx, scope, runID)
	if err != nil {
		return nil, err
	}

	q := tx.NewUpdate((*runModel)(nil)).
		Set("state = ?", string(run.StateRunning)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", runID.String()).
		Where("state = ?", string(run.StatePaused))
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: claim suspension: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("cortex: claim suspension rows affected: %w", err)
	}
	if n == 0 {
		return nil, cortex.ErrNotSuspended
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cortex: commit claim suspension: %w", err)
	}
	return suspensionFromModel(m)
}

// DeleteSuspension removes a run's suspension within the caller's scope.
func (s *Store) DeleteSuspension(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.pgdb.NewDelete((*suspensionModel)(nil)).Where("run_id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete suspension: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: delete suspension rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrNotSuspended
	}
	return nil
}

// ListExpired returns suspensions whose deadline has passed, oldest
// first, within the caller's scope.
func (s *Store) ListExpired(ctx context.Context, now time.Time, limit int) ([]*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []suspensionModel
	q := s.pgdb.NewSelect(&models).
		Where("expires_at IS NOT NULL").
		Where("expires_at <= ?", now.UTC()).
		OrderExpr("expires_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list expired suspensions: %w", err)
	}
	result := make([]*suspension.Suspension, len(models))
	for i := range models {
		susp, err := suspensionFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list expired suspensions: %w", err)
		}
		result[i] = susp
	}
	return result, nil
}

// selectSuspensionRow reads one run's suspension row under scope. It
// exists so GetSuspension and ClaimSuspension apply identical
// predicates: a claim that read more broadly than a get would be a
// cross-scope hole nobody would spot from either call site alone.
func selectSuspensionRow(ctx context.Context, q suspensionSelector, scope cortex.Scope, runID id.AgentRunID) (*suspensionModel, error) {
	m := new(suspensionModel)
	sel := q.NewSelect(m).Where("run_id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		sel = sel.Where(p.Column+" = ?", p.Value)
	}
	if err := sel.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, fmt.Errorf("cortex: get suspension: %w", err)
	}
	return m, nil
}
