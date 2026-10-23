package optimization_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type callerIdentity func(context.Context) (optimization.IdentityRef, error)

func (f callerIdentity) Current(ctx context.Context) (optimization.IdentityRef, error) { return f(ctx) }

type phaseHandler func(context.Context, optimization.WorkItem) (optimization.Transition, error)

func (f phaseHandler) Execute(ctx context.Context, item optimization.WorkItem) (optimization.Transition, error) {
	return f(ctx, item)
}

func TestWorkerCancelStopsRunningPhase(t *testing.T) {
	s, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bundle := storetest.Fixture(t, "worker")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	started := make(chan struct{})
	handler := phaseHandler(func(ctx context.Context, item optimization.WorkItem) (optimization.Transition, error) {
		if item.Experiment.State != optimization.StateRunning || item.Lease.Generation != 1 {
			t.Error("phase started without claimed work")
		}
		close(started)
		<-ctx.Done()
		return optimization.Transition{}, ctx.Err()
	})
	optimizer, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), IdentityResolver: meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}), Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return bundle.Experiment.Spec.Requester, nil }), Handler: handler, Worker: optimization.WorkerConfig{LeaseDuration: time.Second, HeartbeatInterval: 10 * time.Millisecond, PollInterval: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := optimizer.Create(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := optimizer.Start(ctx, created.ID, created.Revision)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- optimizer.WorkOnce(ctx, queued.ID) }()
	<-started
	running, err := optimizer.Get(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = optimizer.Cancel(ctx, running.ID, running.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancelled worker succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker ignored persisted cancellation")
	}
	final, err := s.GetExperiment(ctx, running.ID)
	if err != nil || final.State != optimization.StateCancelled {
		t.Fatalf("cancel state: %+v %v", final, err)
	}
}

func TestWorkerShutdownLeavesRecoverableLease(t *testing.T) {
	s, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bundle := storetest.Fixture(t, "shutdown")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	started := make(chan struct{})
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), IdentityResolver: meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}), Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return bundle.Experiment.Spec.Requester, nil }), Handler: phaseHandler(func(ctx context.Context, _ optimization.WorkItem) (optimization.Transition, error) {
		close(started)
		<-ctx.Done()
		return optimization.Transition{}, ctx.Err()
	}), Worker: optimization.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	created, err := op.Create(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = op.Start(ctx, created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- op.WorkOnce(runCtx, created.ID) }()
	<-started
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	saved, err := s.GetExperiment(ctx, created.ID)
	if err != nil || saved.State != optimization.StateRunning || saved.Phase != optimization.PhaseBaseline || saved.Lease.Generation != 1 {
		t.Fatalf("shutdown lost recovery state: %+v %v", saved, err)
	}
	if err = op.WorkOnce(context.Background(), id.New(optimization.PrefixExperiment)); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped worker: %v", err)
	}
}

func TestWorkerTakeoverRecoversUsageBeforePhase(t *testing.T) {
	for _, mode := range []string{"verified", "unknown", "revoked", "recovery-denied"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC().Add(-time.Minute)
			s, err := memory.New(memory.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			bundle := storetest.Fixture(t, "takeover")
			bundle.Experiment.Spec.Budget = optimization.Budget{MaxTokens: 100, TokenOnly: true}
			ctx := cortex.WithScope(context.WithValue(context.Background(), workerSubjectKey{}, "worker"), bundle.Experiment.Spec.Scope)
			var lease optimization.WorkLease
			if err = s.Transact(ctx, func(tx optimization.Transaction) error {
				if createErr := tx.CreateExperiment(bundle); createErr != nil {
					return createErr
				}
				if startErr := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); startErr != nil {
					return startErr
				}
				var claimErr error
				lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "old", Duration: time.Second})
				return claimErr
			}); err != nil {
				t.Fatal(err)
			}
			req := optimization.UsageReservation{ExperimentID: bundle.Experiment.ID, AttemptID: id.New(optimization.PrefixAttempt), CallKey: "lost", Operation: optimization.OperationProposal, MaxTokens: 60, Lease: lease, ExpiresAt: now.Add(time.Minute)}
			if _, err = allowModelMeter(t, s).BeginCall(ctx, req, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			now = time.Now().UTC()
			auth := meterAuthorizer(func(ctx context.Context, action optimization.Action, _ optimization.Resource) error {
				if mode == "recovery-denied" && action == optimization.ActionReconcileUsage && ctx.Value(workerSubjectKey{}) == "worker" {
					return optimization.ErrPolicyDenied
				}
				if mode == "revoked" {
					return optimization.ErrPolicyDenied
				}
				return nil
			})
			resolver := meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return context.WithValue(ctx, workerSubjectKey{}, "requester"), nil
			})
			recovered := false
			recovery, err := optimization.NewUsageRecovery(s, auth, resolver, usageReceiptProvider(func(_ context.Context, r optimization.Reservation) (optimization.VerifiedUsageReceipt, error) {
				if mode == "unknown" {
					return optimization.VerifiedUsageReceipt{}, optimization.ErrUnknownAttempt
				}
				recovered = true
				return optimization.VerifiedUsageReceipt{RequestDigest: r.Call.RequestDigest, Settlement: optimization.UsageSettlement{ExperimentID: r.Request.ExperimentID, AttemptID: r.Request.AttemptID, CallKey: r.Request.CallKey, Receipt: "late", Complete: true, Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "late"}}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			phaseCalls := 0
			phaseDone := errors.New("phase observed")
			op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: auth, IdentityResolver: resolver, Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return bundle.Experiment.Spec.Requester, nil }), UsageRecovery: recovery, Worker: optimization.DefaultWorkerConfig(), Handler: phaseHandler(func(_ context.Context, item optimization.WorkItem) (optimization.Transition, error) {
				phaseCalls++
				if !recovered || item.Lease.Generation != 2 {
					t.Error("phase preceded reconciliation")
				}
				return optimization.Transition{}, phaseDone
			})})
			if err != nil {
				t.Fatal(err)
			}
			err = op.WorkOnce(ctx, bundle.Experiment.ID)
			if mode == "verified" {
				if !errors.Is(err, phaseDone) || phaseCalls != 1 {
					t.Fatalf("takeover: %v calls=%d", err, phaseCalls)
				}
			} else if err == nil || phaseCalls != 0 {
				t.Fatalf("unverified takeover: %v calls=%d", err, phaseCalls)
			}
		})
	}
}

func TestWorkerPersistsBudgetExhaustion(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	experiment, err := s.GetExperiment(ctx, req.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	auth := meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil })
	resolver := meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	})
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: auth, IdentityResolver: resolver, Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return experiment.Spec.Requester, nil }), Worker: optimization.DefaultWorkerConfig(), Handler: phaseHandler(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
		return optimization.Transition{}, optimization.ErrBudgetExceeded
	})})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := op.Pause(ctx, experiment.ID, experiment.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = op.Resume(ctx, experiment.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
	if err = op.WorkOnce(ctx, experiment.ID); !errors.Is(err, optimization.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	saved, err := s.GetExperiment(ctx, experiment.ID)
	if err != nil || saved.State != optimization.StateBudgetExhausted {
		t.Fatalf("exhaustion not persisted: %+v %v", saved, err)
	}
}

type workerSubjectKey struct{}

func TestWorkerHeartbeatRechecksCallingPrincipal(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	experiment, err := s.GetExperiment(ctx, req.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, workerSubjectKey{}, "worker")
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var revoked atomic.Bool
	auth := meterAuthorizer(func(ctx context.Context, _ optimization.Action, _ optimization.Resource) error {
		if ctx.Value(workerSubjectKey{}) == "worker" && revoked.Load() {
			return optimization.ErrPolicyDenied
		}
		return nil
	})
	resolver := meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return context.WithValue(ctx, workerSubjectKey{}, "requester"), nil
	})
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: auth, IdentityResolver: resolver, Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return experiment.Spec.Requester, nil }), Worker: optimization.WorkerConfig{LeaseDuration: time.Second, HeartbeatInterval: 10 * time.Millisecond, PollInterval: time.Millisecond}, Handler: phaseHandler(func(ctx context.Context, _ optimization.WorkItem) (optimization.Transition, error) {
		revoked.Store(true)
		<-ctx.Done()
		return optimization.Transition{}, ctx.Err()
	})})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := op.Pause(ctx, experiment.ID, experiment.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = op.Resume(ctx, experiment.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
	if err = op.WorkOnce(ctx, experiment.ID); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("caller revocation missed by heartbeat: %v", err)
	}
}
