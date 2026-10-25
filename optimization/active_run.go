package optimization

import (
	"context"
	"errors"

	"github.com/xraph/cortex"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// Option binds an optional host integration during optimizer construction.
type Option func(*Optimizer) error

// WithActiveEngine supplies the production engine used only by RunActive.
func WithActiveEngine(runtime *engine.Engine) Option {
	return func(o *Optimizer) error {
		if runtime == nil {
			return ErrInvalidSpec
		}
		o.activeEngine = runtime
		return nil
	}
}

// ActiveRunRequest carries normal production input for an explicitly active route.
// The caller cannot override its prepared configuration or execution purpose.
type ActiveRunRequest struct {
	AgentID   id.AgentID   `json:"agent_id"`
	Channel   string       `json:"channel"`
	Input     string       `json:"input"`
	SessionID id.SessionID `json:"session_id,omitempty"`
}

// RunActive resolves one activation and executes its immutable production snapshot.
func (o *Optimizer) RunActive(ctx context.Context, request ActiveRunRequest) (*run.Run, error) {
	if _, err := o.authorizeActivation(ctx, request.AgentID, ActionRunActive); err != nil {
		return nil, err
	}
	if o.activeEngine == nil || o.config.Compatibility == nil || o.config.ArtifactVerifier == nil {
		return nil, ErrUnavailable
	}
	active, err := o.config.Repository.GetActivation(copyScopeContext(ctx), request.AgentID, request.Channel)
	if err != nil {
		return nil, err
	}
	decision, err := o.config.Repository.GetDecision(copyScopeContext(ctx), active.DecisionID)
	if err != nil {
		return nil, err
	}
	resolved, done, err := o.meter.authorizeAction(ctx, decision.ExperimentID, ActionRunActive)
	if err != nil {
		return nil, err
	}
	defer done()
	version, err := o.activationVersion(resolved, ActivationChange{AgentID: active.AgentID, Version: active.Version, DecisionID: active.DecisionID, PolicyDigest: active.PolicyDigest}, decision)
	if err != nil {
		return nil, err
	}
	if compatibilityErr := o.checkCompatibility(ctx, version); compatibilityErr != nil {
		return nil, compatibilityErr
	}
	if evidenceErr := o.verifyActivationArtifacts(resolved, decision); evidenceErr != nil {
		return nil, evidenceErr
	}
	if _, err = o.authorizeActivation(ctx, request.AgentID, ActionRunActive); err != nil {
		return nil, err
	}
	if _, resolveErr := o.config.Repository.ResolveActivation(copyScopeContext(ctx), active.ID); resolveErr != nil {
		return nil, resolveErr
	}
	result, err := o.activeEngine.RunPrepared(copyScopeContext(ctx), version.Prepared, request.Input, engine.PreparedRunOptions{SessionID: request.SessionID, Purpose: run.PurposeNormal, Provenance: run.ExecutionProvenance{ExperimentID: decision.ExperimentID.String(), HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String()}})
	if errors.Is(err, cortex.ErrInvalidPrepared) {
		return result, errors.Join(ErrStaleVersion, err)
	}
	return result, err
}
