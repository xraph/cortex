package coordination_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

func TestTaskSettlementPagesSuccessiveAcceptancesOfSameTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, storage, cfg := settlementServiceFixture(t)
		first := storage.claim
		plan, err := planning.Seal(storage.plan, planning.Limits{})
		require.NoError(t, err)
		plan.Tasks[0].Instruction = "revised task"
		plan, err = planning.ApplyRevision(storage.plan, planning.Revision{Plan: plan, Actor: "host", Reason: "retry revised task"}, storage.plan.Revision, planning.Limits{})
		require.NoError(t, err)
		second, err := coordination.AcquireClaim(&first, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: plan.ID, PlanDigest: plan.Digest, PlanRevision: plan.Revision, TaskID: first.TaskID, Worker: first.Worker, GrantRef: first.GrantRef, Duration: time.Minute, ExpectedRevision: first.Revision}, plan, first.At)
		require.NoError(t, err)
		second, err = coordination.BeginDispatch(second, second.Token(), id.NewAgentRunID(), first.At)
		require.NoError(t, err)
		second, err = coordination.CompleteClaim(second, second.Token(), id.New(coordination.PrefixTaskAcceptance), first.At)
		require.NoError(t, err)
		claims := []coordination.Claim{first, second}
		if first.AcceptanceID.String() > second.AcceptanceID.String() {
			claims[0], claims[1] = second, first
		}
		var cursors []id.ID
		var pages []string
		storage.acceptedList = func(_ context.Context, r coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error) {
			pages = append(pages, "accepted")
			require.NoError(t, r.Validate())
			cursors = append(cursors, r.After)
			for _, claim := range claims {
				if r.After.IsNil() || claim.AcceptanceID.String() > r.After.String() {
					return []coordination.Claim{claim}, nil
				}
			}
			return nil, nil
		}
		storage.sealRead = func(runID id.AgentRunID) (*execution.RunCallSeal, error) {
			request := storage.seal.Request
			request.RunID = runID
			request, digest, freezeErr := request.Freeze()
			if freezeErr != nil {
				return nil, freezeErr
			}
			return &execution.RunCallSeal{Request: request, RequestDigest: digest, At: first.At}, nil
		}
		storage.noBudget = true
		storage.list = func(_ context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
			pages = append(pages, "current")
			require.True(t, r.AfterTaskID.IsNil(), "acceptance cursor leaked into task traversal")
			return nil, nil
		}
		cfg.UnsuccessfulRuns = settlementRunReader(nil)
		// A typed-nil optional port is invalid, not an enabled fallback.
		_, err = coordination.NewTaskSettlementService(cfg)
		require.Error(t, err)
		cfg.UnsuccessfulRuns = settlementRunReader(func(context.Context, id.AgentRunID) (*run.Run, error) {
			t.Fatal("unbudgeted accepted claims must not read runs")
			return nil, nil
		})
		var listed atomic.Bool
		cfg.Discovery = taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if !listed.CompareAndSwap(false, true) {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: first.Scope, PlanID: first.PlanID}}}, nil
		})
		cfg.PageSize = 1
		service, err := coordination.NewTaskSettlementService(cfg)
		require.NoError(t, err)
		defer func() { require.NoError(t, service.Stop(ctx)) }()
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.NoError(t, service.Stop(ctx))
		require.Equal(t, []id.ID{{}, *claims[0].AcceptanceID, *claims[1].AcceptanceID}, cursors)
		require.EqualValues(t, 2, storage.seals.Load())
		require.Equal(t, []string{"accepted", "current", "accepted", "accepted"}, pages)
		require.Zero(t, service.Status().Failures)
	})
}

func TestTaskSettlementRejectsMalformedAcceptancePagesBeforeSealing(t *testing.T) {
	for _, mode := range []string{"duplicate", "oversized", "unaccepted", "foreign plan"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, storage, cfg := settlementServiceFixture(t)
				cfg.PageSize = 2
				claims := []coordination.Claim{storage.claim}
				switch mode {
				case "duplicate":
					claims = append(claims, storage.claim)
				case "oversized":
					cfg.PageSize = 1
					claims = append(claims, storage.claim)
				case "unaccepted":
					claims[0] = storage.budget.Claim
				case "foreign plan":
					claims[0] = taskAcceptanceFixture(t).Claim
				}
				storage.acceptedList = func(context.Context, coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error) {
					return claims, nil
				}
				service, err := coordination.NewTaskSettlementService(cfg)
				require.NoError(t, err)
				defer func() { require.NoError(t, service.Stop(ctx)) }()
				require.NoError(t, service.Start(ctx))
				synctest.Wait()
				require.NoError(t, service.Stop(ctx))
				require.Positive(t, service.Status().Failures)
				require.Zero(t, storage.seals.Load())
				require.Zero(t, storage.settlements.Load())
			})
		})
	}
}
