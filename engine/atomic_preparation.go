package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type preparationSourcesKey struct{}

func (e *Engine) preparationSources(ctx context.Context) store.PreparationSources {
	if sources, ok := ctx.Value(preparationSourcesKey{}).(store.PreparationSources); ok {
		return sources
	}
	return e.store
}

func isAtomicPreparation(ctx context.Context) bool { return ctx.Value(preparationSourcesKey{}) != nil }

// prepareAgent freezes an agent for one run under a prepare agent span.
func (e *Engine) prepareAgent(ctx context.Context, ag *agent.Config, overrides *RunOverrides) (*PreparedAgent, error) {
	agentID := ""
	if ag != nil {
		agentID = ag.ID.String()
	}
	ctx, span := e.startPrepare(ctx, agentID)
	p, err := e.buildPrepared(ctx, ag, overrides)
	endPrepare(span, err)
	return p, err
}

func (e *Engine) buildPrepared(ctx context.Context, ag *agent.Config, overrides *RunOverrides) (*PreparedAgent, error) {
	if ag == nil || e.store == nil {
		return nil, cortex.ErrInvalidPrepared
	}
	if err := requireAgentScope(ctx, ag); err != nil {
		return nil, err
	}
	if e.atomicPreparation {
		return e.prepareAtomicAgent(ctx, ag.ID, overrides)
	}
	snap, err := e.collectPreparedAgent(ctx, ag, overrides)
	if err != nil {
		return nil, err
	}
	annotatePrepared(ctx, snap)
	return freezePrepared(snap)
}

func (e *Engine) prepareAtomicAgent(ctx context.Context, agentID id.AgentID, overrides *RunOverrides) (*PreparedAgent, error) {
	provider, ok := e.store.(store.PreparationSnapshotter)
	if !ok {
		return nil, cortex.ErrPreparationUnavailable
	}
	var snap *preparedSnapshot
	receipt, err := capturePreparation(ctx, provider, func(sources store.PreparationSources) error {
		captureCtx := context.WithValue(ctx, preparationSourcesKey{}, sources)
		ag, readErr := sources.Get(captureCtx, agentID)
		if readErr != nil {
			return readErr
		}
		if ag == nil || ag.ID != agentID {
			return cortex.ErrInvalidPrepared
		}
		var err error
		snap, err = e.collectPreparedAgent(captureCtx, ag, overrides)
		return err
	})
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, cortex.ErrPreparationUnavailable
	}
	snap.Provenance.SourceValidation = "atomic_store_snapshot"
	snap.Provenance.SourceSnapshot = *receipt
	// The transaction is closed before scanner calls. Publication examines only
	// the captured values, so scanner latency cannot hold database resources.
	if publicationErr := e.checkPreparedSources(ctx, &snap.Agent, snap); publicationErr != nil {
		return nil, publicationErr
	}
	annotatePrepared(ctx, snap)
	return freezePrepared(snap)
}
