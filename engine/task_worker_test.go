package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

type taskWorkerResolver func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) (engine.TaskWork, error)

func (f taskWorkerResolver) ResolveTaskWork(ctx context.Context, request coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim) (engine.TaskWork, error) {
	return f(ctx, request, receipt)
}

type taskWorkerDispatchFence func(context.Context, coordination.TaskDispatchRequest, func(coordination.TaskDispatchRequest) error) error

func (f taskWorkerDispatchFence) WithTaskDispatchFence(ctx context.Context, r coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
	return f(ctx, r, next)
}

type taskWorkerDispatchAuthorizer func(context.Context, coordination.TaskDispatchRequest) error

func (f taskWorkerDispatchAuthorizer) AuthorizeTaskDispatch(ctx context.Context, r coordination.TaskDispatchRequest) error {
	return f(ctx, r)
}

type taskWorkerDispatchAdmitter func(context.Context, coordination.TaskDispatchRequest) (coordination.TaskDispatch, error)

func (f taskWorkerDispatchAdmitter) Admit(ctx context.Context, r coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
	return f(ctx, r)
}

func taskWorkerConfig(t *testing.T, f taskClaimRuntimeFixture) engine.TaskWorkerConfig {
	t.Helper()
	require.NoError(t, engine.WithTaskRunAdmitter(f.admitter)(f.engine))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(f.engine))
	dispatch, err := coordination.NewTaskDispatchAdmitter(f.f.storage, taskWorkerDispatchFence(func(_ context.Context, r coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
		return next(r)
	}), taskWorkerDispatchAuthorizer(func(context.Context, coordination.TaskDispatchRequest) error { return nil }))
	require.NoError(t, err)
	return engine.TaskWorkerConfig{Dispatch: dispatch, Resolver: taskWorkerResolver(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) (engine.TaskWork, error) {
		return engine.TaskWork{Prepared: f.prepared, Dispatch: f.dispatch, Input: "task", SessionID: f.options.SessionID, Reason: "execute admitted task"}, nil
	})}
}

func TestTaskWorkerConnectsGuardedDispatchToVerifiedExecution(t *testing.T) {
	f := preparedTaskClaimFixture(t)
	worker, err := f.engine.NewTaskWorker(taskWorkerConfig(t, f))
	require.NoError(t, err)
	require.NoError(t, worker.RunClaimedTask(f.f.ctx, f.request, f.ready))
	result, err := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.VerificationStatus)
	require.Equal(t, "done", result.Output)
	require.Equal(t, 1, f.client.calls)
	calls, err := f.f.storage.ListRunCalls(f.f.ctx, execution.RunCallListRequest{RunID: result.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].Completion)
	// Even a caller reusing the old Fresh flag cannot dispatch this claim twice.
	require.ErrorIs(t, worker.RunClaimedTask(f.f.ctx, f.request, f.ready), cortex.ErrTaskRunReconciliationRequired)
	require.Equal(t, 1, f.client.calls)
}

func TestTaskWorkerStopsBeforeExecutionOnUncertainDispatch(t *testing.T) {
	for _, mode := range []string{"history", "lost_ack", "invalid_receipt", "revoked", "cancelled_after_commit", "changed_claim", "changed_admission", "wrong_worker", "scope", "prepared", "mutated_resolver_input"} {
		t.Run(mode, func(t *testing.T) {
			f := preparedTaskClaimFixture(t)
			cfg := taskWorkerConfig(t, f)
			ctx, cancel := context.WithCancel(f.f.ctx)
			defer cancel()
			resolver := cfg.Resolver
			cfg.Resolver = taskWorkerResolver(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim) (engine.TaskWork, error) {
				work, err := resolver.ResolveTaskWork(ctx, req, receipt)
				switch mode {
				case "changed_claim":
					work.Dispatch.Claim.Fence++
				case "changed_admission":
					work.Dispatch.AdmissionDigest = "invalid"
				case "wrong_worker":
					work.Dispatch.Claim.Worker.Agent = id.NewAgentID().String()
				case "prepared":
					work.Prepared = nil
				case "mutated_resolver_input":
					req.Acquisition.Reservation.Scope.Levels[0].Value = "changed"
					receipt.Acquisition.Claim.Scope.Levels[0].Value = "changed"
				}
				return work, err
			})
			dispatch := cfg.Dispatch
			cfg.Dispatch = taskWorkerDispatchAdmitter(func(ctx context.Context, req coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
				if mode == "revoked" {
					return coordination.TaskDispatch{}, errors.New("revoked")
				}
				receipt, err := dispatch.Admit(ctx, req)
				switch mode {
				case "history":
					receipt.Fresh = false
				case "lost_ack":
					return receipt, errors.New("lost acknowledgment")
				case "invalid_receipt":
					receipt.RequestDigest = "invalid"
				case "cancelled_after_commit":
					cancel()
				}
				return receipt, err
			})
			if mode == "scope" {
				ctx = context.Background()
			}
			worker, err := f.engine.NewTaskWorker(cfg)
			require.NoError(t, err)
			err = worker.RunClaimedTask(ctx, f.request, f.ready)
			if mode == "mutated_resolver_input" {
				require.NoError(t, err)
				require.Equal(t, 1, f.client.calls)
			} else {
				require.Error(t, err)
				require.Zero(t, f.client.calls)
			}
		})
	}
}

type engineSchedulingSource func(context.Context, id.ID) (planning.Plan, []planning.TaskState, error)

func (f engineSchedulingSource) LoadSchedulingState(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
	return f(ctx, planID)
}

type engineClaimResolver func(context.Context, planning.Plan, id.ID) (coordination.ReadyTaskClaimRequest, error)

func (f engineClaimResolver) ResolveTaskClaim(ctx context.Context, p planning.Plan, task id.ID) (coordination.ReadyTaskClaimRequest, error) {
	return f(ctx, p, task)
}

type engineClaimFence func(context.Context, coordination.ReadyTaskClaimRequest, func(coordination.ReadyTaskClaimRequest) error) error

func (f engineClaimFence) WithTaskClaimFence(ctx context.Context, r coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
	return f(ctx, r, next)
}

type engineClaimAuthorizer func(context.Context, coordination.ReadyTaskClaimRequest) error

func (f engineClaimAuthorizer) AuthorizeTaskClaim(ctx context.Context, r coordination.ReadyTaskClaimRequest) error {
	return f(ctx, r)
}

func schedulerTaskWorkerConfig(t *testing.T, f taskClaimRuntimeFixture) coordination.SchedulerConfig {
	t.Helper()
	cfg := taskWorkerConfig(t, f)
	resolver := cfg.Resolver
	cfg.Resolver = taskWorkerResolver(func(ctx context.Context, req coordination.ReadyTaskClaimRequest, receipt coordination.ReadyTaskClaim) (engine.TaskWork, error) {
		work, err := resolver.ResolveTaskWork(ctx, req, receipt)
		work.Dispatch.Claim = receipt.Acquisition.Claim.Token()
		work.Dispatch.ClaimDigest = receipt.Acquisition.Claim.Digest
		work.Dispatch.AdmissionDigest = receipt.RequestDigest
		return work, err
	})
	worker, err := f.engine.NewTaskWorker(cfg)
	require.NoError(t, err)
	admitter, err := coordination.NewTaskClaimAdmitter(f.f.storage, engineClaimFence(func(_ context.Context, req coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
		return next(req)
	}), engineClaimAuthorizer(func(context.Context, coordination.ReadyTaskClaimRequest) error { return nil }))
	require.NoError(t, err)
	return coordination.SchedulerConfig{Worker: worker, Admitter: admitter, Clock: workerClock{}, Resolver: engineClaimResolver(func(context.Context, planning.Plan, id.ID) (coordination.ReadyTaskClaimRequest, error) {
		return f.request, nil
	}), Source: engineSchedulingSource(func(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
		p, loadErr := f.f.storage.GetPlan(ctx, planID)
		return p, nil, loadErr
	})}
}

func TestSchedulerTaskWorkerRunsExistingPreparedPipeline(t *testing.T) {
	f := newTaskClaimRuntimeFixture(t, false)
	scheduler, err := coordination.NewScheduler(schedulerTaskWorkerConfig(t, f))
	require.NoError(t, err)
	require.NoError(t, scheduler.Tick(f.f.ctx, f.request.Acquisition.Claim.PlanID))
	result, err := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.VerificationStatus)
	require.Equal(t, "done", result.Output)
	require.Equal(t, 1, f.client.calls)
	// Verification does not silently perform task acceptance or shared settlement.
	claim, err := f.f.storage.GetTaskClaim(f.f.ctx, f.request.Acquisition.Claim.PlanID, f.request.Acquisition.Claim.TaskID)
	require.NoError(t, err)
	require.Equal(t, coordination.ClaimDispatching, claim.State)
	budget, err := f.f.storage.GetBudgetSnapshot(f.f.ctx, f.request.Acquisition.Claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, f.request.Acquisition.Reservation.Allocation, budget.Charged)
	require.ErrorIs(t, scheduler.Tick(f.f.ctx, f.request.Acquisition.Claim.PlanID), coordination.ErrSchedulerReconciliationRequired)
	require.Equal(t, 1, f.client.calls)
	require.NoError(t, scheduler.Shutdown(context.Background()))
}

func TestTaskWorkerRetainsUncertainChildAndRechecksParent(t *testing.T) {
	for _, mode := range []string{"cancelled_parent", "lost_child_ack", "call_denied"} {
		t.Run(mode, func(t *testing.T) {
			f := preparedTaskClaimFixture(t)
			cfg := taskWorkerConfig(t, f)
			switch mode {
			case "cancelled_parent":
				parent, err := f.f.storage.GetRun(f.f.ctx, f.options.Assignment.Handoff.ParentRunID)
				require.NoError(t, err)
				parent.State = run.StateCancelled
				require.NoError(t, f.f.storage.UpdateRun(f.f.ctx, parent))
			case "lost_child_ack":
				require.NoError(t, engine.WithTaskRunAdmitter(runtimeTaskAdmitter(func(ctx context.Context, req coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
					receipt, err := f.admitter.Admit(ctx, req)
					if err != nil {
						return receipt, err
					}
					return receipt, errors.New("lost child acknowledgement")
				}))(f.engine))
			case "call_denied":
				require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(func(context.Context, engine.TaskCallRequest) (engine.TaskCallLease, error) {
					return engine.TaskCallLease{}, errors.New("call denied")
				}))(f.engine))
			}
			worker, err := f.engine.NewTaskWorker(cfg)
			require.NoError(t, err)
			require.Error(t, worker.RunClaimedTask(f.f.ctx, f.request, f.ready))
			require.Zero(t, f.client.calls)
			if mode == "lost_child_ack" {
				child, loadErr := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
				require.NoError(t, loadErr)
				require.Equal(t, f.dispatch.RunID, child.ID)
			}
			require.Error(t, worker.RunClaimedTask(f.f.ctx, f.request, f.ready))
			require.Zero(t, f.client.calls)
		})
	}
}

func TestTaskWorkerRequiresExplicitPorts(t *testing.T) {
	f := preparedTaskClaimFixture(t)
	_, err := f.engine.NewTaskWorker(engine.TaskWorkerConfig{})
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
	cfg := taskWorkerConfig(t, f)
	var missing *engine.Engine
	_, err = missing.NewTaskWorker(cfg)
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
	cfg.Resolver = taskWorkerResolver(nil)
	_, err = f.engine.NewTaskWorker(cfg)
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
}

func TestTaskWorkerActivatesRenewalAfterFreshChildCreation(t *testing.T) {
	for _, mode := range []string{"fresh", "denied", "lost_ack", "missing_start"} {
		t.Run(mode, func(t *testing.T) {
			f := preparedTaskClaimFixture(t)
			cfg := taskWorkerConfig(t, f)
			if mode == "lost_ack" {
				original := f.admitter
				require.NoError(t, engine.WithTaskRunAdmitter(runtimeTaskAdmitter(func(ctx context.Context, r coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
					receipt, err := original.Admit(ctx, r)
					if err == nil {
						err = errors.New("lost child acknowledgement")
					}
					return receipt, err
				}))(f.engine))
			}
			worker, err := f.engine.NewTaskWorker(cfg)
			require.NoError(t, err)
			renewing, ok := worker.(coordination.RenewingClaimedTaskWorker)
			require.True(t, ok)
			activations := 0
			start := coordination.TaskRenewalStart(func(claim coordination.Claim) error {
				activations++
				require.Zero(t, f.client.calls)
				require.Equal(t, coordination.ClaimDispatching, claim.State)
				child, readErr := f.f.storage.GetRun(f.f.ctx, claim.RunID)
				require.NoError(t, readErr)
				require.Equal(t, claim.ID.String(), child.Provenance.TaskClaimID)
				require.NoError(t, coordination.ValidateClaimSuccessor(f.ready.Acquisition.Claim, claim))
				if mode == "denied" {
					return errors.New("renewal activation denied")
				}
				return nil
			})
			if mode == "missing_start" {
				start = nil
			}
			err = renewing.RunClaimedTaskWithRenewal(f.f.ctx, f.request, f.ready, start)
			if mode == "fresh" {
				require.NoError(t, err)
				require.Equal(t, 1, f.client.calls)
			} else {
				require.Error(t, err)
				require.Zero(t, f.client.calls)
			}
			if mode == "lost_ack" || mode == "missing_start" {
				require.Zero(t, activations)
			} else {
				require.Equal(t, 1, activations)
			}
		})
	}
}
