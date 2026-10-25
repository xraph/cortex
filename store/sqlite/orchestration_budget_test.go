package sqlite

import (
	"context"
	"errors"
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

var errTeamBudgetFault = errors.New("team budget transaction fault")

type teamBudgetFaultDriver struct {
	assuranceDriver
	rollback bool
}

type teamBudgetClock struct{ at time.Time }

func (c teamBudgetClock) Now() time.Time { return c.at }

func (d teamBudgetFaultDriver) TransactOrchestrationBudget(ctx context.Context, key id.OrchestrationID, callback func(context.Context, assurance.Driver) error) error {
	err := d.assuranceDriver.TransactOrchestrationBudget(ctx, key, func(tc context.Context, tx assurance.Driver) error {
		if callErr := callback(tc, tx); callErr != nil {
			return callErr
		}
		if d.rollback {
			return errTeamBudgetFault
		}
		return nil
	})
	if err != nil {
		return err
	}
	return errTeamBudgetFault
}

func TestOrchestrationBudgetLostAcknowledgmentAndReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "team-budget.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	s := openTestStore(t, dsn)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "team-budget"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	allocation := execution.Resources{Steps: 2, Tokens: 600, ToolCalls: 2, FanOut: 1}
	p := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("team")), Scope: scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 8}, Participants: map[string]execution.Resources{"worker": allocation}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	req := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ExecutionID, PlanDigest: p.PreparedDigest, TaskID: id.NewAgentRunID(), Scope: scope, OwnerRef: "worker", Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: p.Deadline}
	require.NoError(t, s.CreateOrchestrationBudget(ctx, p))
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: true})
	unknown, err := s.ReserveOrchestrationChild(ctx, req)
	require.ErrorIs(t, err, errTeamBudgetFault)
	require.Zero(t, unknown)
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	snapshot, err := s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.Zero(t, snapshot.Reservations)
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}})
	unknown, err = s.ReserveOrchestrationChild(ctx, req)
	require.ErrorIs(t, err, errTeamBudgetFault)
	require.Zero(t, unknown)
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	retained, err := s.GetReservation(ctx, req.ID)
	require.NoError(t, err)
	replay, err := s.ReserveOrchestrationChild(ctx, req)
	require.NoError(t, err)
	require.Equal(t, retained, replay)
	snapshot, err = s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, 1, snapshot.Reservations)
	require.EqualValues(t, 600, snapshot.Charged.Tokens)
	s.assuranceStore = assurance.New(assuranceDriver{store: s}).WithClock(teamBudgetClock{at: p.Deadline.Add(time.Second)})
	replay, err = s.ReserveOrchestrationChild(ctx, req)
	require.NoError(t, err)
	require.Equal(t, retained, replay)
	expired := req
	expired.ID, expired.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
	_, err = s.ReserveOrchestrationChild(ctx, expired)
	require.ErrorIs(t, err, execution.ErrInvalidReservation)
	snapshot, err = s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.EqualValues(t, 600, snapshot.Charged.Tokens)
	settlement := execution.Settlement{ReservationID: req.ID, RequestDigest: retained.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 100}, Known: execution.KnownTokens}, Receipt: "usage:terminal"}
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}})
	require.ErrorIs(t, s.SettleOrchestrationChild(ctx, settlement), errTeamBudgetFault)
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	require.NoError(t, s.SettleOrchestrationChild(ctx, settlement))
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: true})
	require.ErrorIs(t, s.CloseOrchestrationBudget(ctx, p.ExecutionID), errTeamBudgetFault)
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	snapshot, err = s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.False(t, snapshot.Closed)
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}})
	require.ErrorIs(t, s.CloseOrchestrationBudget(ctx, p.ExecutionID), errTeamBudgetFault)
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	require.NoError(t, s.CloseOrchestrationBudget(ctx, p.ExecutionID))
	require.NoError(t, s.CreateOrchestrationBudget(ctx, p))
	snapshot, err = s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.True(t, snapshot.Closed)
	require.EqualValues(t, 100, snapshot.Charged.Tokens)
}
