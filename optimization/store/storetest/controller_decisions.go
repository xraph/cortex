package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// ControllerDecisions preserves child execution provenance in parent decisions.
func ControllerDecisions(t *testing.T, factory Factory) {
	t.Helper()
	f, allocation, evidence, decision := controllerDecisionFixture(t, factory)
	request, err := optimization.BuildControllerComparison(f.parent.Experiment, allocation, evidence)
	if err != nil || len(request.Groups) != 2 || request.ExperimentID != f.parent.Experiment.ID || request.AllocationID != allocation.ID {
		t.Fatalf("frozen controller request: %+v %v", request, err)
	}
	if err = optimization.ValidateControllerDecisionEvidence(f.parent.Experiment, allocation, decision, evidence); err != nil {
		t.Fatalf("valid parent evidence: %v", err)
	}
	for _, mode := range []string{"missing-child", "wrong-child", "changed-controller", "changed-plan", "changed-finalist", "changed-requester", "changed-scope", "trial-owner", "trial-version", "trial-allocation", "missing-trial", "duplicate-trial", "evaluation-owner", "evaluation-plan", "missing-evaluation", "changed-score", "invented-cost", "wrong-kind", "wrong-baseline", "wrong-candidate", "omitted-plan", "invented-plan-digest", "omitted-job", "unexposed-allocation", "stopped-child", "fabricated-recommendation"} {
		t.Run(mode, func(t *testing.T) {
			copyValue := struct {
				Allocation optimization.ValidationAllocation
				Evidence   []optimization.ControllerEvidence
				Decision   optimization.Decision
			}{allocation, evidence, decision}
			data, encodeErr := json.Marshal(copyValue)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			copyValue.Allocation = optimization.ValidationAllocation{}
			copyValue.Evidence = nil
			copyValue.Decision = optimization.Decision{}
			if decodeErr := json.Unmarshal(data, &copyValue); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			a, c, d := &copyValue.Allocation, &copyValue.Evidence, &copyValue.Decision
			switch mode {
			case "missing-child":
				*c = (*c)[:len(*c)-1]
			case "wrong-child":
				(*c)[0].Experiment.ID = id.New(optimization.PrefixExperiment)
			case "changed-controller":
				(*c)[0].Experiment.Spec.Controller = (*c)[1].Experiment.Spec.Controller
			case "changed-plan":
				(*c)[0].Experiment.EvaluationPlan.Ref.Digest = "changed"
			case "changed-finalist":
				a.PairedRoster[0].Finalist = f.parent.Experiment.Spec.Controller
			case "changed-requester":
				(*c)[0].Experiment.Spec.Requester.Subject = "other-requester"
			case "changed-scope":
				(*c)[0].Experiment.Spec.Scope.Levels[0].Value = "other-scope"
			case "trial-owner":
				(*c)[0].Trials[0].Trial.ExperimentID = f.parent.Experiment.ID
			case "trial-version":
				(*c)[0].Trials[0].Trial.Version = f.parent.Experiment.Spec.Controller
			case "trial-allocation":
				(*c)[0].Trials[0].Trial.AllocationID = id.New(optimization.PrefixValidation)
			case "missing-trial":
				(*c)[0].Trials = (*c)[0].Trials[1:]
			case "duplicate-trial":
				(*c)[0].Trials = append((*c)[0].Trials, (*c)[0].Trials[0])
			case "evaluation-owner":
				(*c)[0].Trials[0].Evaluation.ExperimentID = f.parent.Experiment.ID
			case "evaluation-plan":
				(*c)[0].Trials[0].Evaluation.PlanDigest = "other-plan"
			case "missing-evaluation":
				(*c)[0].Trials[0].Evaluation = nil
			case "changed-score":
				d.Comparison.MeanDelta = .4
			case "invented-cost":
				cost := .1
				d.Comparison.CostRatio = &cost
			case "wrong-kind":
				d.SelectionKind = string(optimization.HarnessExecution)
			case "wrong-baseline":
				d.Baseline = f.parent.Experiment.Spec.Baseline
			case "wrong-candidate":
				d.Candidate = f.parent.Experiment.Spec.Baseline
			case "omitted-plan":
				d.Comparison.Plans = d.Comparison.Plans[1:]
			case "invented-plan-digest":
				d.Comparison.PlanDigest = f.parent.Experiment.EvaluationPlan.Ref.Digest
			case "omitted-job":
				d.EvaluationIDs = d.EvaluationIDs[1:]
			case "unexposed-allocation":
				a.ExposedAt = a.ReleasedAt
			case "stopped-child":
				(*c)[0].Experiment.State = optimization.StatePaused
			case "fabricated-recommendation":
				d.Outcome, d.EvidenceClass, d.Comparison.Complete = optimization.DecisionRecommend, optimization.EvidenceValidated, true
				d.Comparison.MissingReasons = nil
			}
			if validationErr := optimization.ValidateControllerDecisionEvidence(f.parent.Experiment, *a, *d, *c); validationErr == nil {
				t.Fatalf("accepted %s", mode)
			}
			if slices.Contains([]string{"changed-score", "invented-cost", "wrong-kind", "wrong-baseline", "wrong-candidate", "omitted-plan", "invented-plan-digest", "omitted-job", "fabricated-recommendation"}, mode) {
				invalid := decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], *d)
				accepted := errors.New("invalid parent decision accepted")
				storeErr := f.s.Transact(f.ctx, func(tx store.Tx) error {
					if commitErr := tx.CommitTransition(invalid); commitErr != nil {
						return commitErr
					}
					return accepted
				})
				if errors.Is(storeErr, accepted) || storeErr == nil {
					t.Fatalf("transaction accepted %s: %v", mode, storeErr)
				}
			}
		})
	}
	change := decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision)
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
		t.Fatalf("store rejected controller evidence: %v", err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
		t.Fatalf("decision retry changed evidence: %v", err)
	}
	saved, err := f.s.GetDecision(f.ctx, decision.ID)
	if err != nil || saved.SelectionKind != string(optimization.HarnessImprovement) || !reflect.DeepEqual(saved.Comparison, decision.Comparison) {
		t.Fatalf("stored parent decision: %+v %v", saved, err)
	}
	for _, child := range evidence {
		for _, item := range child.Trials {
			stored, getErr := f.s.GetTrial(f.ctx, item.Trial.ID)
			if getErr != nil || stored.ExperimentID != child.Experiment.ID || stored.Version != item.Trial.Version {
				t.Fatalf("parent rewrote execution provenance: %+v %v", stored, getErr)
			}
		}
	}
	foreign := Fixture(t, "foreign-controller-decision")
	if _, err = f.s.GetDecision(scoped(foreign.Experiment.Spec.Scope), decision.ID); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("foreign parent decision: %v", err)
	}
}

type controllerDecisionOptions struct {
	qualified, improved, artifacts bool
	configure                      func(*optimization.ExperimentBundle, []optimization.ExperimentBundle)
	evaluate                       func(context.Context, optimization.EvaluationRequest) (optimization.Evaluation, error)
}

// ControllerComparisonFixture records a paired stream using the supplied frozen
// plans and evaluator, so adapter tests exercise the same transactional fixtures.
func ControllerComparisonFixture(t *testing.T, factory Factory, configure func(*optimization.ExperimentBundle, []optimization.ExperimentBundle), evaluate func(context.Context, optimization.EvaluationRequest) (optimization.Evaluation, error)) (optimization.Experiment, optimization.ValidationAllocation, []optimization.ControllerEvidence) {
	t.Helper()
	f, allocation, evidence, _ := controllerDecisionFixture(t, factory, controllerDecisionOptions{configure: configure, evaluate: evaluate})
	return f.parent.Experiment, allocation, evidence
}

func controllerDecisionFixture(t *testing.T, factory Factory, options ...controllerDecisionOptions) (validationFamily, optimization.ValidationAllocation, []optimization.ControllerEvidence, optimization.Decision) {
	t.Helper()
	var config controllerDecisionOptions
	if len(options) > 0 {
		config = options[0]
	}
	f := frozenValidationFamily(t, factory, 2, func(parent *optimization.ExperimentBundle, children []optimization.ExperimentBundle) {
		if config.configure != nil {
			config.configure(parent, children)
		}
		if config.qualified {
			parent.Experiment.Spec.Objective.MinValidationCases = 2
			parent.Experiment.Spec.Objective.RequireReproducibility = false
			for index := range children {
				children[index].Experiment.Spec.Objective = parent.Experiment.Spec.Objective
				parent.Experiment.Spec.Improvement.Children[index].Spec = children[index].Experiment.Spec
			}
		}
	})
	allocation := allocate(f.ctx, t, f.s, f.request)
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
		return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: f.children[0].Experiment.ID, Lease: f.leases[1]})
	}); err != nil {
		t.Fatal(err)
	}
	children := make([]optimization.ControllerEvidence, 0, len(f.children))
	comparison := optimization.Comparison{Confidence: .95, BootstrapSamples: 2000, MissingReasons: []string{"insufficient distinct validation cases"}}
	if config.improved {
		comparison.MeanDelta, comparison.LowerBound, comparison.UpperBound = .4, .4, .4
	}
	for index, child := range f.children {
		for taskIndex, task := range child.Experiment.Spec.Tasks {
			if task.Split != optimization.SplitValidation {
				continue
			}
			for repetition := range child.Experiment.Spec.Limits.Repetitions {
				trial := queuedTrial(child, taskIndex)
				trial.Repetition, trial.AllocationID = repetition, allocation.ID
				var refs []optimization.ArtifactRef
				if config.artifacts {
					refs = []optimization.ArtifactRef{artifactRegistration(child, f.leases[index+1], time.Now()).Ref}
				}
				value := captureFixtureTrial(f.ctx, t, f.s, child, f.leases[index+1], trial, refs...)
				score := .2
				if config.improved && index%2 == 1 {
					score = .6
				}
				value.Score = &score
				for criterion := range value.Criteria {
					value.Criteria[criterion].Score = &score
				}
				if config.evaluate != nil {
					captured, getErr := f.s.GetTrial(f.ctx, trial.ID)
					if getErr != nil {
						t.Fatal(getErr)
					}
					var evalErr error
					value, evalErr = config.evaluate(f.ctx, optimization.EvaluationRequest{ExperimentID: child.Experiment.ID, TrialID: trial.ID, CandidateID: trial.CandidateID, CaseID: trial.CaseID, CaseDigest: trial.CaseDigest, Repetition: repetition, Plan: child.Experiment.EvaluationPlan, Execution: *captured.Execution, Lease: f.leases[index+1], IdempotencyKey: "controller-fixture/" + trial.ID.String()})
					if evalErr != nil {
						t.Fatal(evalErr)
					}
				}
				persistFixtureEvaluation(f.ctx, t, f.s, child, f.leases[index+1], value)
			}
		}
		current, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		item := optimization.ControllerEvidence{Experiment: current}
		page, err := f.s.ListTrials(f.ctx, current.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 100}})
		if err != nil || page.NextCursor != "" {
			t.Fatalf("child trials: %v", err)
		}
		for _, trial := range page.Items {
			if trial.Split != optimization.SplitValidation {
				continue
			}
			value, getErr := f.s.GetEvaluation(f.ctx, trial.EvaluationID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			item.Trials = append(item.Trials, optimization.DecisionEvidence{Trial: trial, Evaluation: &value})
			if index%2 == 0 {
				comparison.BaselineIDs = append(comparison.BaselineIDs, value.ID)
			} else {
				comparison.CandidateIDs = append(comparison.CandidateIDs, value.ID)
			}
		}
		if index%2 == 0 {
			comparison.Plans = append(comparison.Plans, current.EvaluationPlan.Ref)
			for _, task := range current.Spec.Tasks {
				if task.Split == optimization.SplitValidation {
					comparison.Cases = append(comparison.Cases, optimization.CaseDelta{CaseID: task.ID, CaseDigest: task.Digest, Repetitions: current.Spec.Limits.Repetitions, Delta: comparison.MeanDelta, Criteria: map[string]float64{"quality": comparison.MeanDelta}})
				}
			}
		}
		children = append(children, item)
	}
	advanceFixture(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseDecide)
	current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.parent.Experiment = current
	allocation, err = f.s.GetValidationAllocation(f.ctx, allocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision := optimization.Decision{ID: id.New(optimization.PrefixDecision), ExperimentID: current.ID, SpecDigest: current.SpecDigest, AllocationID: allocation.ID, Baseline: current.Spec.Improvement.Controllers[0], Candidate: current.Spec.Improvement.Controllers[1], SelectionKind: string(optimization.HarnessImprovement), Outcome: optimization.DecisionInconclusive, EvidenceClass: optimization.EvidenceExploratory, PolicyDigest: f.parent.Versions[1].PolicyDigest, Comparison: comparison, Reasons: []string{"insufficient distinct validation cases"}}
	decision.EvaluationIDs = append(slices.Clone(comparison.BaselineIDs), comparison.CandidateIDs...)
	if config.qualified {
		decision.Comparison.Complete, decision.Comparison.MissingReasons = true, nil
		decision.Outcome, decision.EvidenceClass = optimization.DecisionRetain, optimization.EvidenceValidated
		decision.Reasons = []string{"The controller did not improve paired quality."}
		if config.improved {
			decision.Outcome = optimization.DecisionRecommend
			decision.Reasons = []string{"Paired controller validation passed the frozen thresholds."}
		}
	}
	return f, allocation, children, decision
}

// ControllerDecisionArtifacts checks child retention before decision and review.
func ControllerDecisionArtifacts(t *testing.T, factory Factory) {
	t.Helper()
	f, allocation, evidence, decision := controllerDecisionFixture(t, factory, controllerDecisionOptions{qualified: true, improved: true, artifacts: true})
	if err := optimization.ValidateControllerDecisionEvidence(f.parent.Experiment, allocation, decision, evidence); err != nil {
		t.Fatalf("qualified controller evidence: %v", err)
	}
	change := decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision)
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("missing child artifacts recommended: %v", err)
	}
	var registration optimization.ArtifactRegistration
	for index, child := range f.children {
		registration = artifactRegistration(child, f.leases[index+1], time.Now())
		registerArtifact(f.ctx, t, f.s, registration)
	}
	commitTrialChange(f.ctx, t, f.s, change)
	current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	finish := optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, Lease: f.leases[0], Actor: current.Spec.Requester, IdempotencyKey: "parent-review", NextState: optimization.StateReviewRequired}
	preview := errors.New("review preview")
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		if finishErr := tx.CommitTransition(finish); finishErr != nil {
			return finishErr
		}
		return preview
	}); !errors.Is(err, preview) {
		t.Fatalf("retained child evidence could not enter review: %v", err)
	}
	deletion := optimization.ArtifactDeletion{Ref: registration.Ref, RequestID: "withdraw-child-proof", Actor: registration.Actor, Reason: "source removed"}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.BeginArtifactDeletion(deletion) }); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(finish) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("withdrawn child proof entered review: %v", err)
	}
}

// ControllerDecisionRecovery keeps the exact comparison through parent takeover.
func ControllerDecisionRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	f, allocation, evidence, decision := controllerDecisionFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) })
	want, err := optimization.BuildControllerComparison(f.parent.Experiment, allocation, evidence)
	if err != nil {
		t.Fatal(err)
	}
	stale := decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision)
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	now = f.leases[0].ExpiresAt.Add(time.Second)
	f.s = open(t, func() time.Time { return now })
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		var claimErr error
		f.leases[0], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: f.parent.Experiment.ID, Owner: "decision-recovery", Duration: time.Minute})
		return claimErr
	}); err != nil {
		t.Fatal(err)
	}
	f.parent.Experiment, err = f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err = f.s.GetValidationAllocation(f.ctx, allocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err = optimization.ReadControllerEvidence(f.ctx, f.s, f.parent.Experiment, allocation)
	if err != nil {
		t.Fatal(err)
	}
	got, err := optimization.BuildControllerComparison(f.parent.Experiment, allocation, evidence)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("takeover changed comparison roster: %+v %v", got, err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(stale) }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("stale parent committed decision: %v", err)
	}
	change := decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision)
	commitTrialChange(f.ctx, t, f.s, change)
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = open(t, func() time.Time { return now })
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
		t.Fatalf("committed decision replay failed: %v", err)
	}
	saved, err := f.s.GetDecision(f.ctx, decision.ID)
	if err != nil || !reflect.DeepEqual(saved.Comparison, decision.Comparison) || !slices.Equal(saved.EvaluationIDs, decision.EvaluationIDs) {
		t.Fatalf("decision changed after reopen: %+v %v", saved, err)
	}
}
