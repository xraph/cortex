package optimization

import (
	"context"

	"github.com/xraph/cortex/id"
)

// ActiveControllerRequest identifies the reviewed route used to start new work.
type ActiveControllerRequest struct {
	AgentID id.AgentID `json:"agent_id"`
	Channel string     `json:"channel"`
}

// ControllerSelection pins a reviewed activation and its immutable controller.
// Use Activation.Version in a new experiment before creation; running experiments
// keep their frozen controller when the route changes.
type ControllerSelection struct {
	Activation Activation     `json:"activation"`
	Version    HarnessVersion `json:"version"`
}

// ResolveActiveController rechecks current grants, evidence and model bindings
// without dispatching a model or changing any existing experiment.
func (o *Optimizer) ResolveActiveController(ctx context.Context, request ActiveControllerRequest) (*ControllerSelection, error) {
	if _, authErr := o.authorizeActivationKind(ctx, request.AgentID, ActionResolveController, HarnessImprovement); authErr != nil {
		return nil, authErr
	}
	if o.config.ControllerCompatibility == nil || o.config.ArtifactVerifier == nil {
		return nil, ErrUnavailable
	}
	active, err := o.config.Repository.GetActivationForKind(copyScopeContext(ctx), request.AgentID, request.Channel, HarnessImprovement)
	if err != nil {
		return nil, err
	}
	decision, err := o.config.Repository.GetDecision(copyScopeContext(ctx), active.DecisionID)
	if err != nil {
		return nil, err
	}
	resolved, done, err := o.meter.authorizeAction(ctx, decision.ExperimentID, ActionResolveController)
	if err != nil {
		return nil, err
	}
	defer done()
	if authErr := o.authorizeControllerDecision(ctx, decision, ActionResolveController); authErr != nil {
		return nil, authErr
	}
	version, err := o.activationVersion(resolved, ActivationChange{Kind: HarnessImprovement, AgentID: active.AgentID, Version: active.Version, DecisionID: active.DecisionID, PolicyDigest: active.PolicyDigest}, decision)
	if err != nil {
		return nil, err
	}
	if artifactErr := o.verifyActivationArtifacts(resolved, decision); artifactErr != nil {
		return nil, artifactErr
	}
	if compatibilityErr := o.checkCompatibility(ctx, version); compatibilityErr != nil {
		return nil, compatibilityErr
	}
	if _, authErr := o.authorizeActivationKind(ctx, request.AgentID, ActionResolveController, HarnessImprovement); authErr != nil {
		return nil, authErr
	}
	if authErr := o.authorizeControllerDecision(ctx, decision, ActionResolveController); authErr != nil {
		return nil, authErr
	}
	if _, resolveErr := o.config.Repository.ResolveActivation(copyScopeContext(ctx), active.ID); resolveErr != nil {
		return nil, resolveErr
	}
	return copyModelJSON(&ControllerSelection{Activation: active, Version: version}, 16<<20)
}
