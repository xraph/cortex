package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

func TestTaskReviewRuntimeRequiresExplicitAuthority(t *testing.T) {
	e, err := engine.New()
	require.NoError(t, err)
	_, err = e.NewTaskReviewRuntime(engine.TaskReviewRuntimeConfig{})
	require.Error(t, err)
}

type reviewRuntimeFence func(context.Context, engine.TaskReviewAction, coordination.TaskCompletion, func(coordination.TaskCompletion) error) error

func (f reviewRuntimeFence) WithTaskReviewFence(ctx context.Context, a engine.TaskReviewAction, c coordination.TaskCompletion, next func(coordination.TaskCompletion) error) error {
	return f(ctx, a, c, next)
}

type reviewRuntimeStore struct {
	store.Store
	receipt coordination.TaskCompletion
	plan    planning.Plan
}

func (s *reviewRuntimeStore) GetTaskCompletion(context.Context, id.ID) (*coordination.TaskCompletion, error) {
	c := s.receipt
	c.Fresh = false
	return &c, nil
}
func (s *reviewRuntimeStore) ReserveTaskCompletion(context.Context, coordination.TaskCompletionRequest) (coordination.TaskCompletion, error) {
	return s.receipt, nil
}
func (s *reviewRuntimeStore) GetReservation(context.Context, id.ID) (execution.Reservation, error) {
	return s.receipt.Reservation, nil
}
func (s *reviewRuntimeStore) GetPlan(context.Context, id.ID) (planning.Plan, error) {
	return s.plan, nil
}

type reviewRuntimeResolver func(context.Context, coordination.TaskCompletion) (engine.TaskReviewWork, error)

func (f reviewRuntimeResolver) ResolveTaskReview(ctx context.Context, c coordination.TaskCompletion) (engine.TaskReviewWork, error) {
	return f(ctx, c)
}

type reviewRuntimeAuthorizer func(context.Context, engine.TaskReviewAction, coordination.TaskCompletion) error

func (f reviewRuntimeAuthorizer) AuthorizeTaskReview(ctx context.Context, a engine.TaskReviewAction, c coordination.TaskCompletion) error {
	return f(ctx, a, c)
}

type reviewRuntimeLLM struct {
	llm.Client
	output string
	calls  int
}

func (c *reviewRuntimeLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	c.calls++
	return &llm.Response{Content: c.output, FinishReason: "stop", Usage: llm.Usage{TotalTokens: 2}}, nil
}

func reviewRuntimeFixture(t *testing.T) (context.Context, *engine.Engine, *reviewRuntimeStore, *reviewRuntimeLLM, engine.TaskReviewRuntimeConfig) {
	t.Helper()
	ctx := overlayCtx("review-runtime")
	scope := cortex.ScopeFromContext(ctx)
	backend := newSQLiteStoreForTest(t)
	storage := &reviewRuntimeStore{Store: backend}
	client := new(reviewRuntimeLLM)
	e, err := engine.New(engine.WithStore(storage), engine.WithLLM(client))
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "independent-reviewer"}
	require.NoError(t, storage.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 2, MaxTokens: 100, MaxToolCalls: 1, Deadline: time.Now().Add(time.Minute)}
	prepared, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{ToolsRestricted: true, Tools: []string{}, Budget: &budget})
	require.NoError(t, err)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked output", Criteria: []verification.Criterion{{ID: "checked", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	taskID := id.New(planning.PrefixTask)
	plan, err := planning.Seal(planning.Plan{SchemaVersion: 2, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"producer"}, Actor: "host", Reason: "review required", Tasks: []planning.Task{{ID: taskID, Instruction: "check output", Owner: "producer", Criteria: []string{"checked"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 4096}, Review: &planning.ReviewRequirement{ReviewerAgent: ag.ID.String(), ReviewerPrincipal: "reviewer", PolicyRevision: "1"}}}}, planning.Limits{})
	require.NoError(t, err)
	storage.plan = plan
	taskContract, err := planning.TaskContract(plan, taskID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", Producer: "producer", MediaType: "text/plain", Sensitivity: evidence.SensitivityInternal, CreatedAt: time.Now(), RetainUntil: time.Now().Add(time.Hour)}, Data: []byte("evidence")})
	require.NoError(t, err)
	handoff, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Scope: scope, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, TaskID: taskID, ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "coordinator", Principal: "host"}, Receiver: coordination.Identity{Agent: ag.ID.String(), Principal: "reviewer"}, GrantRef: "review-grant", Fence: 1, Authority: planning.Authority{Budget: budget}, ContractDigest: plan.Contract.Digest, Criteria: []string{"checked"}, Inputs: []evidence.Ref{artifact.Ref}, Purpose: coordination.HandoffReview, Deadline: budget.Deadline})
	require.NoError(t, err)
	allocation := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: plan.ID, PlanDigest: plan.Digest, Scope: scope, TaskID: taskID, OwnerRef: handoff.AssignmentID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 2, Tokens: 100, ToolCalls: 1, FanOut: 1}, Deadline: budget.Deadline}
	_, allocationDigest, err := allocation.Freeze()
	require.NoError(t, err)
	req := coordination.TaskCompletionRequest{SchemaVersion: 1, AssignmentID: id.New(coordination.PrefixAssignment), AssignmentRevision: 3, AssignmentDigest: evidence.Digest([]byte("subject")), BindingDigest: evidence.Digest([]byte("binding")), Handoff: handoff, SessionID: id.NewSessionID(), RunID: id.NewAgentRunID(), ReviewID: id.New(coordination.PrefixReview), AdjudicationID: id.New(coordination.PrefixTaskAdjudication), AcceptanceID: id.New(coordination.PrefixTaskAcceptance), PreparedDigest: prepared.Digest(), InputDigest: evidence.Digest([]byte("review retained evidence")), Reservation: allocation, Actor: "host", Reason: "review result"}
	_, reqDigest, err := req.Freeze()
	require.NoError(t, err)
	now := time.Now().UTC()
	receipt, err := coordination.SealTaskCompletion(coordination.TaskCompletion{SchemaVersion: 1, Request: req, RequestDigest: reqDigest, Reservation: execution.Reservation{Request: allocation, RequestDigest: allocationDigest, CreatedAt: now}, ObservedAt: now})
	require.NoError(t, err)
	receipt.Fresh = true
	storage.receipt = receipt
	output, err := json.Marshal(coordination.ReviewOutput{SchemaVersion: 1, HandoffDigest: handoff.Digest, SubjectDigest: req.AssignmentDigest, BindingDigest: req.BindingDigest, Findings: []coordination.ReviewFinding{{CriterionID: "checked", Assessment: coordination.ReviewSupported, Reason: "evidence supports result", EvidenceIDs: []id.ID{artifact.Ref.ID}}}})
	require.NoError(t, err)
	client.output = string(output)
	config := engine.TaskReviewRuntimeConfig{Resolver: reviewRuntimeResolver(func(context.Context, coordination.TaskCompletion) (engine.TaskReviewWork, error) {
		return engine.TaskReviewWork{Prepared: prepared, Input: "review retained evidence", Contract: taskContract}, nil
	}), Fence: reviewRuntimeFence(func(_ context.Context, _ engine.TaskReviewAction, c coordination.TaskCompletion, next func(coordination.TaskCompletion) error) error {
		return next(c)
	}), Authorizer: reviewRuntimeAuthorizer(func(context.Context, engine.TaskReviewAction, coordination.TaskCompletion) error { return nil }), EvidenceAuthorizer: captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return nil })}
	return ctx, e, storage, client, config
}

func TestTaskReviewRuntimeRunsReservedIdentityOnceAndObservesAfterRestart(t *testing.T) {
	ctx, e, s, client, cfg := reviewRuntimeFixture(t)
	worker, err := e.NewTaskReviewRuntime(cfg)
	require.NoError(t, err)
	pending, err := worker.Observe(ctx, s.receipt)
	require.NoError(t, err)
	require.Nil(t, pending)
	require.Zero(t, client.calls)
	require.NoError(t, worker.Execute(ctx, s.receipt))
	require.Equal(t, 1, client.calls)
	require.Error(t, worker.Execute(ctx, s.receipt))
	require.Equal(t, 1, client.calls, "duplicate creation cannot repeat provider call")
	historical := s.receipt
	historical.Fresh = false
	require.Error(t, worker.Execute(ctx, historical))
	require.Equal(t, 1, client.calls)
	fresh, err := engine.New(engine.WithStore(s), engine.WithLLM(client))
	require.NoError(t, err)
	observer, err := fresh.NewTaskReviewRuntime(cfg)
	require.NoError(t, err)
	review, err := observer.Observe(ctx, historical)
	require.NoError(t, err)
	require.NotNil(t, review)
	require.Equal(t, s.receipt.Request.RunID, review.RunID)
	require.Equal(t, 1, client.calls)
	row, err := s.GetRun(ctx, review.RunID)
	require.NoError(t, err)
	require.Equal(t, run.PurposeEvaluation, row.Provenance.Purpose)
}

func TestTaskReviewRuntimeRejectsChangedInputsAndCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"denied", "changed input", "changed receipt", "fence mutation", "scope", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, e, s, client, cfg := reviewRuntimeFixture(t)
			offered := s.receipt
			switch mode {
			case "denied":
				cfg.Authorizer = reviewRuntimeAuthorizer(func(context.Context, engine.TaskReviewAction, coordination.TaskCompletion) error {
					return errors.New("denied")
				})
			case "changed input":
				original := cfg.Resolver
				cfg.Resolver = reviewRuntimeResolver(func(ctx context.Context, c coordination.TaskCompletion) (engine.TaskReviewWork, error) {
					w, err := original.ResolveTaskReview(ctx, c)
					w.Input = "other"
					return w, err
				})
			case "changed receipt":
				s.receipt.Digest = evidence.Digest([]byte("other"))
			case "fence mutation":
				cfg.Fence = reviewRuntimeFence(func(_ context.Context, _ engine.TaskReviewAction, c coordination.TaskCompletion, next func(coordination.TaskCompletion) error) error {
					c.Request.InputDigest = evidence.Digest([]byte("other"))
					return next(c)
				})
			case "scope":
				ctx = overlayCtx("other")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			worker, err := e.NewTaskReviewRuntime(cfg)
			require.NoError(t, err)
			require.Error(t, worker.Execute(ctx, offered))
			require.Zero(t, client.calls)
		})
	}
}

func TestTaskReviewRuntimeRejectsMalformedOrMismatchedFindings(t *testing.T) {
	for _, mode := range []string{"malformed", "wrong binding", "unknown field"} {
		t.Run(mode, func(t *testing.T) {
			ctx, e, s, client, cfg := reviewRuntimeFixture(t)
			switch mode {
			case "malformed":
				client.output = "not JSON"
			case "wrong binding":
				var output coordination.ReviewOutput
				require.NoError(t, json.Unmarshal([]byte(client.output), &output))
				output.BindingDigest = evidence.Digest([]byte("different result"))
				data, err := json.Marshal(output)
				require.NoError(t, err)
				client.output = string(data)
			case "unknown field":
				var output map[string]any
				require.NoError(t, json.Unmarshal([]byte(client.output), &output))
				output["override_verdict"] = true
				data, err := json.Marshal(output)
				require.NoError(t, err)
				client.output = string(data)
			}
			worker, err := e.NewTaskReviewRuntime(cfg)
			require.NoError(t, err)
			require.NoError(t, worker.Execute(ctx, s.receipt))
			review, err := worker.Observe(ctx, s.receipt)
			require.Error(t, err)
			require.Nil(t, review)
			require.Equal(t, 1, client.calls)
		})
	}
}

func TestTaskReviewRuntimeRejectsAlteredRetainedExecution(t *testing.T) {
	for _, mode := range []string{"input", "session", "tools", "budget", "start"} {
		t.Run(mode, func(t *testing.T) {
			ctx, e, s, _, cfg := reviewRuntimeFixture(t)
			worker, err := e.NewTaskReviewRuntime(cfg)
			require.NoError(t, err)
			require.NoError(t, worker.Execute(ctx, s.receipt))
			row, err := s.GetRun(ctx, s.receipt.Request.RunID)
			require.NoError(t, err)
			switch mode {
			case "input":
				row.Input = "different evidence"
			case "session":
				row.SessionID = id.NewSessionID()
			case "tools":
				row.Provenance.Tools = []run.Revision{{Name: "unreserved", Revision: "1"}}
			case "budget":
				row.Budget.MaxTokens = 0
			case "start":
				at := s.receipt.ObservedAt.Add(-time.Hour)
				row.StartedAt = &at
			}
			require.NoError(t, s.UpdateRun(ctx, row))
			review, err := worker.Observe(ctx, s.receipt)
			require.Error(t, err)
			require.Nil(t, review)
		})
	}
}

func TestTaskReviewRuntimeExecutionDeadlineBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		parent  time.Duration
		observe bool
		want    time.Duration
	}{
		{name: "default", want: 30 * time.Second},
		{name: "configured", timeout: 45 * time.Second, want: 45 * time.Second},
		{name: "handoff ceiling", timeout: 5 * time.Minute, want: time.Minute},
		{name: "caller ceiling", timeout: 5 * time.Minute, parent: 10 * time.Second, want: 10 * time.Second},
		{name: "observation stays short", timeout: 5 * time.Minute, observe: true, want: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, e, storage, client, cfg := reviewRuntimeFixture(t)
			cfg.ExecutionTimeout = tc.timeout
			if tc.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			stopped := errors.New("deadline inspected before dispatch")
			cfg.Fence = reviewRuntimeFence(func(bounded context.Context, _ engine.TaskReviewAction, _ coordination.TaskCompletion, _ func(coordination.TaskCompletion) error) error {
				deadline, ok := bounded.Deadline()
				require.True(t, ok)
				require.InDelta(t, float64(tc.want), float64(time.Until(deadline)), float64(time.Second))
				return stopped
			})
			worker, err := e.NewTaskReviewRuntime(cfg)
			require.NoError(t, err)
			if tc.observe {
				_, err = worker.Observe(ctx, storage.receipt)
			} else {
				err = worker.Execute(ctx, storage.receipt)
			}
			require.ErrorIs(t, err, stopped)
			require.Zero(t, client.calls)
		})
	}
	for _, timeout := range []time.Duration{-time.Second, 30*time.Minute + time.Nanosecond} {
		_, e, _, _, cfg := reviewRuntimeFixture(t)
		cfg.ExecutionTimeout = timeout
		_, err := e.NewTaskReviewRuntime(cfg)
		require.ErrorIs(t, err, coordination.ErrInvalidReview)
	}
}

// A review run handed off while the engine drains is still in progress:
// Observe reports it pending rather than as a conflict, and Execute's
// error says it was handed off.
func TestTaskReviewRuntimeHandedOffReviewIsPending(t *testing.T) {
	ctx, _, s, client, cfg := reviewRuntimeFixture(t)
	backend, ok := s.Store.(*sqlite.Store)
	require.True(t, ok)
	// The fixture's store hides the lease capabilities; this one keeps them,
	// as a host's real store would.
	leased := &leasedReviewStore{reviewRuntimeStore: s, ContinuationStore: backend, ProgressStore: backend}
	e, err := engine.New(engine.WithStore(leased), engine.WithLLM(client))
	require.NoError(t, err)
	worker, err := e.NewTaskReviewRuntime(cfg)
	require.NoError(t, err)
	require.NoError(t, e.Stop(context.Background()))
	require.ErrorIs(t, worker.Execute(ctx, s.receipt), cortex.ErrRunInterrupted)
	require.Zero(t, client.calls)
	row, err := s.GetRun(ctx, s.receipt.Request.RunID)
	require.NoError(t, err)
	require.Equal(t, run.StateReclaimable, row.State)
	historical := s.receipt
	historical.Fresh = false
	pending, err := worker.Observe(ctx, historical)
	require.NoError(t, err)
	require.Nil(t, pending)
}

type leasedReviewStore struct {
	*reviewRuntimeStore
	run.ContinuationStore
	suspension.ProgressStore
}
