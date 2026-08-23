package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// CreateSuspension writes a new suspension document for a run that just
// paused, stamping the scope from the context. The partial unique index
// on run_id (WHERE scope_canon $gt "") is what enforces one suspension
// per run, which is what makes ClaimSuspension a meaningful primitive.
func (s *Store) CreateSuspension(ctx context.Context, susp *suspension.Suspension) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	susp.CreatedAt = t
	susp.UpdatedAt = t
	susp.Scope = scope
	m := suspensionToModel(susp)

	if _, err := s.mdb.NewInsert(m).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create suspension: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create suspension: %w", err)
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
	m, err := s.findSuspension(ctx, scope, runID)
	if err != nil {
		return nil, err
	}
	return suspensionFromModel(m)
}

// ClaimSuspension is the concurrency primitive for resume: it performs
// the run's paused-to-running transition and the suspension read as ONE
// operation, so two concurrent resumes cannot both proceed.
//
// The transition is a single FindOneAndUpdate whose filter carries
// state = "paused". Mongo applies a FindOneAndUpdate's match and its
// write atomically against the document, so of two concurrent claims
// exactly one can see a paused run: the loser matches nothing, gets
// ErrNoDocuments back, and returns cortex.ErrNotSuspended rather than
// the suspension it lost the race for.
//
// A read followed by a separate write is the exact race this method
// exists to close, so the no-document check below is load-bearing:
// without it both callers would return the same suspension and both
// would go on to resume the same run.
//
// The expiry check sits in front of that write for the same reason the
// SQL backends fold it into their UPDATE's condition: a caller that
// claimed first and rejected an expired suspension second would already
// have flipped the run to running, leaving it stuck there with a
// suspension nobody can claim any more. Mongo cannot express the
// deadline as part of the FindOneAndUpdate's own filter, because the
// deadline lives on the suspension document and the transition writes
// the run one, and this store deliberately does not require a replica
// set to wrap the two in a transaction. Ordering the check before the
// write buys the property that actually matters: an expired resume never
// moves the run.
func (s *Store) ClaimSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}

	// Read first so a run with no suspension at all, or one past its
	// deadline, fails before the state transition rather than leaving a
	// run flipped to running with nothing to continue from. This read is
	// not what serializes the claim; the FindOneAndUpdate below is.
	m, err := s.findSuspension(ctx, scope, runID)
	if err != nil {
		return nil, err
	}
	if m.ExpiresAt != nil && !m.ExpiresAt.After(now()) {
		return nil, cortex.ErrSuspensionExpired
	}

	filter := bson.M{"_id": runID.String(), "state": string(run.StatePaused)}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	update := bson.M{"$set": bson.M{
		"state":      string(run.StateRunning),
		"updated_at": now(),
	}}

	res := s.mdb.Collection(colRuns).FindOneAndUpdate(ctx, filter, update)
	if err := res.Err(); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, fmt.Errorf("cortex/mongo: claim suspension: %w", err)
	}

	return suspensionFromModel(m)
}

// DeleteSuspension removes a run's suspension within the caller's scope.
func (s *Store) DeleteSuspension(ctx context.Context, runID id.AgentRunID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"run_id": runID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*suspensionModel)(nil)).Filter(filter).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete suspension: %w", err)
	}
	if res.DeletedCount() == 0 {
		return cortex.ErrNotSuspended
	}
	return nil
}

// ListExpired returns suspensions whose deadline has passed, oldest
// first, within the caller's scope.
func (s *Store) ListExpired(ctx context.Context, at time.Time, limit int) ([]*suspension.Suspension, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []suspensionModel

	f := bson.M{"expires_at": bson.M{"$ne": nil, "$lte": at.UTC()}}
	for k, v := range scopeFilter(scope, false) {
		f[k] = v
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "expires_at", Value: 1}})
	if limit > 0 {
		q = q.Limit(int64(limit))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list expired suspensions: %w", err)
	}

	result := make([]*suspension.Suspension, len(models))
	for i := range models {
		susp, convErr := suspensionFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = susp
	}
	return result, nil
}

// findSuspension reads one run's suspension document under scope. It
// exists so GetSuspension and ClaimSuspension apply identical filters: a
// claim that read more broadly than a get would be a cross-scope hole
// nobody would spot from either call site alone.
func (s *Store) findSuspension(ctx context.Context, scope cortex.Scope, runID id.AgentRunID) (*suspensionModel, error) {
	var m suspensionModel

	filter := bson.M{"run_id": runID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, fmt.Errorf("cortex/mongo: get suspension: %w", err)
	}
	return &m, nil
}
