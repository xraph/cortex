package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

func (tx *transaction) CommitTransition(change optimization.Transition) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	if change.ExpectedRevision < 1 || strings.TrimSpace(change.IdempotencyKey) == "" || strings.TrimSpace(change.Actor.Subject) == "" {
		return optimization.ErrInvalidSpec
	}
	key := change.ExperimentID.String()
	experiment, exists := tx.data.Experiments[key]
	if !exists {
		return optimization.ErrNotFound
	}
	encoded, err := canonical.Marshal(change)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	prior := tx.data.Transitions[key]
	if committed, found := prior[change.IdempotencyKey]; found {
		if committed != digest {
			return optimization.ErrConflict
		}
		return nil
	}
	if experiment.Revision != change.ExpectedRevision {
		return optimization.ErrConflict
	}
	allowed := experiment.State == optimization.StateDraft && change.NextState == optimization.StateQueued
	allowed = allowed || (experiment.State == optimization.StateDraft || experiment.State == optimization.StateQueued) && change.NextState == optimization.StateCancelled
	if experiment.State == optimization.StateRunning && change.NextState == optimization.StateRunning {
		if !tx.validLease(experiment, change.Lease) {
			return optimization.ErrConflict
		}
		allowed = nextPhase(experiment.Phase, change.NextPhase)
		if allowed && change.NextPhase == optimization.PhasePropose {
			if experiment.Round >= experiment.Spec.Limits.MaxRounds {
				return optimization.ErrConflict
			}
			experiment.Round++
		}
	} else if change.NextPhase != "" || change.Lease.Generation != 0 {
		return optimization.ErrConflict
	}
	if !allowed {
		return optimization.ErrConflict
	}
	resources, err := tx.prepareCandidates(experiment, change)
	if err != nil {
		return err
	}
	experiment.State = change.NextState
	experiment.Phase = change.NextPhase
	experiment.Revision++
	experiment.UpdatedAt = tx.now
	if change.NextState == optimization.StateQueued {
		experiment.Deadline = tx.now.Add(experiment.Spec.Limits.ExperimentTimeout)
	}
	tx.data.Experiments[key] = experiment
	for candidateID, candidate := range resources.candidates {
		tx.data.Candidates[candidateID] = candidate
	}
	for versionID, version := range resources.versions {
		if _, exists := tx.data.Versions[versionID]; !exists {
			tx.data.Versions[versionID] = version
		}
	}
	if prior == nil {
		prior = make(map[string]string)
		tx.data.Transitions[key] = prior
	}
	prior[change.IdempotencyKey] = digest
	eventType := string(change.NextState)
	if change.NextPhase != "" {
		eventType = string(change.NextPhase)
	}
	event := tx.appendEvent(experiment, eventType, change.Actor)
	for _, candidate := range change.Candidates {
		event.Resources = append(event.Resources, candidate.ID)
		if candidate.Accepted {
			event.Resources = append(event.Resources, candidate.Version.ID)
		}
	}
	return nil
}

func nextPhase(current, next optimization.Phase) bool {
	switch current {
	case optimization.PhaseBaseline:
		return next == optimization.PhasePropose
	case optimization.PhasePropose:
		return next == optimization.PhaseProbe
	case optimization.PhaseProbe:
		return next == optimization.PhaseCompose || next == optimization.PhasePropose || next == optimization.PhaseValidate
	case optimization.PhaseCompose:
		return next == optimization.PhaseProbe
	case optimization.PhaseValidate:
		return next == optimization.PhaseDecide
	case optimization.PhaseDecide:
		return next == optimization.PhaseReflect
	default:
		return false
	}
}

func (tx *transaction) appendEvent(experiment optimization.Experiment, eventType string, actor optimization.IdentityRef) *optimization.Event {
	key := experiment.ID.String()
	events := tx.data.Events[key]
	event := optimization.Event{ID: id.New(optimization.PrefixEvent), ExperimentID: experiment.ID, Sequence: int64(len(events)) + 1, Type: eventType, Actor: actor, Resources: []id.ID{experiment.ID, experiment.Spec.Baseline.ID, experiment.Spec.Controller.ID}, Payload: json.RawMessage(`{}`), CreatedAt: tx.now}
	tx.data.Events[key] = append(events, event)
	return &tx.data.Events[key][len(events)]
}
