package optimization_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"
	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	sid "github.com/xraph/sentinel/id"
	ssqlite "github.com/xraph/sentinel/store/sqlite"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	cid "github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	opt "github.com/xraph/cortex/optimization"
	osqlite "github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"
	"github.com/xraph/cortex/run"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type loopAuthority struct{ actor opt.IdentityRef }

func (loopAuthority) Authorize(context.Context, opt.Action, opt.Resource) error { return nil }
func (loopAuthority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}
func (a loopAuthority) Current(context.Context) (opt.IdentityRef, error) { return a.actor, nil }

type loopModel struct {
	proposal    string
	composition string
	calls       int
}

func (p *loopModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	p.calls++
	body := p.proposal
	if p.calls > 1 {
		body = `{"lessons":[]}`
		if p.calls == 2 && p.composition != "" {
			body = p.composition
		}
	}
	return &llm.Response{Content: body, Model: "fixture"}, nil
}
func (*loopModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, opt.ErrUnavailable
}

type loopCounter struct{}

func (loopCounter) Bound(context.Context, *llm.Request) (opt.InputTokenBound, error) {
	return opt.InputTokenBound{Model: "fixture", ModelRevision: "1", Tokens: 100, Bounded: true}, nil
}

// This file-backed host is a recovery fixture, not a qualified sandbox provider.
type loopHost struct {
	dir, base  string
	executions int
	lost       bool
	lostOpen   bool
}
type loopEnvironment struct {
	host    *loopHost
	request opt.TrialRequest
	receipt opt.EnvironmentReceipt
}

func (h *loopHost) Open(_ context.Context, r opt.TrialRequest) (opt.TrialEnvironment, error) {
	receipt := opt.EnvironmentReceipt{ID: r.AttemptID.String(), Provider: "fixture", EnvironmentID: r.AttemptID.String(), Scope: r.Scope, AttemptID: r.AttemptID, Snapshot: r.Task.EnvironmentSnapshot, Fence: r.Fence, CreatedAt: r.Deadline.Add(-time.Minute), Capabilities: opt.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(h.dir, r.AttemptID.String()+".request"), data, 0600); err != nil {
		return nil, err
	}
	if h.lostOpen {
		h.lostOpen = false
		return nil, opt.ErrUnknownAttempt
	}
	return &loopEnvironment{host: h, request: r, receipt: receipt}, nil
}
func (h *loopHost) Recover(_ context.Context, r opt.EnvironmentReceipt) (opt.TrialEnvironment, error) {
	data, err := os.ReadFile(filepath.Join(h.dir, r.AttemptID.String()+".request"))
	if err != nil {
		return nil, err
	}
	var request opt.TrialRequest
	if err = json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	return &loopEnvironment{host: h, request: request, receipt: r}, nil
}
func (h *loopHost) Stop(_ context.Context, r opt.EnvironmentReceipt, f opt.Fence) (opt.StopReceipt, error) {
	return opt.StopReceipt{EnvironmentReceiptID: r.ID, AttemptID: r.AttemptID, Fence: f, State: opt.StopTerminated, Evidence: opt.ContentRef{Ref: "fixture-stop", Revision: "1", Digest: strings.Repeat("a", 64)}}, nil
}
func (e *loopEnvironment) Receipt() opt.EnvironmentReceipt { return e.receipt }
func (e *loopEnvironment) Close(context.Context) error     { return nil }
func (e *loopEnvironment) Inspect(_ context.Context, attempt cid.ID) (opt.AttemptStatus, error) {
	data, err := os.ReadFile(filepath.Join(e.host.dir, attempt.String()+".capture"))
	if err != nil {
		return opt.AttemptStatus{}, err
	}
	var execution opt.ExecutionRecord
	if err = json.Unmarshal(data, &execution); err != nil {
		return opt.AttemptStatus{}, err
	}
	return opt.AttemptStatus{AttemptID: attempt, State: opt.TrialCompleted, Execution: &execution, ObservedAt: time.Now().UTC()}, nil
}
func (e *loopEnvironment) Execute(_ context.Context, p *engine.PreparedAgent, _ string, options engine.PreparedRunOptions) (*opt.ExecutionRecord, error) {
	if options.Purpose != run.PurposeOptimizationTrial || options.SessionID.IsNil() {
		return nil, opt.ErrInvalidSpec
	}
	e.host.executions++
	now := time.Now().UTC()
	output := "answer"
	if p.Digest() == e.host.base {
		output = "incorrect"
	}
	capture, err := opt.SealExecution(opt.ExecutionRecord{RunID: cid.NewAgentRunID(), PreparedDigest: p.Digest(), InputDigest: e.request.Task.InputDigest, State: run.StateCompleted, Outcome: run.Outcome{Output: output, StopReason: run.StopFinalAnswer, CompletedAt: now}, StartedAt: now.Add(-time.Second), CompletedAt: now, Duration: time.Second, Environment: e.receipt, Usage: opt.Usage{Tokens: 10, TokenQuality: opt.UsageKnown}})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(capture)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(e.host.dir, e.receipt.AttemptID.String()+".capture"), data, 0600); err != nil {
		return nil, err
	}
	if e.host.lost {
		e.host.lost = false
		return nil, errors.New("fixture lost execution acknowledgement")
	}
	return &capture, nil
}

type lostEvaluation struct {
	opt.Evaluator
	lost bool
}

func (e *lostEvaluation) Evaluate(ctx context.Context, r opt.EvaluationRequest) (opt.Evaluation, error) {
	v, err := e.Evaluator.Evaluate(ctx, r)
	if err == nil && e.lost {
		e.lost = false
		return opt.Evaluation{}, opt.ErrUnknownAttempt
	}
	return v, err
}
func loopDB(t *testing.T, path string) *grove.DB {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), path+"?_pragma=busy_timeout(10000)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSentinelOptimizationLoopRecoversAcrossReopen(t *testing.T) {
	for _, composition := range []bool{false, true} {
		t.Run(fmt.Sprint("composition=", composition), func(t *testing.T) { testSentinelLoop(t, composition) })
	}
}
func testSentinelLoop(t *testing.T, composition bool) {
	t.Helper()
	bundle := storetest.Fixture(t, "sentinel-loop")
	view, err := bundle.Versions[0].Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	base := bundle.Versions[0]
	base.Policy.MutableSections = []string{view.Sections[0].ID}
	base, err = opt.SealVersion(base)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Versions[0] = base
	bundle.Experiment.Spec.Baseline.Digest = base.Digest
	bundle.Experiment.Spec.Limits.MaxRounds = 1
	bundle.Experiment.Spec.Objective.MinValidationCases = 1
	bundle.Experiment.Spec.Budget.MaxTokens = 10000
	scope := bundle.Experiment.Spec.Scope
	ctx := cortex.WithScope(context.Background(), scope)
	mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
	dir := t.TempDir()
	sentinelPath := filepath.Join(dir, "sentinel.db")
	optimizerPath := filepath.Join(dir, "optimizer.db")
	sentinelStore := ssqlite.New(loopDB(t, sentinelPath))
	if err = sentinelStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suiteRecord := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "adapter-loop"}
	if err = sentinelStore.CreateSuite(mapped, suiteRecord); err != nil {
		t.Fatal(err)
	}
	cases := make([]adapter.CaseBinding, 0, len(bundle.Experiment.Spec.Tasks))
	for _, task := range bundle.Experiment.Spec.Tasks {
		tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: suiteRecord.ID, Name: task.ID, Input: task.Input, Expected: "answer", Scorers: []testcase.ScorerConfig{{Name: "exact"}}}
		if err = sentinelStore.CreateCase(mapped, tc); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, adapter.CaseBinding{Task: task, CaseID: tc.ID})
	}
	sentinelEngine, err := se.New(se.WithStore(sentinelStore))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := sentinelEngine.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: suiteRecord.ID})
	if err != nil {
		t.Fatal(err)
	}
	ref := opt.ContentRef{Ref: frozen.ID.String(), Digest: frozen.Digest, Revision: "1"}
	bundle.Experiment.Spec.Evaluation = ref
	makeEvaluator := func() *adapter.Evaluator {
		t.Helper()
		value, createErr := adapter.NewEvaluator(adapter.Config{Engine: sentinelEngine, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
			return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
		}), Bindings: []adapter.Binding{{Scope: scope, AppID: "app", TenantID: "tenant", Plan: ref, Cases: cases}}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	evaluator := &lostEvaluation{Evaluator: makeEvaluator(), lost: true}
	bundle.Experiment.EvaluationPlan, err = evaluator.Freeze(ctx, opt.EvaluationSpec{Source: ref, Tasks: bundle.Experiment.Spec.Tasks})
	if err != nil {
		t.Fatal(err)
	}
	repository := osqlite.New(loopDB(t, optimizerPath))
	if err = repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	authority := loopAuthority{actor: bundle.Experiment.Spec.Requester}
	proposal := opt.Proposal{Edits: []opt.Edit{{Kind: opt.EditSectionBody, Target: base.Policy.MutableSections[0], Text: "Check the evidence."}}, Rationale: "Test candidate"}
	proposals := []opt.Proposal{proposal}
	expectedExecutions, expectedControllers := 12, 2
	if composition {
		second := opt.Proposal{Edits: []opt.Edit{{Kind: opt.EditSectionBody, Target: base.Policy.MutableSections[0], Text: "Verify the result."}}, Rationale: "Second test candidate"}
		proposals = append(proposals, second)
		bundle.Experiment.Spec.Limits.MaxCandidates = 3
		expectedExecutions, expectedControllers = 18, 3
	}
	body, err := json.Marshal(map[string]any{"proposals": proposals})
	if err != nil {
		t.Fatal(err)
	}
	model := &loopModel{proposal: string(body)}
	if composition {
		combined := opt.Proposal{Edits: []opt.Edit{{Kind: opt.EditSectionBody, Target: base.Policy.MutableSections[0], Text: "Check the evidence and verify the result."}}, Rationale: "Compose tested candidates"}
		data, encodeErr := json.Marshal(map[string]any{"proposal": combined})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		model.composition = string(data)
	}
	host := &loopHost{dir: dir, base: base.Prepared.Digest(), lost: true, lostOpen: true}
	makeOptimizer := func() *opt.Optimizer {
		t.Helper()
		trials, createErr := opt.NewTrialPhaseHandler(opt.TrialConfig{Repository: repository, Authorizer: authority, IdentityResolver: authority, Environments: host, Evaluator: evaluator, Allowance: opt.TrialAllowance{MaxTokens: 100}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		models, createErr := opt.NewControllerFactory(repository, authority, authority, []opt.ModelBinding{{Model: "fixture", Revision: "1", Provider: model, Counter: loopCounter{}}}, opt.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 100, MaxProposals: 2, MaxLessons: 2})
		if createErr != nil {
			t.Fatal(createErr)
		}
		handler, createErr := opt.NewControllerPhaseHandler(repository, models, trials, "fixture")
		if createErr != nil {
			t.Fatal(createErr)
		}
		worker, createErr := opt.NewOptimizer(opt.OptimizerConfig{Repository: repository, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: opt.DefaultWorkerConfig()})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return worker
	}
	worker := makeOptimizer()
	created, err := worker.Create(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Start(ctx, created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	for range 120 {
		err = worker.WorkOnce(ctx, created.ID)
		if err != nil && !errors.Is(err, opt.ErrUnknownAttempt) {
			t.Fatal(err)
		}
		current, getErr := repository.GetExperiment(ctx, created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if errors.Is(err, opt.ErrUnknownAttempt) {
			recoveries++
			paused, pauseErr := worker.Pause(ctx, current.ID, current.Revision)
			if pauseErr != nil {
				t.Fatal(pauseErr)
			}
			if _, resumeErr := worker.Resume(ctx, current.ID, paused.Revision); resumeErr != nil {
				t.Fatal(resumeErr)
			}
			if err = repository.Close(); err != nil {
				t.Fatal(err)
			}
			if err = sentinelStore.Close(); err != nil {
				t.Fatal(err)
			}
			repository = osqlite.New(loopDB(t, optimizerPath))
			sentinelStore = ssqlite.New(loopDB(t, sentinelPath))
			sentinelEngine, err = se.New(se.WithStore(sentinelStore))
			if err != nil {
				t.Fatal(err)
			}
			evaluator.Evaluator = makeEvaluator()
			worker = makeOptimizer()
			continue
		}
		if current.State == opt.StateReviewRequired {
			decision, decisionErr := repository.GetDecision(ctx, current.DecisionID)
			if decisionErr != nil {
				t.Fatal(decisionErr)
			}
			if decision.Outcome != opt.DecisionRecommend || !decision.Comparison.Complete || decision.Comparison.MeanDelta != 1 || len(decision.EvaluationIDs) != 6 || host.executions != expectedExecutions || model.calls != expectedControllers || recoveries != 3 {
				t.Fatalf("loop evidence: decision=%+v executions=%d controller=%d recoveries=%d", decision, host.executions, model.calls, recoveries)
			}
			if err = repository.Close(); err != nil {
				t.Fatal(err)
			}
			if err = sentinelStore.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("optimization loop did not finish")
}
