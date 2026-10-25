package optimization

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex/id"
)

// ControllerPhaseHandler executes proposal, composition and reflection through
// the metered controller. Other phases use the host's trial/evaluation handler.
type ControllerPhaseHandler struct {
	repository Repository
	models     *ControllerFactory
	trials     PhaseHandler
	domain     string
}

// NewControllerPhaseHandler composes controller phases with existing trial work.
// A nil trial handler explicitly reports unavailable for non-controller phases.
func NewControllerPhaseHandler(repo Repository, models *ControllerFactory, trials PhaseHandler, domain string) (*ControllerPhaseHandler, error) {
	if repo == nil || models == nil || !ValidOpaqueKey(domain) {
		return nil, ErrInvalidSpec
	}
	return &ControllerPhaseHandler{repository: repo, models: models, trials: trials, domain: domain}, nil
}

// Execute returns a transition whose resources the worker commits atomically.
func (h *ControllerPhaseHandler) Execute(ctx context.Context, item WorkItem) (Transition, error) {
	if item.Experiment.Spec.Improvement != nil {
		return Transition{}, ErrUnavailable
	}
	phase := item.Experiment.Phase
	if phase == PhaseValidate && item.Experiment.Spec.Parent != nil {
		parent, err := h.repository.GetExperiment(ctx, item.Experiment.Spec.Parent.ExperimentID)
		if err != nil {
			return Transition{}, err
		}
		if parent.Phase == PhaseBaseline {
			if reflectionErr := h.reflectDiscovery(ctx, item); reflectionErr != nil {
				return Transition{}, reflectionErr
			}
			return Transition{}, ErrConflict
		}
	}
	if phase != PhasePropose && phase != PhaseCompose && phase != PhaseReflect {
		if h.trials == nil {
			return Transition{}, ErrUnavailable
		}
		return h.trials.Execute(ctx, item)
	}
	controller, err := h.models.ForWork(ctx, item)
	if err != nil {
		return Transition{}, err
	}
	base, err := h.repository.GetVersion(ctx, item.Experiment.Spec.Baseline.ID)
	if err != nil {
		return Transition{}, err
	}
	evidence, err := h.discovery(ctx, item.Experiment)
	if err != nil {
		return Transition{}, err
	}
	candidates, err := h.candidates(ctx, item.Experiment)
	if err != nil {
		return Transition{}, err
	}
	lessons := make([]Lesson, 0, len(item.Experiment.FrozenLessons))
	for _, ref := range item.Experiment.FrozenLessons {
		lesson, resolveErr := h.repository.ResolveLesson(ctx, ref)
		if resolveErr != nil {
			return Transition{}, resolveErr
		}
		lessons = append(lessons, lesson)
	}
	discoveries := make([]DiscoveryLessons, 0, len(item.Experiment.Discoveries))
	for _, ref := range item.Experiment.Discoveries {
		value, resolveErr := h.repository.ResolveDiscoveryLessons(ctx, item.Experiment.ID, ref)
		if resolveErr != nil {
			return Transition{}, resolveErr
		}
		discoveries = append(discoveries, value)
	}
	input := ProposalInput{Baseline: base, Policy: base.Policy, Round: item.Experiment.Round, Evidence: evidence, Lessons: lessons, Discoveries: discoveries}
	if phase == PhaseReflect {
		return h.reflect(ctx, item, controller, evidence, candidates)
	}
	var proposals []Proposal
	var parents []id.ID
	origin := OriginGeneral
	if phase == PhaseCompose {
		accepted := make([]Candidate, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.Accepted {
				accepted = append(accepted, candidate)
				parents = append(parents, candidate.ID)
			}
		}
		if len(accepted) < 2 {
			return Transition{}, ErrIncompleteEvidence
		}
		proposal, callErr := controller.Compose(ctx, CompositionInput{ProposalInput: input, Candidates: accepted})
		if callErr != nil {
			return Transition{}, callErr
		}
		proposals = []Proposal{proposal}
		origin = OriginComposed
	} else {
		proposals, err = controller.Propose(ctx, input)
		if err != nil {
			return Transition{}, err
		}
	}
	if len(proposals) > item.Experiment.Spec.Limits.MaxCandidates-len(candidates) {
		return Transition{}, ErrBudgetExceeded
	}
	change := Transition{NextState: StateRunning, NextPhase: PhaseProbe}
	seen := map[string]bool{base.Digest: true}
	for _, candidate := range candidates {
		if candidate.Accepted {
			seen[candidate.Version.Digest] = true
		}
	}
	for _, proposal := range proposals {
		candidate := Candidate{ID: id.New(PrefixCandidate), ExperimentID: item.Experiment.ID, Origin: origin, Round: item.Experiment.Round, Proposal: proposal, Parents: parents}
		version, validateErr := ValidateProposal(base, proposal, base.Policy)
		switch {
		case validateErr != nil:
			candidate.Rejection = validateErr.Error()
		case seen[version.Digest]:
			candidate.Rejection = "duplicate harness content"
		default:
			candidate.Accepted = true
			candidate.Version = VersionRef{ID: version.ID, Digest: version.Digest}
			change.Versions = append(change.Versions, version)
			seen[version.Digest] = true
		}
		change.Candidates = append(change.Candidates, candidate)
	}
	return change, nil
}

func (h *ControllerPhaseHandler) discovery(ctx context.Context, experiment Experiment) ([]DiscoveryEvidence, error) {
	var result []DiscoveryEvidence
	query := TrialQuery{PageQuery: PageQuery{Limit: 100}, Split: SplitDiscovery}
	for {
		page, err := h.repository.ListTrials(ctx, experiment.ID, query)
		if err != nil {
			return nil, err
		}
		for _, trial := range page.Items {
			if trial.Execution == nil || trial.EvaluationID == "" {
				continue
			}
			evaluation, getErr := h.repository.GetEvaluation(ctx, trial.EvaluationID)
			if getErr != nil {
				return nil, getErr
			}
			result = append(result, DiscoveryEvidence{Trial: trial, Evaluation: evaluation})
			if len(result) > experiment.Spec.Limits.MaxTrials {
				return nil, ErrInvalidSpec
			}
		}
		if page.NextCursor == "" {
			return result, nil
		}
		query.Cursor = page.NextCursor
	}
}

func (h *ControllerPhaseHandler) candidates(ctx context.Context, experiment Experiment) ([]Candidate, error) {
	var result []Candidate
	query := PageQuery{Limit: 100}
	for {
		page, err := h.repository.ListCandidates(ctx, experiment.ID, query)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if len(result) > experiment.Spec.Limits.MaxCandidates {
			return nil, ErrInvalidSpec
		}
		if page.NextCursor == "" {
			return result, nil
		}
		query.Cursor = page.NextCursor
	}
}

func (h *ControllerPhaseHandler) reflect(ctx context.Context, item WorkItem, controller Controller, evidence []DiscoveryEvidence, candidates []Candidate) (Transition, error) {
	if item.Experiment.DecisionID.IsNil() {
		return Transition{}, ErrIncompleteEvidence
	}
	drafts, err := controller.Reflect(ctx, ReflectionInput{Evidence: evidence, Candidates: candidates, Domain: h.domain})
	if err != nil {
		return Transition{}, err
	}
	change := Transition{NextState: StateReviewRequired}
	if item.Experiment.Spec.Mode == ModeTaskLocal {
		change.NextState = StateCompleted
	}
	allowed := map[id.ID]bool{}
	for _, value := range evidence {
		allowed[value.Trial.ID] = true
	}
	for _, draft := range drafts {
		for _, ref := range draft.EvidenceIDs {
			if !allowed[ref] {
				return Transition{}, ErrPolicyDenied
			}
		}
		lesson := Lesson{ID: id.New(PrefixLesson), Scope: copyScope(item.Experiment.Spec.Scope), Domain: h.domain, HarnessSchema: 1, Statement: draft.Statement, Applicability: draft.Applicability, Contraindications: draft.Contraindications, DecisionIDs: []id.ID{item.Experiment.DecisionID}, TrialIDs: draft.EvidenceIDs, EvidenceClass: EvidenceExploratory}
		if validateErr := ValidateLessonDraft(lesson); validateErr != nil {
			return Transition{}, validateErr
		}
		change.Publications = append(change.Publications, PublicationRequest{ExperimentID: item.Experiment.ID, Lease: item.Lease, Actor: item.Experiment.Spec.Requester, Lesson: lesson, RemoteAggregateID: fmt.Sprintf("optimization/%s", lesson.ID)})
	}
	return change, nil
}

// reflectDiscovery persists learning before the parent opens paired validation.
// A saved result is acknowledged without replaying its metered model call.
func (h *ControllerPhaseHandler) reflectDiscovery(ctx context.Context, item WorkItem) error {
	controller, err := h.models.forWork(ctx, item, true)
	if err != nil {
		return err
	}
	prior, err := h.repository.GetDiscoveryLessons(ctx, item.Experiment.ID)
	var drafts []LessonDraft
	switch {
	case err == nil:
		if prior.Domain != h.domain {
			return ErrConflict
		}
		drafts = prior.Lessons
	case errors.Is(err, ErrNotFound):
		evidence, evidenceErr := h.discovery(ctx, item.Experiment)
		if evidenceErr != nil {
			return evidenceErr
		}
		candidates, candidateErr := h.candidates(ctx, item.Experiment)
		if candidateErr != nil {
			return candidateErr
		}
		drafts, err = controller.Reflect(ctx, ReflectionInput{Evidence: evidence, Candidates: candidates, Domain: h.domain})
		if err != nil {
			return err
		}
	default:
		return err
	}
	resolved, done, authErr := h.models.meter.authorize(ctx, item.Experiment.ID)
	if authErr != nil {
		return authErr
	}
	defer done()
	return h.repository.Transact(resolved, func(tx Transaction) error {
		_, putErr := tx.PutDiscoveryLessons(DiscoveryLessonsRequest{ExperimentID: item.Experiment.ID, Lease: item.Lease, Domain: h.domain, Lessons: drafts})
		return putErr
	})
}
