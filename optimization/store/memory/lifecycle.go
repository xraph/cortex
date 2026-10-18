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
	if change.NextPhase != "" || change.Lease.Generation != 0 {
		return optimization.ErrConflict
	}
	allowed := experiment.State == optimization.StateDraft && change.NextState == optimization.StateQueued
	allowed = allowed || (experiment.State == optimization.StateDraft || experiment.State == optimization.StateQueued) && change.NextState == optimization.StateCancelled
	if !allowed {
		return optimization.ErrConflict
	}
	experiment.State = change.NextState
	experiment.Revision++
	experiment.UpdatedAt = tx.now
	if change.NextState == optimization.StateQueued {
		experiment.Deadline = tx.now.Add(experiment.Spec.Limits.ExperimentTimeout)
	}
	tx.data.Experiments[key] = experiment
	if prior == nil {
		prior = make(map[string]string)
		tx.data.Transitions[key] = prior
	}
	prior[change.IdempotencyKey] = digest
	tx.appendEvent(experiment, string(change.NextState), change.Actor)
	return nil
}

func (tx *transaction) appendEvent(experiment optimization.Experiment, eventType string, actor optimization.IdentityRef) {
	key := experiment.ID.String()
	events := tx.data.Events[key]
	event := optimization.Event{ID: id.New(optimization.PrefixEvent), ExperimentID: experiment.ID, Sequence: int64(len(events)) + 1, Type: eventType, Actor: actor, Resources: []id.ID{experiment.ID, experiment.Spec.Baseline.ID, experiment.Spec.Controller.ID}, Payload: json.RawMessage(`{}`), CreatedAt: tx.now}
	tx.data.Events[key] = append(events, event)
}
