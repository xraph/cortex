package optimization

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/id"
)

// ImprovementPhaseHandler schedules the frozen child stream with a dedicated worker.
type ImprovementPhaseHandler struct{ child *Optimizer }

// NewImprovementPhaseHandler wraps child phase execution and owns no goroutines.
// The returned handler belongs to the parent optimizer; config.Handler executes children.
func NewImprovementPhaseHandler(config OptimizerConfig) (*ImprovementPhaseHandler, error) {
	child, err := NewOptimizer(config)
	if err != nil {
		return nil, err
	}
	return &ImprovementPhaseHandler{child: child}, nil
}

// Execute advances one bounded parent step or delegates a standalone experiment.
// Child progress returns ErrConflict while the parent remains in its current phase.
func (h *ImprovementPhaseHandler) Execute(ctx context.Context, item WorkItem) (Transition, error) {
	resolved, done, err := h.child.meter.authorizeAction(ctx, item.Experiment.ID, ActionStart)
	if err != nil {
		return Transition{}, err
	}
	defer done()
	current, err := h.child.config.Repository.GetExperiment(resolved, item.Experiment.ID)
	if err != nil {
		return Transition{}, err
	}
	if current.State != StateRunning || current.Revision != item.Experiment.Revision || current.Phase != item.Experiment.Phase || current.Lease.Owner != item.Lease.Owner || current.Lease.Generation != item.Lease.Generation || current.Lease.ParentGeneration != item.Lease.ParentGeneration || item.Lease.ExperimentID != current.ID || !time.Now().Before(current.Lease.ExpiresAt) || !time.Now().Before(current.Deadline) {
		return Transition{}, ErrConflict
	}
	item.Experiment = current
	if current.Spec.Improvement == nil {
		return h.child.config.Handler.Execute(ctx, item)
	}
	switch current.Phase {
	case PhaseBaseline:
		return h.discover(ctx, item)
	case PhaseValidate:
		return h.validate(ctx, item)
	case PhaseDecide:
		return h.decide(ctx, item)
	default:
		return Transition{}, ErrUnavailable
	}
}

func (h *ImprovementPhaseHandler) discover(ctx context.Context, item WorkItem) (Transition, error) {
	repo := h.child.config.Repository
	for _, template := range item.Experiment.Spec.Improvement.Children {
		child, err := h.childSnapshot(ctx, template)
		if errors.Is(err, ErrNotFound) {
			if createErr := h.createChild(ctx, item, template); createErr != nil {
				return Transition{}, createErr
			}
			return Transition{}, ErrConflict
		}
		if err != nil {
			return Transition{}, err
		}
		if child.State != StateRunning || child.Phase != PhaseValidate {
			return Transition{}, h.stepChild(ctx, child)
		}
		if _, err = repo.GetDiscoveryLessons(ctx, child.ID); errors.Is(err, ErrNotFound) {
			return Transition{}, h.stepChild(ctx, child)
		}
		if err != nil {
			return Transition{}, err
		}
	}
	return Transition{NextState: StateRunning, NextPhase: PhaseValidate}, nil
}

func (h *ImprovementPhaseHandler) createChild(ctx context.Context, item WorkItem, template ImprovementChild) error {
	resolved, done, err := h.child.meter.authorizeAction(ctx, item.Experiment.ID, ActionCreate)
	if err != nil {
		return err
	}
	defer done()
	resource := Resource{Scope: copyScope(item.Experiment.Spec.Scope), AgentID: item.Experiment.Spec.AgentID, ID: template.ExperimentID, Kind: "experiment"}
	if authErr := h.child.config.Authorizer.Authorize(copyScopeContext(ctx), ActionCreate, resource); authErr != nil {
		return authErr
	}
	if authErr := h.child.config.Authorizer.Authorize(copyScopeContext(resolved), ActionCreate, resource); authErr != nil {
		return authErr
	}
	bundle := ExperimentBundle{Experiment: Experiment{ID: template.ExperimentID, Spec: template.Spec, EvaluationPlan: template.EvaluationPlan, LessonIDs: append([]id.ID(nil), item.Experiment.LessonIDs...)}, ParentLease: &item.Lease}
	return h.child.config.Repository.Transact(resolved, func(tx Transaction) error { return tx.CreateExperiment(bundle) })
}

func (h *ImprovementPhaseHandler) stepChild(ctx context.Context, child Experiment) error {
	switch child.State {
	case StateDraft:
		_, err := h.child.Start(ctx, child.ID, child.Revision)
		if err != nil {
			return err
		}
	case StateQueued, StateRunning:
		if err := h.child.WorkOnce(ctx, child.ID); err != nil {
			return err
		}
	case StatePaused:
		return ErrConflict
	case StateBudgetExhausted:
		return ErrBudgetExceeded
	default:
		return ErrIncompleteEvidence
	}
	return ErrConflict
}

func (h *ImprovementPhaseHandler) validate(ctx context.Context, item WorkItem) (Transition, error) {
	repo := h.child.config.Repository
	page, err := repo.ListValidationAllocations(ctx, item.Experiment.ID, PageQuery{Limit: 100})
	if err != nil {
		return Transition{}, err
	}
	if page.NextCursor != "" || len(page.Items) > 1 {
		return Transition{}, ErrIncompleteEvidence
	}
	if len(page.Items) == 0 {
		if allocationErr := h.allocate(ctx, item); allocationErr != nil {
			return Transition{}, allocationErr
		}
		return Transition{}, ErrConflict
	}
	allocation := page.Items[0]
	if allocation.ExperimentID != item.Experiment.ID || !allocation.ReleasedAt.IsZero() || len(allocation.PairedRoster) != len(item.Experiment.Spec.Improvement.Children) {
		return Transition{}, ErrIncompleteEvidence
	}
	for index, template := range item.Experiment.Spec.Improvement.Children {
		if allocation.PairedRoster[index].ExperimentID != template.ExperimentID {
			return Transition{}, ErrIncompleteEvidence
		}
		child, getErr := h.childSnapshot(ctx, template)
		if getErr != nil {
			return Transition{}, getErr
		}
		if child.State != StateRunning {
			return Transition{}, h.stepChild(ctx, child)
		}
		arm, armErr := assignedValidationArm(child, allocation)
		if armErr != nil {
			return Transition{}, armErr
		}
		trials, listErr := listExperimentTrials(ctx, repo, child.ID)
		if listErr != nil {
			return Transition{}, listErr
		}
		if validationArmReady(child, arm, allocation.ID, trials) {
			continue
		}
		// Leave operator-paused children paused and preserve other worker leases.
		if stepErr := h.stepChild(ctx, child); stepErr != nil && !errors.Is(stepErr, ErrConflict) {
			return Transition{}, stepErr
		}
		return Transition{}, ErrConflict
	}
	return Transition{NextState: StateRunning, NextPhase: PhaseDecide}, nil
}

func (h *ImprovementPhaseHandler) allocate(ctx context.Context, item WorkItem) error {
	repo := h.child.config.Repository
	roster := make([]ValidationArm, 0, len(item.Experiment.Spec.Improvement.Children))
	for _, template := range item.Experiment.Spec.Improvement.Children {
		child, err := h.childSnapshot(ctx, template)
		if err != nil {
			return err
		}
		trials, err := listExperimentTrials(ctx, repo, child.ID)
		if err != nil {
			return err
		}
		finalist, err := selectFinalist(ctx, repo, child, trials)
		if err != nil {
			return err
		}
		roster = append(roster, ValidationArm{ExperimentID: child.ID, Controller: child.Spec.Controller, Baseline: child.Spec.Baseline, Finalist: finalist})
	}
	resolved, done, err := h.child.meter.authorizeAction(ctx, item.Experiment.ID, ActionStart)
	if err != nil {
		return err
	}
	defer done()
	return repo.Transact(resolved, func(tx Transaction) error {
		_, allocationErr := tx.AllocateValidation(ValidationAllocationRequest{ID: id.New(PrefixValidation), ExperimentID: item.Experiment.ID, Lease: item.Lease, PairedRoster: roster})
		return allocationErr
	})
}

func validationArmReady(child Experiment, arm ValidationArm, allocation id.ID, trials []Trial) bool {
	for _, version := range []VersionRef{arm.Baseline, arm.Finalist} {
		for _, digest := range arm.CaseDigests {
			for repetition := range child.Spec.Limits.Repetitions {
				ready := false
				for _, trial := range trials {
					if trial.ExperimentID == child.ID && trial.Version == version && trial.CaseDigest == digest && trial.Repetition == repetition && trial.AllocationID == allocation && trial.Split == SplitValidation && TrialEvidenceReady(trial) {
						ready = true
						break
					}
				}
				if !ready {
					return false
				}
			}
		}
	}
	return true
}

func (h *ImprovementPhaseHandler) childSnapshot(ctx context.Context, template ImprovementChild) (Experiment, error) {
	resolved, done, err := h.child.meter.authorizeAction(ctx, template.ExperimentID, ActionStart)
	if err != nil {
		return Experiment{}, err
	}
	defer done()
	child, err := h.child.config.Repository.GetExperiment(resolved, template.ExperimentID)
	if err != nil {
		return Experiment{}, err
	}
	if !equalControllerData(child.Spec, template.Spec) || !equalControllerData(child.EvaluationPlan, template.EvaluationPlan) {
		return Experiment{}, ErrIncompleteEvidence
	}
	return child, nil
}
