package storetest

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
)

func evaluations(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("EvaluationPlanMustMatchFrozenExperiment", func(t *testing.T) {
		for _, mode := range []string{"missing", "reference", "case", "scorer", "criteria", "required"} {
			t.Run(mode, func(t *testing.T) {
				s := factory(t, time.Now)
				bundle := Fixture(t, "plan-"+mode)
				plan := &bundle.Experiment.EvaluationPlan
				switch mode {
				case "missing":
					*plan = optimization.EvaluationPlan{}
				case "reference":
					plan.Ref.Revision = "changed"
				case "case":
					plan.CaseDigests["case-0"] = strings.Repeat("f", 64)
				case "scorer":
					plan.Scorers[0].Revision = ""
				case "criteria":
					plan.Criteria = nil
				case "required":
					bundle.Experiment.Spec.Objective.RequiredCriteria = []string{"missing"}
				}
				if err := s.Transact(scoped(bundle.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrInvalidSpec) {
					t.Fatalf("unfrozen evaluation admitted: %v", err)
				}
			})
		}
	})
	t.Run("EvaluationBindsCapturedTrialAndPlan", func(t *testing.T) {
		scenario, evaluation := evaluatedTrial(t, factory)
		change := evaluationChange(t, scenario, evaluation)
		commitTrialChange(scenario.ctx, t, scenario.store, change)
		commitTrialChange(scenario.ctx, t, scenario.store, change)
		got, err := scenario.store.GetEvaluation(scenario.ctx, evaluation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.TrialID != scenario.trial.ID || got.ReportID != "report-1" || len(got.Criteria) != 1 || got.Criteria[0].Score == nil || *got.Criteria[0].Score != 0 || !got.Complete || got.RecordedAt.IsZero() {
			t.Fatalf("lost captured score: %+v", got)
		}
		trial, err := scenario.store.GetTrial(scenario.ctx, scenario.trial.ID)
		if err != nil || trial.EvaluationID != evaluation.ID || trial.Revision != 5 {
			t.Fatalf("trial not linked atomically: %+v %v", trial, err)
		}
		events, eventErr := scenario.store.ListEvents(scenario.ctx, scenario.bundle.Experiment.ID, 0, 100)
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		var payload struct {
			EvaluationIDs []string `json:"evaluation_ids"`
		}
		if eventErr = json.Unmarshal(events.Items[len(events.Items)-1].Payload, &payload); eventErr != nil {
			t.Fatal(eventErr)
		}
		if len(payload.EvaluationIDs) != 1 || payload.EvaluationIDs[0] != evaluation.ID {
			t.Fatal("audit event lost evaluation job identity")
		}
		*got.Criteria[0].Score = 1
		again, err := scenario.store.GetEvaluation(scenario.ctx, evaluation.ID)
		if err != nil || *again.Criteria[0].Score != 0 {
			t.Fatal("evaluation read aliases storage")
		}
		if _, err = scenario.store.GetEvaluation(scoped(Fixture(t, "foreign").Experiment.Spec.Scope), evaluation.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("evaluation leaked scope: %v", err)
		}
		changed := evaluation
		changed.ID = "replacement-job"
		replacement := evaluationChange(t, scenario, changed)
		if err = scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(replacement) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("replaced immutable evaluation: %v", err)
		}
	})
	t.Run("EvaluationRejectsMismatchedOrFabricatedEvidence", func(t *testing.T) {
		scenario, original := evaluatedTrial(t, factory)
		for _, mode := range []string{"experiment", "trial", "case", "case-digest", "repetition", "plan", "execution", "scorers", "missing-criterion", "required", "duplicate-criterion", "score-error", "missing-score", "nan", "range", "clock", "scorer-error"} {
			t.Run(mode, func(t *testing.T) {
				value := original
				value.Criteria = append([]optimization.CriterionValue(nil), original.Criteria...)
				value.Scorers = append([]run.Revision(nil), original.Scorers...)
				switch mode {
				case "experiment":
					value.ExperimentID = id.New(optimization.PrefixExperiment)
				case "trial":
					value.TrialID = id.New(optimization.PrefixTrial)
				case "case":
					value.CaseID = "wrong"
				case "case-digest":
					value.CaseDigest = strings.Repeat("e", 64)
				case "repetition":
					value.Repetition++
				case "plan":
					value.PlanDigest = strings.Repeat("e", 64)
				case "execution":
					value.ExecutionDigest = strings.Repeat("e", 64)
				case "scorers":
					value.Scorers[0].Revision = "2"
				case "missing-criterion":
					value.Criteria = nil
				case "required":
					value.Criteria[0].Required = true
				case "duplicate-criterion":
					value.Criteria = append(value.Criteria, value.Criteria[0])
				case "score-error":
					value.Criteria[0].Error = "scorer unavailable"
				case "missing-score":
					value.Criteria[0].Score = nil
				case "nan":
					n := math.NaN()
					value.Criteria[0].Score = &n
				case "range":
					n := 1.1
					value.Criteria[0].Score = &n
				case "clock":
					value.CompletedAt = value.StartedAt.Add(-time.Second)
				case "scorer-error":
					value.Errors = []string{"judge failed"}
				}
				change := evaluationChange(t, scenario, value)
				if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err == nil {
					t.Fatalf("accepted %s", mode)
				}
				trial, err := scenario.store.GetTrial(scenario.ctx, scenario.trial.ID)
				if err != nil || trial.EvaluationID != "" || trial.Revision != 4 {
					t.Fatal("rejected evaluation changed trial")
				}
			})
		}
	})
	t.Run("IncompleteEvaluationRetainsErrorsWithoutZeroScore", func(t *testing.T) {
		scenario, value := evaluatedTrial(t, factory)
		value.Complete = false
		value.Criteria[0].Score = nil
		value.Criteria[0].Passed = false
		value.Criteria[0].Error = "judge unavailable"
		value.Errors = []string{"judge unavailable"}
		change := evaluationChange(t, scenario, value)
		commitTrialChange(scenario.ctx, t, scenario.store, change)
		got, err := scenario.store.GetEvaluation(scenario.ctx, value.ID)
		if err != nil || got.Complete || got.Criteria[0].Score != nil || len(got.Errors) != 1 {
			t.Fatalf("incomplete evidence became zero: %+v %v", got, err)
		}
	})
	t.Run("EvaluationRollbackAndLeaseFence", func(t *testing.T) {
		scenario, value := evaluatedTrial(t, factory)
		change := evaluationChange(t, scenario, value)
		change.Lease.Generation++
		if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale evaluator persisted: %v", err)
		}
		change.Lease = scenario.lease
		abort := errors.New("abort evaluation")
		if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := scenario.store.GetEvaluation(scenario.ctx, value.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("rolled back evaluation remains: %v", err)
		}
		trial, err := scenario.store.GetTrial(scenario.ctx, scenario.trial.ID)
		if err != nil || trial.EvaluationID != "" {
			t.Fatal("rollback left trial link")
		}
	})
}

func evaluatedTrial(t *testing.T, factory Factory) (trialScenario, optimization.Evaluation) {
	t.Helper()
	scenario := runningTrial(t, factory)
	record := capturedExecution(t, scenario.bundle, scenario.receipt)
	update := scenario.update(t, optimization.TrialCompleted)
	update.Execution = &record
	commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "capture", update))
	return scenario, evaluationFor(scenario, record)
}

func evaluationFor(scenario trialScenario, record optimization.ExecutionRecord) optimization.Evaluation {
	zero := 0.0
	plan := scenario.bundle.Experiment.EvaluationPlan
	return optimization.Evaluation{ID: "job-1", ReportID: "report-1", ExperimentID: scenario.bundle.Experiment.ID, TrialID: scenario.trial.ID, CaseID: scenario.trial.CaseID, CaseDigest: scenario.trial.CaseDigest, Repetition: scenario.trial.Repetition, PlanDigest: plan.Ref.Digest, ExecutionDigest: record.Digest, Scorers: plan.Scorers, Criteria: []optimization.CriterionValue{{ID: "quality", Score: &zero}}, Complete: true, JudgeUsage: optimization.Usage{Tokens: 3, TokenQuality: optimization.UsageKnown}, StartedAt: record.CompletedAt, CompletedAt: record.CompletedAt.Add(time.Millisecond)}
}

func evaluationChange(t *testing.T, s trialScenario, value optimization.Evaluation) optimization.Transition {
	t.Helper()
	change := trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, "evaluation/"+value.ID, nil, nil)
	change.Evaluations = []optimization.Evaluation{value}
	return change
}
