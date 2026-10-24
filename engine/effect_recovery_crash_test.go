package engine_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
)

type crashEffectAdapter struct {
	path   string
	park   func()
	probes atomic.Int32
}

func (a *crashEffectAdapter) Execute(_ context.Context, r execution.EffectRequest, _ []byte) (execution.EffectReceipt, error) {
	_, digest, err := r.Freeze()
	if err != nil {
		return execution.EffectReceipt{}, err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return execution.EffectReceipt{}, err
	}
	_, writeErr := file.WriteString(digest + "\n")
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return execution.EffectReceipt{}, err
	}
	if a.park != nil {
		a.park()
	}
	return engineEffectReceipt(r), nil
}
func (a *crashEffectAdapter) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	a.probes.Add(1)
	data, err := os.ReadFile(a.path)
	if err != nil {
		return execution.EffectStatus{}, err
	}
	_, digest, err := r.Freeze()
	if err != nil || string(data) != digest+"\n" {
		return execution.EffectStatus{Outcome: execution.EffectUnknown}, nil
	}
	receipt := engineEffectReceipt(r)
	return execution.EffectStatus{Outcome: receipt.Outcome, Receipt: &receipt}, nil
}
func TestEffectRecoveryAfterWorkerProcessCrash(t *testing.T) {
	m := killTaskWorkerAtBoundary(t, "effect_pending")
	s := openSQLiteStoreForTest(t, m.Database)
	ctx := cortex.WithScope(t.Context(), m.Claim.Scope)
	records, err := s.ListEffects(ctx, execution.EffectListRequest{RunID: m.Dispatch.RunID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, records, 1)
	original := records[0]
	require.Nil(t, original.Receipt)
	require.NotNil(t, original.Request.RunCall)
	call, err := s.GetRunCall(ctx, m.Dispatch.RunID, original.Request.RunCall.OperationDigest)
	require.NoError(t, err)
	require.Nil(t, call.Completion)
	require.Equal(t, original.Request.RunCall, &call.Request)
	_, err = s.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: m.Claim.Scope, RunID: m.Dispatch.RunID, Actor: "recovery", Reason: "worker killed"})
	require.NoError(t, err)
	recovering, err := s.WithClock(crashRecoveryClock{at: m.Claim.ExpiresAt}).ExpireTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID, m.Claim.Revision)
	require.NoError(t, err)
	require.Equal(t, coordination.ClaimRecovering, recovering.State)
	beforeRun, err := s.GetRun(ctx, m.Dispatch.RunID)
	require.NoError(t, err)
	beforeBudget, err := s.GetBudgetSnapshot(ctx, m.Claim.PlanID)
	require.NoError(t, err)
	adapter := &crashEffectAdapter{path: m.EffectLog}
	for range 2 {
		reopened := openSQLiteStoreForTest(t, m.Database)
		var discoveries, scans atomic.Int32
		cfg := coordination.EffectRecoveryServiceConfig{Interval: 10 * time.Millisecond, Recovery: execution.EffectExecutorConfig{Store: reopened, Adapters: []execution.EffectRegistration{{Requirement: original.Request.Requirement, Adapter: adapter}}, Authorize: func(_ context.Context, r execution.EffectRequest, action execution.EffectAction) error {
			require.Equal(t, original.Request, r)
			require.Equal(t, execution.EffectActionReconcile, action)
			return nil
		}, AuthorizeScan: func(context.Context, execution.RunCallSeal) error { scans.Add(1); return nil }, MaxConcurrent: 1}, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: m.Claim.Scope, PlanID: m.Claim.PlanID}}}, nil
		})}
		service, makeErr := engine.New(engine.WithEffectRecovery(cfg), engine.WithSuspensionTTL(0))
		require.NoError(t, makeErr)
		require.NoError(t, service.Start(ctx))
		t.Cleanup(func() { require.NoError(t, service.Stop(context.Background())) })
		require.Eventually(t, func() bool {
			status, _ := service.EffectRecoveryStatus()
			return scans.Load() >= 2 && status.Active == 0
		}, 10*time.Second, 10*time.Millisecond)
		require.NoError(t, service.Stop(context.Background()))
		status, configured := service.EffectRecoveryStatus()
		require.True(t, configured)
		require.Zero(t, status.Failures)
		retained, readErr := reopened.GetEffect(ctx, original.Request.OperationKey)
		require.NoError(t, readErr)
		require.NotNil(t, retained.Receipt)
		require.NoError(t, retained.Receipt.Validate(original.Request))
	}
	require.Equal(t, int32(1), adapter.probes.Load())
	data, err := os.ReadFile(m.EffectLog)
	require.NoError(t, err)
	_, digest, err := original.Request.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest+"\n", string(data))
	afterCall, err := s.GetRunCall(ctx, m.Dispatch.RunID, call.Request.OperationDigest)
	require.NoError(t, err)
	require.Equal(t, call, afterCall)
	afterClaim, err := s.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
	require.NoError(t, err)
	require.Equal(t, recovering, afterClaim)
	afterRun, err := s.GetRun(ctx, m.Dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, beforeRun, afterRun)
	afterBudget, err := s.GetBudgetSnapshot(ctx, m.Claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, beforeBudget, afterBudget)
}
