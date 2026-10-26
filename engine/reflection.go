package engine

import (
	"context"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/run"
)

// ReflectionObservationInput exposes execution identity and cumulative usage,
// never prompts or private reasoning. Its binding cannot be replaced by a model.
type ReflectionObservationInput struct {
	Binding cognitive.ReflectionBinding
	Step    int
	Usage   execution.Usage
}

// ReflectionObserver resolves authenticated progress and retained evidence.
// No model or side-effect work may run here. Missing observations fail closed.
type ReflectionObserver interface {
	ObserveReflection(context.Context, ReflectionObservationInput) (cognitive.ReflectionSnapshot, error)
}

// ReflectionFence excludes concurrent user correction and authority revocation
// until its single synchronous callback returns. All host mutation paths must
// share this fence. It is not a substitute for normal tool or model authorization.
type ReflectionFence interface {
	WithReflectionFence(context.Context, cognitive.ReflectionBinding, func(cognitive.ReflectionBinding) error) error
}

// ReflectionAuthorizer checks current caller grants before reads and decisions,
// including evidence disclosure and the meaning of trusted progress observations.
type ReflectionAuthorizer interface {
	AuthorizeReflectionRead(context.Context, cognitive.ReflectionBinding) error
	AuthorizeReflection(context.Context, cognitive.ReflectionRequest) error
}

// ReflectionReplanSource supplies immutable proposal input from the retained
// decision. It must not dispatch work. The shared plan must match current task
// ownership; PlanReplanner still admits budget and rechecks all live authority.
type ReflectionReplanSource interface {
	ResolveReflectionReplan(context.Context, cognitive.ReflectionReceipt) (*coordination.PlanReplanRequest, error)
}

// ReflectionConfig enables deterministic reflection for normal prepared runs.
// Proposal runs keep evaluation purpose and never recursively reflect. The
// optional replan path uses the existing shared-budget prepared runtime.
type ReflectionConfig struct {
	Policy       cognitive.ReflectionPolicy
	Observer     ReflectionObserver
	Fence        ReflectionFence
	Authorizer   ReflectionAuthorizer
	Replanner    *coordination.PlanReplanner
	ReplanSource ReflectionReplanSource
	Timeout      time.Duration
}

// WithReflection validates opt-in strategy support without changing legacy
// persona behavior. Decisions and phase transitions require durable storage.
func WithReflection(config ReflectionConfig) Option {
	return func(e *Engine) error {
		policy, err := config.Policy.Normalize()
		if err != nil {
			return err
		}
		config.Policy = policy
		if config.Timeout == 0 {
			config.Timeout = 30 * time.Second
		}
		if hostport.IsNil(config.Observer) || hostport.IsNil(config.Fence) || hostport.IsNil(config.Authorizer) || config.Timeout < time.Millisecond || config.Timeout > 30*time.Second {
			return cognitive.ErrInvalidReflection
		}
		if policy.OnStuck == cognitive.ActionReplan && (config.Replanner == nil || hostport.IsNil(config.ReplanSource)) {
			return cognitive.ErrUnsupportedReflection
		}
		e.reflection = &config
		return nil
	}
}
func reflectionBinding(r *run.Run) cognitive.ReflectionBinding {
	return cognitive.ReflectionBinding{Scope: cortex.Scope{Levels: append([]cortex.Level(nil), r.Scope.Levels...)}, RunID: r.ID, AgentID: r.AgentID, PreparedDigest: r.Provenance.PreparedDigest, Budget: r.Budget, VerificationRequired: r.Provenance.VerificationRequired}
}

// reflectionBoundary runs before either ReAct transport starts a provider step.
// The returned stop does not certify completion. Actionable receipts are never
// replayed after a lost acknowledgement or failed callback.
func (e *Engine) reflectionBoundary(ctx context.Context, r *run.Run, st *reactState) (bool, error) {
	if e.reflection == nil || r.Provenance.Purpose != run.PurposeNormal {
		return false, nil
	}
	if st.preparedSnapshot == nil {
		return true, cognitive.ErrInvalidReflection
	}
	receipt, err := e.reflectionDecision(ctx, r, st.stepIndex, st.usage, nil, false)
	if err != nil {
		reflectionMetadata(r, cognitive.ReflectionDecision{Action: cognitive.ActionStop, Reason: cognitive.ReflectionInvalid})
		return true, err
	}
	if receipt.Decision.Action == cognitive.ActionContinue {
		return false, nil
	}
	reflectionMetadata(r, receipt.Decision)
	return true, nil
}
func reflectionMetadata(r *run.Run, d cognitive.ReflectionDecision) {
	metadata := make(map[string]any, len(r.Metadata)+2)
	for k, v := range r.Metadata {
		metadata[k] = v
	}
	metadata["reflection_action"] = string(d.Action)
	metadata["reflection_reason"] = string(d.Reason)
	r.Metadata = metadata
}
func (e *Engine) reflectionDecision(ctx context.Context, r *run.Run, step int, usage execution.Usage, intervention *cognitive.ContextSource, hadError bool) (cognitive.ReflectionReceipt, error) {
	config := e.reflection
	if config == nil {
		return cognitive.ReflectionReceipt{}, cognitive.ErrUnsupportedReflection
	}
	storage, ok := e.store.(cognitive.ReflectionStore)
	if !ok {
		return cognitive.ReflectionReceipt{}, cognitive.ErrInvalidReflection
	}
	binding := reflectionBinding(r)
	if binding.Validate() != nil || binding.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return cognitive.ReflectionReceipt{}, cognitive.ErrInvalidReflection
	}
	bounded, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	fenceBinding := binding
	fenceBinding.Scope.Levels = append([]cortex.Level(nil), binding.Scope.Levels...)
	return hostport.WithSynchronousFence(bounded, cognitive.ErrReflectionConflict, func(tc context.Context, next func(cognitive.ReflectionBinding) error) error {
		return config.Fence.WithReflectionFence(tc, fenceBinding, next)
	}, func(tc context.Context, current cognitive.ReflectionBinding) (cognitive.ReflectionReceipt, error) {
		if !reflect.DeepEqual(current, binding) {
			return cognitive.ReflectionReceipt{}, cognitive.ErrReflectionConflict
		}
		readBinding := binding
		readBinding.Scope.Levels = append([]cortex.Level(nil), binding.Scope.Levels...)
		if err := config.Authorizer.AuthorizeReflectionRead(tc, readBinding); err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		if err := tc.Err(); err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		// Refresh the actual run revision inside the same user/authority fence.
		saved, err := e.store.GetRun(tc, r.ID)
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		if !reflect.DeepEqual(reflectionBinding(saved), binding) {
			return cognitive.ReflectionReceipt{}, cognitive.ErrReflectionConflict
		}
		sharedPlan, err := e.reflectionSharedPlan(tc, saved)
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		binding.SharedPlanID = sharedPlan
		previous, err := storage.GetReflection(tc, r.ID)
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		var before cognitive.ReflectionState
		if previous != nil {
			if previous.Validate() != nil {
				return cognitive.ReflectionReceipt{}, cognitive.ErrInvalidReflection
			}
			before = previous.State
		}
		if intervention != nil && previous != nil {
			step = max(step, before.LastStep)
			usage.Steps = max(usage.Steps, previous.Request.Snapshot.Usage.Steps)
			usage.Tokens = max(usage.Tokens, previous.Request.Snapshot.Usage.Tokens)
			usage.ReportedTokens = max(usage.ReportedTokens, previous.Request.Snapshot.Usage.ReportedTokens)
			usage.ToolCalls = max(usage.ToolCalls, previous.Request.Snapshot.Usage.ToolCalls)
		}
		offered := ReflectionObservationInput{Binding: binding, Step: step, Usage: usage}
		offered.Binding.Scope.Levels = append([]cortex.Level(nil), binding.Scope.Levels...)
		snapshot, err := config.Observer.ObserveReflection(tc, offered)
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		if !reflect.DeepEqual(snapshot.Binding, binding) || snapshot.Step != step || snapshot.Usage != usage {
			return cognitive.ReflectionReceipt{}, cognitive.ErrReflectionConflict
		}
		snapshot.HadError = snapshot.HadError || hadError
		request := cognitive.ReflectionRequest{SchemaVersion: 1, ExpectedVersion: before.Version, ExpectedControlRevision: before.ControlRevision, ExpectedRunRevision: saved.Revision, Snapshot: snapshot, Policy: config.Policy, Intervention: intervention}
		frozen, digest, err := request.Freeze()
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		authorized, _, err := frozen.Freeze()
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		if authorizeErr := config.Authorizer.AuthorizeReflection(tc, authorized); authorizeErr != nil {
			return cognitive.ReflectionReceipt{}, authorizeErr
		}
		if contextErr := tc.Err(); contextErr != nil {
			return cognitive.ReflectionReceipt{}, contextErr
		}
		receipt, err := storage.AdmitReflection(tc, frozen)
		if err != nil {
			return cognitive.ReflectionReceipt{}, err
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return cognitive.ReflectionReceipt{}, cognitive.ErrReflectionConflict
		}
		if !receipt.Fresh {
			return receipt, nil
		}
		if receipt.Decision.PhaseChanged {
			e.extensions.EmitCognitivePhaseChanged(tc, r.ID, string(frozen.Policy.Phases[before.Phase].Strategy), string(frozen.Policy.Phases[receipt.State.Phase].Strategy))
		}
		if contextErr := tc.Err(); contextErr != nil {
			return cognitive.ReflectionReceipt{}, contextErr
		}
		if receipt.Decision.Action == cognitive.ActionReplan {
			if binding.SharedPlanID.IsNil() || config.Replanner == nil || hostport.IsNil(config.ReplanSource) {
				return receipt, cognitive.ErrUnsupportedReflection
			}
			detached, _, freezeErr := receipt.Request.Freeze()
			if freezeErr != nil {
				return receipt, freezeErr
			}
			offeredReceipt := receipt
			offeredReceipt.Request = detached
			proposal, resolveErr := config.ReplanSource.ResolveReflectionReplan(tc, offeredReceipt)
			if resolveErr != nil {
				return receipt, resolveErr
			}
			if proposal == nil {
				return receipt, nil
			}
			proposed, _, freezeErr := proposal.Freeze()
			if freezeErr != nil {
				return receipt, freezeErr
			}
			if proposed.Progress.Plan.ID != binding.SharedPlanID || proposed.Progress.Plan.Scope.Canonical() != binding.Scope.Canonical() {
				return receipt, cognitive.ErrReflectionConflict
			}
			_, dispatchErr := config.Replanner.Execute(tc, proposed)
			if dispatchErr != nil {
				return receipt, dispatchErr
			}
		}
		return receipt, nil
	})
}
func (e *Engine) reflectionSharedPlan(ctx context.Context, r *run.Run) (id.ID, error) {
	if r.Provenance.TaskClaimID == "" {
		return id.ID{}, nil
	}
	claimID, err := id.ParseWithPrefix(r.Provenance.TaskClaimID, coordination.PrefixClaim)
	if err != nil {
		return id.ID{}, err
	}
	budgetStore, ok := e.store.(coordination.BudgetedClaimStore)
	if !ok {
		return id.ID{}, cognitive.ErrInvalidReflection
	}
	claims, ok := e.store.(coordination.ClaimStore)
	if !ok {
		return id.ID{}, cognitive.ErrInvalidReflection
	}
	acquisition, err := budgetStore.GetBudgetedTaskClaim(ctx, claimID)
	if err != nil {
		return id.ID{}, err
	}
	current, err := claims.GetTaskClaim(ctx, acquisition.Claim.PlanID, acquisition.Claim.TaskID)
	if err != nil {
		return id.ID{}, err
	}
	if current.Validate() != nil || current.ID != claimID || current.RunID != r.ID || current.Scope.Canonical() != r.Scope.Canonical() || current.State != coordination.ClaimDispatching || !time.Now().Before(current.ExpiresAt) {
		return id.ID{}, cognitive.ErrReflectionConflict
	}
	return current.PlanID, nil
}

// InterveneReflection retains a user correction under the current authority
// fence. It supersedes pending controller action without resuming a run, changing
// its prepared input or resetting consumed attempts. Resume still uses its own
// existing checks and a terminal run requires a new explicitly prepared run.
func (e *Engine) InterveneReflection(ctx context.Context, runID id.AgentRunID, source cognitive.ContextSource) (cognitive.ReflectionReceipt, error) {
	if e.reflection == nil {
		return cognitive.ReflectionReceipt{}, cognitive.ErrUnsupportedReflection
	}
	// The host receives only scoped identity before the run is loaded.
	if _, err := cortex.RequireScope(ctx); err != nil {
		return cognitive.ReflectionReceipt{}, err
	}
	if err := e.reflection.Authorizer.AuthorizeReflectionRead(ctx, cognitive.ReflectionBinding{Scope: cortex.ScopeFromContext(ctx), RunID: runID}); err != nil {
		return cognitive.ReflectionReceipt{}, err
	}
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return cognitive.ReflectionReceipt{}, err
	}
	return e.reflectionDecision(ctx, r, r.StepCount, r.Usage, &source, false)
}

func (e *Engine) reflectionErrorBoundary(ctx context.Context, r *run.Run) {
	if e.reflection == nil || r.Provenance.Purpose != run.PurposeNormal || ctx.Err() != nil {
		return
	}
	runtime, ok := ctx.Value(operationalContextKey{}).(operationalRuntime)
	if !ok || runtime.runID != r.ID || runtime.state == nil || runtime.state.preparedSnapshot == nil {
		return
	}
	receipt, err := e.reflectionDecision(ctx, r, runtime.state.stepIndex, runtime.state.usage, nil, true)
	if err == nil && receipt.Decision.Action != cognitive.ActionContinue {
		reflectionMetadata(r, receipt.Decision)
	}
}
