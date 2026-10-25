package optimization

import (
	"context"
	"fmt"
	"slices"

	"github.com/xraph/cortex/id"
)

func (h *TrialPhaseHandler) candidates(ctx context.Context, experiment Experiment) ([]Candidate, error) {
	var result []Candidate
	query := PageQuery{Limit: 100}
	for {
		page, err := h.config.Repository.ListCandidates(ctx, experiment.ID, query)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if len(result) > experiment.Spec.Limits.MaxCandidates {
			return nil, ErrIncompleteEvidence
		}
		if page.NextCursor == "" {
			return result, nil
		}
		query.Cursor = page.NextCursor
	}
}
func scheduleTrials(e Experiment, existing []Trial, version VersionRef, candidate, allocation id.ID, split Split) []Trial {
	var result []Trial
	for _, task := range e.Spec.Tasks {
		if task.Split != split {
			continue
		}
		for rep := range e.Spec.Limits.Repetitions {
			if slices.ContainsFunc(existing, func(t Trial) bool {
				return t.Version == version && t.CaseDigest == task.Digest && t.Repetition == rep && t.AllocationID == allocation
			}) {
				continue
			}
			result = append(result, Trial{ID: id.New(PrefixTrial), ExperimentID: e.ID, CandidateID: candidate, AllocationID: allocation, Version: version, CaseID: task.ID, CaseDigest: task.Digest, Split: split, Repetition: rep})
		}
	}
	return result
}
func (h *TrialPhaseHandler) schedule(ctx context.Context, item WorkItem, trials []Trial) (Transition, error) {
	change := Transition{NextState: StateRunning, NextPhase: item.Experiment.Phase}
	switch item.Experiment.Phase {
	case PhaseBaseline:
		change.Trials = scheduleTrials(item.Experiment, trials, item.Experiment.Spec.Baseline, id.ID{}, id.ID{}, SplitDiscovery)
	case PhaseProbe:
		candidates, err := h.candidates(ctx, item.Experiment)
		if err != nil {
			return change, err
		}
		for _, c := range candidates {
			if c.Accepted {
				change.Trials = append(change.Trials, scheduleTrials(item.Experiment, trials, c.Version, c.ID, id.ID{}, SplitDiscovery)...)
			}
		}
	case PhaseValidate:
		allocation, err := h.allocation(ctx, item, trials)
		if err != nil {
			return change, err
		}
		arm := allocation.PairedRoster[0]
		change.Trials = scheduleTrials(item.Experiment, trials, arm.Baseline, id.ID{}, allocation.ID, SplitValidation)
		if arm.Finalist != arm.Baseline {
			candidates, getErr := h.candidates(ctx, item.Experiment)
			if getErr != nil {
				return change, getErr
			}
			var candidate id.ID
			for _, c := range candidates {
				if c.Accepted && c.Version == arm.Finalist {
					candidate = c.ID
				}
			}
			if candidate.IsNil() {
				return change, ErrIncompleteEvidence
			}
			change.Trials = append(change.Trials, scheduleTrials(item.Experiment, trials, arm.Finalist, candidate, allocation.ID, SplitValidation)...)
		}
	}
	return change, nil
}
func (h *TrialPhaseHandler) afterProbe(ctx context.Context, e Experiment) (Transition, error) {
	candidates, err := h.candidates(ctx, e)
	if err != nil {
		return Transition{}, err
	}
	accepted, composed := 0, false
	for _, c := range candidates {
		if c.Accepted {
			accepted++
		}
		if c.Origin == OriginComposed && c.Round == e.Round {
			composed = true
		}
	}
	phase := PhaseValidate
	if e.Spec.Mode == ModeTaskLocal {
		phase = PhaseDecide
	}
	if len(candidates) < e.Spec.Limits.MaxCandidates {
		if accepted >= 2 && !composed {
			phase = PhaseCompose
		} else if e.Round < e.Spec.Limits.MaxRounds {
			phase = PhasePropose
		}
	}
	return Transition{NextState: StateRunning, NextPhase: phase}, nil
}
func (h *TrialPhaseHandler) allocation(ctx context.Context, item WorkItem, trials []Trial) (ValidationAllocation, error) {
	page, err := h.config.Repository.ListValidationAllocations(ctx, item.Experiment.ID, PageQuery{Limit: 100})
	if err != nil {
		return ValidationAllocation{}, err
	}
	if page.NextCursor != "" || len(page.Items) > 1 {
		return ValidationAllocation{}, ErrIncompleteEvidence
	}
	var allocation ValidationAllocation
	if len(page.Items) == 1 {
		allocation = page.Items[0]
		if len(allocation.PairedRoster) != 1 || allocation.PairedRoster[0].ExperimentID != item.Experiment.ID || !allocation.ReleasedAt.IsZero() {
			return allocation, ErrUnavailable
		}
	} else {
		finalist, selectErr := h.finalist(ctx, item.Experiment, trials)
		if selectErr != nil {
			return allocation, selectErr
		}
		err = h.config.Repository.Transact(ctx, func(tx Transaction) error {
			var allocateErr error
			allocation, allocateErr = tx.AllocateValidation(ValidationAllocationRequest{ID: id.New(PrefixValidation), ExperimentID: item.Experiment.ID, Lease: item.Lease, PairedRoster: []ValidationArm{{ExperimentID: item.Experiment.ID, Controller: item.Experiment.Spec.Controller, Baseline: item.Experiment.Spec.Baseline, Finalist: finalist}}})
			return allocateErr
		})
		if err != nil {
			return allocation, err
		}
	}
	if allocation.ExposedAt.IsZero() {
		err = h.config.Repository.Transact(ctx, func(tx Transaction) error {
			return tx.ExposeValidation(ValidationAccess{AllocationID: allocation.ID, ExperimentID: item.Experiment.ID, Lease: item.Lease})
		})
		if err != nil {
			return allocation, err
		}
	}
	return allocation, nil
}
func (h *TrialPhaseHandler) finalist(ctx context.Context, e Experiment, trials []Trial) (VersionRef, error) {
	type score struct {
		sum   float64
		count int
		valid bool
	}
	scores := map[VersionRef]*score{}
	count := 0
	for _, task := range e.Spec.Tasks {
		if task.Split == SplitDiscovery {
			count += e.Spec.Limits.Repetitions
		}
	}
	for _, t := range trials {
		if t.Split != SplitDiscovery {
			continue
		}
		value := scores[t.Version]
		if value == nil {
			value = &score{valid: true}
			scores[t.Version] = value
		}
		value.count++
		if t.Execution == nil || t.EvaluationID == "" {
			value.valid = false
			continue
		}
		if err := h.checkArtifacts(ctx, t); err != nil {
			return VersionRef{}, err
		}
		evaluation, err := h.config.Repository.GetEvaluation(ctx, t.EvaluationID)
		if err != nil {
			return VersionRef{}, err
		}
		if !evaluation.Complete || evaluation.Score == nil {
			value.valid = false
			continue
		}
		value.sum += *evaluation.Score
	}
	best := e.Spec.Baseline
	bestScore := -1.0
	if value := scores[best]; value != nil && value.valid && value.count == count {
		bestScore = value.sum / float64(count)
	}
	versions := make([]VersionRef, 0, len(scores))
	for version := range scores {
		versions = append(versions, version)
	}
	slices.SortFunc(versions, func(a, b VersionRef) int {
		if a.ID.String() < b.ID.String() {
			return -1
		}
		if a.ID == b.ID {
			return 0
		}
		return 1
	})
	for _, version := range versions {
		value := scores[version]
		if value.valid && value.count == count && value.sum/float64(count) > bestScore {
			best, bestScore = version, value.sum/float64(count)
		}
	}
	return best, nil
}
func (h *TrialPhaseHandler) decide(ctx context.Context, item WorkItem, trials []Trial) (Transition, error) {
	e := item.Experiment
	page, err := h.config.Repository.ListValidationAllocations(ctx, e.ID, PageQuery{Limit: 100})
	if err != nil {
		return Transition{}, err
	}
	if len(page.Items) != 1 || page.NextCursor != "" || len(page.Items[0].PairedRoster) != 1 {
		return Transition{}, ErrIncompleteEvidence
	}
	allocation := page.Items[0]
	arm := allocation.PairedRoster[0]
	base, err := h.config.Repository.GetVersion(ctx, e.Spec.Baseline.ID)
	if err != nil {
		return Transition{}, err
	}
	decision := Decision{ID: id.New(PrefixDecision), ExperimentID: e.ID, SpecDigest: e.SpecDigest, AllocationID: allocation.ID, Baseline: arm.Baseline, Candidate: arm.Finalist, PolicyDigest: base.PolicyDigest, SelectionKind: string(HarnessExecution), Outcome: DecisionInconclusive, EvidenceClass: EvidenceExploratory}
	var evidence []DecisionEvidence
	request := ComparisonRequest{Plan: e.EvaluationPlan, Objective: e.Spec.Objective}
	for _, t := range trials {
		if t.Split != SplitValidation {
			continue
		}
		if t.AllocationID != allocation.ID {
			return Transition{}, ErrIncompleteEvidence
		}
		if artifactErr := h.checkArtifacts(ctx, t); artifactErr != nil {
			return Transition{}, artifactErr
		}
		value := DecisionEvidence{Trial: t}
		if t.EvaluationID != "" {
			evaluation, getErr := h.config.Repository.GetEvaluation(ctx, t.EvaluationID)
			if getErr != nil {
				return Transition{}, getErr
			}
			value.Evaluation = &evaluation
			decision.EvaluationIDs = append(decision.EvaluationIDs, t.EvaluationID)
			if t.Version == arm.Baseline {
				request.BaselineIDs = append(request.BaselineIDs, t.EvaluationID)
			} else {
				request.CandidateIDs = append(request.CandidateIDs, t.EvaluationID)
			}
		}
		evidence = append(evidence, value)
	}
	if arm.Baseline == arm.Finalist {
		decision.Outcome = DecisionRetain
		decision.Reasons = []string{"No discovery candidate improved on the baseline."}
		decision.Comparison = Comparison{BaselineIDs: request.BaselineIDs, PlanDigest: request.Plan.Ref.Digest, Confidence: request.Objective.Confidence, BootstrapSamples: request.Objective.BootstrapSamples, BootstrapSeed: request.Objective.BootstrapSeed, MissingReasons: []string{"The finalist is the unchanged baseline."}}
	} else {
		decision.Comparison, err = h.config.Evaluator.Compare(ctx, request)
		if err != nil {
			return Transition{}, err
		}
		c := decision.Comparison
		o := e.Spec.Objective
		decision.Reasons = []string{"Validation evidence is insufficient for a recommendation."}
		if c.Complete {
			decision.Outcome = DecisionRetain
			decision.EvidenceClass = EvidenceValidated
			decision.Reasons = []string{"The candidate did not pass every frozen selection threshold."}
			protected := true
			for _, criterion := range o.ProtectedCriteria {
				regression, ok := c.ProtectedRegressions[criterion]
				protected = protected && ok && regression <= o.MaxProtectedRegression
			}
			if c.MeanDelta >= o.MinImprovement && c.LowerBound > 0 && len(c.RequiredFailures) == 0 && protected && ratioPass(o.MaxCostRatio, c.CostRatio) && ratioPass(o.MaxLatencyRatio, c.LatencyRatio) && e.Spec.Mode != ModeTaskLocal {
				decision.Outcome = DecisionRecommend
				decision.Reasons = []string{"Paired validation passed the frozen quality and resource thresholds."}
			}
		} else {
			decision.Reasons = append(decision.Reasons, c.MissingReasons...)
		}
	}
	if err = ValidateDecisionEvidence(e, decision, evidence); err != nil {
		return Transition{}, fmt.Errorf("validate trial decision: %w", err)
	}
	return Transition{NextState: StateRunning, NextPhase: PhaseReflect, Decision: &decision}, nil
}
