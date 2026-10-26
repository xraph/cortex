package optimization

import (
	"context"
	"fmt"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
)

// CallerIdentity attributes commands to the authenticated host caller.
type CallerIdentity interface {
	Current(context.Context) (IdentityRef, error)
}

// OptimizerConfig supplies explicit persistence, authority and phase execution.
// A Handler must reconcile durable external attempts before dispatching effects.
type OptimizerConfig struct {
	Repository              Repository
	Authorizer              Authorizer
	IdentityResolver        IdentityResolver
	Caller                  CallerIdentity
	Lessons                 LessonStore
	Compatibility           Compatibility
	ControllerCompatibility Compatibility
	ArtifactVerifier        *ArtifactVerifier
	TaskEvaluator           Evaluator
	ImprovementEvaluator    Evaluator
	Handler                 PhaseHandler
	UsageRecovery           *UsageRecovery
	Worker                  WorkerConfig
}

// Optimizer provides experiment commands and a bounded, host-started worker.
type Optimizer struct {
	activeEngine    *engine.Engine
	artifactAdopter ArtifactAdopter
	config          OptimizerConfig
	meter           *LedgerMeter
	owner           string
	work            sync.Mutex
}

// NewOptimizer never starts background work. The host runs Work with an exact
// scoped context and cancels it during shutdown. Each instance has a unique owner.
func NewOptimizer(config OptimizerConfig, options ...Option) (*Optimizer, error) {
	if config.Repository == nil || config.Caller == nil || config.Handler == nil || config.Worker.validate() != nil {
		return nil, ErrInvalidSpec
	}
	meter, err := NewLedgerMeter(config.Repository, config.Authorizer, config.IdentityResolver)
	if err != nil {
		return nil, err
	}
	value := &Optimizer{config: config, meter: meter, owner: id.New(PrefixEvent).String()}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalidSpec
		}
		if err := option(value); err != nil {
			return nil, err
		}
	}
	return value, nil
}

func (o *Optimizer) caller(ctx context.Context) (IdentityRef, error) {
	return currentCaller(ctx, o.config.Caller)
}

func currentCaller(ctx context.Context, caller CallerIdentity) (IdentityRef, error) {
	if err := ctx.Err(); err != nil {
		return IdentityRef{}, err
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		return IdentityRef{}, cortex.ErrNoScope
	}
	if _, err := ScopeKey(cortex.ScopeFromContext(ctx)); err != nil {
		return IdentityRef{}, err
	}
	actor, err := caller.Current(copyScopeContext(ctx))
	if err != nil {
		return IdentityRef{}, err
	}
	if !ValidOpaqueKey(actor.Subject) {
		return IdentityRef{}, ErrPolicyDenied
	}
	return actor, nil
}

// Create saves a host-frozen bundle, including its immutable versions and plan.
func (o *Optimizer) Create(ctx context.Context, bundle ExperimentBundle) (Experiment, error) {
	copyBundle, err := o.creationBundle(ctx, bundle)
	if err != nil {
		return Experiment{}, err
	}
	if createErr := o.config.Repository.Transact(ctx, func(tx Transaction) error { return tx.CreateExperiment(*copyBundle) }); createErr != nil {
		return Experiment{}, createErr
	}
	return o.config.Repository.GetExperiment(ctx, copyBundle.Experiment.ID)
}

func (o *Optimizer) creationBundle(ctx context.Context, bundle ExperimentBundle) (*ExperimentBundle, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return nil, err
	}
	copyBundle, err := copyModelJSON(&bundle, 16<<20)
	if err != nil {
		return nil, err
	}
	if authErr := o.authorizeCreation(ctx, copyBundle.Experiment); authErr != nil {
		return nil, authErr
	}
	return copyBundle, nil
}

func (o *Optimizer) authorizeCreation(ctx context.Context, experiment Experiment) error {
	actor, err := o.caller(ctx)
	if err != nil {
		return err
	}
	if experiment.Spec.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || experiment.Spec.Requester != actor {
		return ErrPolicyDenied
	}
	resource := Resource{Scope: copyScope(experiment.Spec.Scope), AgentID: experiment.Spec.AgentID, ID: experiment.ID, Kind: "experiment"}
	if authErr := o.config.Authorizer.Authorize(copyScopeContext(ctx), ActionCreate, resource); authErr != nil {
		return authErr
	}
	if experiment.Spec.Parent != nil {
		_, done, authErr := o.meter.authorizeAction(ctx, experiment.Spec.Parent.ExperimentID, ActionCreate)
		if authErr != nil {
			return authErr
		}
		done()
	}
	return nil
}

// Get reads one experiment after checking its exact scope and current authority.
func (o *Optimizer) Get(ctx context.Context, experimentID id.ID) (Experiment, error) {
	return o.readExperiment(ctx, experimentID)
}

// List reads one bounded page in an explicitly authorized exact scope.
func (o *Optimizer) List(ctx context.Context, query ListQuery) (Page[Experiment], error) {
	page, err := o.ListExperiments(ctx, query)
	return Page[Experiment]{Items: page.Items, NextCursor: page.NextCursor}, err
}

// Start queues a frozen draft and returns without running the controller.
func (o *Optimizer) Start(ctx context.Context, experimentID id.ID, revision int64) (Experiment, error) {
	return o.command(ctx, experimentID, revision, ActionStart, StateQueued)
}

// Cancel invalidates running authority immediately; worker polling cancels active work.
func (o *Optimizer) Cancel(ctx context.Context, experimentID id.ID, revision int64) (Experiment, error) {
	return o.command(ctx, experimentID, revision, ActionCancel, StateCancelled)
}

// Pause stops scheduling while retaining the frozen phase, deadline and history.
func (o *Optimizer) Pause(ctx context.Context, experimentID id.ID, revision int64) (Experiment, error) {
	return o.command(ctx, experimentID, revision, ActionCancel, StatePaused)
}

// Resume revalidates the original requester and queues the same frozen phase.
func (o *Optimizer) Resume(ctx context.Context, experimentID id.ID, revision int64) (Experiment, error) {
	return o.command(ctx, experimentID, revision, ActionResume, StateQueued)
}

func (o *Optimizer) authorizeCommand(ctx context.Context, experimentID id.ID, action Action) (Experiment, IdentityRef, error) {
	actor, err := o.caller(ctx)
	if err != nil {
		return Experiment{}, actor, err
	}
	value, err := o.config.Repository.GetExperiment(copyScopeContext(ctx), experimentID)
	if err != nil {
		return Experiment{}, actor, err
	}
	if value.ID != experimentID || value.Spec.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return Experiment{}, actor, ErrPolicyDenied
	}
	err = o.config.Authorizer.Authorize(copyScopeContext(ctx), action, Resource{Scope: copyScope(value.Spec.Scope), AgentID: value.Spec.AgentID, ID: experimentID, Kind: "experiment"})
	return value, actor, err
}

func (o *Optimizer) command(ctx context.Context, experimentID id.ID, revision int64, action Action, next State) (Experiment, error) {
	change := Transition{ExperimentID: experimentID, ExpectedRevision: revision, NextState: next, IdempotencyKey: fmt.Sprintf("command/%s/%s/%d", action, next, revision)}
	return o.executeCommand(ctx, change, action)
}

func (o *Optimizer) executeCommand(ctx context.Context, change Transition, action Action) (Experiment, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Experiment{}, err
	}
	callerContext := ctx
	_, actor, err := o.authorizeCommand(ctx, change.ExperimentID, action)
	if err != nil {
		return Experiment{}, err
	}
	if action == ActionStart || action == ActionResume {
		resolved, done, resolveErr := o.meter.authorizeAction(ctx, change.ExperimentID, action)
		if resolveErr != nil {
			return Experiment{}, resolveErr
		}
		defer done()
		ctx = resolved
		_, currentActor, authErr := o.authorizeCommand(callerContext, change.ExperimentID, action)
		if authErr != nil {
			return Experiment{}, authErr
		}
		if currentActor != actor {
			return Experiment{}, ErrPolicyDenied
		}
	}
	change.Actor = actor
	if commitErr := o.config.Repository.Transact(ctx, func(tx Transaction) error { return tx.CommitTransition(change) }); commitErr != nil {
		return Experiment{}, commitErr
	}
	return o.config.Repository.GetExperiment(ctx, change.ExperimentID)
}
