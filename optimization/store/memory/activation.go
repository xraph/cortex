package memory

import (
	"context"
	"math"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// GetActivation returns the current generation for the exact scope and channel.
func (s *Store) GetActivation(ctx context.Context, agentID id.AgentID, channel string) (optimization.Activation, error) {
	key, err := activationKey(agentID, channel)
	if err != nil {
		return optimization.Activation{}, err
	}
	return s.readActivation(ctx, func(data *scopeData) id.ID { return data.ActivationHeads[key] })
}

// GetActivationGeneration reads an immutable historical activation by ID.
func (s *Store) GetActivationGeneration(ctx context.Context, activationID id.ID) (optimization.Activation, error) {
	return s.readActivation(ctx, func(*scopeData) id.ID { return activationID })
}

func (s *Store) readActivation(ctx context.Context, selectID func(*scopeData) id.ID) (optimization.Activation, error) {
	var empty optimization.Activation
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	if data := s.scopes[key]; data != nil {
		if value, exists := data.Activations[selectID(data).String()]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func activationKey(agentID id.AgentID, channel string) (string, error) {
	if _, err := id.ParseWithPrefix(agentID.String(), id.PrefixAgent); err != nil {
		return "", optimization.ErrInvalidSpec
	}
	normalized, err := optimization.ActivationChannel(channel)
	if err != nil {
		return "", err
	}
	return agentID.String() + "/" + normalized, nil
}

func (tx *transaction) CompareAndSwapActivation(change optimization.ActivationChange) (optimization.Activation, error) {
	var empty optimization.Activation
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	if _, err := optimization.ParseID(change.ID.String(), optimization.PrefixActivation); err != nil {
		return empty, err
	}
	if change.ExpectedGeneration < 0 || strings.TrimSpace(change.Actor.Subject) == "" {
		return empty, optimization.ErrInvalidSpec
	}
	key, err := activationKey(change.AgentID, change.Channel)
	if err != nil {
		return empty, err
	}
	change.Channel, err = optimization.ActivationChannel(change.Channel)
	if err != nil {
		return empty, err
	}
	if prior, exists := tx.data.Activations[change.ID.String()]; exists {
		original := optimization.ActivationChange{ID: prior.ID, AgentID: prior.AgentID, Channel: prior.Channel, Version: prior.Version, ExpectedGeneration: prior.ExpectedGeneration, Actor: prior.Actor, PolicyDigest: prior.PolicyDigest, DecisionID: prior.DecisionID, RollbackTo: prior.RollbackTo}
		if original != change {
			return empty, optimization.ErrConflict
		}
		return clone(prior)
	}
	current := tx.data.Activations[tx.data.ActivationHeads[key].String()]
	if current.Generation != change.ExpectedGeneration || current.Generation == math.MaxInt64 {
		return empty, optimization.ErrConflict
	}
	decision, exists := tx.data.Decisions[change.DecisionID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	experiment, exists := tx.data.Experiments[decision.ExperimentID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	version, exists := tx.data.Versions[change.Version.ID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if evidenceErr := optimization.ValidateActivationEvidence(change, experiment, decision, version); evidenceErr != nil {
		return empty, evidenceErr
	}
	if !change.RollbackTo.IsNil() {
		previous, found := tx.data.Activations[change.RollbackTo.String()]
		if !found || previous.AgentID != change.AgentID || previous.Channel != change.Channel || previous.Version != change.Version || previous.DecisionID != change.DecisionID || previous.Generation > current.Generation || previous.PolicyDigest != change.PolicyDigest {
			return empty, optimization.ErrIncompleteEvidence
		}
	}
	value := optimization.Activation{ID: change.ID, Scope: cortex.ScopeFromContext(tx.ctx), AgentID: change.AgentID, Channel: change.Channel, Version: change.Version, Generation: current.Generation + 1, ExpectedGeneration: change.ExpectedGeneration, Actor: change.Actor, PolicyDigest: change.PolicyDigest, DecisionID: change.DecisionID, RollbackTo: change.RollbackTo, CreatedAt: tx.now}
	detached, err := clone(value)
	if err != nil {
		return empty, err
	}
	payload, err := canonical.Marshal(map[string]any{"activation_id": value.ID, "channel": value.Channel, "generation": value.Generation, "previous_activation_id": current.ID, "rollback_to": value.RollbackTo})
	if err != nil {
		return empty, err
	}
	if tx.data.Activations == nil {
		tx.data.Activations = make(map[string]optimization.Activation)
	}
	if tx.data.ActivationHeads == nil {
		tx.data.ActivationHeads = make(map[string]id.ID)
	}
	tx.data.Activations[value.ID.String()] = detached
	tx.data.ActivationHeads[key] = value.ID
	eventType := "activated"
	if !value.RollbackTo.IsNil() {
		eventType = "activation_rolled_back"
	}
	event := tx.appendEvent(experiment, eventType, change.Actor)
	event.Resources = append(event.Resources, value.ID, value.Version.ID, value.DecisionID)
	event.Payload = payload
	return value, nil
}
