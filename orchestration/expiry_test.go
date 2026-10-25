package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type expiryStore struct {
	parent               *Run
	expired              bool
	expireErr, updateErr error
	writes, reads        int
}

func (s *expiryStore) ExpireOrchestrationBudget(context.Context, id.OrchestrationID) (bool, error) {
	return s.expired, s.expireErr
}
func (s *expiryStore) GetOrchestrationRun(context.Context, id.OrchestrationID) (*Run, error) {
	s.reads++
	if s.parent == nil {
		return nil, cortex.ErrOrchestrationRunNotFound
	}
	return s.parent, nil
}
func (s *expiryStore) UpdateOrchestrationRun(_ context.Context, p *Run) error {
	s.writes++
	if s.updateErr != nil {
		return s.updateErr
	}
	detached := *p
	s.parent = &detached
	return nil
}
func TestTeamExpirySealsBeforeParentWriteAndRetainsChildren(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "expiry"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	key := id.NewOrchestrationID()
	child := id.NewAgentRunID()
	for _, status := range []string{StatusRunning, StatusWaiting} {
		s := &expiryStore{parent: &Run{ID: key, Scope: scope, Status: status, ContinuationGeneration: 1, AgentRunIDs: []id.AgentRunID{child}}}
		expirer, err := NewExpirer(s)
		require.NoError(t, err)
		require.NoError(t, expirer.Expire(ctx, key))
		require.Zero(t, s.reads)
		s.expired = true
		s.expireErr = errors.New("closure acknowledgment lost")
		require.Error(t, expirer.Expire(ctx, key))
		require.Zero(t, s.reads)
		s.expireErr = nil
		s.updateErr = errors.New("parent write unavailable")
		require.Error(t, expirer.Expire(ctx, key))
		require.Equal(t, status, s.parent.Status)
		s.updateErr = nil
		require.NoError(t, expirer.Expire(ctx, key))
		require.Equal(t, StatusFailed, s.parent.Status)
		require.NotNil(t, s.parent.CompletedAt)
		require.Equal(t, []id.AgentRunID{child}, s.parent.AgentRunIDs)
		require.NoError(t, expirer.Expire(ctx, key))
		require.Equal(t, 2, s.writes)
	}
}
func TestTeamExpiryPreservesTerminalAndLegacyRecords(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "expiry"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	key := id.NewOrchestrationID()
	for _, parent := range []*Run{nil, {ID: key, Scope: scope, Status: StatusCompleted, ContinuationGeneration: 1, Output: "retained"}, {ID: key, Scope: scope, Status: StatusWaiting}} {
		s := &expiryStore{parent: parent, expired: true}
		expirer, err := NewExpirer(s)
		require.NoError(t, err)
		require.NoError(t, expirer.Expire(ctx, key))
		require.Zero(t, s.writes)
		require.Equal(t, parent, s.parent)
	}
}
