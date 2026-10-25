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

// WithActiveEngine supplies the production engine for explicitly active routes.
func WithActiveEngine(runtime *engine.Engine) Option {
	return func(o *Optimizer) error {
		if runtime == nil {
			return ErrInvalidSpec
		}
		if checker, ok := o.config.Compatibility.(*EngineCompatibility); ok && checker.config.Engine != runtime {
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
	bound, err := o.resolveActiveExecution(ctx, request.AgentID, request.Channel, false)
	if err != nil {
		return nil, err
	}
	defer bound.release()
	version, decision := bound.version, bound.decision
	result, err := o.activeEngine.RunPrepared(copyScopeContext(ctx), version.Prepared, request.Input, engine.PreparedRunOptions{SessionID: request.SessionID, Purpose: run.PurposeNormal, Provenance: run.ExecutionProvenance{ExperimentID: decision.ExperimentID.String(), HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String()}})
	if errors.Is(err, cortex.ErrInvalidPrepared) {
		return result, errors.Join(ErrStaleVersion, err)
	}
	return result, err
}

type activeExecutionBinding struct {
	version  HarnessVersion
	decision Decision
	release  func()
}

func (o *Optimizer) resolveActiveExecution(ctx context.Context, agentID id.AgentID, channel string, team bool) (*activeExecutionBinding, error) {
	if _, err := o.authorizeActivation(ctx, agentID, ActionRunActive); err != nil {
		return nil, err
	}
	if o.activeEngine == nil || o.config.Compatibility == nil || o.config.ArtifactVerifier == nil {
		return nil, ErrUnavailable
	}
	active, err := o.config.Repository.GetActivation(copyScopeContext(ctx), agentID, channel)
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
	transferred := false
	defer func() {
		if !transferred {
			done()
		}
	}()
	version, err := o.activationVersion(resolved, ActivationChange{AgentID: active.AgentID, Version: active.Version, DecisionID: active.DecisionID, PolicyDigest: active.PolicyDigest}, decision)
	if err != nil {
		return nil, err
	}
	if (version.Orchestration != nil) != team {
		return nil, ErrUnavailable
	}
	if evidenceErr := o.verifyActivationArtifacts(resolved, decision); evidenceErr != nil {
		return nil, evidenceErr
	}
	if compatibilityErr := o.checkCompatibility(ctx, version); compatibilityErr != nil {
		return nil, compatibilityErr
	}
	if _, err = o.authorizeActivation(ctx, agentID, ActionRunActive); err != nil {
		return nil, err
	}
	current, rechecked, err := o.meter.authorizeAction(ctx, decision.ExperimentID, ActionRunActive)
	if err != nil {
		return nil, err
	}
	defer rechecked()
	if _, err = o.config.Repository.ResolveActivation(copyScopeContext(current), active.ID); err != nil {
		return nil, err
	}
	transferred = true
	return &activeExecutionBinding{version: version, decision: decision, release: done}, nil
}
