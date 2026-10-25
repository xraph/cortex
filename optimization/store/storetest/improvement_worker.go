package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// ImprovementWorker checks ordered child creation and restartable parent scheduling.
func ImprovementWorker(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PollingLeavesChildrenToParentWorker", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		authority := controllerAuthority{actor: f.parent.Experiment.Spec.Requester}
		calls := 0
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
			calls++
			return optimization.Transition{}, optimization.ErrUnavailable
		}), Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range f.children {
			current, getErr := f.s.GetExperiment(f.ctx, child.Experiment.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			paused, pauseErr := op.Pause(f.ctx, current.ID, current.Revision)
			if pauseErr != nil {
				t.Fatal(pauseErr)
			}
			if _, err = op.Resume(f.ctx, current.ID, paused.Revision); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
		defer cancel()
		if err = op.Work(ctx); !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
			t.Fatalf("poller ran a parent-owned child: %v calls=%d", err, calls)
		}
	})
	t.Run("FrozenChildrenStartOneStepAtATime", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := validationFamilyBundles(t, 2)
		ctx := scoped(parent.Experiment.Spec.Scope)
		calls := 0
		childHandler := improvementStep(func(_ context.Context, item optimization.WorkItem) (optimization.Transition, error) {
			calls++
			if item.Experiment.ID != children[0].Experiment.ID || item.Experiment.Spec.Parent == nil {
				t.Fatal("unexpected child")
			}
			return optimization.Transition{}, optimization.ErrUnknownAttempt
		})
		op := parentWorker(t, s, parent, childHandler)
		created, err := op.Create(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Start(ctx, created.ID, created.Revision); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err = op.WorkOnce(ctx, created.ID); !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("create/queue step: %v", err)
			}
		}
		child, err := s.GetExperiment(ctx, children[0].Experiment.ID)
		if err != nil || child.State != optimization.StateQueued || !reflect.DeepEqual(child.Spec, children[0].Experiment.Spec) {
			t.Fatalf("frozen child: %+v %v", child, err)
		}
		if err = op.WorkOnce(ctx, created.ID); !errors.Is(err, optimization.ErrUnknownAttempt) || calls != 1 {
			t.Fatalf("unknown child work: %v calls=%d", err, calls)
		}
		for _, later := range children[1:] {
			if _, err = s.GetExperiment(ctx, later.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("scheduled beyond incomplete child: %v", err)
			}
		}
		allocations, err := s.ListValidationAllocations(ctx, created.ID, optimization.PageQuery{Limit: 100})
		if err != nil || len(allocations.Items) != 0 {
			t.Fatalf("early holdout allocation: %v", err)
		}
	})
	t.Run("ChildBudgetStopsParent", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := validationFamilyBundles(t, 1)
		ctx := scoped(parent.Experiment.Spec.Scope)
		op := parentWorker(t, s, parent, improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
			return optimization.Transition{}, optimization.ErrBudgetExceeded
		}))
		created, err := op.Create(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Start(ctx, created.ID, created.Revision); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err = op.WorkOnce(ctx, created.ID); !errors.Is(err, optimization.ErrConflict) {
				t.Fatal(err)
			}
		}
		if err = op.WorkOnce(ctx, created.ID); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("parent ignored exhausted child: %v", err)
		}
		for _, key := range []id.ID{created.ID, children[0].Experiment.ID} {
			current, getErr := s.GetExperiment(ctx, key)
			if getErr != nil || current.State != optimization.StateBudgetExhausted {
				t.Fatalf("exhaustion not persisted: %+v %v", current, getErr)
			}
		}
		if _, err = s.GetExperiment(ctx, children[1].Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("exhausted parent created next arm: %v", err)
		}
	})
	t.Run("DeniedChildIsNotCreated", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := validationFamilyBundles(t, 1)
		ctx := scoped(parent.Experiment.Spec.Scope)
		op := parentWorker(t, s, parent, improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
			return optimization.Transition{}, optimization.ErrUnavailable
		}), &improvementAuthority{denied: children[0].Experiment.ID})
		created, err := op.Create(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Start(ctx, created.ID, created.Revision); err != nil {
			t.Fatal(err)
		}
		if err = op.WorkOnce(ctx, created.ID); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("created denied child: %v", err)
		}
		if _, err = s.GetExperiment(ctx, children[0].Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("denied child persisted: %v", err)
		}
	})
	t.Run("MissingReflectionCannotOpenValidation", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		err = f.s.Transact(f.ctx, func(tx store.Tx) error {
			return tx.CommitTransition(optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, Lease: f.leases[0], Actor: current.Spec.Requester, IdempotencyKey: "early-parent-validation", NextState: optimization.StateRunning, NextPhase: optimization.PhaseValidate})
		})
		if !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("parent skipped discovery reflection: %v", err)
		}
	})
	for _, status := range []optimization.State{optimization.StatePaused, optimization.StateCancelled, optimization.StateBudgetExhausted} {
		t.Run("StoppedValidationChild/"+string(status), func(t *testing.T) {
			f := frozenValidationFamily(t, factory, 1)
			allocation := allocate(f.ctx, t, f.s, f.request)
			if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
				return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: f.children[0].Experiment.ID, Lease: f.leases[1]})
			}); err != nil {
				t.Fatal(err)
			}
			for index, child := range f.children {
				fillVersionTrials(f.ctx, t, f.s, child, f.leases[index+1], child.Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
			}
			current, err := f.s.GetExperiment(f.ctx, f.children[0].Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			change := optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, Actor: current.Spec.Requester, IdempotencyKey: "stop-child", NextState: status}
			want := optimization.ErrIncompleteEvidence
			if status == optimization.StateBudgetExhausted {
				change.Lease = f.leases[1]
				want = optimization.ErrBudgetExceeded
			}
			if status == optimization.StatePaused {
				want = optimization.ErrConflict
			}
			if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
				t.Fatal(err)
			}
			assertPhaseRefused(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseDecide)
			op := parentWorker(t, f.s, f.parent, improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
				return optimization.Transition{}, optimization.ErrUnavailable
			}))
			requeueParent(t, f, op)
			if err = op.WorkOnce(f.ctx, f.parent.Experiment.ID); !errors.Is(err, want) {
				t.Fatalf("parent advanced despite %s child: %v", status, err)
			}
		})
	}
	t.Run("CompletedChildStillNeedsCurrentAuthority", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		for i := range f.children {
			req := discoveryRequest(t, f, i)
			if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, putErr := tx.PutDiscoveryLessons(req); return putErr }); err != nil {
				t.Fatal(err)
			}
		}
		authority := &improvementAuthority{denied: f.children[0].Experiment.ID}
		op := parentWorker(t, f.s, f.parent, improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
			return optimization.Transition{}, optimization.ErrUnavailable
		}), authority)
		requeueParent(t, f, op)
		if err := op.WorkOnce(f.ctx, f.parent.Experiment.ID); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("parent ignored child revocation: %v", err)
		}
	})
	t.Run("DiscoveryCompletionOpensOneFrozenAllocation", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		for i := range f.children {
			req := discoveryRequest(t, f, i)
			if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, err := tx.PutDiscoveryLessons(req); return err }); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		op := parentWorker(t, f.s, f.parent, improvementStep(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
			calls++
			return optimization.Transition{}, optimization.ErrUnknownAttempt
		}))
		requeueParent(t, f, op)
		if err := op.WorkOnce(f.ctx, f.parent.Experiment.ID); err != nil {
			t.Fatalf("open validation: %v", err)
		}
		if err := op.WorkOnce(f.ctx, f.parent.Experiment.ID); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("allocate: %v", err)
		}
		page, err := f.s.ListValidationAllocations(f.ctx, f.parent.Experiment.ID, optimization.PageQuery{Limit: 100})
		if err != nil || len(page.Items) != 1 || len(page.Items[0].PairedRoster) != 2 {
			t.Fatalf("missing paired roster: %+v %v", page, err)
		}
		before := page.Items[0]
		if calls != 0 {
			t.Fatalf("replayed discovery %d times", calls)
		}
		if err = op.WorkOnce(f.ctx, f.parent.Experiment.ID); !errors.Is(err, optimization.ErrUnknownAttempt) || calls != 1 {
			t.Fatalf("validation scheduling: %v calls=%d", err, calls)
		}
		after, err := f.s.GetValidationAllocation(f.ctx, before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("allocation rewritten: %v", err)
		}
	})
}

type improvementStep func(context.Context, optimization.WorkItem) (optimization.Transition, error)

func (f improvementStep) Execute(ctx context.Context, item optimization.WorkItem) (optimization.Transition, error) {
	return f(ctx, item)
}

func parentWorker(t *testing.T, s store.Store, parent optimization.ExperimentBundle, child optimization.PhaseHandler, auth ...optimization.Authorizer) *optimization.Optimizer {
	t.Helper()
	authority := controllerAuthority{actor: parent.Experiment.Spec.Requester}
	config := optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: child, Worker: optimization.DefaultWorkerConfig()}
	if len(auth) > 0 {
		config.Authorizer = auth[0]
	}
	handler, err := optimization.NewImprovementPhaseHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Handler = handler
	op, err := optimization.NewOptimizer(config)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func requeueParent(t *testing.T, f validationFamily, op *optimization.Optimizer) {
	t.Helper()
	current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := op.Pause(f.ctx, current.ID, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = op.Resume(f.ctx, current.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
}

// ImprovementWorkerLoop exercises both arms through real controller and trial handlers.
func ImprovementWorkerLoop(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t, time.Now)
	runImprovementWorkerLoop(t, s, nil)
}

// ImprovementWorkerRecovery closes storage during discovery and after holdout allocation.
func ImprovementWorkerRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	s := open(t, time.Now)
	runImprovementWorkerLoop(t, s, func(previous store.Store, expiry time.Time) store.Store {
		if err := previous.Close(); err != nil {
			t.Fatal(err)
		}
		// The replacement process must wait for the original worker's lease to expire.
		time.Sleep(max(0, time.Until(expiry)) + 100*time.Millisecond)
		return open(t, time.Now)
	})
}

func runImprovementWorkerLoop(t *testing.T, s store.Store, reopen func(store.Store, time.Time) store.Store) {
	t.Helper()
	parent, _ := validationFamilyBundles(t, 2)
	parent.Experiment.Spec.Budget.MaxTokens = 1000
	parent.Experiment.Spec.Limits.Repetitions = 1
	for i := range parent.Experiment.Spec.Improvement.Children {
		spec := &parent.Experiment.Spec.Improvement.Children[i].Spec
		spec.Budget.MaxTokens = 1000
		spec.Limits.Repetitions, spec.Limits.MaxRounds, spec.Limits.MaxCandidates = 1, 1, 1
	}
	ctx := scoped(parent.Experiment.Spec.Scope)
	providers := [2]*streamControllerProvider{{t: t}, {t: t}}
	environments := &trialFixtureFactory{t: t, bundle: parent, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}}
	evaluator := &streamEvaluator{results: map[string]optimization.Evaluation{}}
	makeWorker := func() *optimization.Optimizer {
		authority := controllerAuthority{actor: parent.Experiment.Spec.Requester}
		trials, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Environments: environments, Evaluator: evaluator, Allowance: optimization.TrialAllowance{MaxTokens: 10}})
		if err != nil {
			t.Fatal(err)
		}
		bindings := make([]optimization.ModelBinding, 0, 2)
		for i, revision := range []string{"1", "2"} {
			bindings = append(bindings, optimization.ModelBinding{Model: "fixture", Revision: revision, Provider: providers[i], Counter: streamCounter{revision: revision}, Reporter: streamUsageReporter{}})
		}
		models, err := optimization.NewControllerFactory(s, authority, authority, bindings, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 1, MaxLessons: 1})
		if err != nil {
			t.Fatal(err)
		}
		controllers, err := optimization.NewControllerPhaseHandler(s, models, trials, "comparison")
		if err != nil {
			t.Fatal(err)
		}
		config := optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: controllers, Worker: optimization.DefaultWorkerConfig()}
		handler, err := optimization.NewImprovementPhaseHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		config.Handler = handler
		op, err := optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	op := makeWorker()
	created, err := op.Create(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = op.Start(ctx, created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	restartedDiscovery, restartedValidation := false, false
	var frozen optimization.ValidationAllocation
	for iteration := range 160 {
		if err = op.WorkOnce(ctx, created.ID); err != nil && !errors.Is(err, optimization.ErrConflict) {
			for _, template := range parent.Experiment.Spec.Improvement.Children {
				debug, debugErr := s.GetExperiment(ctx, template.ExperimentID)
				t.Logf("child %s state=%s phase=%s generation=%d parent=%d read=%v", template.ExperimentID, debug.State, debug.Phase, debug.Lease.Generation, debug.Lease.ParentGeneration, debugErr)
			}
			t.Fatalf("step %d: %v", iteration, err)
		}
		current, getErr := s.GetExperiment(ctx, created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		allocations, listErr := s.ListValidationAllocations(ctx, created.ID, optimization.PageQuery{Limit: 100})
		if listErr != nil || len(allocations.Items) > 1 {
			t.Fatalf("allocation count: %v", listErr)
		}
		if len(allocations.Items) == 1 {
			if frozen.ID.IsNil() {
				frozen = allocations.Items[0]
			}
			if frozen.ID != allocations.Items[0].ID || !reflect.DeepEqual(frozen.PairedRoster, allocations.Items[0].PairedRoster) {
				t.Fatal("restart changed frozen allocation")
			}
		}
		if reopen != nil && ((!restartedDiscovery && providers[0].reflections == 1 && providers[1].proposals == 0) || (!restartedValidation && len(allocations.Items) == 1)) {
			if len(allocations.Items) == 1 {
				restartedValidation = true
			} else {
				restartedDiscovery = true
			}
			s = reopen(s, current.Lease.ExpiresAt)
			op = makeWorker()
		}
		if current.Phase != optimization.PhaseDecide {
			continue
		}
		if environments.calls != 8 || evaluator.calls != 8 || frozen.ID.IsNil() {
			t.Fatalf("unpaired execution: executions=%d evaluations=%d", environments.calls, evaluator.calls)
		}
		for arm, provider := range providers {
			if provider.proposals != 2 || provider.reflections != 2 {
				t.Fatalf("arm %d replayed: %+v", arm, provider)
			}
		}
		for index, child := range current.Spec.Improvement.Children {
			stored, childErr := s.GetExperiment(ctx, child.ExperimentID)
			if childErr != nil || !stored.DecisionID.IsNil() || len(stored.Discoveries) != index/2 {
				t.Fatalf("child inheritance/decision: %+v %v", stored, childErr)
			}
			batch, batchErr := s.GetDiscoveryLessons(ctx, child.ExperimentID)
			if batchErr != nil || len(batch.Lessons) != 1 {
				t.Fatalf("missing private batch: %v", batchErr)
			}
			reservations, usageErr := s.ListReservations(ctx, child.ExperimentID, optimization.PageQuery{Limit: 100})
			if usageErr != nil || len(reservations.Items) != 4 {
				t.Fatalf("missing trial/controller charges: %+v %v", reservations, usageErr)
			}
		}
		if reopen != nil && (!restartedDiscovery || !restartedValidation) {
			t.Fatal("recovery checkpoints were not reached")
		}
		return
	}
	t.Fatal("parent did not finish paired validation")
}

type streamControllerProvider struct {
	t                      *testing.T
	proposals, reflections int
}

func (p *streamControllerProvider) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(request.Messages[0].Content), &data); err != nil {
		return nil, err
	}
	body := `{"proposals":[{}]}`
	if _, ok := data["baseline"]; ok {
		var input optimization.ProposalInput
		if err := json.Unmarshal([]byte(request.Messages[0].Content), &input); err != nil {
			return nil, err
		}
		if len(input.Discoveries) != p.proposals {
			p.t.Fatalf("proposal saw wrong private stream: %d after %d", len(input.Discoveries), p.proposals)
		}
		p.proposals++
	} else {
		var input optimization.ReflectionInput
		if err := json.Unmarshal([]byte(request.Messages[0].Content), &input); err != nil {
			return nil, err
		}
		if len(input.Evidence) != 1 || input.Evidence[0].Trial.Split != optimization.SplitDiscovery {
			p.t.Fatal("reflection saw holdout or missing evidence")
		}
		value, err := json.Marshal(map[string]any{"lessons": []optimization.LessonDraft{{Statement: "Check the captured outcome.", Domain: input.Domain, EvidenceIDs: []id.ID{input.Evidence[0].Trial.ID}}}})
		if err != nil {
			return nil, err
		}
		body = string(value)
		p.reflections++
	}
	return &llm.Response{Content: body, Model: "fixture", Usage: llm.Usage{TotalTokens: 2}}, nil
}
func (*streamControllerProvider) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, optimization.ErrUnavailable
}

type streamCounter struct{ revision string }

func (c streamCounter) Bound(context.Context, *llm.Request) (optimization.InputTokenBound, error) {
	return optimization.InputTokenBound{Model: "fixture", ModelRevision: c.revision, Tokens: 1, Bounded: true}, nil
}

// streamUsageReporter is a deterministic provider receipt fixture.
type streamUsageReporter struct{}

func (streamUsageReporter) Observe(_ context.Context, value optimization.ModelObservation) (optimization.ModelUsageReport, error) {
	return optimization.ModelUsageReport{Usage: optimization.Usage{Tokens: int64(value.Usage.TotalTokens), TokenQuality: optimization.UsageKnown, Receipt: "stream-receipt/" + value.Reservation.AttemptID.String()}, Complete: value.Completed}, nil
}

// streamEvaluator retains the external evaluator's idempotent job results across reopen.
type streamEvaluator struct {
	trialFixtureEvaluator
	results map[string]optimization.Evaluation
}

func (e *streamEvaluator) Evaluate(ctx context.Context, request optimization.EvaluationRequest) (optimization.Evaluation, error) {
	if saved, ok := e.results[request.IdempotencyKey]; ok {
		return saved, nil
	}
	result, err := e.trialFixtureEvaluator.Evaluate(ctx, request)
	if err != nil {
		return optimization.Evaluation{}, err
	}
	e.results[request.IdempotencyKey] = result
	return result, nil
}
