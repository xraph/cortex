package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
	"github.com/xraph/cortex/suspension"
)

// MarkReady, ClaimContinuation and MarkReclaimable come from the shared
// assurance repository, which runs them in a TransactRun transaction, so
// Mongo gives them the same guarantee as the SQL stores. The methods below
// touch one run document each and need no transaction.
var _ run.ContinuationStore = (*Store)(nil)

// SetRunContinuation writes the continuation fields inside TransactRun.
func (d assuranceDriver) SetRunContinuation(ctx context.Context, r *run.Run, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	revision, err := runstate.NextRevision(r.Revision)
	if err != nil {
		return err
	}
	owner, expiresAt := runstate.LeaseColumns(r.Lease)
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"], filter["revision"] = r.ID.String(), scope.Canonical(), r.Revision
	set := bson.M{
		"revision":         revision,
		"state":            runstate.Encode(r),
		"lease_owner":      owner,
		"lease_expires_at": expiresAt,
		"ready":            readyToDocument(r.Ready),
		"reclaim_count":    r.Reclaims,
		"notified_at":      runstate.UTCTime(r.NotifiedAt),
		"updated_at":       at,
	}
	result, err := d.store.mdb.Collection(colRuns).UpdateOne(ctx, filter, bson.M{"$set": set})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}

// ClearSuspensionDeadline drops the deadline of the suspension LockSuspension holds.
func (d assuranceDriver) ClearSuspensionDeadline(ctx context.Context, runID id.AgentRunID) error {
	session := mongo.SessionFromContext(ctx)
	if !d.resultTransaction || session == nil || !session.TransactionRunning() {
		return suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	filter := scopeFilter(scope, true)
	filter["run_id"], filter["scope_canon"] = runID.String(), scope.Canonical()
	result, err := d.store.mdb.Collection(colSuspensions).UpdateOne(ctx, filter, bson.M{"$unset": bson.M{"expires_at": ""}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return cortex.ErrNotSuspended
	}
	return nil
}

// HoldLease sets or extends a running run's lease in one conditional
// update. The owner filter is what keeps two workers from both holding the
// run: only an unowned lease or the caller's own matches.
func (s *Store) HoldLease(ctx context.Context, runID id.AgentRunID, lease run.Lease) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if lease.Owner == "" || lease.ExpiresAt.IsZero() {
		return cortex.ErrRunLeaseLost
	}
	filter := scopeFilter(scope, true)
	// A run written before leases existed has no lease_owner field; nil
	// matches it as unowned.
	filter["_id"], filter["state"] = runID.String(), string(run.StateRunning)
	filter["lease_owner"] = bson.M{"$in": bson.A{"", nil, lease.Owner}}
	update := bson.M{"$set": bson.M{"lease_owner": lease.Owner, "lease_expires_at": lease.ExpiresAt.UTC()}}
	result, err := s.mdb.Collection(colRuns).UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("cortex/mongo: hold run lease: %w", err)
	}
	if result.MatchedCount == 0 {
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
	filter := scopeFilter(scope, true)
	filter["_id"] = runID.String()
	filter["state"] = bson.M{"$in": bson.A{string(run.StateReady), string(run.StateReclaimable)}}
	result, err := s.mdb.Collection(colRuns).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"notified_at": at.UTC()}})
	if err != nil {
		return fmt.Errorf("cortex/mongo: mark run notified: %w", err)
	}
	if result.MatchedCount == 0 {
		return cortex.ErrRunNotContinuable
	}
	return nil
}

// ListLapsedAcrossScopes reads running runs whose lease has lapsed from
// every scope, with no scope guard and no scope filter. That is deliberate
// and it is why the method is named for it: the sweeper runs without a
// request scope, and a sweep that filtered would silently sweep nothing.
// It is for the sweeper only, which acts on each run under that run's own
// scope.
func (s *Store) ListLapsedAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*run.Run, error) {
	filter := bson.M{"state": string(run.StateRunning), "lease_expires_at": bson.M{"$ne": nil, "$lte": now.UTC()}}
	sort := bson.D{{Key: "lease_expires_at", Value: 1}, {Key: "_id", Value: 1}}
	return s.findRuns(ctx, filter, sort, limit, "list lapsed runs")
}

// ListContinuableAcrossScopes reads ready and reclaimable runs nobody was
// told about since notifiedBefore, from every scope, with no scope guard
// and no scope filter. Like ListLapsedAcrossScopes it deliberately bypasses
// scope, for the sweeper only.
func (s *Store) ListContinuableAcrossScopes(ctx context.Context, notifiedBefore time.Time, limit int) ([]*run.Run, error) {
	filter := bson.M{
		"state": bson.M{"$in": bson.A{string(run.StateReady), string(run.StateReclaimable)}},
		"$or":   bson.A{bson.M{"notified_at": nil}, bson.M{"notified_at": bson.M{"$lt": notifiedBefore.UTC()}}},
	}
	sort := bson.D{{Key: "updated_at", Value: 1}, {Key: "_id", Value: 1}}
	return s.findRuns(ctx, filter, sort, limit, "list continuable runs")
}

func (s *Store) findRuns(ctx context.Context, filter bson.M, sort bson.D, limit int, operation string) ([]*run.Run, error) {
	opts := options.Find().SetSort(sort)
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cursor, err := s.mdb.Collection(colRuns).Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: %s: %w", operation, err)
	}
	defer cursor.Close(ctx)
	var models []runModel
	if err = cursor.All(ctx, &models); err != nil {
		return nil, fmt.Errorf("cortex/mongo: %s: %w", operation, err)
	}
	result := make([]*run.Run, len(models))
	for i := range models {
		r, convErr := runFromModel(&models[i])
		if convErr != nil {
			return nil, fmt.Errorf("cortex/mongo: %s: %w", operation, convErr)
		}
		result[i] = r
	}
	return result, nil
}
