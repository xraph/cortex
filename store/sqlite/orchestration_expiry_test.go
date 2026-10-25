package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/internal/assurance"
)

func TestOrchestrationExpiryLostAcknowledgmentRetainsCharges(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "expiry.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	s := openTestStore(t, dsn)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "expiry"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	allocation := execution.Resources{Steps: 2, Tokens: 600, ToolCalls: 2, FanOut: 1}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("team")), Scope: scope, Limits: allocation, Participants: map[string]execution.Resources{"worker": allocation}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	require.NoError(t, s.CreateOrchestrationBudget(ctx, policy))
	req := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: policy.ExecutionID, PlanDigest: policy.PreparedDigest, TaskID: id.NewAgentRunID(), Scope: scope, OwnerRef: "worker", Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: policy.Deadline}
	_, err := s.ReserveOrchestrationChild(ctx, req)
	require.NoError(t, err)
	parent := &orchestration.Run{ID: policy.ExecutionID, Status: orchestration.StatusWaiting, ContinuationGeneration: 1, AgentRunIDs: []id.AgentRunID{req.TaskID}, StartedAt: time.Now()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, parent))
	for _, rollback := range []bool{true, false} {
		s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: rollback}).WithClock(teamBudgetClock{at: policy.Deadline})
		expired, expireErr := s.ExpireOrchestrationBudget(ctx, policy.ExecutionID)
		require.ErrorIs(t, expireErr, errTeamBudgetFault)
		require.False(t, expired, "an uncertain transaction cannot grant parent cleanup")
		require.NoError(t, s.Close())
		s = openTestStore(t, dsn)
		snapshot, readErr := s.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
		require.NoError(t, readErr)
		require.Equal(t, !rollback, snapshot.Closed)
		require.Equal(t, allocation, snapshot.Charged)
		require.Equal(t, 1, snapshot.Reservations)
	}
	s.assuranceStore = assurance.New(assuranceDriver{store: s}).WithClock(teamBudgetClock{at: policy.Deadline})
	expired, err := s.ExpireOrchestrationBudget(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.True(t, expired)
	retained, err := s.GetReservation(ctx, req.ID)
	require.NoError(t, err)
	require.Nil(t, retained.Settlement)
	expirer, err := orchestration.NewExpirer(s)
	require.NoError(t, err)
	require.NoError(t, expirer.Expire(ctx, policy.ExecutionID))
	failed, err := s.GetOrchestrationRun(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusFailed, failed.Status)
	require.Equal(t, parent.AgentRunIDs, failed.AgentRunIDs)
	require.ErrorIs(t, s.UpdateOrchestrationRun(ctx, parent), cortex.ErrOrchestrationRunClosed)
}
