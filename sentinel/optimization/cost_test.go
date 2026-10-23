package optimization

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/recorded"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
)

type costRepository struct {
	opt.Repository
	experiment opt.Experiment
	trial      opt.Trial
	evaluation opt.Evaluation
}

func (r *costRepository) GetExperiment(context.Context, id.ID) (opt.Experiment, error) {
	return r.experiment, nil
}
func (r *costRepository) GetTrial(context.Context, id.ID) (opt.Trial, error) { return r.trial, nil }
func (r *costRepository) GetEvaluation(context.Context, string) (opt.Evaluation, error) {
	return r.evaluation, nil
}

func TestCostProvenanceRequiresOriginalCaptureAndFrozenPricing(t *testing.T) {
	for _, scenario := range []string{"known", "unknown", "estimated", "wrong-pricing", "token-only", "changed-capture", "wrong-job", "wrong-plan", "wrong-case"} {
		t.Run(scenario, func(t *testing.T) {
			scope := cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "cost"}}}
			ctx := cortex.WithScope(context.Background(), scope)
			now := time.Now().UTC()
			digest := strings.Repeat("a", 64)
			repo := &costRepository{experiment: opt.Experiment{ID: id.New(opt.PrefixExperiment), Spec: opt.ExperimentSpec{Budget: opt.Budget{PricingRevision: "prices-1"}}, EvaluationPlan: opt.EvaluationPlan{Ref: opt.ContentRef{Digest: digest}}}}
			usage := opt.Usage{USDMicros: 100, MoneyQuality: opt.UsageKnown, PricingRevision: "prices-1", Receipt: "provider-receipt"}
			switch scenario {
			case "unknown":
				usage.MoneyQuality = opt.UsageUnknown
			case "estimated":
				usage.MoneyQuality = opt.UsageEstimated
			case "wrong-pricing":
				usage.PricingRevision = "prices-2"
			case "token-only":
				repo.experiment.Spec.Budget.TokenOnly = true
			}
			execution, err := opt.SealExecution(opt.ExecutionRecord{RunID: id.NewAgentRunID(), PreparedDigest: digest, InputDigest: digest, State: run.StateCompleted, Outcome: run.Outcome{Output: "answer", StopReason: run.StopFinalAnswer, CompletedAt: now}, StartedAt: now.Add(-time.Second), CompletedAt: now, Duration: time.Second, Environment: opt.EnvironmentReceipt{ID: "receipt", Provider: "fixture", EnvironmentID: "env", Scope: scope, AttemptID: id.New(opt.PrefixAttempt), Snapshot: opt.ContentRef{Ref: "snapshot", Revision: "1", Digest: digest}, Fence: opt.Fence{Owner: "worker", Generation: 1}}, Usage: usage})
			if err != nil {
				t.Fatal(err)
			}
			capture, err := ConvertExecution(execution)
			if err != nil {
				t.Fatal(err)
			}
			capture.CaseID = sid.NewCaseID()
			capture.InputDigest = recorded.InputDigest("question")
			job := &recorded.Job{ID: sid.New("ejob"), RunID: sid.NewEvalRunID(), Request: recorded.Request{SourceExperimentID: repo.experiment.ID.String(), PlanDigest: digest, Cases: []recorded.Case{capture}}}
			repo.trial = opt.Trial{ID: id.New(opt.PrefixTrial), ExperimentID: repo.experiment.ID, Execution: &execution, EvaluationID: job.ID.String(), CaseID: "task", CaseDigest: digest}
			repo.evaluation = opt.Evaluation{ID: job.ID.String(), ReportID: job.RunID.String(), TrialID: repo.trial.ID, ExperimentID: repo.experiment.ID, PlanDigest: digest, ExecutionDigest: execution.Digest, CaseID: "task", CaseDigest: digest}
			binding := Binding{Scope: scope, Plan: repo.experiment.EvaluationPlan.Ref, Cases: []CaseBinding{{CaseID: capture.CaseID, Task: opt.TaskCase{ID: "task", Digest: digest, InputDigest: digest, Input: "question"}}}}
			switch scenario {
			case "wrong-case":
				binding.Cases[0].CaseID = sid.NewCaseID()
			case "changed-capture":
				job.Request.Cases[0].Usage.Cost = 0
			case "wrong-job":
				repo.evaluation.ReportID = "other"
			case "wrong-plan":
				repo.experiment.EvaluationPlan.Ref.Digest = strings.Repeat("b", 64)
			}
			value, known, err := (&Evaluator{config: Config{Repository: repo, Bindings: []Binding{binding}}}).executionCost(ctx, job)
			invalid := scenario == "changed-capture" || scenario == "wrong-job" || scenario == "wrong-plan" || scenario == "wrong-case"
			if invalid {
				if err == nil {
					t.Fatal("accepted conflicting provenance")
				}
				return
			}
			if err != nil || known != (scenario == "known") || value != 100 {
				t.Fatalf("cost=%d known=%t err=%v", value, known, err)
			}
		})
	}
	if _, err := addCost(math.MaxInt64, 1); err == nil {
		t.Fatal("cost overflow")
	}
}
