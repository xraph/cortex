package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

const (
	maxPreparedOrchestrationBytes       = 16 << 20
	maxPreparedOrchestrationConfigBytes = 1 << 20
)

// PreparedOrchestration freezes a strategy and every agent it can invoke.
// Its digest establishes content integrity, not approval or execution authority.
type PreparedOrchestration struct {
	payload json.RawMessage
	digest  string
}

// PreparedOrchestrationView is detached from the originating artifact. Snapshot
// covers stored sources, not external retrieval or model/tool implementations.
type PreparedOrchestrationView struct {
	Config       orchestration.Config      `json:"config"`
	Participants map[string]*PreparedAgent `json:"participants"`
	Snapshot     run.SourceSnapshot        `json:"snapshot"`
}

type preparedOrchestrationSnapshot struct {
	SchemaVersion int `json:"schema_version"`
	PreparedOrchestrationView
}

// Digest identifies the frozen team.
func (p *PreparedOrchestration) Digest() string {
	if p == nil {
		return ""
	}
	return p.digest
}

// Inspect returns independently decoded configuration and participant snapshots.
func (p *PreparedOrchestration) Inspect() (PreparedOrchestrationView, error) {
	if p == nil || len(p.payload) == 0 || contentDigest(p.payload) != p.digest {
		return PreparedOrchestrationView{}, cortex.ErrInvalidPrepared
	}
	var snapshot preparedOrchestrationSnapshot
	if err := decodePreparedJSON(p.payload, &snapshot); err != nil {
		return PreparedOrchestrationView{}, err
	}
	return snapshot.PreparedOrchestrationView, nil
}

// MarshalJSON serializes the complete frozen team with its integrity digest.
func (p PreparedOrchestration) MarshalJSON() ([]byte, error) {
	if _, err := p.Inspect(); err != nil {
		return nil, err
	}
	return json.Marshal(preparedWire{Digest: p.digest, Snapshot: p.payload})
}

// UnmarshalJSON validates bounded content, participant membership and scope.
func (p *PreparedOrchestration) UnmarshalJSON(data []byte) error {
	if len(data) > maxPreparedOrchestrationBytes {
		return cortex.ErrInvalidPrepared
	}
	var wire preparedWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	canonical, err := json.Marshal(wire.Snapshot)
	if err != nil || contentDigest(canonical) != wire.Digest {
		return cortex.ErrInvalidPrepared
	}
	var snapshot preparedOrchestrationSnapshot
	if decodeErr := decodePreparedJSON(canonical, &snapshot); decodeErr != nil {
		return decodeErr
	}
	if validationErr := validatePreparedOrchestration(snapshot); validationErr != nil {
		return validationErr
	}
	p.payload, p.digest = append([]byte(nil), canonical...), wire.Digest
	return nil
}

func validatePreparedOrchestration(s preparedOrchestrationSnapshot) error {
	cfg := s.Config
	encoded, encodeErr := json.Marshal(cfg)
	if encodeErr != nil || len(encoded) > maxPreparedOrchestrationConfigBytes {
		return cortex.ErrInvalidPrepared
	}
	if s.SchemaVersion != 1 || cfg.ID.IsNil() || cfg.ID.Prefix() != id.PrefixOrchestrationConfig || cortex.ValidateScope(cfg.Scope) != nil || s.Snapshot.ID == "" || s.Snapshot.Provider == "" || s.Snapshot.Isolation == "" || s.Snapshot.Scope != cfg.Scope.Canonical() {
		return cortex.ErrInvalidPrepared
	}
	names, err := orchestration.ExecutionRoster(&cfg)
	if err != nil || len(names) != len(s.Participants) {
		return cortex.ErrInvalidPrepared
	}
	for _, name := range names {
		child := s.Participants[name]
		frozen, childErr := child.snapshot()
		if childErr != nil || frozen.Agent.Name != name || frozen.Scope.Canonical() != cfg.Scope.Canonical() || frozen.Provenance.SourceSnapshot != s.Snapshot || frozen.Provenance.SourceValidation != "atomic_store_snapshot" || (cfg.Settings.Model != "" && frozen.Config.Model != cfg.Settings.Model) {
			return cortex.ErrInvalidPrepared
		}
	}
	return nil
}

// PrepareOrchestration captures config and all named participants in one store
// snapshot. It requires OrchestrationPreparationSources; independent reads are
// never substituted. Publication checks run after the transaction closes.
// Preparation grants no approval and invokes no strategy, model or tools.
func (e *Engine) PrepareOrchestration(ctx context.Context, key id.OrchestrationConfigID) (*PreparedOrchestration, error) {
	return e.prepareOrchestration(ctx, key, nil)
}

// PrepareOrchestrationWithBudgets freezes explicit per-invocation ceilings in
// every participant, including auxiliary agents. Steps, tokens and deadline are
// required. A zero tool limit is allowed only when the captured tool list is
// empty. Budgets must match the complete execution roster. Shared team capacity
// still needs its own durable reservation policy before dispatch.
func (e *Engine) PrepareOrchestrationWithBudgets(ctx context.Context, key id.OrchestrationConfigID, budgets map[string]execution.Budget) (*PreparedOrchestration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(budgets) < 1 || len(budgets) > 257 {
		return nil, cortex.ErrInvalidPrepared
	}
	budgets = maps.Clone(budgets)
	for _, budget := range budgets {
		if budget.Validate() != nil || budget.MaxSteps <= 0 || budget.MaxTokens <= 0 || budget.Deadline.IsZero() {
			return nil, cortex.ErrInvalidPrepared
		}
	}
	return e.prepareOrchestration(ctx, key, budgets)
}

func (e *Engine) prepareOrchestration(ctx context.Context, key id.OrchestrationConfigID, budgets map[string]execution.Budget) (*PreparedOrchestration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	if key.IsNil() || key.Prefix() != id.PrefixOrchestrationConfig {
		return nil, cortex.ErrInvalidPrepared
	}
	provider, ok := e.store.(store.PreparationSnapshotter)
	if !ok {
		return nil, cortex.ErrPreparationUnavailable
	}
	var config orchestration.Config
	children := make(map[string]*preparedSnapshot)
	receipt, err := capturePreparation(ctx, provider, func(sources store.PreparationSources) error {
		team, available := sources.(store.OrchestrationPreparationSources)
		if !available {
			return cortex.ErrPreparationUnavailable
		}
		current, readErr := team.GetOrchestration(ctx, key)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != key || current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			return cortex.ErrInvalidPrepared
		}
		data, encodeErr := json.Marshal(current)
		if encodeErr != nil || len(data) > maxPreparedOrchestrationConfigBytes {
			return cortex.ErrInvalidPrepared
		}
		if decodeErr := decodePreparedJSON(data, &config); decodeErr != nil {
			return decodeErr
		}
		names, rosterErr := orchestration.ExecutionRoster(&config)
		if rosterErr != nil {
			return fmt.Errorf("cortex: prepare orchestration roster: %w: %w", cortex.ErrInvalidPrepared, rosterErr)
		}
		if budgets != nil && len(budgets) != len(names) {
			return cortex.ErrInvalidPrepared
		}
		captured := context.WithValue(ctx, preparationSourcesKey{}, sources)
		for _, name := range names {
			ag, agentErr := team.GetByName(captured, name)
			if agentErr != nil {
				return agentErr
			}
			if ag == nil || ag.Name != name {
				return cortex.ErrInvalidPrepared
			}
			overrides := &RunOverrides{Model: config.Settings.Model}
			if budgets != nil {
				budget, exists := budgets[name]
				if !exists {
					return cortex.ErrInvalidPrepared
				}
				overrides.Budget = &budget
			}
			child, prepareErr := e.collectPreparedAgent(captured, ag, overrides)
			if prepareErr != nil {
				return prepareErr
			}
			if budgets != nil && len(child.Tools) > 0 && child.Config.Budget.MaxToolCalls <= 0 {
				return cortex.ErrInvalidPrepared
			}
			children[name] = child
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	snapshot := preparedOrchestrationSnapshot{SchemaVersion: 1, PreparedOrchestrationView: PreparedOrchestrationView{Config: config, Participants: make(map[string]*PreparedAgent, len(children)), Snapshot: *receipt}}
	for name, child := range children {
		child.Provenance.SourceValidation = "atomic_store_snapshot"
		child.Provenance.SourceSnapshot = *receipt
		if publicationErr := e.checkPreparedSources(ctx, &child.Agent, child); publicationErr != nil {
			return nil, publicationErr
		}
		prepared, freezeErr := freezePrepared(child)
		if freezeErr != nil {
			return nil, freezeErr
		}
		snapshot.Participants[name] = prepared
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	prepared, freezeErr := freezePreparedOrchestration(snapshot)
	if freezeErr != nil {
		return nil, freezeErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	return prepared, nil
}

func freezePreparedOrchestration(snapshot preparedOrchestrationSnapshot) (*PreparedOrchestration, error) {
	if err := validatePreparedOrchestration(snapshot); err != nil {
		return nil, err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if len(data) > maxPreparedOrchestrationBytes-256 {
		return nil, cortex.ErrInvalidPrepared
	}
	return &PreparedOrchestration{payload: data, digest: contentDigest(data)}, nil
}
