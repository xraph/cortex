package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func decisions(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("DraftCannotInjectDecision", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "injected-decision")
		bundle.Experiment.DecisionID = id.New(optimization.PrefixDecision)
		if err := s.Transact(scoped(bundle.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("draft injected decision: %v", err)
		}
	})
	t.Run("DecisionRecommendationPreservesQualifiedComparison", func(t *testing.T) {
		s, ctx, bundle, lease, value := decisionFixture(t, factory, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
		})
		value.Outcome = optimization.DecisionRecommend
		value.EvidenceClass = optimization.EvidenceValidated
		value.Comparison.Complete = true
		value.Comparison.MissingReasons = nil
		value.Reasons = []string{"fixture satisfies the configured comparison thresholds"}
		for _, mode := range []string{"no-confidence", "under-improvement", "unscored-cost", "invented-failure", "exploratory", "missing-reason", "validated-inconclusive"} {
			t.Run(mode, func(t *testing.T) {
				changed := value
				switch mode {
				case "no-confidence":
					changed.Comparison.LowerBound = 0
				case "under-improvement":
					changed.Comparison.MeanDelta = .001
				case "unscored-cost":
					ratio := 1.0
					changed.Comparison.CostRatio = &ratio
				case "invented-failure":
					changed.Comparison.RequiredFailures = []string{"invented"}
				case "exploratory":
					changed.EvidenceClass = optimization.EvidenceExploratory
				case "missing-reason":
					changed.Reasons = nil
				case "validated-inconclusive":
					changed.Outcome = optimization.DecisionInconclusive
				}
				accepted := errors.New("invalid decision accepted")
				change := decisionChange(ctx, t, s, bundle, lease, changed)
				err := s.Transact(ctx, func(tx store.Tx) error {
					if err := tx.CommitTransition(change); err != nil {
						return err
					}
					return accepted
				})
				if errors.Is(err, accepted) {
					t.Fatalf("accepted %s", mode)
				}
			})
		}
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, value))
		got, err := s.GetDecision(ctx, value.ID)
		if err != nil || got.Outcome != optimization.DecisionRecommend || got.Comparison.MeanDelta != .4 {
			t.Fatalf("valid recommendation lost: %+v %v", got, err)
		}
	})
	t.Run("RecommendationRequiresReproducibleReceipts", func(t *testing.T) {
		s, ctx, bundle, lease, value := decisionFixture(t, factory, func(bundle *optimization.ExperimentBundle) { bundle.Experiment.Spec.Objective.MinValidationCases = 1 })
		value.Outcome = optimization.DecisionRecommend
		value.EvidenceClass = optimization.EvidenceValidated
		value.Comparison.Complete = true
		value.Comparison.MissingReasons = nil
		change := decisionChange(ctx, t, s, bundle, lease, value)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("unqualified source capture recommended: %v", err)
		}
	})
	t.Run("DecisionRetainsScoringFailureAsInconclusive", func(t *testing.T) {
		s, ctx, bundle, lease, value := decisionFixtureWithEvaluation(t, factory, func(arm int, value *optimization.Evaluation) {
			if arm == 1 && value.Repetition == 0 {
				value.Complete = false
				value.Score = nil
				value.Errors = []string{"judge unavailable"}
				value.Criteria[0].Score = nil
				value.Criteria[0].Error = "judge unavailable"
				value.Criteria[0].Passed = false
			}
		})
		value.Comparison.Cases = nil
		value.Comparison.MeanDelta = 0
		value.Comparison.LowerBound = 0
		value.Comparison.UpperBound = 0
		value.Comparison.MissingReasons = []string{"judge unavailable"}
		value.Reasons = []string{"judge unavailable"}
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, value))
		got, err := s.GetDecision(ctx, value.ID)
		if err != nil || got.Outcome != optimization.DecisionInconclusive || got.Comparison.Complete || len(got.EvaluationIDs) != 6 {
			t.Fatalf("scorer failure lost: %+v %v", got, err)
		}
	})
	t.Run("DecisionCanRetainBaselineWithoutInventingCandidate", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "retain-baseline")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		allocation := allocate(ctx, t, s, allocationRequest(bundle, lease))
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: bundle.Experiment.ID, Lease: lease})
		}); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for index, task := range bundle.Experiment.Spec.Tasks {
			if task.Split != optimization.SplitValidation {
				continue
			}
			for rep := range bundle.Experiment.Spec.Limits.Repetitions {
				trial := queuedTrial(bundle, index)
				trial.Repetition = rep
				trial.AllocationID = allocation.ID
				value := captureFixtureTrial(ctx, t, s, bundle, lease, trial)
				persistFixtureEvaluation(ctx, t, s, bundle, lease, value)
				ids = append(ids, value.ID)
			}
		}
		advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseDecide)
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		value := optimization.Decision{ID: id.New(optimization.PrefixDecision), ExperimentID: current.ID, SpecDigest: current.SpecDigest, AllocationID: allocation.ID, Baseline: current.Spec.Baseline, Candidate: current.Spec.Baseline, SelectionKind: string(optimization.HarnessExecution), Outcome: optimization.DecisionRetain, EvidenceClass: optimization.EvidenceExploratory, PolicyDigest: bundle.Versions[0].PolicyDigest, EvaluationIDs: ids, Reasons: []string{"no accepted candidate"}, Comparison: optimization.Comparison{BaselineIDs: ids, PlanDigest: current.EvaluationPlan.Ref.Digest, Confidence: current.Spec.Objective.Confidence, BootstrapSeed: current.Spec.Objective.BootstrapSeed, BootstrapSamples: current.Spec.Objective.BootstrapSamples, MissingReasons: []string{"no accepted candidate"}}}
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, value))
		got, err := s.GetDecision(ctx, value.ID)
		if err != nil || got.Candidate != got.Baseline || got.Comparison.Complete || len(got.Comparison.CandidateIDs) != 0 {
			t.Fatalf("invented candidate evidence: %+v %v", got, err)
		}
	})
	t.Run("DecisionCannotHideHardCriteriaBehindAggregate", func(t *testing.T) {
		for _, mode := range []string{"required", "protected"} {
			t.Run(mode, func(t *testing.T) {
				configure := func(bundle *optimization.ExperimentBundle) {
					bundle.Experiment.Spec.Objective.MinValidationCases = 1
					bundle.Experiment.Spec.Objective.RequireReproducibility = false
					if mode == "required" {
						bundle.Experiment.Spec.Objective.RequiredCriteria = []string{"quality"}
						bundle.Experiment.EvaluationPlan.Criteria[0].Required = true
					} else {
						bundle.Experiment.Spec.Objective.ProtectedCriteria = []string{"quality"}
					}
				}
				evaluate := func(arm int, value *optimization.Evaluation) {
					if mode == "required" {
						value.Criteria[0].Required = true
						value.Criteria[0].Passed = arm == 0
					} else {
						score := .8 - float64(arm)*.2
						value.Criteria[0].Score = &score
					}
				}
				s, ctx, bundle, lease, value := decisionFixtureWithEvaluation(t, factory, evaluate, configure)
				value.Outcome = optimization.DecisionRecommend
				value.EvidenceClass = optimization.EvidenceValidated
				value.Comparison.Complete = true
				value.Comparison.MissingReasons = nil
				if mode == "required" {
					for _, task := range bundle.Experiment.Spec.Tasks {
						if task.Split == optimization.SplitValidation {
							for rep := range bundle.Experiment.Spec.Limits.Repetitions {
								value.Comparison.RequiredFailures = append(value.Comparison.RequiredFailures, optimization.CriterionFailureKey(task.ID, rep, "quality"))
							}
						}
					}
				} else {
					for i := range value.Comparison.Cases {
						value.Comparison.Cases[i].Criteria = map[string]float64{"quality": -.2}
					}
					value.Comparison.ProtectedRegressions = map[string]float64{"quality": .2}
				}
				change := decisionChange(ctx, t, s, bundle, lease, value)
				if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
					t.Fatalf("high aggregate hid %s failure: %v", mode, err)
				}
				value.Outcome = optimization.DecisionReject
				value.Reasons = []string{"hard criterion failed"}
				commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, value))
			})
		}
	})
	t.Run("DecisionRequiredBeforeReflection", func(t *testing.T) {
		s, ctx, bundle, lease, _ := decisionFixture(t, factory)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseReflect)
	})
	t.Run("DecisionCommitsWithPhaseAndAudit", func(t *testing.T) {
		s, ctx, bundle, lease, value := decisionFixture(t, factory)
		change := decisionChange(ctx, t, s, bundle, lease, value)
		commitTrialChange(ctx, t, s, change)
		commitTrialChange(ctx, t, s, change)
		got, err := s.GetDecision(ctx, value.ID)
		if err != nil || got.ID != value.ID || got.AllocationID != value.AllocationID || got.CreatedAt.IsZero() || got.SpecDigest != value.SpecDigest || len(got.EvaluationIDs) != 6 {
			t.Fatalf("lost decision: %+v %v", got, err)
		}
		experiment, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || experiment.Phase != optimization.PhaseReflect || experiment.DecisionID != value.ID {
			t.Fatalf("decision not linked atomically: %+v %v", experiment, err)
		}
		events, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 100)
		if err != nil || !slices.Contains(events.Items[len(events.Items)-1].Resources, value.ID) {
			t.Fatal("decision omitted from audit")
		}
		got.Reasons[0] = "changed"
		got.Comparison.BaselineIDs[0] = "changed"
		again, err := s.GetDecision(ctx, value.ID)
		if err != nil || again.Reasons[0] == "changed" || again.Comparison.BaselineIDs[0] == "changed" {
			t.Fatal("decision read aliases storage")
		}
		if _, err = s.GetDecision(scoped(Fixture(t, "foreign-decision").Experiment.Spec.Scope), value.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("decision leaked scope: %v", err)
		}
	})
	t.Run("DecisionRollbackAndFences", func(t *testing.T) {
		s, ctx, bundle, lease, value := decisionFixture(t, factory)
		change := decisionChange(ctx, t, s, bundle, lease, value)
		change.Lease.Generation++
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale decision committed: %v", err)
		}
		change.Lease = lease
		abort := errors.New("abort decision")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetDecision(ctx, value.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("rolled back decision remains: %v", err)
		}
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || current.Phase != optimization.PhaseDecide || !current.DecisionID.IsNil() {
			t.Fatal("rollback advanced experiment")
		}
		commitTrialChange(ctx, t, s, change)
		replacement := value
		replacement.ID = id.New(optimization.PrefixDecision)
		changed := decisionChange(ctx, t, s, bundle, lease, replacement)
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(changed) }); err == nil {
			t.Fatal("replaced immutable decision")
		}
	})
	t.Run("DecisionRejectsSubstitutedEvidence", func(t *testing.T) {
		s, ctx, bundle, lease, original := decisionFixture(t, factory)
		for _, mode := range []string{"experiment", "spec", "policy", "allocation", "baseline", "candidate", "plan", "kind", "outcome", "duplicate-id", "missing-id", "foreign-job", "swapped-arms", "case", "delta", "mean", "repetition", "seed", "confidence", "fake-recommendation"} {
			t.Run(mode, func(t *testing.T) {
				value := original
				value.EvaluationIDs = slices.Clone(original.EvaluationIDs)
				value.Comparison.BaselineIDs = slices.Clone(original.Comparison.BaselineIDs)
				value.Comparison.CandidateIDs = slices.Clone(original.Comparison.CandidateIDs)
				value.Comparison.Cases = slices.Clone(original.Comparison.Cases)
				switch mode {
				case "experiment":
					value.ExperimentID = id.New(optimization.PrefixExperiment)
				case "spec":
					value.SpecDigest = strings.Repeat("e", 64)
				case "policy":
					value.PolicyDigest = strings.Repeat("e", 64)
				case "allocation":
					value.AllocationID = id.New(optimization.PrefixValidation)
				case "baseline":
					value.Baseline = value.Candidate
				case "candidate":
					value.Candidate = bundle.Experiment.Spec.Controller
				case "plan":
					value.Comparison.PlanDigest = strings.Repeat("e", 64)
				case "kind":
					value.SelectionKind = "unknown"
				case "outcome":
					value.Outcome = "unknown"
				case "duplicate-id":
					value.EvaluationIDs = append(value.EvaluationIDs, value.EvaluationIDs[0])
				case "missing-id":
					value.EvaluationIDs = value.EvaluationIDs[1:]
				case "foreign-job":
					value.EvaluationIDs[0] = "foreign-job"
				case "swapped-arms":
					value.Comparison.BaselineIDs, value.Comparison.CandidateIDs = value.Comparison.CandidateIDs, value.Comparison.BaselineIDs
				case "case":
					value.Comparison.Cases[0].CaseDigest = strings.Repeat("e", 64)
				case "delta":
					value.Comparison.Cases[0].Delta = .9
				case "mean":
					value.Comparison.MeanDelta = .9
				case "repetition":
					value.Comparison.Cases[0].Repetitions++
				case "seed":
					value.Comparison.BootstrapSeed++
				case "confidence":
					value.Comparison.Confidence = .5
				case "fake-recommendation":
					value.Outcome = optimization.DecisionRecommend
					value.EvidenceClass = optimization.EvidenceValidated
					value.Comparison.Complete = true
					value.Comparison.MissingReasons = nil
				}
				change := decisionChange(ctx, t, s, bundle, lease, value)
				accepted := errors.New("invalid decision accepted")
				err := s.Transact(ctx, func(tx store.Tx) error {
					if err := tx.CommitTransition(change); err != nil {
						return err
					}
					return accepted
				})
				if errors.Is(err, accepted) {
					t.Fatalf("accepted %s", mode)
				}
				current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
				if err != nil || current.Phase != optimization.PhaseDecide || !current.DecisionID.IsNil() {
					t.Fatal("invalid decision advanced experiment")
				}
			})
		}
	})
}

func decisionFixture(t *testing.T, factory Factory, configure ...func(*optimization.ExperimentBundle)) (store.Store, context.Context, optimization.ExperimentBundle, optimization.WorkLease, optimization.Decision) {
	t.Helper()
	return decisionFixtureWithEvaluation(t, factory, nil, configure...)
}

func decisionFixtureWithEvaluation(t *testing.T, factory Factory, evaluate func(int, *optimization.Evaluation), configure ...func(*optimization.ExperimentBundle)) (store.Store, context.Context, optimization.ExperimentBundle, optimization.WorkLease, optimization.Decision) {
	t.Helper()
	return decisionFixtureWithArtifacts(t, factory, evaluate, nil, configure...)
}

func decisionFixtureWithArtifacts(t *testing.T, factory Factory, evaluate func(int, *optimization.Evaluation), artifacts func(optimization.ExperimentBundle) []optimization.ArtifactRef, configure ...func(*optimization.ExperimentBundle)) (store.Store, context.Context, optimization.ExperimentBundle, optimization.WorkLease, optimization.Decision) {
	t.Helper()
	s := factory(t, time.Now)
	bundle := candidateBundle(t)
	for _, apply := range configure {
		apply(&bundle)
	}
	ctx := scoped(bundle.Experiment.Spec.Scope)
	lease := proposalExperiment(ctx, t, s, bundle)
	candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
	commitTrialChange(ctx, t, s, candidateTransition(ctx, t, s, bundle, lease, candidate, version))
	fillVersionTrials(ctx, t, s, bundle, lease, candidate.Version, candidate.ID, optimization.SplitDiscovery, id.ID{}, nil)
	advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseValidate)
	request := allocationRequest(bundle, lease)
	request.PairedRoster[0].Finalist = candidate.Version
	allocation := allocate(ctx, t, s, request)
	if err := s.Transact(ctx, func(tx store.Tx) error {
		return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: bundle.Experiment.ID, Lease: lease})
	}); err != nil {
		t.Fatal(err)
	}
	comparison := optimization.Comparison{PlanDigest: bundle.Experiment.EvaluationPlan.Ref.Digest, MeanDelta: .4, LowerBound: .4, UpperBound: .4, Confidence: .95, BootstrapSamples: 2000, MissingReasons: []string{"insufficient distinct validation cases"}}
	for arm, ref := range []optimization.VersionRef{bundle.Experiment.Spec.Baseline, candidate.Version} {
		for i, task := range bundle.Experiment.Spec.Tasks {
			if task.Split != optimization.SplitValidation {
				continue
			}
			for rep := range bundle.Experiment.Spec.Limits.Repetitions {
				trial := queuedTrial(bundle, i)
				trial.Version = ref
				trial.Repetition = rep
				trial.AllocationID = allocation.ID
				if arm == 1 {
					trial.CandidateID = candidate.ID
				}
				var refs []optimization.ArtifactRef
				if artifacts != nil {
					refs = artifacts(bundle)
				}
				value := captureFixtureTrial(ctx, t, s, bundle, lease, trial, refs...)
				score := .2 + float64(arm)*.4
				value.Score = &score
				if evaluate != nil {
					evaluate(arm, &value)
				}
				persistFixtureEvaluation(ctx, t, s, bundle, lease, value)
				if arm == 0 {
					comparison.BaselineIDs = append(comparison.BaselineIDs, value.ID)
				} else {
					comparison.CandidateIDs = append(comparison.CandidateIDs, value.ID)
				}
			}
			if arm == 0 {
				comparison.Cases = append(comparison.Cases, optimization.CaseDelta{CaseID: task.ID, CaseDigest: task.Digest, Repetitions: bundle.Experiment.Spec.Limits.Repetitions, Delta: .4})
			}
		}
	}
	advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseDecide)
	current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	value := optimization.Decision{ID: id.New(optimization.PrefixDecision), ExperimentID: current.ID, AllocationID: allocation.ID, SpecDigest: current.SpecDigest, Baseline: bundle.Experiment.Spec.Baseline, Candidate: candidate.Version, SelectionKind: string(optimization.HarnessExecution), Outcome: optimization.DecisionInconclusive, EvidenceClass: optimization.EvidenceExploratory, PolicyDigest: version.PolicyDigest, Comparison: comparison, Reasons: []string{"insufficient distinct validation cases"}}
	value.EvaluationIDs = append(slices.Clone(comparison.BaselineIDs), comparison.CandidateIDs...)
	return s, ctx, bundle, lease, value
}

func decisionChange(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, value optimization.Decision) optimization.Transition {
	t.Helper()
	change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "decision/"+value.ID.String(), nil, nil)
	change.NextPhase = optimization.PhaseReflect
	change.Decision = &value
	return change
}
