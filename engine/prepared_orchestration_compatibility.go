package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

// CheckPreparedOrchestrationSources compares the original configuration and
// every participant's stored read set in one read-only snapshot. Derived edits
// retain their source identity. SourceCount includes the configuration and each
// participant's read set, including shared sources read for several participants.
// This observation grants no execution authority and performs no provider, tool
// or publication calls. Missing snapshot capabilities never fall back to reads.
func (e *Engine) CheckPreparedOrchestrationSources(ctx context.Context, p *PreparedOrchestration) (*PreparedSourceCheck, error) {
	view, names, err := preparedOrchestrationForCheck(ctx, p)
	if err != nil {
		return nil, err
	}
	if e == nil || e.store == nil {
		return nil, cortex.ErrNoStore
	}
	provider, ok := e.store.(store.PreparationSnapshotter)
	if !ok {
		return nil, cortex.ErrPreparationUnavailable
	}
	children := make([]*preparedSnapshot, 0, len(names))
	count := 1
	for _, name := range names {
		child, childErr := view.Participants[name].snapshot()
		if childErr != nil {
			return nil, childErr
		}
		if manifestErr := validatePreparedReadSet(child); manifestErr != nil {
			return nil, manifestErr
		}
		children = append(children, child)
		count += len(child.Provenance.SourceReads)
	}
	expected, err := json.Marshal(view.Config)
	if err != nil {
		return nil, err
	}
	receipt, err := capturePreparation(ctx, provider, func(sources store.PreparationSources) error {
		teams, available := sources.(store.OrchestrationPreparationSources)
		if !available {
			return cortex.ErrPreparationUnavailable
		}
		current, readErr := teams.GetOrchestration(ctx, view.Config.ID)
		if readErr != nil {
			return errors.Join(cortex.ErrPreparationConflict, readErr)
		}
		encoded, encodeErr := json.Marshal(current)
		if encodeErr != nil || !bytes.Equal(expected, encoded) {
			return cortex.ErrPreparationConflict
		}
		for _, child := range children {
			if checkErr := checkPreparedSourceReads(ctx, sources, child); checkErr != nil {
				return checkErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PreparedSourceCheck{PreparedDigest: p.Digest(), SourceCount: count, Snapshot: *receipt}, nil
}

// CheckPreparedOrchestrationBindings checks current host-declared model and tool
// revisions for the complete frozen roster, including auxiliary agents. It uses
// CheckPreparedBindings without refreshing stored sources or invoking providers.
// This point-in-time observation is separate from the atomic source check and
// does not grant execution authority or authenticate remote implementations.
func (e *Engine) CheckPreparedOrchestrationBindings(ctx context.Context, p *PreparedOrchestration) error {
	view, names, err := preparedOrchestrationForCheck(ctx, p)
	if err != nil {
		return err
	}
	for _, name := range names {
		if bindingErr := e.CheckPreparedBindings(ctx, view.Participants[name]); bindingErr != nil {
			return bindingErr
		}
	}
	return ctx.Err()
}

func preparedOrchestrationForCheck(ctx context.Context, p *PreparedOrchestration) (PreparedOrchestrationView, []string, error) {
	if err := ctx.Err(); err != nil {
		return PreparedOrchestrationView{}, nil, err
	}
	view, err := p.Inspect()
	if err != nil {
		return PreparedOrchestrationView{}, nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return PreparedOrchestrationView{}, nil, err
	}
	if scope.Canonical() != view.Config.Scope.Canonical() {
		return PreparedOrchestrationView{}, nil, cortex.ErrInvalidScope
	}
	if validationErr := validatePreparedOrchestration(preparedOrchestrationSnapshot{SchemaVersion: 1, PreparedOrchestrationView: view}); validationErr != nil {
		return PreparedOrchestrationView{}, nil, validationErr
	}
	names, err := orchestration.ExecutionRoster(&view.Config)
	return view, names, err
}
