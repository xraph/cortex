package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
)

// MarkReady, ClaimContinuation and MarkReclaimable come from the shared
// assurance repository, which runs them under TransactRun. The methods
// below touch one run row each and need no transaction.
var _ run.ContinuationStore = (*Store)(nil)

// SetRunContinuation writes the continuation columns inside TransactRun.
func (d assuranceDriver) SetRunContinuation(ctx context.Context, r *run.Run, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	revision, err := runstate.NextRevision(r.Revision)
	if err != nil {
		return err
	}
	ready, err := runstate.EncodeReady(r.Ready)
	if err != nil {
		return err
	}
	m := runToModel(r)
	m.Revision, m.UpdatedAt, m.Ready = revision, at, ready
	q := d.queries().NewUpdate(m).Column("revision", "state", "lease_owner", "lease_expires_at", "ready", "reclaim_count", "notified_at", "updated_at").WherePK().Where("revision = ?", r.Revision)
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}

// ClearSuspensionDeadline drops the deadline of the suspension LockSuspension holds.
func (d assuranceDriver) ClearSuspensionDeadline(ctx context.Context, runID id.AgentRunID) error {
	if d.transaction == nil {
		return suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*suspensionModel)(nil)).Set("expires_at = NULL").Where("run_id = ?", runID.String())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return cortex.ErrNotSuspended
	}
	return nil
}

// HoldLease sets or extends a running run's lease in one conditional
// UPDATE. The owner predicate is what keeps two workers from both holding
// the run: only an unowned lease or the caller's own matches.
func (s *Store) HoldLease(ctx context.Context, runID id.AgentRunID, lease run.Lease) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if lease.Owner == "" || lease.ExpiresAt.IsZero() {
		return cortex.ErrRunLeaseLost
	}
	q := s.pgdb.NewUpdate((*runModel)(nil)).
		Set("lease_owner = ?", lease.Owner).
		Set("lease_expires_at = ?", lease.ExpiresAt.UTC()).
		Where("id = ?", runID.String()).
		Where("state = ?", string(run.StateRunning)).
		Where("lease_owner IN (?, ?)", "", lease.Owner)
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: hold run lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: hold run lease rows affected: %w", err)
	}
	if count == 0 {
		return cortex.ErrRunLeaseLost
	}
	return nil
}

// MarkNotified records when the host was last told about a waiting run.
func (s *Store) MarkNotified(ctx context.Context, runID id.AgentRunID, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := s.pgdb.NewUpdate((*runModel)(nil)).
		Set("notified_at = ?", at.UTC()).
		Where("id = ?", runID.String()).
		Where("state IN (?, ?)", string(run.StateReady), string(run.StateReclaimable))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: mark run notified: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: mark run notified rows affected: %w", err)
	}
	if count == 0 {
		return cortex.ErrRunNotContinuable
	}
	return nil
}

// ListLapsedAcrossScopes reads running runs whose lease has lapsed from
// every scope, with no scope guard and no scope predicates. That is
// deliberate and it is why the method is named for it: the sweeper runs
// without a request scope, and a sweep that filtered would silently sweep
// nothing. It is for the sweeper only, which acts on each run under that
// run's own scope.
func (s *Store) ListLapsedAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*run.Run, error) {
	var models []runModel
	q := s.pgdb.NewSelect(&models).
		Where("state = ?", string(run.StateRunning)).
		Where("lease_expires_at IS NOT NULL").
		Where("lease_expires_at <= ?", now.UTC()).
		OrderExpr("lease_expires_at ASC, id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list lapsed runs: %w", err)
	}
	return runsFromModels(models)
}

// ListContinuableAcrossScopes reads ready and reclaimable runs nobody was
// told about since notifiedBefore, from every scope, with no scope guard
// and no scope predicates. Like ListLapsedAcrossScopes it deliberately
// bypasses scope, for the sweeper only.
func (s *Store) ListContinuableAcrossScopes(ctx context.Context, notifiedBefore time.Time, limit int) ([]*run.Run, error) {
	var models []runModel
	q := s.pgdb.NewSelect(&models).
		Where("state IN (?, ?)", string(run.StateReady), string(run.StateReclaimable)).
		Where("(notified_at IS NULL OR notified_at < ?)", notifiedBefore.UTC()).
		OrderExpr("updated_at ASC, id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list continuable runs: %w", err)
	}
	return runsFromModels(models)
}

func runsFromModels(models []runModel) ([]*run.Run, error) {
	result := make([]*run.Run, len(models))
	for i := range models {
		r, err := runFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list runs: %w", err)
		}
		result[i] = r
	}
	return result, nil
}
