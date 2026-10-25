package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
)

type boundedTeamPreparer interface {
	PrepareOrchestrationWithBudgets(context.Context, id.OrchestrationConfigID, map[string]execution.Budget) (*engine.PreparedOrchestration, error)
}

func TestPrepareOrchestrationFreezesExplicitChildBudgets(t *testing.T) {
	ctx := overlayCtx("bounded-team")
	s := newSQLiteStoreForTest(t)
	for _, name := range []string{"worker", "judge"} {
		if err := s.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, MaxSteps: 4}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: orchestration.StrategyDebate, Participants: []orchestration.Participant{{AgentName: "worker"}}, Settings: orchestration.Settings{Judge: "judge"}}
	if err := s.CreateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatal(err)
	}
	preparer, ok := any(e).(boundedTeamPreparer)
	if !ok {
		t.Fatal("bounded team preparation unavailable")
	}
	deadline := time.Now().Add(time.Hour)
	budgets := map[string]execution.Budget{"worker": {MaxSteps: 2, MaxTokens: 100, Deadline: deadline}, "judge": {MaxSteps: 3, MaxTokens: 200, Deadline: deadline}}
	p, err := preparer.PrepareOrchestrationWithBudgets(ctx, cfg.ID, budgets)
	if err != nil {
		t.Fatal(err)
	}
	budgets["worker"] = execution.Budget{}
	view, err := p.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	for name, child := range view.Participants {
		participant, inspectErr := child.Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		want := 100
		if name == "judge" {
			want = 200
		}
		if participant.Config.Budget.MaxTokens != want || !participant.Config.Budget.Deadline.Equal(deadline) || participant.Config.Budget.MaxSteps < 1 || len(participant.Tools) != 0 {
			t.Fatalf("unbounded participant %s: %+v", name, participant)
		}
	}
}

func TestPrepareOrchestrationBudgetDenials(t *testing.T) {
	for _, mode := range []string{"nil", "missing", "extra", "replaced", "steps", "tokens", "deadline", "tools", "negative", "valid"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("bounded-team-denied")
			s := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Tools: []string{"probe"}}
			if err := s.Create(ctx, ag); err != nil {
				t.Fatal(err)
			}
			cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: ag.Name}}}
			if err := s.CreateOrchestration(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			e, err := engine.New(engine.WithStore(s), engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
				t.Error("preparation invoked a tool")
				return "", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			preparer, ok := any(e).(boundedTeamPreparer)
			if !ok {
				t.Fatal("bounded team preparation unavailable")
			}
			budget := execution.Budget{MaxSteps: 2, MaxTokens: 100, MaxToolCalls: 1, Deadline: time.Now().Add(time.Hour)}
			budgets := map[string]execution.Budget{"worker": budget}
			switch mode {
			case "nil":
				budgets = nil
			case "missing":
				delete(budgets, "worker")
			case "extra":
				budgets["outsider"] = budget
			case "replaced":
				delete(budgets, "worker")
				budgets["outsider"] = budget
			case "steps":
				budget.MaxSteps = 0
				budgets["worker"] = budget
			case "tokens":
				budget.MaxTokens = 0
				budgets["worker"] = budget
			case "deadline":
				budget.Deadline = time.Time{}
				budgets["worker"] = budget
			case "tools":
				budget.MaxToolCalls = 0
				budgets["worker"] = budget
			case "negative":
				budget.MaxToolCalls = -1
				budgets["worker"] = budget
			}
			p, err := preparer.PrepareOrchestrationWithBudgets(ctx, cfg.ID, budgets)
			if mode == "valid" {
				if err != nil || p == nil {
					t.Fatalf("valid bounded tools refused: %v", err)
				}
				view, inspectErr := p.Inspect()
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				child, inspectErr := view.Participants["worker"].Inspect()
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				if child.Config.Budget.MaxToolCalls != 1 || len(child.Tools) != 1 {
					t.Fatalf("tool budget not frozen: %+v", child)
				}
				return
			}
			if !errors.Is(err, cortex.ErrInvalidPrepared) || p != nil {
				t.Fatalf("unbounded policy accepted: %v %v", p, err)
			}
		})
	}
}
