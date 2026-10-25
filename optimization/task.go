package optimization

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/id"
)

// TaskPolicy freezes host isolation and optional artifact adoption policy.
// A nil adoption policy permits returning output but forbids artifact adoption.
type TaskPolicy struct {
	Deadline       time.Time             `json:"deadline"`
	Isolation      IsolationCapabilities `json:"isolation"`
	AdoptionPolicy *ContentRef           `json:"adoption_policy,omitempty"`
}

// TaskSpec carries one discovery task, frozen versions and evaluator criteria.
// Limits and budget are host-approved; zero rounds default to one.
type TaskSpec struct {
	Bundle         ExperimentBundle      `json:"bundle"`
	Isolation      IsolationCapabilities `json:"isolation"`
	AdoptionPolicy *ContentRef           `json:"adoption_policy,omitempty"`
}

// TaskResult returns the selected capture and every trial outcome for review.
// No execution or artifact adoption occurs when reading a completed result.
type TaskResult struct {
	ExperimentID  id.ID              `json:"experiment_id"`
	Trial         Trial              `json:"trial"`
	Output        string             `json:"output"`
	Artifacts     []ArtifactRef      `json:"artifacts"`
	EvidenceClass EvidenceClass      `json:"evidence_class"`
	Decision      Decision           `json:"decision"`
	Evidence      []DecisionEvidence `json:"evidence"`
}

// OptimizeTask runs bounded task-local work through the configured phase handler.
// After interruption, the returned experiment ID supports explicit recovery.
func (o *Optimizer) OptimizeTask(ctx context.Context, request TaskSpec) (*TaskResult, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return nil, err
	}
	if _, err := o.caller(ctx); err != nil {
		return nil, err
	}
	input, err := copyModelJSON(&request, 16<<20)
	if err != nil {
		return nil, err
	}
	request = *input
	bundle := &request.Bundle
	spec := &bundle.Experiment.Spec
	spec.Mode = ModeTaskLocal
	spec.Task = &TaskPolicy{Isolation: request.Isolation, AdoptionPolicy: request.AdoptionPolicy}
	if spec.Limits == (Limits{}) {
		spec.Limits = DefaultLimits()
		spec.Limits.MaxRounds, spec.Limits.Repetitions = 1, 1
	}
	if spec.Limits.MaxRounds == 0 {
		spec.Limits.MaxRounds = 1
	}
	if spec.Limits.Repetitions == 0 {
		spec.Limits.Repetitions = 1
	}
	spec.Task.Deadline = time.Now().UTC().Add(spec.Limits.ExperimentTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		if deadline.Before(spec.Task.Deadline) {
			spec.Task.Deadline = deadline.UTC()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if remaining < spec.Limits.ExperimentTimeout {
			spec.Limits.ExperimentTimeout = remaining
		}
		if spec.Limits.TrialTimeout > spec.Limits.ExperimentTimeout {
			spec.Limits.TrialTimeout = spec.Limits.ExperimentTimeout
		}
	}
	if validationErr := ValidateSpec(*spec); validationErr != nil {
		return nil, validationErr
	}
	if validationErr := bundle.Experiment.EvaluationPlan.Validate(*spec); validationErr != nil {
		return nil, validationErr
	}
	ctx, cancel := context.WithDeadline(ctx, spec.Task.Deadline)
	defer cancel()
	if bundle.Experiment.ID.IsNil() {
		bundle.Experiment.ID = id.New(PrefixExperiment)
	}
	if _, authorityErr := o.creationBundle(ctx, *bundle); authorityErr != nil {
		return nil, authorityErr
	}
	if qualificationErr := verifyEvaluationPlan(ctx, o.config.TaskEvaluator, bundle.Experiment); qualificationErr != nil {
		return nil, qualificationErr
	}
	created, err := o.Create(ctx, *bundle)
	if err != nil {
		return nil, err
	}
	result := &TaskResult{ExperimentID: created.ID}
	if _, err = o.Start(ctx, created.ID, created.Revision); err != nil {
		return result, o.stopCancelledTask(ctx, created.ID, err)
	}
	for {
		current, getErr := o.Get(ctx, created.ID)
		if getErr != nil {
			return result, o.stopCancelledTask(ctx, created.ID, getErr)
		}
		switch current.State {
		case StateCompleted:
			return o.GetTaskResult(ctx, current.ID)
		case StateQueued, StateRunning:
		default:
			if current.State == StateBudgetExhausted {
				return result, ErrBudgetExceeded
			}
			return result, ErrIncompleteEvidence
		}
		if err = o.WorkOnce(ctx, created.ID); err == nil {
			continue
		}
		if !errors.Is(err, ErrUnknownAttempt) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrBudgetExceeded) {
			return result, o.stopCancelledTask(ctx, created.ID, err)
		}
		timer := time.NewTimer(o.config.Worker.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, o.stopCancelledTask(ctx, created.ID, ctx.Err())
		case <-timer.C:
		}
	}
}

func (o *Optimizer) stopCancelledTask(ctx context.Context, experimentID id.ID, cause error) error {
	if ctx.Err() == nil {
		return cause
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := o.Get(cleanup, experimentID)
	if err != nil {
		return errors.Join(cause, err)
	}
	if current.State == StateQueued || current.State == StateRunning || current.State == StatePaused || current.State == StateDraft {
		_, err = o.Cancel(cleanup, experimentID, current.Revision)
	}
	return errors.Join(cause, err)
}

// GetTaskResult rechecks authority, lessons and artifacts without rerunning work.
func (o *Optimizer) GetTaskResult(ctx context.Context, experimentID id.ID) (*TaskResult, error) {
	resolved, done, err := o.meter.authorizeAction(ctx, experimentID, ActionRead)
	if err != nil {
		return nil, err
	}
	defer done()
	experiment, err := o.config.Repository.GetExperiment(resolved, experimentID)
	if err != nil {
		return nil, err
	}
	if experiment.Spec.Mode != ModeTaskLocal || experiment.State != StateCompleted {
		return nil, ErrConflict
	}
	decision, err := o.config.Repository.GetDecision(resolved, experiment.DecisionID)
	if err != nil {
		return nil, err
	}
	evidence, err := taskEvidence(resolved, o.config.Repository, experimentID)
	if err != nil {
		return nil, err
	}
	if validationErr := ValidateTaskDecision(experiment, decision, evidence); validationErr != nil {
		return nil, validationErr
	}
	result := &TaskResult{ExperimentID: experimentID, Decision: decision, Evidence: evidence, EvidenceClass: decision.EvidenceClass}
	if decision.TaskSelection == nil {
		return result, ErrIncompleteEvidence
	}
	for _, value := range evidence {
		for _, ref := range TrialArtifactRefs(value.Trial) {
			if o.config.ArtifactVerifier == nil {
				return nil, ErrUnavailable
			}
			if _, err = o.config.ArtifactVerifier.Verify(ctx, experimentID, ref); err != nil {
				return nil, err
			}
		}
		if value.Trial.ID == decision.TaskSelection.TrialID {
			result.Trial = value.Trial
			result.Output = value.Trial.Execution.Outcome.Output
			result.Artifacts = ExecutionArtifactRefs(*value.Trial.Execution)
		}
	}
	_, recheck, err := o.meter.authorizeAction(ctx, experimentID, ActionRead)
	if err != nil {
		return nil, err
	}
	recheck()
	if evidenceErr := o.checkTaskEvidence(resolved, experiment, evidence); evidenceErr != nil {
		return nil, evidenceErr
	}
	return result, nil
}

func (o *Optimizer) checkTaskEvidence(ctx context.Context, experiment Experiment, evidence []DecisionEvidence) error {
	for _, lesson := range experiment.FrozenLessons {
		if _, err := o.config.Repository.ResolveLesson(ctx, lesson); err != nil {
			return err
		}
	}
	for _, value := range evidence {
		if err := checkTrialArtifacts(ctx, o.config.Repository, value.Trial); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func taskEvidence(ctx context.Context, repository Repository, experimentID id.ID) ([]DecisionEvidence, error) {
	var evidence []DecisionEvidence
	query := TrialQuery{PageQuery: PageQuery{Limit: 100}}
	for {
		page, err := repository.ListTrials(ctx, experimentID, query)
		if err != nil {
			return nil, err
		}
		for _, trial := range page.Items {
			value := DecisionEvidence{Trial: trial}
			if trial.EvaluationID != "" {
				evaluation, getErr := repository.GetEvaluation(ctx, trial.EvaluationID)
				if getErr != nil {
					return nil, getErr
				}
				value.Evaluation = &evaluation
			}
			evidence = append(evidence, value)
		}
		if page.NextCursor == "" {
			return evidence, nil
		}
		query.Cursor = page.NextCursor
	}
}

func (h *TrialPhaseHandler) decideTask(ctx context.Context, item WorkItem) (Transition, error) {
	evidence, err := taskEvidence(ctx, h.config.Repository, item.Experiment.ID)
	if err != nil {
		return Transition{}, err
	}
	for _, value := range evidence {
		if artifactErr := h.checkArtifacts(ctx, value.Trial); artifactErr != nil {
			return Transition{}, artifactErr
		}
	}
	decision, err := BuildTaskDecision(item.Experiment, evidence)
	if err != nil {
		return Transition{}, err
	}
	base, err := h.config.Repository.GetVersion(ctx, item.Experiment.Spec.Baseline.ID)
	if err != nil {
		return Transition{}, err
	}
	decision.ID, decision.PolicyDigest = id.New(PrefixDecision), base.PolicyDigest
	return Transition{NextState: StateRunning, NextPhase: PhaseReflect, Decision: &decision}, nil
}
