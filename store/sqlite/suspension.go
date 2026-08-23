package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/grove/drivers/sqlitedriver"

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
	if _, err := s.sdb.NewInsert(m).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create suspension: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create suspension: %w", err)
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
	m, err := selectSuspensionRow(ctx, s.sdb, scope, runID)
	if err != nil {
		return nil, err
	}
	return suspensionFromModel(m)
}

// suspensionSelector is the sliver of grove's query surface both a
// connection (*sqlitedriver.SqliteDB) and a transaction (*sqlitedriver.SqliteTx) expose,
// so selectSuspensionRow can serve GetSuspension from the pool and
// ClaimSuspension from inside its transaction without either one
// restating the scope predicates.
type suspensionSelector interface {
	NewSelect(model ...any) *sqlitedriver.SelectQuery
}

// ClaimSuspension is the concurrency primitive for resume: it performs
// the run's paused-to-running transition and the suspension read as ONE
// operation, so two concurrent resumes cannot both proceed.
//
// The conditional UPDATE and the suspension read share a transaction.
// SQLite serializes write transactions outright, so the second caller's
// UPDATE only runs once the first has committed, and its own
// state = 'paused' predicate then matches nothing. Zero rows affected
// means someone else won the race, and the loser gets
// cortex.ErrNotSuspended rather than the suspension it lost.
//
// A read followed by a separate write is the exact race this method
// exists to close, so the rowcount check below is load-bearing: without
// it both callers would return the same suspension and both would go on
// to resume the same run.
//
// The expiry predicate lives in that same UPDATE rather than in a check
// the caller runs afterwards. A caller that claimed first and rejected
// an expired suspension second would already have flipped the run to
// running, leaving it stuck there with a suspension nobody can claim any
// more: strictly worse than the expired-but-paused state the sweeper
// exists to clean up.
//
// Issuing the UPDATE first is also what keeps a loser's error readable
// here. A transaction that opens with a SELECT and only later writes
// starts as a read and upgrades, and on a database with more than one
// open connection the upgrade is what surfaces a raw SQLITE_BUSY instead
// of ErrNotSuspended. Writing first means the transaction takes its
// write lock up front and the loser waits for it rather than colliding
// with it.
func (s *Store) ClaimSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	tx, err := s.sdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: begin claim suspension: %w", err)
	}
	// After a successful Commit, Rollback is a documented no-op; before
	// one, its error can't be acted on any further than the failure that
	// triggered this defer already is.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	now := time.Now().UTC()
	q := tx.NewUpdate((*runModel)(nil)).
		Set("state = ?", string(run.StateRunning)).
		Set("updated_at = ?", now).
		Where("id = ?", runID.String()).
		Where("state = ?", string(run.StatePaused))
	q = q.Where(unexpiredSuspensionExists(scope), unexpiredSuspensionArgs(scope, runID, now)...)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: claim suspension: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return nil, fmt.Errorf("cortex/sqlite: claim suspension rows affected: %w", rowsErr)
	}
	if n == 0 {
		// The claim already lost, so this extra read costs nothing on
		// any path that succeeds. It is the only way to tell the caller
		// which of the two failures it hit.
		return nil, classifyFailedClaim(ctx, tx, scope, runID, now)
	}

	m, err := selectSuspensionRow(ctx, tx, scope, runID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: commit claim suspension: %w", err)
	}
	return suspensionFromModel(m)
}

// unexpiredSuspensionExists is the claim's expiry predicate. It is a
// subquery rather than a join because the state transition it guards
// writes to cortex_runs while the deadline lives on cortex_suspensions,
// and it repeats the scope columns so a claim can never read a
// suspension the caller's own GetSuspension would refuse.
func unexpiredSuspensionExists(scope cortex.Scope) string {
	q := "EXISTS (SELECT 1 FROM cortex_suspensions cs WHERE cs.run_id = ?" +
		" AND (cs.expires_at IS NULL OR cs.expires_at > ?)"
	for _, p := range scopePredicates(scope, false) {
		q += " AND cs." + p.Column + " = ?"
	}
	return q + ")"
}

// unexpiredSuspensionArgs supplies unexpiredSuspensionExists's bind
// values in the order its placeholders appear.
func unexpiredSuspensionArgs(scope cortex.Scope, runID id.AgentRunID, now time.Time) []any {
	preds := scopePredicates(scope, false)
	args := make([]any, 0, len(preds)+2)
	args = append(args, runID.String(), now)
	for _, p := range preds {
		args = append(args, p.Value)
	}
	return args
}

// classifyFailedClaim says why a claim matched no run. A missing
// suspension (or one another caller already claimed, which leaves the
// run no longer paused) is ErrNotSuspended; a suspension that is still
// there but past its deadline is ErrSuspensionExpired, which tells the
// caller the difference between "try again" and "too late".
func classifyFailedClaim(ctx context.Context, q suspensionSelector, scope cortex.Scope, runID id.AgentRunID, now time.Time) error {
	m, err := selectSuspensionRow(ctx, q, scope, runID)
	if err != nil {
		return err
	}
	if m.ExpiresAt != nil && !m.ExpiresAt.After(now) {
		return cortex.ErrSuspensionExpired
	}
	return cortex.ErrNotSuspended
}

// DeleteSuspension removes a run's suspension within the caller's scope.
func (s *Store) DeleteSuspension(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*suspensionModel)(nil)).Where("run_id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete suspension: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete suspension rows affected: %w", rowsErr)
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
	q := s.sdb.NewSelect(&models).
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
		return nil, fmt.Errorf("cortex/sqlite: list expired suspensions: %w", err)
	}
	result := make([]*suspension.Suspension, len(models))
	for i := range models {
		susp, err := suspensionFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex/sqlite: list expired suspensions: %w", err)
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
		if isNoRows(err) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, fmt.Errorf("cortex/sqlite: get suspension: %w", err)
	}
	return m, nil
}
