package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func newTestStep(runID id.AgentRunID) *run.Step {
	return &run.Step{
		ID:    id.NewStepID(),
		RunID: runID,
		Index: 0,
		Type:  "generation",
		Input: "original",
	}
}

func newTestToolCall(runID id.AgentRunID, stepID id.StepID) *run.ToolCall {
	return &run.ToolCall{
		ID:       id.NewToolCallID(),
		StepID:   stepID,
		RunID:    runID,
		ToolName: "spy-tool",
	}
}

// TestStepStore_ZeroScopeRejected covers CreateStep and ListSteps against a
// context that carries no scope: both must return cortex.ErrNoScope
// without touching the database, mirroring
// TestCheckpointStore_ZeroScopeRejected. Step rows hold the verbatim LLM
// input and output — the same content class as the conversation data that
// leaked — so these guards close the same hole for steps.
func TestStepStore_ZeroScopeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background() // deliberately no scope
	step := newTestStep(id.NewAgentRunID())

	if err := s.CreateStep(ctx, step); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CreateStep with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.ListSteps(ctx, step.RunID); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("ListSteps with no scope = %v, want ErrNoScope", err)
	}
}

// TestToolCallStore_ZeroScopeRejected is the tool-call sibling of
// TestStepStore_ZeroScopeRejected.
func TestToolCallStore_ZeroScopeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background() // deliberately no scope
	tc := newTestToolCall(id.NewAgentRunID(), id.NewStepID())

	if err := s.CreateToolCall(ctx, tc); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CreateToolCall with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.ListToolCalls(ctx, tc.StepID); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("ListToolCalls with no scope = %v, want ErrNoScope", err)
	}
}

// TestStepStore_CrossScopeReadsRefused proves the scope predicate on
// ListSteps actually filters instead of silently matching everything: two
// steps are created under different top-level scopes for runs that
// otherwise share nothing distinguishing, and listing from one scope must
// return only the matching step. This is the regression shape for the
// gap the scope guard closes: before it, every reader that skipped a
// scoped GetRun could read another tenant's step trail by run ID alone.
func TestStepStore_CrossScopeReadsRefused(t *testing.T) {
	s := newTestStore(t)
	runID := id.NewAgentRunID()

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	stepA := newTestStep(runID)
	if err := s.CreateStep(ctxA, stepA); err != nil {
		t.Fatalf("create step A: %v", err)
	}
	stepB := newTestStep(runID)
	if err := s.CreateStep(ctxB, stepB); err != nil {
		t.Fatalf("create step B: %v", err)
	}

	gotA, err := s.ListSteps(ctxA, runID)
	if err != nil {
		t.Fatalf("list steps A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != stepA.ID {
		t.Fatalf("ListSteps(ctxA) = %d step(s), want exactly stepA (predicate isn't filtering)", len(gotA))
	}
	const wantA = "workspace=ws_a"
	if gotA[0].Scope.Canonical() != wantA {
		t.Errorf("stepA.Scope.Canonical() = %q, want %q", gotA[0].Scope.Canonical(), wantA)
	}

	gotB, err := s.ListSteps(ctxB, runID)
	if err != nil {
		t.Fatalf("list steps B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].ID != stepB.ID {
		t.Fatalf("ListSteps(ctxB) = %d step(s), want exactly stepB", len(gotB))
	}
}

// TestToolCallStore_CrossScopeReadsRefused is the tool-call sibling of
// TestStepStore_CrossScopeReadsRefused.
func TestToolCallStore_CrossScopeReadsRefused(t *testing.T) {
	s := newTestStore(t)
	runID := id.NewAgentRunID()
	stepID := id.NewStepID()

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	tcA := newTestToolCall(runID, stepID)
	if err := s.CreateToolCall(ctxA, tcA); err != nil {
		t.Fatalf("create tool call A: %v", err)
	}
	tcB := newTestToolCall(runID, stepID)
	if err := s.CreateToolCall(ctxB, tcB); err != nil {
		t.Fatalf("create tool call B: %v", err)
	}

	gotA, err := s.ListToolCalls(ctxA, stepID)
	if err != nil {
		t.Fatalf("list tool calls A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != tcA.ID {
		t.Fatalf("ListToolCalls(ctxA) = %d call(s), want exactly tcA (predicate isn't filtering)", len(gotA))
	}
	const wantA = "workspace=ws_a"
	if gotA[0].Scope.Canonical() != wantA {
		t.Errorf("tcA.Scope.Canonical() = %q, want %q", gotA[0].Scope.Canonical(), wantA)
	}

	gotB, err := s.ListToolCalls(ctxB, stepID)
	if err != nil {
		t.Fatalf("list tool calls B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].ID != tcB.ID {
		t.Fatalf("ListToolCalls(ctxB) = %d call(s), want exactly tcB", len(gotB))
	}
}

// TestStepStore_CreateWritesNonNullScopeExtra is the step/tool-call
// sibling of TestRunStore_CreateWritesNonNullScopeExtra: proves
// scopeColumns/stepToModel never hand grove a nil map, which sqlite's
// scope_extra NOT NULL column would reject.
func TestStepStore_CreateWritesNonNullScopeExtra(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))
	runID := id.NewAgentRunID()

	step := newTestStep(runID)
	if err := s.CreateStep(ctx, step); err != nil {
		t.Fatalf("create step: %v", err)
	}
	tc := newTestToolCall(runID, step.ID)
	if err := s.CreateToolCall(ctx, tc); err != nil {
		t.Fatalf("create tool call: %v", err)
	}

	steps, err := s.ListSteps(ctx, runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	const want = "workspace=ws_x/project=proj_y"
	if len(steps) != 1 || steps[0].Scope.Canonical() != want {
		t.Fatalf("step scope = %+v, want exactly one step scoped %q", steps, want)
	}

	toolCalls, err := s.ListToolCalls(ctx, step.ID)
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].Scope.Canonical() != want {
		t.Fatalf("tool call scope = %+v, want exactly one call scoped %q", toolCalls, want)
	}
}
