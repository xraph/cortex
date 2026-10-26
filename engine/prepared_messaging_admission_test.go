package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
)

// Hold the first b run after its reservation is linked but before CreateRun
// writes it. Let a finish, forcing its messaging scan to observe that gap.
type pendingTeamChildStore struct {
	*sqlite.Store
	a, b           id.AgentID
	mu             sync.Mutex
	pending        id.AgentRunID
	ready, release chan struct{}
	releaseOnce    sync.Once
	missing        atomic.Int32
}

func (s *pendingTeamChildStore) CreateRun(ctx context.Context, r *run.Run) error {
	if r.AgentID == s.b {
		s.mu.Lock()
		first := s.pending.IsNil()
		if first {
			s.pending = r.ID
		}
		s.mu.Unlock()
		if first {
			close(s.ready)
			select {
			case <-s.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if r.AgentID == s.a {
		select {
		case <-s.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.CreateRun(ctx, r)
}

func (s *pendingTeamChildStore) GetRun(ctx context.Context, key id.AgentRunID) (*run.Run, error) {
	r, err := s.Store.GetRun(ctx, key)
	s.mu.Lock()
	pending := key == s.pending
	s.mu.Unlock()
	if pending && errors.Is(err, cortex.ErrRunNotFound) {
		s.missing.Add(1)
		s.releaseOnce.Do(func() { close(s.release) })
	}
	return r, err
}

func TestPreparedTeamMessagingWaitsForReservedChildCreation(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategyParallel)
	a, err := f.s.GetByName(f.ctx, "a")
	require.NoError(t, err)
	b, err := f.s.GetByName(f.ctx, "b")
	require.NoError(t, err)
	s := &pendingTeamChildStore{Store: f.s, a: a.ID, b: b.ID, ready: make(chan struct{}), release: make(chan struct{})}
	refreshPreparedTeam(t, &f, engine.WithStore(s), engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	parent, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Positive(t, s.missing.Load(), "fixture must observe a reserved but unwritten child")
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, parent.Status)
	require.Len(t, parent.AgentRunIDs, 3)
	require.Equal(t, 3, f.client.calls, "each worker and aggregator must execute once")
}
