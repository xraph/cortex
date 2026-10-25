package coordination_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type settlementRunReader func(context.Context, id.AgentRunID) (*run.Run, error)

func (f settlementRunReader) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	return f(ctx, runID)
}

func terminalServiceFixture(t *testing.T) (context.Context, *settlementServiceStore, coordination.TaskSettlementServiceConfig, *run.Run) {
	t.Helper()
	ctx, s, cfg := settlementServiceFixture(t)
	worker := id.NewAgentID()
	original := s.budget.Claim
	held, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: original.ID, PlanID: s.plan.ID, PlanDigest: s.plan.Digest, PlanRevision: s.plan.Revision, TaskID: original.TaskID, Worker: coordination.Identity{Agent: worker.String(), Principal: original.Worker.Principal}, GrantRef: original.GrantRef, Duration: time.Minute}, s.plan, original.At)
	require.NoError(t, err)
	dispatched, err := coordination.BeginDispatch(held, held.Token(), s.claim.RunID, held.At)
	require.NoError(t, err)
	recovering, err := coordination.ExpireClaim(dispatched, dispatched.Revision, dispatched.ExpiresAt)
	require.NoError(t, err)
	s.claim, s.snapshot.Claim, s.budget.Claim = recovering, recovering, held
	s.snapshot, err = coordination.SealTaskUsageSnapshot(s.snapshot)
	require.NoError(t, err)
	ended := recovering.At
	current := &run.Run{ID: recovering.RunID, AgentID: worker, Scope: recovering.Scope, Revision: 3, State: run.StateFailed, CompletedAt: &ended, Provenance: run.ExecutionProvenance{TaskClaimID: recovering.ID.String()}}
	cfg.UnsuccessfulRuns = settlementRunReader(func(context.Context, id.AgentRunID) (*run.Run, error) { return current, nil })
	return ctx, s, cfg, current
}

func TestTaskSettlementUnsuccessfulDiscoveryRequiresAuthorityAndTerminalRun(t *testing.T) {
	for _, mode := range []string{"allowed", "disabled", "unsealed", "scan denied", "closure denied", "running", "paused", "completed", "wrong run", "wrong scope", "wrong worker", "wrong claim", "no completion", "nil run", "accepted page error"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, s, cfg, current := terminalServiceFixture(t)
				var runReads int
				cfg.UnsuccessfulRuns = settlementRunReader(func(context.Context, id.AgentRunID) (*run.Run, error) { runReads++; return current, nil })
				switch mode {
				case "disabled":
					cfg.UnsuccessfulRuns = nil
				case "unsealed":
					s.seal = nil
					// These accepted-task ports must not seal recovering work.
					cfg.CallSealFence = taskCallSealFence(func(context.Context, coordination.TaskCallSealRequest, func(coordination.TaskCallSealRequest) error) error {
						t.Fatal("accepted sealer used for recovering claim")
						return nil
					})
					cfg.CallSealAuthorizer = taskCallSealAuthorizer(func(context.Context, coordination.TaskCallSealRequest) error { return nil })
				case "scan denied":
					cfg.AuthorizeScan = func(context.Context, coordination.Claim, execution.RunCallSeal) error { return errors.New("denied") }
				case "closure denied":
					cfg.ClosureAuthorizer = usageClosureAuthorizer(func(context.Context, coordination.TaskUsageClosureRequest) error { return errors.New("denied") })
				case "running":
					current.State = run.StateRunning
				case "paused":
					current.State = run.StatePaused
				case "completed":
					current.State = run.StateCompleted
				case "wrong run":
					current.ID = id.NewAgentRunID()
				case "wrong scope":
					current.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}}
				case "wrong worker":
					current.AgentID = id.NewAgentID()
				case "wrong claim":
					current.Provenance.TaskClaimID = "other"
				case "no completion":
					current.CompletedAt = nil
				case "nil run":
					current = nil
				case "accepted page error":
					s.acceptedList = func(context.Context, coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error) {
						return nil, errors.New("accepted page unavailable")
					}
				}
				service, err := coordination.NewTaskSettlementService(cfg)
				require.NoError(t, err)
				defer func() { require.NoError(t, service.Stop(context.Background())) }()
				require.NoError(t, service.Start(ctx))
				synctest.Wait()
				require.NoError(t, service.Stop(ctx))
				if mode == "allowed" || mode == "accepted page error" {
					require.EqualValues(t, 1, s.settlements.Load())
					require.NotNil(t, s.closed.Request.Termination)
					require.Equal(t, current.Revision, s.closed.Request.Termination.RunRevision)
				} else {
					require.Zero(t, s.settlements.Load())
					require.Zero(t, s.closes.Load())
				}
				if mode == "disabled" || mode == "unsealed" || mode == "scan denied" {
					require.Zero(t, runReads)
				}
				if mode == "running" || mode == "paused" || mode == "completed" {
					require.Zero(t, service.Status().Failures)
				}
			})
		})
	}
}

func TestTaskSettlementJoinsUnsuccessfulRunReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, s, cfg, current := terminalServiceFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		cfg.UnsuccessfulRuns = settlementRunReader(func(ctx context.Context, _ id.AgentRunID) (*run.Run, error) {
			close(entered)
			<-ctx.Done()
			<-release
			return current, nil
		})
		service, err := coordination.NewTaskSettlementService(cfg)
		require.NoError(t, err)
		defer func() { unblock(); require.NoError(t, service.Stop(context.Background())) }()
		require.NoError(t, service.Start(ctx))
		<-entered
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, service.Stop(ctx))
		require.Zero(t, s.closes.Load())
		require.Zero(t, s.settlements.Load())
	})
}
