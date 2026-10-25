package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type parallelCall struct {
	release chan struct{}
	done    chan struct{}
}

// Serialize fixture bookkeeping only. Gates keep real adapter calls overlapping.
type parallelFactory struct {
	mu      sync.Mutex
	fixture *trialFixtureFactory
	gate    atomic.Bool
	started chan parallelCall
}

func (f *parallelFactory) Open(ctx context.Context, r optimization.TrialRequest) (optimization.TrialEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	environment, err := f.fixture.Open(ctx, r)
	return &parallelEnvironment{TrialEnvironment: environment, factory: f}, err
}
func (f *parallelFactory) Recover(ctx context.Context, r optimization.EnvironmentReceipt) (optimization.TrialEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	environment, err := f.fixture.Recover(ctx, r)
	return &parallelEnvironment{TrialEnvironment: environment, factory: f}, err
}
func (f *parallelFactory) Stop(ctx context.Context, r optimization.EnvironmentReceipt, fence optimization.Fence) (optimization.StopReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fixture.Stop(ctx, r, fence)
}

type parallelEnvironment struct {
	optimization.TrialEnvironment
	factory *parallelFactory
}

func (e *parallelEnvironment) Execute(ctx context.Context, p *engine.PreparedAgent, input string, opts engine.PreparedRunOptions) (*optimization.ExecutionRecord, error) {
	if e.factory.gate.Load() {
		call := parallelCall{release: make(chan struct{}), done: make(chan struct{})}
		e.factory.started <- call
		defer close(call.done)
		select {
		case <-call.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e.factory.mu.Lock()
	defer e.factory.mu.Unlock()
	return e.TrialEnvironment.Execute(ctx, p, input, opts)
}
func (e *parallelEnvironment) Inspect(ctx context.Context, attempt id.ID) (optimization.AttemptStatus, error) {
	e.factory.mu.Lock()
	defer e.factory.mu.Unlock()
	return e.TrialEnvironment.Inspect(ctx, attempt)
}

type parallelEvaluator struct {
	trialFixtureEvaluator
	mu sync.Mutex
}

func (e *parallelEvaluator) Evaluate(ctx context.Context, r optimization.EvaluationRequest) (optimization.Evaluation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.trialFixtureEvaluator.Evaluate(ctx, r)
}

type parallelScenario struct {
	store   store.Store
	ctx     context.Context
	bundle  optimization.ExperimentBundle
	factory *parallelFactory
	op      *optimization.Optimizer
}

func newParallelScenario(t *testing.T, s store.Store, budget int64) *parallelScenario {
	t.Helper()
	bundle := Fixture(t, "parallel")
	bundle.Experiment.Spec.Mode = optimization.ModeParallel
	bundle.Experiment.Spec.Limits.Concurrency = 2
	bundle.Experiment.Spec.Budget.MaxTokens = budget
	factory := &parallelFactory{fixture: &trialFixtureFactory{t: t, bundle: bundle, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}}, started: make(chan parallelCall, 4)}
	p := &parallelScenario{store: s, ctx: scoped(bundle.Experiment.Spec.Scope), bundle: bundle, factory: factory}
	p.bind(t)
	created, err := p.op.Create(p.ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.op.Start(p.ctx, created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *parallelScenario) bind(t *testing.T) {
	t.Helper()
	authority := controllerAuthority{actor: p.bundle.Experiment.Spec.Requester}
	handler, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: p.store, Authorizer: authority, IdentityResolver: authority, Environments: p.factory, Evaluator: &parallelEvaluator{}, Allowance: optimization.TrialAllowance{MaxTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	p.op, err = optimization.NewOptimizer(optimization.OptimizerConfig{Repository: p.store, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
}

func (p *parallelScenario) trials(t *testing.T) []optimization.Trial {
	t.Helper()
	page, err := p.store.ListTrials(p.ctx, p.bundle.Experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func (p *parallelScenario) ready(t *testing.T) {
	t.Helper()
	for range 10 {
		ready := 0
		for _, trial := range p.trials(t) {
			if len(trial.Attempts) == 1 && trial.Attempts[0].Receipt != nil {
				ready++
			}
		}
		if ready == 2 {
			return
		}
		if err := p.op.WorkOnce(p.ctx, p.bundle.Experiment.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("two attempts were not admitted together")
}

func (p *parallelScenario) dispatch(t *testing.T, reverse bool) error {
	t.Helper()
	p.factory.gate.Store(true)
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.op.WorkOnce(ctx, p.bundle.Experiment.ID) }()
	calls := make([]parallelCall, 0, 2)
	for range 2 {
		select {
		case call := <-p.factory.started:
			calls = append(calls, call)
		case err := <-done:
			t.Fatalf("batch returned before two dispatches: %v", err)
		case <-ctx.Done():
			t.Fatal("trial dispatches did not overlap")
		}
	}
	select {
	case <-p.factory.started:
		t.Fatal("concurrency limit exceeded")
	default:
	}
	if reverse {
		calls[0], calls[1] = calls[1], calls[0]
	}
	for _, call := range calls {
		close(call.release)
		select {
		case <-call.done:
		case <-ctx.Done():
			t.Fatal("released trial did not finish")
		}
	}
	err := <-done
	p.factory.gate.Store(false)
	return err
}

func (p *parallelScenario) finish(t *testing.T) {
	t.Helper()
	for range 20 {
		current, err := p.store.GetExperiment(p.ctx, p.bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Phase == optimization.PhasePropose {
			var tokens int64
			for _, trial := range p.trials(t) {
				if trial.Execution == nil || trial.EvaluationID == "" || len(trial.Attempts) != 1 || trial.Execution.Outcome.Output != "Discovery task" {
					t.Fatalf("missing or replayed parallel evidence: %+v", trial)
				}
				tokens += trial.Execution.Usage.Tokens
			}
			p.checkEvidence(t, tokens)
			if p.factory.fixture.calls != p.bundle.Experiment.Spec.Limits.Repetitions {
				t.Fatal("parallel work replayed a completed dispatch")
			}
			return
		}
		if err = p.op.WorkOnce(p.ctx, current.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("parallel baseline did not finish")
}

func (p *parallelScenario) checkEvidence(t *testing.T, tokens int64) {
	t.Helper()
	reservations, err := p.store.ListReservations(p.ctx, p.bundle.Experiment.ID, optimization.PageQuery{Limit: 100})
	if err != nil || len(reservations.Items) != p.bundle.Experiment.Spec.Limits.Repetitions {
		t.Fatalf("parallel reservation count: %+v %v", reservations, err)
	}
	var debited int64
	for _, reservation := range reservations.Items {
		if reservation.State != optimization.ReservationSettled || reservation.Call == nil || reservation.Settlement == nil {
			t.Fatalf("parallel call did not settle: %+v", reservation)
		}
		debited += reservation.DebitedTokens
	}
	if debited != tokens {
		t.Fatalf("completion order changed accounting: debited=%d captured=%d", debited, tokens)
	}
	evaluations, err := p.store.ListEvaluations(p.ctx, p.bundle.Experiment.ID, optimization.PageQuery{Limit: 100})
	if err != nil || len(evaluations.Items) != p.bundle.Experiment.Spec.Limits.Repetitions {
		t.Fatalf("parallel evaluation count: %+v %v", evaluations, err)
	}
	for _, evaluation := range evaluations.Items {
		if !evaluation.Complete || evaluation.Score == nil || *evaluation.Score != 1 {
			t.Fatalf("completion order changed scoring: %+v", evaluation)
		}
	}
}

func parallelTrials(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ParallelOrderIndependentEvidence", func(t *testing.T) {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprint(reverse), func(t *testing.T) {
				p := newParallelScenario(t, factory(t, time.Now), 1000)
				p.ready(t)
				if err := p.dispatch(t, reverse); err != nil {
					t.Fatal(err)
				}
				p.finish(t)
			})
		}
	})
	t.Run("ParallelRetainsSuccessfulSibling", func(t *testing.T) {
		p := newParallelScenario(t, factory(t, time.Now), 1000)
		p.ready(t)
		p.factory.fixture.lost = true
		if err := p.dispatch(t, true); !errors.Is(err, optimization.ErrUnknownAttempt) {
			t.Fatalf("lost response: %v", err)
		}
		captured := 0
		for _, trial := range p.trials(t) {
			if trial.Execution != nil {
				captured++
			}
		}
		if captured != 1 {
			t.Fatalf("successful sibling was discarded: %d captured", captured)
		}
		p.finish(t)
	})
	t.Run("ParallelReservationsBoundConcurrency", func(t *testing.T) {
		p := newParallelScenario(t, factory(t, time.Now), 100)
		p.ready(t)
		err := p.op.WorkOnce(p.ctx, p.bundle.Experiment.ID)
		if !errors.Is(err, optimization.ErrBudgetExceeded) || p.factory.fixture.calls != 1 {
			t.Fatalf("shared budget admission: calls=%d err=%v", p.factory.fixture.calls, err)
		}
		captured := 0
		for _, trial := range p.trials(t) {
			if trial.Execution != nil {
				captured++
			}
		}
		current, err := p.store.GetExperiment(p.ctx, p.bundle.Experiment.ID)
		if err != nil || current.State != optimization.StateRunning || captured != 1 {
			t.Fatalf("partial budget failure lost recoverable work: %+v captured=%d err=%v", current, captured, err)
		}
	})
	t.Run("ParallelBudgetFailureCannotHideUnknownAttempt", func(t *testing.T) {
		p := newParallelScenario(t, factory(t, time.Now), 100)
		p.ready(t)
		p.factory.fixture.lost = true
		err := p.op.WorkOnce(p.ctx, p.bundle.Experiment.ID)
		if !errors.Is(err, optimization.ErrUnknownAttempt) || !errors.Is(err, optimization.ErrBudgetExceeded) || p.factory.fixture.calls != 1 {
			t.Fatalf("mixed batch failure: calls=%d err=%v", p.factory.fixture.calls, err)
		}
		current, err := p.store.GetExperiment(p.ctx, p.bundle.Experiment.ID)
		if err != nil || current.State != optimization.StateRunning {
			t.Fatalf("budget failure closed unknown work: %+v %v", current, err)
		}
		// The successful remote call is recovered first; its dispatch is never replayed.
		err = p.op.WorkOnce(p.ctx, p.bundle.Experiment.ID)
		if !errors.Is(err, optimization.ErrBudgetExceeded) || p.factory.fixture.calls != 1 {
			t.Fatalf("recovery replayed a charged call: calls=%d err=%v", p.factory.fixture.calls, err)
		}
		captured := 0
		for _, trial := range p.trials(t) {
			if trial.Execution != nil {
				captured++
			}
		}
		if captured != 1 {
			t.Fatal("recovered sibling was lost to concurrent budget refusal")
		}
	})
	t.Run("ParallelCancellationPreservesDispatchMarkers", func(t *testing.T) {
		p := newParallelScenario(t, factory(t, time.Now), 1000)
		p.ready(t)
		p.factory.gate.Store(true)
		ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.op.WorkOnce(ctx, p.bundle.Experiment.ID) }()
		for range 2 {
			select {
			case <-p.factory.started:
			case <-ctx.Done():
				t.Fatal("parallel calls never reached cancellation gate")
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled parallel phase: %v", err)
		}
		p.factory.gate.Store(false)
		reservations, err := p.store.ListReservations(p.ctx, p.bundle.Experiment.ID, optimization.PageQuery{Limit: 100})
		if err != nil || len(reservations.Items) != 2 {
			t.Fatalf("cancelled dispatches lost: %+v %v", reservations, err)
		}
		for _, reservation := range reservations.Items {
			if reservation.Call == nil || reservation.DebitedTokens != 100 || reservation.State == optimization.ReservationSettled {
				t.Fatalf("unknown call refunded after cancellation: %+v", reservation)
			}
		}
		if err = p.op.WorkOnce(p.ctx, p.bundle.Experiment.ID); !errors.Is(err, optimization.ErrUnknownAttempt) || p.factory.fixture.calls != 0 {
			t.Fatalf("cancelled dispatch was replayed: calls=%d err=%v", p.factory.fixture.calls, err)
		}
	})
}

// ParallelRecovery reopens a database after a mixed batch result, takes over its
// lease, and inspects the remote attempt without repeating either dispatch.
func ParallelRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	p := newParallelScenario(t, open(t, time.Now), 1000)
	p.ready(t)
	p.factory.fixture.lost = true
	if err := p.dispatch(t, true); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatalf("lost parallel response: %v", err)
	}
	current, err := p.store.GetExperiment(p.ctx, p.bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := p.op.Pause(p.ctx, current.ID, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.store.Close(); err != nil {
		t.Fatal(err)
	}
	p.store = open(t, time.Now)
	p.bind(t)
	if _, err = p.op.Resume(p.ctx, current.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
	p.finish(t)
	final, err := p.store.GetExperiment(p.ctx, current.ID)
	if err != nil || final.Lease.Generation <= current.Lease.Generation {
		t.Fatalf("reopened worker did not advance fencing: %+v %v", final.Lease, err)
	}
}
