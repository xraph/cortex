package optimization_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	"github.com/xraph/sentinel/evalplan"
	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/store/memory"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/target"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	cid "github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type mapper func(context.Context, cortex.Scope) (context.Context, error)

func (f mapper) Map(ctx context.Context, s cortex.Scope) (context.Context, error) { return f(ctx, s) }
func TestEvaluatorPreservesCaptureAndDeduplicates(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "test"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
	s := memory.New()
	su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "recorded"}
	if err := s.CreateSuite(mapped, su); err != nil {
		t.Fatal(err)
	}
	tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Name: "case", Input: "question", Expected: "answer", Scorers: []testcase.ScorerConfig{{Name: "exact"}}}
	if err := s.CreateCase(mapped, tc); err != nil {
		t.Fatal(err)
	}
	calls := 0
	eng, err := se.New(se.WithStore(s), se.WithTarget("must-not-run", "", target.FromFunc("must-not-run", func(context.Context, string) (string, error) { calls++; return "", nil })))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := eng.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID})
	if err != nil {
		t.Fatal(err)
	}
	task, err := opt.SealTaskCase(opt.TaskCase{ID: "public-case", Input: "question", Split: opt.SplitDiscovery, EnvironmentSnapshot: opt.ContentRef{Ref: "snapshot", Revision: "1", Digest: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	ref := opt.ContentRef{Ref: frozen.ID.String(), Revision: "1", Digest: frozen.Digest}
	binding := adapter.Binding{Scope: scope, AppID: "app", TenantID: "tenant", Plan: ref, Cases: []adapter.CaseBinding{{Task: task, CaseID: tc.ID}}}
	a, err := adapter.NewEvaluator(adapter.Config{Engine: eng, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
		return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
	}), Bindings: []adapter.Binding{binding}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := a.Freeze(ctx, opt.EvaluationSpec{Source: ref, Tasks: []opt.TaskCase{task}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Criteria) != 1 || plan.Criteria[0].RequiresExpected == nil || !*plan.Criteria[0].RequiresExpected {
		t.Fatal("frozen exact scorer lost its expected-answer requirement")
	}
	now := time.Now().UTC()
	execution, err := opt.SealExecution(opt.ExecutionRecord{RunID: cid.NewAgentRunID(), PreparedDigest: strings.Repeat("a", 64), InputDigest: task.InputDigest, State: run.StateCompleted, Outcome: run.Outcome{Output: "answer", StopReason: run.StopFinalAnswer, CompletedAt: now.Add(17 * time.Second)}, StartedAt: now, CompletedAt: now.Add(17 * time.Second), Duration: 17 * time.Second, Environment: opt.EnvironmentReceipt{ID: "receipt", Provider: "test", EnvironmentID: "environment", Scope: scope, AttemptID: cid.New(opt.PrefixAttempt), Snapshot: task.EnvironmentSnapshot, Fence: opt.Fence{Owner: "worker", Generation: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	req := opt.EvaluationRequest{ExperimentID: cid.New(opt.PrefixExperiment), TrialID: cid.New(opt.PrefixTrial), CaseID: task.ID, CaseDigest: task.Digest, Plan: plan, Execution: execution, IdempotencyKey: "capture"}
	first, err := a.Evaluate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Evaluate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ReportID != second.ReportID || !first.Complete || first.Score == nil || *first.Score != 1 || calls != 0 {
		t.Fatalf("capture scoring/retry: %+v %+v calls=%d", first, second, calls)
	}
	jobID, err := sid.Parse(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.GetRecordedJob(mapped, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Request.Cases[0].Duration != 17*time.Second || job.Request.Cases[0].Usage.CostKnown() || job.Request.Cases[0].SourceRunID != execution.RunID.String() {
		t.Fatalf("original evidence lost: %+v", job.Request)
	}
	t.Run("scope-and-capture-denial", func(t *testing.T) {
		foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "other"}}})
		for _, denied := range []context.Context{foreign, sentinel.WithApp(ctx, "wrong-app"), sentinel.WithTenant(ctx, "wrong-tenant")} {
			if _, evalErr := a.Evaluate(denied, req); evalErr == nil {
				t.Fatal("accepted mismatched scope")
			}
		}
		altered := req
		altered.Execution.Outcome.Output = "tampered"
		if _, evalErr := a.Evaluate(ctx, altered); evalErr == nil {
			t.Fatal("accepted altered capture")
		}
		altered = req
		altered.CaseDigest = strings.Repeat("b", 64)
		if _, evalErr := a.Evaluate(ctx, altered); evalErr == nil {
			t.Fatal("accepted changed task identity")
		}
		altered = req
		altered.Execution.RunID = cid.NewAgentRunID()
		altered.Execution, err = opt.SealExecution(altered.Execution)
		if err != nil {
			t.Fatal(err)
		}
		if _, evalErr := a.Evaluate(ctx, altered); evalErr == nil {
			t.Fatal("reused evaluation key for different execution")
		}
	})

	t.Run("incomplete-output-is-not-a-score", func(t *testing.T) {
		incomplete := req
		incomplete.TrialID = cid.New(opt.PrefixTrial)
		incomplete.IdempotencyKey = "incomplete"
		incomplete.Execution.RunID = cid.NewAgentRunID()
		incomplete.Execution.State = run.StateIncomplete
		incomplete.Execution.Outcome.StopReason = run.StopStepLimit
		incomplete.Execution, err = opt.SealExecution(incomplete.Execution)
		if err != nil {
			t.Fatal(err)
		}
		value, evalErr := a.Evaluate(ctx, incomplete)
		if evalErr != nil || value.Complete || value.Score != nil || len(value.Errors) == 0 {
			t.Fatalf("incomplete became scored: %+v %v", value, evalErr)
		}
	})

	candidate := req
	candidate.CandidateID = cid.New(opt.PrefixCandidate)
	candidate.TrialID = cid.New(opt.PrefixTrial)
	candidate.IdempotencyKey = "candidate"
	candidate.Execution.RunID = cid.NewAgentRunID()
	candidate.Execution.Outcome.Output = "wrong"
	candidate.Execution, err = opt.SealExecution(candidate.Execution)
	if err != nil {
		t.Fatal(err)
	}
	scored, err := a.Evaluate(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	objective := opt.DefaultObjective()
	objective.MinValidationCases = 1
	objective.ProtectedCriteria = []string{plan.Criteria[0].ID}
	comparison, err := a.Compare(ctx, opt.ComparisonRequest{Plan: plan, BaselineIDs: []string{first.ID}, CandidateIDs: []string{scored.ID}, Objective: objective})
	if err != nil || !comparison.Complete || comparison.MeanDelta != -1 || comparison.LowerBound != -1 || len(comparison.Cases) != 1 || comparison.Cases[0].CaseID != task.ID || comparison.ProtectedRegressions[plan.Criteria[0].ID] != 1 || comparison.LatencyRatio == nil || *comparison.LatencyRatio != 1 || comparison.CostRatio != nil {
		t.Fatalf("comparison: %+v %v", comparison, err)
	}
	objective.Confidence = .99
	if _, err = a.Compare(ctx, opt.ComparisonRequest{Plan: plan, BaselineIDs: []string{first.ID}, CandidateIDs: []string{scored.ID}, Objective: objective}); err == nil {
		t.Fatal("silently changed frozen objective")
	}
	t.Run("unknown-cost-cannot-pass-cost-scorer", func(t *testing.T) {
		priced, freezeErr := eng.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID, Scorers: []evalplan.Criterion{{ID: "cost", Scorer: "cost", Config: map[string]any{"max_cost": 1.0}}}})
		if freezeErr != nil {
			t.Fatal(freezeErr)
		}
		costRef := opt.ContentRef{Ref: priced.ID.String(), Revision: "1", Digest: priced.Digest}
		costBinding := binding
		costBinding.Plan = costRef
		costAdapter, createErr := adapter.NewEvaluator(adapter.Config{Engine: eng, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
			return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
		}), Bindings: []adapter.Binding{costBinding}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		costPlan, planErr := costAdapter.Freeze(ctx, opt.EvaluationSpec{Source: costRef, Tasks: []opt.TaskCase{task}})
		if planErr != nil {
			t.Fatal(planErr)
		}
		costRequest := req
		costRequest.Plan = costPlan
		costRequest.IdempotencyKey = "unknown-cost"
		if _, evalErr := costAdapter.Evaluate(ctx, costRequest); evalErr == nil {
			t.Fatal("unknown cost was treated as zero")
		}
	})
}
