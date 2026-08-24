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
// The conditional UPDATE and the suspension read share a transaction.
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
//
// The expiry predicate lives in that same UPDATE rather than in a check
// the caller runs afterwards, and the difference matters. A caller that
// claimed first and rejected an expired suspension second would already
// have flipped the run to running, leaving it stuck there with a
// suspension nobody can claim any more: strictly worse than the
// expired-but-paused state the sweeper exists to clean up. Conditioning
// the write on an unexpired suspension means an expired resume never
// moves the run at all.
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

	now := time.Now().UTC()
	q := tx.NewUpdate((*runModel)(nil)).
		Set("state = ?", string(run.StateRunning)).
		Set("updated_at = ?", now).
		Where("id = ?", runID.String()).
		Where("state = ?", string(run.StatePaused))
	q = q.Where(unexpiredSuspensionExists(scope), suspensionDeadlineArgs(scope, runID, now)...)
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
		return nil, fmt.Errorf("cortex: commit claim suspension: %w", err)
	}
	return suspensionFromModel(m)
}

// ClaimExpiredSuspension is ClaimSuspension with the deadline predicate
// inverted, for the sweeper. See the interface doc for why the two
// partition the same row rather than overlapping on it.
//
// The rowcount check carries more weight here than it does above,
// because the sweeper's next move is to fail the run. Without it a sweep
// would mark failed a run some resume had already taken and was part way
// through continuing, and the run would keep executing under a terminal
// state nothing put it in.
//
// Zero rows is reported as ErrNotSuspended without a second read to
// classify it. The three ways to get here (the row is gone, the run is
// no longer paused, the deadline moved) all mean the same thing to the
// only caller: somebody else owns this run, leave it alone.
func (s *Store) ClaimExpiredSuspension(ctx context.Context, runID id.AgentRunID, now time.Time) (*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	tx, err := s.pgdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cortex: begin claim expired suspension: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	at := now.UTC()
	q := tx.NewUpdate((*runModel)(nil)).
		Set("state = ?", string(run.StateRunning)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", runID.String()).
		Where("state = ?", string(run.StatePaused))
	q = q.Where(expiredSuspensionExists(scope), suspensionDeadlineArgs(scope, runID, at)...)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: claim expired suspension: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("cortex: claim expired suspension rows affected: %w", err)
	}
	if n == 0 {
		return nil, cortex.ErrNotSuspended
	}

	m, err := selectSuspensionRow(ctx, tx, scope, runID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cortex: commit claim expired suspension: %w", err)
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

// expiredSuspensionExists is the sweeper's half of the same split: the
// suspension is there, it carries a deadline, and the deadline has
// passed. It is the exact complement of unexpiredSuspensionExists over
// rows that have a suspension at all, so no row can satisfy both and a
// resume and a sweep can never each believe they own the same run.
func expiredSuspensionExists(scope cortex.Scope) string {
	q := "EXISTS (SELECT 1 FROM cortex_suspensions cs WHERE cs.run_id = ?" +
		" AND cs.expires_at IS NOT NULL AND cs.expires_at <= ?"
	for _, p := range scopePredicates(scope, false) {
		q += " AND cs." + p.Column + " = ?"
	}
	return q + ")"
}

// suspensionDeadlineArgs supplies the bind values for either deadline
// predicate above, in the order their placeholders appear. The two share
// it because they take the same three things in the same order, and a
// second copy would be a second place to get the ordering wrong.
func suspensionDeadlineArgs(scope cortex.Scope, runID id.AgentRunID, now time.Time) []any {
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
	return s.expiredSuspensions(ctx, scopePredicates(scope, false), now, limit)
}

// ListExpiredAcrossScopes reads expired suspensions from every scope,
// with no scope guard and no scope predicates. That is deliberate and it
// is why the method is named for it: the expiry sweeper runs without a
// request scope, and a sweep that filtered would silently sweep nothing.
// See the suspension.Store doc for the full argument.
func (s *Store) ListExpiredAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*suspension.Suspension, error) {
	return s.expiredSuspensions(ctx, nil, now, limit)
}

// expiredSuspensions is the query both list methods run, differing only
// in whether they hand it scope predicates. Sharing it keeps the
// deadline condition and the ordering identical, so the sweeper can
// never see a row a scoped ListExpired would have hidden for any reason
// other than scope.
func (s *Store) expiredSuspensions(ctx context.Context, preds []scopePredicate, now time.Time, limit int) ([]*suspension.Suspension, error) {
	var models []suspensionModel
	q := s.pgdb.NewSelect(&models).
		Where("expires_at IS NOT NULL").
		Where("expires_at <= ?", now.UTC()).
		OrderExpr("expires_at ASC")
	for _, p := range preds {
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
