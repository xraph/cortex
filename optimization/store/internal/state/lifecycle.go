package state

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
	if change.NextState != optimization.StateCancelled {
		if lessonErr := checkLessonBindings(tx.data, experiment, tx.now); lessonErr != nil {
			return lessonErr
		}
	}
	allowed := experiment.State == optimization.StateDraft && change.NextState == optimization.StateQueued
	allowed = allowed || (experiment.State == optimization.StateDraft || experiment.State == optimization.StateQueued) && change.NextState == optimization.StateCancelled
	finishing := experiment.State == optimization.StateRunning && experiment.Phase == optimization.PhaseReflect && (change.NextState == optimization.StateReviewRequired || change.NextState == optimization.StateCompleted)
	if finishing {
		if !tx.validLease(experiment, change.Lease) || change.NextPhase != "" || change.Decision != nil || len(change.Candidates)+len(change.Versions)+len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations) > 0 {
			return optimization.ErrConflict
		}
		if (experiment.Spec.Mode == optimization.ModeTaskLocal) != (change.NextState == optimization.StateCompleted) {
			return optimization.ErrInvalidSpec
		}
		decision, exists := tx.data.Decisions[experiment.DecisionID.String()]
		if !exists {
			return optimization.ErrIncompleteEvidence
		}
		if artifactErr := checkDecisionArtifacts(tx.data, decision, tx.now); artifactErr != nil {
			return artifactErr
		}
		allowed = true
	} else if len(change.Publications) > 0 {
		return optimization.ErrInvalidSpec
	}
	control := (experiment.State == optimization.StateRunning && (change.NextState == optimization.StatePaused || change.NextState == optimization.StateCancelled)) || (experiment.State == optimization.StatePaused && (change.NextState == optimization.StateQueued || change.NextState == optimization.StateCancelled))
	if control {
		if change.NextPhase != "" || change.Lease != (optimization.WorkLease{}) || change.Decision != nil || len(change.Candidates)+len(change.Versions)+len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations) > 0 {
			return optimization.ErrInvalidSpec
		}
		if change.NextState == optimization.StateQueued && !tx.now.Before(experiment.Deadline) {
			return optimization.ErrBudgetExceeded
		}
		allowed = true
	}
	exhausted := experiment.State == optimization.StateRunning && change.NextState == optimization.StateBudgetExhausted
	if exhausted {
		if !tx.validLease(experiment, change.Lease) || change.NextPhase != "" || change.Decision != nil || len(change.Candidates)+len(change.Versions)+len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations) > 0 {
			return optimization.ErrConflict
		}
		allowed = true
	}
	if experiment.State == optimization.StateRunning && change.NextState == optimization.StateRunning {
		if !tx.validLease(experiment, change.Lease) {
			return optimization.ErrConflict
		}
		allowed = nextPhase(experiment.Phase, change.NextPhase)
		if experiment.Spec.Mode == optimization.ModeTaskLocal {
			allowed = allowed && change.NextPhase != optimization.PhaseValidate || experiment.Phase == optimization.PhaseProbe && change.NextPhase == optimization.PhaseDecide
		}
		allowed = allowed || experiment.Phase == change.NextPhase && (len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations) > 0)
		if allowed && experiment.Phase != change.NextPhase && change.NextPhase == optimization.PhasePropose {
			if experiment.Round >= experiment.Spec.Limits.MaxRounds {
				return optimization.ErrConflict
			}
		}
	} else if !finishing && !exhausted && (change.NextPhase != "" || change.Lease.Generation != 0) {
		return optimization.ErrConflict
	}
	if !allowed {
		return optimization.ErrConflict
	}
	resources, err := tx.prepareCandidates(experiment, change)
	if err != nil {
		return err
	}
	trialResources, err := tx.prepareTrials(experiment, change, resources)
	if err != nil {
		return err
	}
	evaluationResources, err := tx.prepareEvaluations(experiment, change, trialResources)
	if err != nil {
		return err
	}
	decision, err := tx.prepareDecision(experiment, change)
	if err != nil {
		return err
	}
	if phaseErr := tx.validatePhaseEvidence(experiment, change, resources, trialResources); phaseErr != nil {
		return phaseErr
	}
	for _, publication := range change.Publications {
		if publication.ExperimentID != experiment.ID || publication.Lease != change.Lease || publication.Actor != change.Actor {
			return optimization.ErrInvalidSpec
		}
		if publicationErr := tx.PutPublication(publication); publicationErr != nil {
			return publicationErr
		}
	}
	if experiment.Phase != change.NextPhase && change.NextPhase == optimization.PhasePropose {
		experiment.Round++
	}
	payload := json.RawMessage("{}")
	if len(change.Evaluations) > 0 {
		evaluationIDs := make([]string, 0, len(change.Evaluations))
		for _, evaluation := range change.Evaluations {
			evaluationIDs = append(evaluationIDs, evaluation.ID)
		}
		payload, err = canonical.Marshal(map[string][]string{"evaluation_ids": evaluationIDs})
		if err != nil {
			return err
		}
	}
	previousState := experiment.State
	experiment.State = change.NextState
	if !control {
		experiment.Phase = change.NextPhase
	}
	experiment.Revision++
	experiment.UpdatedAt = tx.now
	if change.NextState == optimization.StateQueued && previousState == optimization.StateDraft {
		experiment.Deadline = tx.now.Add(experiment.Spec.Limits.ExperimentTimeout)
		if experiment.Spec.Task != nil && experiment.Spec.Task.Deadline.Before(experiment.Deadline) {
			experiment.Deadline = experiment.Spec.Task.Deadline
		}
		if !tx.now.Before(experiment.Deadline) {
			return optimization.ErrBudgetExceeded
		}
	}
	if decision != nil {
		tx.data.Decisions[decision.ID.String()] = *decision
		experiment.DecisionID = decision.ID
	}
	tx.data.Experiments[key] = experiment
	for evaluationID, evaluation := range evaluationResources {
		tx.data.Evaluations[evaluationID] = evaluation
	}
	for trialID, trial := range trialResources {
		tx.data.Trials[trialID] = trial
	}
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
	event.Payload = payload
	if decision != nil {
		event.Resources = append(event.Resources, decision.ID, decision.AllocationID)
	}
	for _, candidate := range change.Candidates {
		event.Resources = append(event.Resources, candidate.ID)
		if candidate.Accepted {
			event.Resources = append(event.Resources, candidate.Version.ID)
		}
	}
	for _, trial := range change.Trials {
		event.Resources = append(event.Resources, trial.ID)
	}
	for _, update := range change.TrialUpdates {
		event.Resources = append(event.Resources, update.TrialID)
		if !update.StartAttemptID.IsNil() {
			event.Resources = append(event.Resources, update.StartAttemptID)
		} else if !update.AttemptID.IsNil() {
			event.Resources = append(event.Resources, update.AttemptID)
		}
	}
	for _, evaluation := range change.Evaluations {
		event.Resources = append(event.Resources, evaluation.TrialID)
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
