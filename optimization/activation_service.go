package optimization

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ActivationRequest identifies a reviewed version and the expected route generation.
type ActivationRequest struct {
	Kind               HarnessKind `json:"kind,omitempty"`
	ID                 id.ID       `json:"id"`
	AgentID            id.AgentID  `json:"agent_id"`
	Channel            string      `json:"channel"`
	Version            VersionRef  `json:"version"`
	DecisionID         id.ID       `json:"decision_id"`
	ExpectedGeneration int64       `json:"expected_generation"`
	Reason             string      `json:"reason"`
}

// RollbackRequest appends a generation referring to a previous activation.
type RollbackRequest struct {
	Kind               HarnessKind `json:"kind,omitempty"`
	ID                 id.ID       `json:"id"`
	AgentID            id.AgentID  `json:"agent_id"`
	Channel            string      `json:"channel"`
	TargetID           id.ID       `json:"target_id"`
	ExpectedGeneration int64       `json:"expected_generation"`
	Reason             string      `json:"reason"`
}

// DeactivationRequest disables a route without removing its history.
type DeactivationRequest struct {
	Kind               HarnessKind `json:"kind,omitempty"`
	ID                 id.ID       `json:"id"`
	AgentID            id.AgentID  `json:"agent_id"`
	Channel            string      `json:"channel"`
	ExpectedGeneration int64       `json:"expected_generation"`
	Reason             string      `json:"reason"`
}

// Activate rechecks current authority, compatibility and retained evidence.
func (o *Optimizer) Activate(ctx context.Context, request ActivationRequest) (*Activation, error) {
	change := ActivationChange{Kind: request.Kind, ID: request.ID, AgentID: request.AgentID, Channel: request.Channel, Version: request.Version, DecisionID: request.DecisionID, ExpectedGeneration: request.ExpectedGeneration, Reason: request.Reason}
	return o.activate(ctx, change, ActionActivate)
}

// Rollback revalidates a previously activated version under current policy.
func (o *Optimizer) Rollback(ctx context.Context, request RollbackRequest) (*Activation, error) {
	if _, err := o.authorizeActivationKind(ctx, request.AgentID, ActionRollback, request.Kind); err != nil {
		return nil, err
	}
	channel, err := ActivationChannel(request.Channel)
	if err != nil {
		return nil, err
	}
	previous, err := o.config.Repository.GetActivationGeneration(copyScopeContext(ctx), request.TargetID)
	if err != nil {
		return nil, err
	}
	kind, kindErr := ActivationKind(request.Kind)
	if kindErr != nil {
		return nil, kindErr
	}
	previousKind, kindErr := ActivationKind(previous.Kind)
	if kindErr != nil {
		return nil, kindErr
	}
	if previousKind != kind || previous.Deactivated || previous.AgentID != request.AgentID || previous.Channel != channel || previous.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, ErrIncompleteEvidence
	}
	return o.activate(ctx, ActivationChange{Kind: kind, ID: request.ID, AgentID: request.AgentID, Channel: channel, Version: previous.Version, DecisionID: previous.DecisionID, ExpectedGeneration: request.ExpectedGeneration, RollbackTo: previous.ID, Reason: request.Reason}, ActionRollback)
}

func (o *Optimizer) activate(ctx context.Context, change ActivationChange, action Action) (*Activation, error) {
	actor, err := o.authorizeActivationKind(ctx, change.AgentID, action, change.Kind)
	if err != nil {
		return nil, err
	}
	if !validActivationReason(change.Reason) {
		return nil, ErrInvalidSpec
	}
	if o.config.ArtifactVerifier == nil {
		return nil, ErrUnavailable
	}
	decision, err := o.config.Repository.GetDecision(copyScopeContext(ctx), change.DecisionID)
	if err != nil {
		return nil, err
	}
	resolved, cleanup, err := o.meter.authorizeAction(ctx, decision.ExperimentID, action)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if childErr := o.authorizeControllerDecision(ctx, decision, action); childErr != nil {
		return nil, childErr
	}
	version, err := o.activationVersion(resolved, change, decision)
	if err != nil {
		return nil, err
	}
	change.PolicyDigest, change.Actor = version.PolicyDigest, actor
	if evidenceErr := o.verifyActivationArtifacts(resolved, decision); evidenceErr != nil {
		return nil, evidenceErr
	}
	if compatibilityErr := o.checkCompatibility(ctx, version); compatibilityErr != nil {
		return nil, compatibilityErr
	}
	// External checks may take time. Recheck both identities before the atomic write.
	current, done, err := o.meter.authorizeAction(ctx, decision.ExperimentID, action)
	if err != nil {
		return nil, err
	}
	defer done()
	if childErr := o.authorizeControllerDecision(ctx, decision, action); childErr != nil {
		return nil, childErr
	}
	currentActor, err := o.authorizeActivationKind(ctx, change.AgentID, action, change.Kind)
	if err != nil {
		return nil, err
	}
	if currentActor != actor {
		return nil, ErrPolicyDenied
	}
	var value Activation
	err = o.config.Repository.Transact(current, func(tx Transaction) error {
		var commitErr error
		value, commitErr = tx.CompareAndSwapActivation(change)
		return commitErr
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (o *Optimizer) activationVersion(ctx context.Context, change ActivationChange, decision Decision) (HarnessVersion, error) {
	var empty HarnessVersion
	experiment, err := o.config.Repository.GetExperiment(copyScopeContext(ctx), decision.ExperimentID)
	if err != nil {
		return empty, err
	}
	version, err := o.config.Repository.GetVersion(copyScopeContext(ctx), change.Version.ID)
	if err != nil {
		return empty, err
	}
	sealed, err := SealVersion(version)
	if err != nil || sealed.Digest != change.Version.Digest || version.Digest != sealed.Digest {
		return empty, ErrStaleVersion
	}
	if change.PolicyDigest == "" {
		change.PolicyDigest = sealed.PolicyDigest
	}
	if validationErr := ValidateActivationEvidence(change, experiment, decision, sealed); validationErr != nil {
		return empty, validationErr
	}
	return sealed, nil
}

func (o *Optimizer) authorizeActivation(ctx context.Context, agentID id.AgentID, action Action) (IdentityRef, error) {
	return o.authorizeActivationKind(ctx, agentID, action, HarnessExecution)
}

func (o *Optimizer) authorizeActivationKind(ctx context.Context, agentID id.AgentID, action Action, kind HarnessKind) (IdentityRef, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return IdentityRef{}, err
	}
	actor, err := o.caller(ctx)
	if err != nil {
		return actor, err
	}
	if _, err = id.ParseWithPrefix(agentID.String(), id.PrefixAgent); err != nil {
		return actor, ErrInvalidSpec
	}
	normalized, err := ActivationKind(kind)
	if err != nil {
		return actor, err
	}
	resourceKind := "activation"
	if normalized == HarnessImprovement {
		resourceKind = "controller_activation"
	}
	err = o.config.Authorizer.Authorize(copyScopeContext(ctx), action, Resource{Scope: copyScope(cortex.ScopeFromContext(ctx)), AgentID: agentID, Kind: resourceKind})
	return actor, err
}

func (o *Optimizer) checkCompatibility(ctx context.Context, version HarnessVersion) error {
	detached, err := copyModelJSON(&version, 16<<20)
	if err != nil {
		return err
	}
	checker := o.config.Compatibility
	if version.Kind == HarnessImprovement {
		checker = o.config.ControllerCompatibility
	}
	if checker == nil {
		return ErrUnavailable
	}
	report, err := checker.Check(copyScopeContext(ctx), *detached)
	if err != nil {
		return err
	}
	if !report.Compatible || report.Version != (VersionRef{ID: version.ID, Digest: version.Digest}) || report.PolicyDigest != version.PolicyDigest {
		return ErrStaleVersion
	}
	return ctx.Err()
}

func (o *Optimizer) verifyActivationArtifacts(ctx context.Context, decision Decision) error {
	parent, err := o.config.Repository.GetExperiment(copyScopeContext(ctx), decision.ExperimentID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	if parent.Spec.Improvement == nil {
		return o.verifyTrialArtifacts(ctx, parent.ID, decision.AllocationID, seen)
	}
	for _, child := range parent.Spec.Improvement.Children {
		if err := o.verifyTrialArtifacts(ctx, child.ExperimentID, decision.AllocationID, seen); err != nil {
			return err
		}
	}
	return nil
}

func (o *Optimizer) authorizeControllerDecision(ctx context.Context, decision Decision, action Action) error {
	parent, err := o.config.Repository.GetExperiment(copyScopeContext(ctx), decision.ExperimentID)
	if err != nil {
		return err
	}
	if parent.Spec.Improvement == nil {
		return nil
	}
	for _, child := range parent.Spec.Improvement.Children {
		_, done, authErr := o.meter.authorizeAction(ctx, child.ExperimentID, action)
		if authErr != nil {
			return authErr
		}
		done()
	}
	allocation, err := o.config.Repository.GetValidationAllocation(ctx, decision.AllocationID)
	if err != nil {
		return err
	}
	evidence, err := ReadControllerEvidence(ctx, o.config.Repository, parent, allocation)
	if err != nil {
		return err
	}
	return ValidateControllerDecisionEvidence(parent, allocation, decision, evidence)
}

func (o *Optimizer) verifyTrialArtifacts(ctx context.Context, experimentID, allocationID id.ID, seen map[string]bool) error {
	query := TrialQuery{PageQuery: PageQuery{Limit: 100}, Split: SplitValidation}
	cursors := map[string]bool{}
	for {
		page, err := o.config.Repository.ListTrials(copyScopeContext(ctx), experimentID, query)
		if err != nil {
			return err
		}
		for _, trial := range page.Items {
			if trial.ExperimentID != experimentID {
				return ErrIncompleteEvidence
			}
			if trial.AllocationID != allocationID {
				continue
			}
			for _, ref := range TrialArtifactRefs(trial) {
				binding, bindErr := ref.BindingDigest()
				if bindErr != nil {
					return bindErr
				}
				if seen[binding] {
					continue
				}
				seen[binding] = true
				if _, verifyErr := o.config.ArtifactVerifier.Verify(copyScopeContext(ctx), experimentID, ref); verifyErr != nil {
					return verifyErr
				}
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if cursors[page.NextCursor] {
			return ErrIncompleteEvidence
		}
		cursors[page.NextCursor] = true
		query.Cursor = page.NextCursor
	}
}

// Deactivate turns off the route even when its previous evidence is unavailable.
func (o *Optimizer) Deactivate(ctx context.Context, request DeactivationRequest) (*Activation, error) {
	actor, err := o.authorizeActivationKind(ctx, request.AgentID, ActionDeactivate, request.Kind)
	if err != nil {
		return nil, err
	}
	if !validActivationReason(request.Reason) {
		return nil, ErrInvalidSpec
	}
	var value Activation
	err = o.config.Repository.Transact(ctx, func(tx Transaction) error {
		var commitErr error
		value, commitErr = tx.Deactivate(DeactivationChange{DeactivationRequest: request, Actor: actor})
		return commitErr
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func validActivationReason(reason string) bool {
	return strings.TrimSpace(reason) != "" && len(reason) <= 512 && utf8.ValidString(reason)
}

// DeactivationChange carries a service-authenticated actor to the transaction.
type DeactivationChange struct {
	DeactivationRequest
	Actor IdentityRef `json:"actor"`
}
