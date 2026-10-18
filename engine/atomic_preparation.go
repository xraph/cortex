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

func (e *Engine) prepareAgent(ctx context.Context, ag *agent.Config, overrides *RunOverrides) (*PreparedAgent, error) {
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
	return freezePrepared(snap)
}

func (e *Engine) prepareAtomicAgent(ctx context.Context, agentID id.AgentID, overrides *RunOverrides) (*PreparedAgent, error) {
	provider, ok := e.store.(store.PreparationSnapshotter)
	if !ok {
		return nil, cortex.ErrPreparationUnavailable
	}
	var snap *preparedSnapshot
	calls := 0
	var collectionErr error
	receipt, err := provider.CapturePreparation(ctx, func(sources store.PreparationSources) error {
		calls++
		if calls != 1 || sources == nil {
			collectionErr = cortex.ErrPreparationUnavailable
			return collectionErr
		}
		captureCtx := context.WithValue(ctx, preparationSourcesKey{}, sources)
		ag, readErr := sources.Get(captureCtx, agentID)
		if readErr != nil {
			collectionErr = readErr
			return readErr
		}
		if ag == nil || ag.ID != agentID {
			collectionErr = cortex.ErrInvalidPrepared
			return collectionErr
		}
		snap, collectionErr = e.collectPreparedAgent(captureCtx, ag, overrides)
		return collectionErr
	})
	if err != nil {
		return nil, err
	}
	if collectionErr != nil {
		return nil, collectionErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if calls != 1 || snap == nil || receipt == nil || receipt.ID == "" || receipt.Provider == "" || receipt.Isolation == "" || receipt.Scope != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrPreparationUnavailable
	}
	snap.Provenance.SourceValidation = "atomic_store_snapshot"
	snap.Provenance.SourceSnapshot = *receipt
	// The transaction is closed before scanner calls. Publication examines only
	// the captured values, so scanner latency cannot hold database resources.
	if publicationErr := e.checkPreparedSources(ctx, &snap.Agent, snap); publicationErr != nil {
		return nil, publicationErr
	}
	return freezePrepared(snap)
}
