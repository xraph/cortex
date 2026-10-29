package scopespy

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

var _ suspension.LeasedClaimStore = (*Spy)(nil)

// ClaimSuspensionLeased claims the pinned pause once, takes the lease and
// keeps the first snapshot, with the real stores' refusals.
func (s *Spy) ClaimSuspensionLeased(ctx context.Context, claim suspension.LeasedClaim) (*suspension.Suspension, error) {
	s.record(ctx, "ClaimSuspensionLeased")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := spyLeasedClaim(claim.Version.RunID, claim.Lease, claim.First); err != nil {
		return nil, err
	}
	r, ok := s.runs[claim.Version.RunID]
	if !ok || r.State != run.StatePaused || s.claimed[claim.Version.RunID] {
		return nil, cortex.ErrNotSuspended
	}
	if r.Revision != claim.Version.RunRevision {
		return nil, cortex.ErrRunConflict
	}
	susp, err := s.findSuspension(claim.Version.RunID)
	if err != nil || susp.ID != claim.Version.SuspensionID {
		return nil, cortex.ErrNotSuspended
	}
	now := time.Now().UTC()
	if !claim.ExpiredBy.IsZero() {
		if susp.ExpiresAt == nil || susp.ExpiresAt.After(claim.ExpiredBy) {
			return nil, cortex.ErrNotSuspended
		}
	} else if susp.ExpiresAt != nil && !susp.ExpiresAt.After(now) {
		return nil, cortex.ErrSuspensionExpired
	}
	s.claimed[claim.Version.RunID] = true
	held := claim.Lease
	r.State, r.Lease, r.UpdatedAt = run.StateRunning, &held, now
	r.Revision++
	s.runs[r.ID] = r
	s.keepFirst(ctx, claim.First)
	claimed := r
	out := *susp
	out.ClaimedRun = &claimed
	return &out, nil
}

// ClaimContinuationLeased is ClaimContinuation under a lease that must be
// set, keeping the first snapshot.
func (s *Spy) ClaimContinuationLeased(ctx context.Context, runID id.AgentRunID, lease run.Lease, first *suspension.Progress) (*run.Run, error) {
	s.record(ctx, "ClaimContinuationLeased")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := spyLeasedClaim(runID, lease, first); err != nil {
		return nil, err
	}
	r, ok := s.runs[runID]
	if !ok || (r.State != run.StateReady && r.State != run.StateReclaimable) {
		return nil, cortex.ErrRunNotContinuable
	}
	held := lease
	r.State, r.Ready, r.NotifiedAt, r.Lease = run.StateRunning, nil, nil, &held
	r.Revision++
	r.UpdatedAt = time.Now().UTC()
	s.runs[runID] = r
	s.keepFirst(ctx, first)
	return &r, nil
}

// StartResumeLeased records its call. The spy keeps no resume admissions.
func (s *Spy) StartResumeLeased(ctx context.Context, _ id.AgentRunID, _ id.SuspensionID, _ string, _ run.Lease, _ *suspension.Progress) (suspension.ResumeStart, error) {
	s.record(ctx, "StartResumeLeased")
	return suspension.ResumeStart{}, suspension.ErrResumeAdmissionUnavailable
}

func spyLeasedClaim(runID id.AgentRunID, lease run.Lease, first *suspension.Progress) error {
	if lease.Owner == "" || lease.ExpiresAt.IsZero() {
		return cortex.ErrRunLeaseLost
	}
	if first != nil && (first.Owner != lease.Owner || first.RunID != runID) {
		return cortex.ErrRunLeaseLost
	}
	return nil
}

// keepFirst stores a claim's first snapshot; the caller holds s.mu.
func (s *Spy) keepFirst(ctx context.Context, first *suspension.Progress) {
	if first == nil {
		return
	}
	if s.progress == nil {
		s.progress = make(map[id.AgentRunID]suspension.Progress)
	}
	saved := cloneProgress(*first)
	saved.Scope = cortex.ScopeFromContext(ctx)
	saved.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	s.progress[first.RunID] = saved
	first.Scope, first.UpdatedAt = saved.Scope, saved.UpdatedAt
}
