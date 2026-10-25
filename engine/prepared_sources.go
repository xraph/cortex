package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// PreparedSourceCheck binds matching source records to a new atomic observation.
// It covers only the stored read set, not model/tool implementations, external
// knowledge, host policy or authorization. It grants no execution authority.
type PreparedSourceCheck struct {
	PreparedDigest string             `json:"prepared_digest"`
	SourceCount    int                `json:"source_count"`
	Snapshot       run.SourceSnapshot `json:"snapshot"`
}

// CheckPreparedSources compares a frozen source read set with current records in
// one read-only store snapshot. Derived prompt edits do not change that read set.
// Missing sources or changed digests return ErrPreparationConflict. Stores must
// support PreparationSnapshotter; there is no fallback to independent reads.
// This point-in-time check performs no model, scanner, tool, or execution calls.
func (e *Engine) CheckPreparedSources(ctx context.Context, p *PreparedAgent) (*PreparedSourceCheck, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	snap, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	if scopeErr := requireAgentScope(ctx, &snap.Agent); scopeErr != nil {
		return nil, scopeErr
	}
	if snap.Scope.Canonical() != snap.Agent.Scope.Canonical() {
		return nil, cortex.ErrInvalidPrepared
	}
	if manifestErr := validatePreparedReadSet(snap); manifestErr != nil {
		return nil, manifestErr
	}
	provider, ok := e.store.(store.PreparationSnapshotter)
	if !ok {
		return nil, cortex.ErrPreparationUnavailable
	}
	receipt, err := capturePreparation(ctx, provider, func(sources store.PreparationSources) error {
		for _, revision := range snap.Provenance.SourceReads {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			value, readErr := currentPreparedSource(ctx, sources, snap, revision.Name)
			if readErr != nil {
				return errors.Join(cortex.ErrPreparationConflict, readErr)
			}
			data, encodeErr := json.Marshal(value)
			if encodeErr != nil || contentDigest(data) != revision.Digest {
				return cortex.ErrPreparationConflict
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PreparedSourceCheck{PreparedDigest: p.Digest(), SourceCount: len(snap.Provenance.SourceReads), Snapshot: *receipt}, nil
}

func validatePreparedReadSet(snap *preparedSnapshot) error {
	seen := make(map[string]string, len(snap.Provenance.SourceReads))
	for _, revision := range snap.Provenance.SourceReads {
		digest, err := hex.DecodeString(revision.Digest)
		if err != nil || len(digest) != 32 || revision.Name == "" || revision.Revision != "" {
			return cortex.ErrInvalidPrepared
		}
		if previous, exists := seen[revision.Name]; exists && previous != revision.Digest {
			return cortex.ErrInvalidPrepared
		}
		seen[revision.Name] = revision.Digest
	}
	if _, ok := seen["agent:"+snap.Agent.ID.String()]; !ok {
		return cortex.ErrPreparationUnavailable
	}
	for depth := 1; depth <= len(snap.Scope.Levels); depth++ {
		ancestor := cortex.Scope{Levels: snap.Scope.Levels[:depth]}
		if _, ok := seen["overlay:"+ancestor.Canonical()]; !ok {
			return cortex.ErrPreparationUnavailable
		}
	}
	return nil
}

func currentPreparedSource(ctx context.Context, sources store.PreparationSources, snap *preparedSnapshot, name string) (any, error) {
	kind, key, ok := strings.Cut(name, ":")
	if !ok || key == "" {
		return nil, cortex.ErrInvalidPrepared
	}
	switch kind {
	case "agent":
		if key != snap.Agent.ID.String() {
			return nil, cortex.ErrInvalidPrepared
		}
		return sources.Get(ctx, snap.Agent.ID)
	case "persona":
		return sources.GetPersonaByName(ctx, key)
	case "skill":
		return sources.GetSkillByName(ctx, key)
	case "trait":
		return sources.GetTraitByName(ctx, key)
	case "overlay":
		for depth := 1; depth <= len(snap.Scope.Levels); depth++ {
			ancestor := cortex.Scope{Levels: snap.Scope.Levels[:depth]}
			if ancestor.Canonical() == key {
				value, err := sources.GetOverlayForAgentAt(ctx, snap.Agent.ID, ancestor)
				if errors.Is(err, cortex.ErrOverlayNotFound) {
					return nil, nil
				}
				return value, err
			}
		}
	}
	return nil, cortex.ErrInvalidPrepared
}
