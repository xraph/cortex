package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/sqlite"
)

type preparedTeamClient struct {
	recordingLLM
	mu       sync.Mutex
	calls    int
	complete func(context.Context, *llm.Request, int) (*llm.Response, error)
}

func (c *preparedTeamClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.complete != nil {
		return c.complete(ctx, req, c.calls)
	}
	return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
}

type preparedTeamFixture struct {
	ctx    context.Context
	s      *sqlite.Store
	e      *engine.Engine
	p      *engine.PreparedOrchestration
	policy orchestration.BudgetPolicy
	client *preparedTeamClient
	cfg    *orchestration.Config
}

func newPreparedTeamFixture(t *testing.T, strategy string) preparedTeamFixture {
	t.Helper()
	return newPreparedTeamFixtureAt(t, strategy, filepath.Join(t.TempDir(), "team.db"))
}

func newPreparedTeamFixtureAt(t *testing.T, strategy, database string) preparedTeamFixture {
	t.Helper()
	ctx := overlayCtx("team-runtime")
	s := openSQLiteStoreForTest(t, database+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	for _, name := range []string{"a", "b", "judge"} {
		ag := &agent.Config{ID: id.NewAgentID(), Name: name, SystemPrompt: "frozen " + name, Model: "team-model"}
		require.NoError(t, s.Create(ctx, ag))
		sid := id.NewSessionID()
		require.NoError(t, s.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID, IsDefault: true}))
		require.NoError(t, s.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "unrelated-secret"}}))
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: strategy, Participants: []orchestration.Participant{{AgentName: "a"}, {AgentName: "b"}}, Settings: orchestration.Settings{Model: "team-model", Rounds: 1, RouterRules: map[string]string{"work": "b"}}}
	if strategy == orchestration.StrategyParallel {
		cfg.Settings.Aggregator = "judge"
	}
	if strategy == orchestration.StrategyDebate {
		cfg.Settings.Judge = "judge"
	}
	require.NoError(t, s.CreateOrchestration(ctx, cfg))
	client := &preparedTeamClient{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client))
	require.NoError(t, err)
	names, err := orchestration.ExecutionRoster(cfg)
	require.NoError(t, err)
	deadline := time.Now().Add(time.Hour)
	budgets := make(map[string]execution.Budget)
	limits := make(map[string]execution.Resources)
	for _, name := range names {
		budgets[name] = execution.Budget{MaxSteps: 2, MaxTokens: 100, Deadline: deadline}
		limits[name] = execution.Resources{Steps: 2, Tokens: 100, FanOut: 1}
	}
	p, err := e.PrepareOrchestrationWithBudgets(ctx, cfg.ID, budgets)
	require.NoError(t, err)
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: p.Digest(), Scope: cortex.ScopeFromContext(ctx), Limits: execution.Resources{Steps: 32, Tokens: 1600, FanOut: 16}, Participants: limits, Deadline: deadline, TokenOnly: true}
	return preparedTeamFixture{ctx: ctx, s: s, e: e, p: p, policy: policy, client: client, cfg: cfg}
}

func TestRunPreparedOrchestrationPinsParticipantsAndIsolatesSessions(t *testing.T) {
	for _, strategy := range []string{orchestration.StrategySequential, orchestration.StrategyParallel, orchestration.StrategyRouter, orchestration.StrategyHierarchical, orchestration.StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			f := newPreparedTeamFixture(t, strategy)
			for _, name := range []string{"a", "b", "judge"} {
				ag, err := f.s.GetByName(f.ctx, name)
				require.NoError(t, err)
				ag.SystemPrompt = "live mutation"
				require.NoError(t, f.s.Update(f.ctx, ag))
			}
			f.cfg.Settings.Model = "live-model"
			require.NoError(t, f.s.UpdateOrchestration(f.ctx, f.cfg))
			seen := make(map[id.SessionID]bool)
			f.client.complete = func(ctx context.Context, req *llm.Request, call int) (*llm.Response, error) {
				if !strings.HasPrefix(req.System, "frozen ") || req.Model != "team-model" {
					t.Errorf("live configuration used: %+v", req)
				}
				if len(req.Messages) != 1 || strings.Contains(req.Messages[0].Content, "unrelated-secret") {
					t.Error("shared conversation leaked")
				}
				p, ok := engine.ExecutionProvenanceFromContext(ctx)
				if !ok || p.Purpose != run.PurposeOptimizationTrial || p.ExperimentID != "experiment" || p.OrchestrationID != f.policy.ExecutionID.String() || p.OrchestrationDigest != f.p.Digest() {
					t.Errorf("team purpose/binding lost: %+v", p)
				}
				key, err := id.Parse(p.OrchestrationReservationID)
				if err != nil {
					t.Error(err)
					return nil, err
				}
				reservation, err := f.s.GetReservation(ctx, key)
				if err != nil {
					t.Error(err)
					return nil, err
				}
				child, err := f.s.GetRun(ctx, reservation.Request.TaskID)
				if err != nil {
					t.Error(err)
					return nil, err
				}
				team, err := f.s.GetOrchestrationRun(ctx, f.policy.ExecutionID)
				if err != nil {
					t.Error(err)
					return nil, err
				}
				if !slices.Contains(team.AgentRunIDs, child.ID) {
					t.Error("child linkage must precede provider dispatch")
					return nil, cortex.ErrRunIncomplete
				}
				invocation, err := f.s.GetOrchestrationInvocation(ctx, team.ID, orchestration.InvocationSlot(ctx))
				if err != nil || invocation.RunID != child.ID || invocation.Intent.AgentName != reservation.Request.OwnerRef {
					t.Errorf("invocation identity missing before provider dispatch: %+v %v", invocation, err)
					return nil, cortex.ErrRunIncomplete
				}
				sess, err := f.s.GetSession(ctx, child.SessionID)
				if err != nil {
					t.Error(err)
					return nil, err
				}
				if sess.IsDefault || seen[sess.ID] {
					t.Error("child session reused")
				}
				seen[sess.ID] = true
				output := "answer"
				if strategy == orchestration.StrategyHierarchical && call == 1 {
					output = `[{"agent":"b","task":"work"}]`
				}
				return &llm.Response{Content: output, FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			r, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy, Purpose: run.PurposeOptimizationTrial, Provenance: run.ExecutionProvenance{ExperimentID: "experiment", EnvironmentReceiptID: "isolated-host-receipt"}})
			require.NoError(t, err)
			require.Equal(t, orchestration.StatusCompleted, r.Status)
			require.Len(t, r.AgentRunIDs, f.client.calls)
			snapshot, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, r.ID)
			require.NoError(t, err)
			require.True(t, snapshot.Closed)
			require.EqualValues(t, 100*f.client.calls, snapshot.Charged.Tokens, "unknown billing must retain capacity")
			_, err = f.e.RunPreparedOrchestration(f.ctx, f.p, "retry", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.Error(t, err)
			require.Equal(t, len(seen), f.client.calls)
		})
	}
}

func TestRunPreparedOrchestrationSharesCapacityAndRetainsFailure(t *testing.T) {
	for _, mode := range []string{"capacity", "provider"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparedTeamFixture(t, orchestration.StrategySequential)
			failure := errors.New("provider failed")
			if mode == "capacity" {
				f.policy.Limits.Tokens = 150
			}
			f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
				if f.client.calls == 2 {
					return nil, failure
				}
				return &llm.Response{Content: "answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			r, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.Error(t, err)
			require.NotNil(t, r)
			require.Equal(t, orchestration.StatusFailed, r.Status)
			want := 2
			if mode == "capacity" {
				want = 1
				require.ErrorIs(t, err, execution.ErrBudgetExceeded)
			}
			require.Equal(t, want, f.client.calls)
			require.Len(t, r.AgentRunIDs, want)
			for _, key := range r.AgentRunIDs {
				_, readErr := f.s.GetRun(f.ctx, key)
				require.NoError(t, readErr)
			}
			snapshot, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, r.ID)
			require.NoError(t, err)
			require.True(t, snapshot.Closed)
			require.EqualValues(t, want*100, snapshot.Charged.Tokens)
		})
	}
}

func TestRunPreparedOrchestrationRejectsInvalidAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "scope", "digest", "participant", "ceiling", "session", "purpose", "money"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparedTeamFixture(t, orchestration.StrategySequential)
			opts := engine.PreparedRunOptions{OrchestrationBudget: &f.policy}
			switch mode {
			case "missing":
				opts.OrchestrationBudget = nil
			case "scope":
				f.policy.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}}
			case "digest":
				f.policy.PreparedDigest = strings.Repeat("a", 64)
			case "participant":
				delete(f.policy.Participants, "b")
			case "ceiling":
				f.policy.Participants["a"] = execution.Resources{Steps: 2, Tokens: 99, FanOut: 1}
			case "session":
				opts.SessionID = id.NewSessionID()
			case "purpose":
				opts.Purpose = "untrusted"
			case "money":
				f.policy.TokenOnly = false
				f.policy.Limits.USDMicros = 100
				f.policy.PricingRevision = "price:1"
			}
			r, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", opts)
			require.Error(t, err)
			require.Nil(t, r)
			require.Zero(t, f.client.calls)
			teams, err := f.s.ListOrchestrationRuns(f.ctx, nil)
			require.NoError(t, err)
			require.Empty(t, teams)
		})
	}
}
