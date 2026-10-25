package state

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// GetActivation returns the current generation for the exact scope and channel.
func (s *Store) GetActivation(ctx context.Context, agentID id.AgentID, channel string) (optimization.Activation, error) {
	return s.GetActivationForKind(ctx, agentID, channel, optimization.HarnessExecution)
}

// GetActivationForKind resolves a controller or execution route independently.
func (s *Store) GetActivationForKind(ctx context.Context, agentID id.AgentID, channel string, kind optimization.HarnessKind) (optimization.Activation, error) {
	key, err := activationKey(agentID, channel, kind)
	if err != nil {
		return optimization.Activation{}, err
	}
	return s.readActivation(ctx, func(data *scopeData) id.ID { return data.ActivationHeads[key] })
}

// ResolveActivation checks retained evidence and lesson eligibility for a pinned
// generation without resolving the current head again.
func (s *Store) ResolveActivation(ctx context.Context, activationID id.ID) (optimization.Activation, error) {
	if _, err := optimization.ParseID(activationID.String(), optimization.PrefixActivation); err != nil {
		return optimization.Activation{}, err
	}
	return s.readActivation(ctx, func(*scopeData) id.ID { return activationID })
}

// GetActivationGeneration reads an immutable historical activation by ID.
func (s *Store) GetActivationGeneration(ctx context.Context, activationID id.ID) (optimization.Activation, error) {
	key, err := contextScope(ctx)
	if err != nil {
		return optimization.Activation{}, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return optimization.Activation{}, optimization.ErrUnavailable
	}
	data, err := s.loadResource(ctx, key, RecordKey{Kind: "activations", Key: activationID.String()})
	if err != nil {
		return optimization.Activation{}, err
	}
	if data != nil {
		if value, exists := data.Activations[activationID.String()]; exists {
			return clone(value)
		}
	}
	return optimization.Activation{}, optimization.ErrNotFound
}

func (s *Store) readActivation(ctx context.Context, selectID func(*scopeData) id.ID) (optimization.Activation, error) {
	var empty optimization.Activation
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadScope(ctx, key)
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if value, exists := data.Activations[selectID(data).String()]; exists {
			if value.Deactivated {
				return empty, optimization.ErrInactiveActivation
			}
			decision, found := data.Decisions[value.DecisionID.String()]
			if !found {
				return empty, optimization.ErrIncompleteEvidence
			}
			if artifactErr := checkDecisionArtifacts(data, decision, s.now().UTC()); artifactErr != nil {
				return empty, artifactErr
			}
			experiment, found := data.Experiments[decision.ExperimentID.String()]
			if !found {
				return empty, optimization.ErrIncompleteEvidence
			}
			if lessonErr := checkLessonBindings(data, experiment, s.now().UTC()); lessonErr != nil {
				return empty, lessonErr
			}
			if experiment.Spec.Improvement != nil {
				if evidenceErr := checkControllerDecision(data, experiment, decision, s.now().UTC()); evidenceErr != nil {
					return empty, evidenceErr
				}
			}
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func activationKey(agentID id.AgentID, channel string, kind optimization.HarnessKind) (string, error) {
	if _, err := id.ParseWithPrefix(agentID.String(), id.PrefixAgent); err != nil {
		return "", optimization.ErrInvalidSpec
	}
	normalized, err := optimization.ActivationChannel(channel)
	if err != nil {
		return "", err
	}
	normalizedKind, err := optimization.ActivationKind(kind)
	if err != nil {
		return "", err
	}
	if normalizedKind == optimization.HarnessImprovement {
		return agentID.String() + "/improvement/" + normalized, nil
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
	if change.ExpectedGeneration < 0 || !optimization.ValidOpaqueKey(change.Actor.Subject) || len(change.Reason) > 512 || !utf8.ValidString(change.Reason) {
		return empty, optimization.ErrInvalidSpec
	}
	key, err := activationKey(change.AgentID, change.Channel, change.Kind)
	if err != nil {
		return empty, err
	}
	change.Kind, err = optimization.ActivationKind(change.Kind)
	if err != nil {
		return empty, err
	}
	change.Channel, err = optimization.ActivationChannel(change.Channel)
	if err != nil {
		return empty, err
	}
	if prior, exists := tx.data.Activations[change.ID.String()]; exists {
		if prior.Deactivated {
			return empty, optimization.ErrConflict
		}
		original := optimization.ActivationChange{Kind: prior.Kind, Reason: prior.Reason, ID: prior.ID, AgentID: prior.AgentID, Channel: prior.Channel, Version: prior.Version, ExpectedGeneration: prior.ExpectedGeneration, Actor: prior.Actor, PolicyDigest: prior.PolicyDigest, DecisionID: prior.DecisionID, RollbackTo: prior.RollbackTo}
		original.Kind, err = optimization.ActivationKind(original.Kind)
		if err != nil {
			return empty, err
		}
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
	if experiment.Spec.Improvement != nil {
		if evidenceErr := checkControllerDecision(tx.data, experiment, decision, tx.now); evidenceErr != nil {
			return empty, evidenceErr
		}
	}
	if artifactErr := checkDecisionArtifacts(tx.data, decision, tx.now); artifactErr != nil {
		return empty, artifactErr
	}
	if lessonErr := checkLessonBindings(tx.data, experiment, tx.now); lessonErr != nil {
		return empty, lessonErr
	}
	if !change.RollbackTo.IsNil() {
		previous, found := tx.data.Activations[change.RollbackTo.String()]
		if !found || previous.Deactivated || previous.AgentID != change.AgentID || previous.Channel != change.Channel || !activationKindMatches(previous.Kind, change.Kind) || previous.Version != change.Version || previous.DecisionID != change.DecisionID || previous.Generation > current.Generation || previous.PolicyDigest != change.PolicyDigest {
			return empty, optimization.ErrIncompleteEvidence
		}
	}
	value := optimization.Activation{Kind: change.Kind, Reason: change.Reason, ID: change.ID, Scope: cortex.ScopeFromContext(tx.ctx), AgentID: change.AgentID, Channel: change.Channel, Version: change.Version, Generation: current.Generation + 1, ExpectedGeneration: change.ExpectedGeneration, Actor: change.Actor, PolicyDigest: change.PolicyDigest, DecisionID: change.DecisionID, RollbackTo: change.RollbackTo, CreatedAt: tx.now}
	eventType := "activated"
	if !value.RollbackTo.IsNil() {
		eventType = "activation_rolled_back"
	}
	return tx.saveActivation(key, current.ID, value, experiment, eventType)
}

func (tx *transaction) Deactivate(change optimization.DeactivationChange) (optimization.Activation, error) {
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
	if change.ExpectedGeneration < 1 || !optimization.ValidOpaqueKey(change.Actor.Subject) || strings.TrimSpace(change.Reason) == "" || len(change.Reason) > 512 || !utf8.ValidString(change.Reason) {
		return empty, optimization.ErrInvalidSpec
	}
	key, err := activationKey(change.AgentID, change.Channel, change.Kind)
	if err != nil {
		return empty, err
	}
	change.Kind, err = optimization.ActivationKind(change.Kind)
	if err != nil {
		return empty, err
	}
	change.Channel, err = optimization.ActivationChannel(change.Channel)
	if err != nil {
		return empty, err
	}
	if prior, exists := tx.data.Activations[change.ID.String()]; exists {
		if !prior.Deactivated || prior.AgentID != change.AgentID || prior.Channel != change.Channel || !activationKindMatches(prior.Kind, change.Kind) || prior.ExpectedGeneration != change.ExpectedGeneration || prior.Actor != change.Actor || prior.Reason != change.Reason {
			return empty, optimization.ErrConflict
		}
		return clone(prior)
	}
	current, exists := tx.data.Activations[tx.data.ActivationHeads[key].String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if current.Deactivated || current.Generation != change.ExpectedGeneration || current.Generation == math.MaxInt64 {
		return empty, optimization.ErrConflict
	}
	decision, exists := tx.data.Decisions[current.DecisionID.String()]
	if !exists {
		return empty, optimization.ErrIncompleteEvidence
	}
	experiment, exists := tx.data.Experiments[decision.ExperimentID.String()]
	if !exists {
		return empty, optimization.ErrIncompleteEvidence
	}
	value := current
	value.ID, value.Actor, value.Reason, value.Kind = change.ID, change.Actor, change.Reason, change.Kind
	value.ExpectedGeneration, value.Generation = change.ExpectedGeneration, current.Generation+1
	value.Deactivated, value.RollbackTo, value.CreatedAt = true, id.Nil, tx.now
	return tx.saveActivation(key, current.ID, value, experiment, "activation_deactivated")
}

func (tx *transaction) saveActivation(key string, previous id.ID, value optimization.Activation, experiment optimization.Experiment, eventType string) (optimization.Activation, error) {
	var empty optimization.Activation
	detached, err := clone(value)
	if err != nil {
		return empty, err
	}
	payload, err := canonical.Marshal(map[string]any{"activation_id": value.ID, "channel": value.Channel, "kind": value.Kind, "generation": value.Generation, "previous_activation_id": previous, "rollback_to": value.RollbackTo, "deactivated": value.Deactivated, "reason": value.Reason})
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
	event := tx.appendEvent(experiment, eventType, value.Actor)
	event.Resources = append(event.Resources, value.ID, value.Version.ID, value.DecisionID)
	event.Payload = payload
	return value, nil
}

func activationKindMatches(stored, normalized optimization.HarnessKind) bool {
	kind, err := optimization.ActivationKind(stored)
	return err == nil && kind == normalized
}
