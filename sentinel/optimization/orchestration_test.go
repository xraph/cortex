package optimization_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	sid "github.com/xraph/sentinel/id"
	smemory "github.com/xraph/sentinel/store/memory"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/storetest"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type teamModel struct {
	calls atomic.Int32
	fail  bool
}

func (m *teamModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	call := m.calls.Add(1)
	if m.fail && call == 1 {
		return nil, errors.New("fixture provider failed")
	}
	return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 7}}, nil
}
func (*teamModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, opt.ErrUnavailable
}

func TestParallelTeamTracePreservesOriginalReceipts(t *testing.T) {
	model := &teamModel{}
	bundle, runtime, source, reopen := storetest.OrchestrationFixture(t, model)
	version := bundle.Versions[0]
	ctx := cortex.WithScope(context.Background(), version.Scope)
	receipt := opt.EnvironmentReceipt{ID: "team-receipt", Provider: "fixture", EnvironmentID: "team-env", Scope: version.Scope, AttemptID: id.New(opt.PrefixAttempt), Snapshot: bundle.Experiment.Spec.Tasks[0].EnvironmentSnapshot, Fence: opt.Fence{Owner: "worker", Generation: 1}}
	view, err := version.Orchestration.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), Scope: version.Scope, PreparedDigest: version.Orchestration.Prepared.Digest(), TokenOnly: true, Deadline: time.Now().Add(time.Minute), Limits: version.Policy.OrchestrationPresets[0].Limits, Participants: map[string]execution.Resources{}}
	for name := range view.Participants {
		policy.Participants[name] = execution.Resources{Steps: 2, Tokens: 100, FanOut: 1}
	}
	team, err := runtime.RunPreparedOrchestration(ctx, version.Orchestration.Prepared, bundle.Experiment.Spec.Tasks[0].Input, engine.PreparedRunOptions{Purpose: run.PurposeOptimizationTrial, Provenance: run.ExecutionProvenance{HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String(), EnvironmentReceiptID: receipt.ID}, OrchestrationBudget: &policy})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := opt.CaptureOrchestration(ctx, source, version, receipt, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Usage.TokenQuality != opt.UsageUnknown || unknown.Usage.Tokens != 0 || unknown.Orchestration.Budget.Charged.Tokens != 200 {
		t.Fatalf("reservation became measured usage: %+v", unknown.Usage)
	}
	for _, child := range unknown.Orchestration.Children {
		if child.Execution == nil {
			t.Fatal("missing terminal child")
		}
		req := child.Reservation
		err = source.SettleOrchestrationChild(ctx, execution.Settlement{ReservationID: req.Request.ID, RequestDigest: req.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 7}, Known: execution.KnownTokens}, Receipt: "trusted-fixture/" + req.Request.TaskID.String()})
		if err != nil {
			t.Fatal(err)
		}
	}
	captured, err := opt.CaptureOrchestration(ctx, source, version, receipt, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := adapter.ConvertExecution(captured)
	if err != nil {
		t.Fatal(err)
	}
	if converted.SourceRunID != team.ID.String() || converted.Output != team.Output || converted.Usage.Tokens != 14 || converted.Usage.CostKnown() || model.calls.Load() != 2 {
		t.Fatalf("team capture lost identity or cost provenance: %+v calls=%d", converted, model.calls.Load())
	}
	evaluateCapturedTeam(t, bundle, captured)
	for _, child := range captured.Orchestration.Children {
		if !slices.Contains(converted.ArtifactRefs, "cortex/run/"+child.Execution.RunID.String()) || !slices.Contains(converted.ArtifactRefs, "cortex/reservation/"+child.Reservation.Request.ID.String()) || !slices.Contains(converted.ArtifactRefs, "cortex/settlement/"+child.Reservation.Settlement.Digest) {
			t.Fatal("child provenance lost")
		}
		for _, step := range child.Execution.StepIDs {
			if !slices.Contains(converted.ArtifactRefs, "cortex/step/"+step.String()) {
				t.Fatal("child trace lost")
			}
		}
	}
	again, err := opt.CaptureOrchestration(ctx, reopen(), version, receipt, team.ID)
	if err != nil || again.Digest != captured.Digest || model.calls.Load() != 2 {
		t.Fatalf("capture recovery changed original: %v", err)
	}
}

func TestParallelTeamFailureRetainsPartialTrace(t *testing.T) {
	model := &teamModel{fail: true}
	bundle, runtime, source, reopen := storetest.OrchestrationFixture(t, model)
	version := bundle.Versions[0]
	ctx := cortex.WithScope(context.Background(), version.Scope)
	receipt := opt.EnvironmentReceipt{ID: "failed-team", Provider: "fixture", EnvironmentID: "isolated-failure", Scope: version.Scope, AttemptID: id.New(opt.PrefixAttempt), Snapshot: bundle.Experiment.Spec.Tasks[0].EnvironmentSnapshot, Fence: opt.Fence{Owner: "worker", Generation: 1}}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), Scope: version.Scope, PreparedDigest: version.Orchestration.Prepared.Digest(), TokenOnly: true, Deadline: time.Now().Add(time.Minute), Limits: version.Policy.OrchestrationPresets[0].Limits, Participants: map[string]execution.Resources{"worker": {Steps: 2, Tokens: 100, FanOut: 1}, "reviewer": {Steps: 2, Tokens: 100, FanOut: 1}}}
	team, err := runtime.RunPreparedOrchestration(ctx, version.Orchestration.Prepared, "task", engine.PreparedRunOptions{Purpose: run.PurposeOptimizationTrial, Provenance: run.ExecutionProvenance{HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String(), EnvironmentReceiptID: receipt.ID}, OrchestrationBudget: &policy})
	if err == nil || team == nil {
		t.Fatalf("expected partial runtime failure: %v", err)
	}
	captured, err := opt.CaptureOrchestration(ctx, source, version, receipt, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if captured.State != run.StateFailed || len(captured.Orchestration.Children) == 0 || captured.Usage.TokenQuality != opt.UsageUnknown {
		t.Fatal("partial failure lost evidence")
	}
	before := model.calls.Load()
	restored, err := opt.CaptureOrchestration(ctx, reopen(), version, receipt, team.ID)
	if err != nil || restored.Digest != captured.Digest || before != model.calls.Load() {
		t.Fatalf("failed capture replayed: %v", err)
	}
	converted, err := adapter.ConvertExecution(restored)
	if err != nil || converted.SourceRunID != team.ID.String() || len(converted.ArtifactRefs) == 0 {
		t.Fatalf("failed trace conversion: %v", err)
	}
}

func evaluateCapturedTeam(t *testing.T, bundle opt.ExperimentBundle, captured opt.ExecutionRecord) {
	t.Helper()
	scope := bundle.Experiment.Spec.Scope
	ctx := cortex.WithScope(context.Background(), scope)
	mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
	source := smemory.New()
	su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "team-capture"}
	if err := source.CreateSuite(mapped, su); err != nil {
		t.Fatal(err)
	}
	task := bundle.Experiment.Spec.Tasks[0]
	tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Name: "team", Input: task.Input, Expected: captured.Outcome.Output, Scorers: []testcase.ScorerConfig{{Name: "exact"}}}
	if err := source.CreateCase(mapped, tc); err != nil {
		t.Fatal(err)
	}
	runtime, err := se.New(se.WithStore(source))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := runtime.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID})
	if err != nil {
		t.Fatal(err)
	}
	ref := opt.ContentRef{Ref: frozen.ID.String(), Revision: "1", Digest: frozen.Digest}
	evaluator, err := adapter.NewEvaluator(adapter.Config{Engine: runtime, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
		return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
	}), Bindings: []adapter.Binding{{Scope: scope, AppID: "app", TenantID: "tenant", Plan: ref, Cases: []adapter.CaseBinding{{Task: task, CaseID: tc.ID}}}}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := evaluator.Freeze(ctx, opt.EvaluationSpec{Source: ref, Tasks: []opt.TaskCase{task}})
	if err != nil {
		t.Fatal(err)
	}
	request := opt.EvaluationRequest{ExperimentID: bundle.Experiment.ID, TrialID: id.New(opt.PrefixTrial), CaseID: task.ID, CaseDigest: task.Digest, Plan: plan, Execution: captured, IdempotencyKey: "team-capture"}
	first, err := evaluator.Evaluate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := evaluator.Evaluate(ctx, request)
	if err != nil || again.ID != first.ID || !again.Complete {
		t.Fatalf("team evaluation retry: %v", err)
	}
	key, err := sid.Parse(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := source.GetRecordedJob(mapped, key)
	if err != nil || len(job.Request.Cases) != 1 || job.Request.Cases[0].SourceRunID != captured.ExecutionID().String() || job.Request.Cases[0].Usage.CostKnown() {
		t.Fatalf("recorded team provenance changed: %v", err)
	}
}
