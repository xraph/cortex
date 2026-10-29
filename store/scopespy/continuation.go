package scopespy

import (
	"context"
	"sort"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

var _ run.ContinuationStore = (*Spy)(nil)

// MarkReady applies the real stores' rules to the recorded run and
// suspension: only a paused run with a live suspension becomes ready, and
// the suspension loses its deadline.
func (s *Spy) MarkReady(ctx context.Context, runID id.AgentRunID, ready run.Readiness) (*run.Run, error) {
	s.record(ctx, "MarkReady")
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.State != run.StatePaused || s.claimed[runID] {
		return nil, cortex.ErrNotSuspended
	}
	susp, err := s.findSuspension(runID)
	if err != nil {
		return nil, err
	}
	if susp.ID != ready.SuspensionID {
		return nil, cortex.ErrNotSuspended
	}
	if susp.ExpiresAt != nil && !susp.ExpiresAt.After(time.Now()) {
		return nil, cortex.ErrSuspensionExpired
	}
	saved := ready
	saved.Results = append([]byte(nil), ready.Results...)
	saved.DecidedByPrincipal = append([]byte(nil), ready.DecidedByPrincipal...)
	r.State, r.Ready = run.StateReady, &saved
	r.Revision++
	susp.ExpiresAt = nil
	s.runs[runID] = r
	return &r, nil
}

// HoldLease sets or extends the lease of a running run that nobody else holds.
func (s *Spy) HoldLease(ctx context.Context, runID id.AgentRunID, lease run.Lease) error {
	s.record(ctx, "HoldLease")
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.State != run.StateRunning || lease.Owner == "" || lease.ExpiresAt.IsZero() {
		return cortex.ErrRunLeaseLost
	}
	if r.Lease != nil && r.Lease.Owner != "" && r.Lease.Owner != lease.Owner {
		return cortex.ErrRunLeaseLost
	}
	held := lease
	r.Lease = &held
	s.runs[runID] = r
	return nil
}

// MarkReclaimable takes a running run from a lapsed lease, or from its
// owner when the owner hands it off.
func (s *Spy) MarkReclaimable(ctx context.Context, runID id.AgentRunID, now time.Time, owner string) (*run.Run, error) {
	s.record(ctx, "MarkReclaimable")
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, cortex.ErrRunNotFound
	}
	if r.State != run.StateRunning {
		return nil, cortex.ErrRunConflict
	}
	if owner == "" {
		if r.Lease == nil || r.Lease.ExpiresAt.IsZero() || r.Lease.ExpiresAt.After(now) {
			return nil, cortex.ErrRunLeased
		}
		r.Reclaims++
	} else if r.Lease == nil || r.Lease.Owner != owner {
		return nil, cortex.ErrRunLeaseLost
	}
	r.State, r.Lease = run.StateReclaimable, nil
	r.Revision++
	s.runs[runID] = r
	return &r, nil
}

// ClaimContinuation lets exactly one caller take a ready or reclaimable
// run, and refuses a half lease the way the stores do.
func (s *Spy) ClaimContinuation(ctx context.Context, runID id.AgentRunID, lease run.Lease) (*run.Run, error) {
	s.record(ctx, "ClaimContinuation")
	s.mu.Lock()
	defer s.mu.Unlock()
	if (lease.Owner == "") != lease.ExpiresAt.IsZero() {
		return nil, cortex.ErrRunLeaseLost
	}
	r, ok := s.runs[runID]
	if !ok || (r.State != run.StateReady && r.State != run.StateReclaimable) {
		return nil, cortex.ErrRunNotContinuable
	}
	r.State, r.Ready, r.NotifiedAt, r.Lease = run.StateRunning, nil, nil, nil
	if lease.Owner != "" || !lease.ExpiresAt.IsZero() {
		held := lease
		r.Lease = &held
	}
	r.Revision++
	s.runs[runID] = r
	return &r, nil
}

// ListLapsedAcrossScopes deliberately reads every scope, as the sweeper does.
func (s *Spy) ListLapsedAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*run.Run, error) {
	s.record(ctx, "ListLapsedAcrossScopes")
	return s.listRuns(func(r *run.Run) bool {
		return r.State == run.StateRunning && r.Lease != nil && !r.Lease.ExpiresAt.IsZero() && !r.Lease.ExpiresAt.After(now)
	}, func(a, b *run.Run) bool { return a.Lease.ExpiresAt.Before(b.Lease.ExpiresAt) }, limit), nil
}

// ListContinuableAcrossScopes deliberately reads every scope, as the sweeper does.
func (s *Spy) ListContinuableAcrossScopes(ctx context.Context, notifiedBefore time.Time, limit int) ([]*run.Run, error) {
	s.record(ctx, "ListContinuableAcrossScopes")
	return s.listRuns(func(r *run.Run) bool {
		return (r.State == run.StateReady || r.State == run.StateReclaimable) && (r.NotifiedAt == nil || r.NotifiedAt.Before(notifiedBefore))
	}, func(a, b *run.Run) bool { return a.UpdatedAt.Before(b.UpdatedAt) }, limit), nil
}

// MarkNotified stamps a ready or reclaimable run.
func (s *Spy) MarkNotified(ctx context.Context, runID id.AgentRunID, at time.Time) error {
	s.record(ctx, "MarkNotified")
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || (r.State != run.StateReady && r.State != run.StateReclaimable) {
		return cortex.ErrRunNotContinuable
	}
	notified := at
	r.NotifiedAt = &notified
	s.runs[runID] = r
	return nil
}

// ListOwnerlessAcrossScopes deliberately reads every scope, as the sweeper does.
func (s *Spy) ListOwnerlessAcrossScopes(ctx context.Context, idleBefore time.Time, limit int) ([]*run.Run, error) {
	s.record(ctx, "ListOwnerlessAcrossScopes")
	return s.listRuns(func(r *run.Run) bool {
		return r.State == run.StateRunning && ownerlessIdle(r, idleBefore)
	}, func(a, b *run.Run) bool { return a.UpdatedAt.Before(b.UpdatedAt) }, limit), nil
}

// MarkOwnerlessReclaimable takes a running run nobody holds, once it has
// been idle since idleBefore, as the stores do.
func (s *Spy) MarkOwnerlessReclaimable(ctx context.Context, runID id.AgentRunID, idleBefore time.Time) (*run.Run, error) {
	s.record(ctx, "MarkOwnerlessReclaimable")
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, cortex.ErrRunNotFound
	}
	if r.State != run.StateRunning {
		return nil, cortex.ErrRunConflict
	}
	if !ownerlessIdle(&r, idleBefore) {
		return nil, cortex.ErrRunLeased
	}
	r.State = run.StateReclaimable
	r.Reclaims++
	r.Revision++
	r.UpdatedAt = time.Now().UTC()
	s.runs[runID] = r
	return &r, nil
}

// ownerlessIdle is the stores' ownerless rule: no lease, not a run
// coordination owns, and idle since idleBefore.
func ownerlessIdle(r *run.Run, idleBefore time.Time) bool {
	return r.Lease == nil && !r.Provenance.VerificationRequired && r.Provenance.TaskClaimID == "" && !r.UpdatedAt.After(idleBefore)
}

func (s *Spy) listRuns(keep func(*run.Run) bool, less func(a, b *run.Run) bool, limit int) []*run.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*run.Run
	for _, r := range s.runs {
		if keep(&r) {
			out = append(out, &r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
