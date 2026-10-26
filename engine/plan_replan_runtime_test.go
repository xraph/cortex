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
	"github.com/xraph/cortex/verification"
)

type replanInput func(context.Context, coordination.PlanReplanRequest) (*engine.PreparedAgent, string, error)

func (f replanInput) ResolvePlanReplanInput(ctx context.Context, r coordination.PlanReplanRequest) (*engine.PreparedAgent, string, error) {
	return f(ctx, r)
}

type replanAuthority struct {
	deny  bool
	calls int
	held  bool
}

func (a *replanAuthority) WithPlanReplanFence(_ context.Context, c coordination.PlanReplanControl, next func(coordination.PlanReplanControl) error) error {
	a.held = true
	defer func() { a.held = false }()
	return next(c)
}
func (a *replanAuthority) AuthorizePlanReplan(context.Context, coordination.PlanReplanControl) error {
	a.calls++
	if a.deny {
		return errors.New("revoked")
	}
	return nil
}

type replanClient struct {
	complete func(context.Context, *llm.Request) (*llm.Response, error)
}

func (c replanClient) Complete(ctx context.Context, r *llm.Request) (*llm.Response, error) {
	return c.complete(ctx, r)
}
func (replanClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errNoStream
}

func TestPlanReplanRuntimeChargesBeforeModelAndNeverRepeatsAdmission(t *testing.T) {
	ctx := overlayCtx("replan-runtime")
	backend := newSQLiteStoreForTest(t)
	scope := cortex.ScopeFromContext(ctx)
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 500, MaxToolCalls: 1, Deadline: time.Now().Add(time.Hour)}
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked report", Criteria: []verification.Criterion{{ID: "checked", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	p, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Actor: "host", Reason: "requested", Tasks: []planning.Task{{ID: id.New(planning.PrefixTask), Instruction: "original approach", Owner: "worker", Criteria: []string{"checked"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 4096}}}}, planning.Limits{})
	require.NoError(t, err)
	plans := planning.Store(backend)
	require.NoError(t, plans.CreatePlan(ctx, p))
	budgets := execution.BudgetStore(backend)
	require.NoError(t, budgets.CreateReservationPolicy(ctx, execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: scope, Limits: execution.Resources{Steps: 5, Tokens: 500, ToolCalls: 1, FanOut: 5, Replans: 2}, Deadline: budget.Deadline, TokenOnly: true}))
	authority := new(replanAuthority)
	calls := 0
	next, err := planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	next.Tasks[0].Instruction = "bounded revised approach"
	output, err := json.Marshal(next)
	require.NoError(t, err)
	client := replanClient{complete: func(ctx context.Context, r *llm.Request) (*llm.Response, error) {
		calls++
		require.True(t, authority.held)
		require.Empty(t, r.Tools)
		account, readErr := budgets.GetBudgetSnapshot(ctx, p.ID)
		require.NoError(t, readErr)
		require.EqualValues(t, 1, account.Charged.Replans)
		return &llm.Response{Content: string(output), FinishReason: "stop"}, nil
	}}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "replanner"}
	require.NoError(t, backend.Create(ctx, ag))
	runBudget := execution.Budget{MaxSteps: 1, MaxTokens: 100, Deadline: budget.Deadline}
	prepared, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{ToolsRestricted: true, Tools: []string{}, Budget: &runBudget})
	require.NoError(t, err)
	runtime, err := e.NewPlanReplanRuntime(replanInput(func(context.Context, coordination.PlanReplanRequest) (*engine.PreparedAgent, string, error) {
		return prepared, "input", nil
	}))
	require.NoError(t, err)
	storage := coordination.PlanReplanStore(backend)
	controller, err := coordination.NewPlanReplanner(storage, authority, authority, runtime)
	require.NoError(t, err)
	account, err := budgets.GetBudgetSnapshot(ctx, p.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	attempt := id.New(coordination.PrefixPlanReplan)
	req := coordination.PlanReplanRequest{SchemaVersion: 1, ID: attempt, Progress: planning.ProgressSnapshot{SchemaVersion: 1, Plan: p, At: now, StartedAt: now.Add(-time.Hour), LastProgressAt: now.Add(-time.Hour), Account: account}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: scope, TaskID: p.Tasks[0].ID, OwnerRef: attempt.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 100, FanOut: 1, Replans: 1}, Deadline: budget.Deadline}, RunID: id.NewAgentRunID(), SessionID: id.NewSessionID(), AgentID: ag.ID, PreparedDigest: prepared.Digest(), InputDigest: evidence.Digest([]byte("input")), Actor: "host", Reason: "bounded proposal"}
	admitted, err := controller.Execute(ctx, req)
	require.NoError(t, err)
	require.True(t, admitted.Fresh)
	require.Equal(t, 1, calls)
	replay, err := controller.Execute(ctx, req)
	require.NoError(t, err)
	require.False(t, replay.Fresh)
	require.Equal(t, 1, calls)
	require.Error(t, runtime.ExecutePlanReplan(ctx, admitted))
	require.Equal(t, 1, calls, "even a copied Fresh receipt cannot create the reserved run twice")
	outcome, err := controller.Observe(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
	require.NoError(t, err)
	require.NotNil(t, outcome)
	require.NotNil(t, outcome.Proposed)
	committed, err := controller.Commit(ctx, coordination.PlanReplanControl{Request: req, Operation: "commit"})
	require.NoError(t, err)
	require.EqualValues(t, 2, committed.Revision)
	authority.deny = true
	_, err = controller.Execute(ctx, req)
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
