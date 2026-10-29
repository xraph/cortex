package scopespy

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

var _ suspension.ProgressStore = (*Spy)(nil)

// SaveProgress applies the real stores' fence: only the owner of a running
// run's lease may save its snapshot, and an empty owner never matches. Like
// the real stores it sets p.Scope and p.UpdatedAt to what it kept.
func (s *Spy) SaveProgress(ctx context.Context, p *suspension.Progress) error {
	s.record(ctx, "SaveProgress")
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil || p.Owner == "" {
		return cortex.ErrRunLeaseLost
	}
	r, ok := s.runs[p.RunID]
	if !ok || r.State != run.StateRunning || r.Lease == nil || r.Lease.Owner != p.Owner {
		return cortex.ErrRunLeaseLost
	}
	if s.progress == nil {
		s.progress = make(map[id.AgentRunID]suspension.Progress)
	}
	saved := cloneProgress(*p)
	saved.Scope = cortex.ScopeFromContext(ctx)
	// Millisecond precision, the same truncation the real stores apply.
	saved.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	s.progress[p.RunID] = saved
	p.Scope = saved.Scope
	p.UpdatedAt = saved.UpdatedAt
	return nil
}

// GetProgress reads back a snapshot saved at the exact same scope.
func (s *Spy) GetProgress(ctx context.Context, runID id.AgentRunID) (*suspension.Progress, error) {
	s.record(ctx, "GetProgress")
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, ok := s.progress[runID]
	if !ok || saved.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrProgressNotFound
	}
	out := cloneProgress(saved)
	return &out, nil
}

// DeleteProgress removes a snapshot saved at the exact same scope; a run
// with none is not an error.
func (s *Spy) DeleteProgress(ctx context.Context, runID id.AgentRunID) error {
	s.record(ctx, "DeleteProgress")
	s.mu.Lock()
	defer s.mu.Unlock()
	if saved, ok := s.progress[runID]; ok && saved.Scope.Canonical() == cortex.ScopeFromContext(ctx).Canonical() {
		delete(s.progress, runID)
	}
	return nil
}

func cloneProgress(p suspension.Progress) suspension.Progress {
	p.Cont.Prepared = append([]byte(nil), p.Cont.Prepared...)
	p.Started = append([]suspension.PendingCall(nil), p.Started...)
	p.Done = append([]suspension.ProgressResult(nil), p.Done...)
	if p.Ending != nil {
		ending := *p.Ending
		p.Ending = &ending
	}
	return p
}
