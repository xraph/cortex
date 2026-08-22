// Package storetest is a backend-agnostic conformance suite for
// store.Store implementations. Postgres, sqlite, and mongo each hand-write
// the same scope predicate/upsert logic independently — Conformance exists
// so the contract that logic has to satisfy is defined exactly once and
// run against all three, instead of three parallel hand-maintained test
// files that can silently drift out of sync with each other.
package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// Conformance runs the full scope-isolation contract against any backend.
// newStore must return a freshly migrated, empty store; Conformance calls
// it once per subtest so tests never see another subtest's rows.
func Conformance(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("ZeroScopeRejection", func(t *testing.T) { testZeroScopeRejection(t, newStore) })
	t.Run("CrossScopeTwoRow", func(t *testing.T) { testCrossScopeTwoRow(t, newStore) })
	t.Run("SameIdentifierCrossScope", func(t *testing.T) { testSameIdentifierCrossScope(t, newStore) })
	t.Run("PrefixMatching", func(t *testing.T) { testPrefixMatching(t, newStore) })
	t.Run("ScopeImmutability", func(t *testing.T) { testScopeImmutability(t, newStore) })
	t.Run("ScopeExtraNeverNull", func(t *testing.T) { testScopeExtraNeverNull(t, newStore) })
}

// ──────────────────────────────────────────────────
// Scope / fixture helpers
// ──────────────────────────────────────────────────

// scopeOf builds a cortex.Scope from positional level values, using the
// workspace/project/environment key ordering shared by every backend's
// hand-written scope tests (store/postgres/scope_test.go's ws(),
// store/sqlite/run_scope_test.go's scopeOf()).
func scopeOf(vals ...string) cortex.Scope {
	keys := []string{"workspace", "project", "environment"}
	s := cortex.Scope{}
	for i, v := range vals {
		s.Levels = append(s.Levels, cortex.Level{Key: keys[i], Value: v})
	}
	return s
}

func ctxWithScope(vals ...string) context.Context {
	return cortex.WithScope(context.Background(), scopeOf(vals...))
}

// mustCreateAgent creates an agent under ctx and returns its ID. The agent
// store isn't scope-guarded — Create never rejects a zero scope, and it
// stays app-keyed rather than scope-filtered this phase — but postgres's
// cortex_runs.agent_id and cortex_checkpoints.agent_id are real foreign
// keys, so every run/checkpoint fixture below needs a genuinely persisted
// agent row to reference regardless of backend.
func mustCreateAgent(t *testing.T, s store.Store, ctx context.Context) id.AgentID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	agentID := id.NewAgentID()
	cfg := &agent.Config{
		ID:    agentID,
		Name:  "conformance-" + agentID.String(),
		AppID: "conformance-app",
	}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("fixture: create agent: %v", err)
	}
	return agentID
}

func newRun(agentID id.AgentID) *run.Run {
	return &run.Run{ID: id.NewAgentRunID(), AgentID: agentID, State: run.StateCreated, Input: "input"}
}

// mustCreateRun creates a fresh agent and a run under ctx that references
// it, so the fixture is valid against postgres's real foreign key as well
// as sqlite/mongo's unchecked one. Returns the stored run (with its
// server-assigned scope/timestamps).
func mustCreateRun(t *testing.T, s store.Store, ctx context.Context) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	agentID := mustCreateAgent(t, s, ctx)
	r := newRun(agentID)
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("fixture: create run: %v", err)
	}
	return r
}

func newStep(runID id.AgentRunID) *run.Step {
	return &run.Step{ID: id.NewStepID(), RunID: runID, Index: 0, Type: "generation", Input: "input"}
}

func mustCreateStep(t *testing.T, s store.Store, ctx context.Context, runID id.AgentRunID) *run.Step { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	st := newStep(runID)
	if err := s.CreateStep(ctx, st); err != nil {
		t.Fatalf("fixture: create step: %v", err)
	}
	return st
}

func newToolCall(runID id.AgentRunID, stepID id.StepID) *run.ToolCall {
	return &run.ToolCall{ID: id.NewToolCallID(), StepID: stepID, RunID: runID, ToolName: "conformance-tool"}
}

func mustCreateToolCall(t *testing.T, s store.Store, ctx context.Context, runID id.AgentRunID, stepID id.StepID) *run.ToolCall { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	tc := newToolCall(runID, stepID)
	if err := s.CreateToolCall(ctx, tc); err != nil {
		t.Fatalf("fixture: create tool call: %v", err)
	}
	return tc
}

func newCheckpoint(runID id.AgentRunID, agentID id.AgentID) *checkpoint.Checkpoint {
	return &checkpoint.Checkpoint{
		ID:        id.NewCheckpointID(),
		RunID:     runID,
		AgentID:   agentID,
		Reason:    "needs approval",
		StepIndex: 1,
		State:     "pending",
	}
}

func mustCreateCheckpoint(t *testing.T, s store.Store, ctx context.Context, runID id.AgentRunID, agentID id.AgentID) *checkpoint.Checkpoint { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	cp := newCheckpoint(runID, agentID)
	if err := s.CreateCheckpoint(ctx, cp); err != nil {
		t.Fatalf("fixture: create checkpoint: %v", err)
	}
	return cp
}

// ──────────────────────────────────────────────────
// Zero-scope rejection
// ──────────────────────────────────────────────────

// testZeroScopeRejection covers every scoped read/write method against a
// context that carries no scope at all: each must return cortex.ErrNoScope
// immediately, before touching the database. Fixture IDs here are never
// persisted (the whole point is that these calls short-circuit before any
// FK or row would matter), so plain generated IDs are fine even on
// postgres.
func testZeroScopeRejection(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background() // deliberately no scope

	t.Run("Run", func(t *testing.T) {
		s := newStore(t)
		r := newRun(id.NewAgentID())
		if err := s.CreateRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateRun with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetRun with no scope = %v, want ErrNoScope", err)
		}
		if err := s.UpdateRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("UpdateRun with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListRuns with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.CountRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CountRuns with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Step", func(t *testing.T) {
		s := newStore(t)
		step := newStep(id.NewAgentRunID())
		if err := s.CreateStep(ctx, step); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateStep with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListSteps(ctx, step.RunID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListSteps with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("ToolCall", func(t *testing.T) {
		s := newStore(t)
		tc := newToolCall(id.NewAgentRunID(), id.NewStepID())
		if err := s.CreateToolCall(ctx, tc); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateToolCall with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListToolCalls(ctx, tc.StepID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListToolCalls with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Checkpoint", func(t *testing.T) {
		s := newStore(t)
		cp := newCheckpoint(id.NewAgentRunID(), id.NewAgentID())
		if err := s.CreateCheckpoint(ctx, cp); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateCheckpoint with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetCheckpoint(ctx, cp.ID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetCheckpoint with no scope = %v, want ErrNoScope", err)
		}
		if err := s.Resolve(ctx, cp.ID, checkpoint.Decision{Approved: true}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("Resolve with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListPending(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListPending with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.CountPending(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CountPending with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Conversation", func(t *testing.T) {
		s := newStore(t)
		agentID := id.NewAgentID()
		if err := s.SaveConversation(ctx, agentID, []memory.Message{{Role: "user", Content: "hi"}}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("SaveConversation with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.LoadConversation(ctx, agentID, 0); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("LoadConversation with no scope = %v, want ErrNoScope", err)
		}
		if err := s.ClearConversation(ctx, agentID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ClearConversation with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("WorkingMemory", func(t *testing.T) {
		s := newStore(t)
		runID := id.NewAgentRunID()
		if err := s.SaveWorking(ctx, runID, "k", "v"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("SaveWorking with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.LoadWorking(ctx, runID, "k"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("LoadWorking with no scope = %v, want ErrNoScope", err)
		}
		if err := s.ClearWorking(ctx, runID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ClearWorking with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Summary", func(t *testing.T) {
		s := newStore(t)
		agentID := id.NewAgentID()
		if err := s.SaveSummary(ctx, agentID, "summary text"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("SaveSummary with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.LoadSummaries(ctx, agentID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("LoadSummaries with no scope = %v, want ErrNoScope", err)
		}
	})
}

// ──────────────────────────────────────────────────
// Cross-scope isolation, two-row shape
// ──────────────────────────────────────────────────

// testCrossScopeTwoRow creates rows with distinct identifiers under two
// genuinely different scopes and proves a read from one scope returns only
// that scope's row.
func testCrossScopeTwoRow(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Run", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA := mustCreateRun(t, s, ctxA)
		runB := mustCreateRun(t, s, ctxB)

		got, err := s.ListRuns(ctxA, nil)
		if err != nil {
			t.Fatalf("list runs A: %v", err)
		}
		if len(got) != 1 || got[0].ID != runA.ID {
			t.Fatalf("ListRuns(ctxA) = %d run(s), want exactly runA (predicate isn't filtering)", len(got))
		}
		count, err := s.CountRuns(ctxA, nil)
		if err != nil {
			t.Fatalf("count runs A: %v", err)
		}
		if count != 1 {
			t.Fatalf("CountRuns(ctxA) = %d, want 1", count)
		}

		gotB, err := s.ListRuns(ctxB, nil)
		if err != nil {
			t.Fatalf("list runs B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != runB.ID {
			t.Fatalf("ListRuns(ctxB) = %d run(s), want exactly runB", len(gotB))
		}

		if _, err := s.GetRun(ctxB, runA.ID); !errors.Is(err, cortex.ErrRunNotFound) {
			t.Errorf("GetRun(ctxB, runA.ID) = %v, want ErrRunNotFound", err)
		}
	})

	t.Run("Checkpoint", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA := mustCreateRun(t, s, ctxA)
		runB := mustCreateRun(t, s, ctxB)
		cpA := mustCreateCheckpoint(t, s, ctxA, runA.ID, runA.AgentID)
		cpB := mustCreateCheckpoint(t, s, ctxB, runB.ID, runB.AgentID)

		got, err := s.ListPending(ctxA, nil)
		if err != nil {
			t.Fatalf("list pending A: %v", err)
		}
		if len(got) != 1 || got[0].ID != cpA.ID {
			t.Fatalf("ListPending(ctxA) = %d checkpoint(s), want exactly cpA", len(got))
		}
		count, err := s.CountPending(ctxA, nil)
		if err != nil {
			t.Fatalf("count pending A: %v", err)
		}
		if count != 1 {
			t.Fatalf("CountPending(ctxA) = %d, want 1", count)
		}

		gotB, err := s.ListPending(ctxB, nil)
		if err != nil {
			t.Fatalf("list pending B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != cpB.ID {
			t.Fatalf("ListPending(ctxB) = %d checkpoint(s), want exactly cpB", len(gotB))
		}

		if _, err := s.GetCheckpoint(ctxB, cpA.ID); !errors.Is(err, cortex.ErrCheckpointNotFound) {
			t.Errorf("GetCheckpoint(ctxB, cpA.ID) = %v, want ErrCheckpointNotFound", err)
		}
	})

	// Step_ScopedListSmoke and ToolCall_ScopedListSmoke are deliberately
	// NOT named "Step"/"ToolCall": ListSteps/ListToolCalls take the
	// parent run/step ID as an explicit query parameter, so passing two
	// distinct run/step IDs here would separate the results on that
	// parameter alone, whether or not the scope predicate does anything
	// at all — deleting the scope predicate from either method would
	// still pass this. These two just confirm the basic scoped
	// list-and-round-trip path works end to end; the real cross-scope
	// isolation proof for steps and tool calls (reusing the SAME run/step
	// ID across two scopes, where the scope predicate is the only thing
	// left to separate them) lives in SameIdentifierCrossScope below.
	t.Run("Step_ScopedListSmoke", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA := mustCreateRun(t, s, ctxA)
		runB := mustCreateRun(t, s, ctxB)
		stepA := mustCreateStep(t, s, ctxA, runA.ID)
		stepB := mustCreateStep(t, s, ctxB, runB.ID)

		gotA, err := s.ListSteps(ctxA, runA.ID)
		if err != nil {
			t.Fatalf("list steps A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != stepA.ID {
			t.Fatalf("ListSteps(ctxA) = %d step(s), want exactly stepA", len(gotA))
		}

		gotB, err := s.ListSteps(ctxB, runB.ID)
		if err != nil {
			t.Fatalf("list steps B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != stepB.ID {
			t.Fatalf("ListSteps(ctxB) = %d step(s), want exactly stepB", len(gotB))
		}
	})

	t.Run("ToolCall_ScopedListSmoke", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA := mustCreateRun(t, s, ctxA)
		runB := mustCreateRun(t, s, ctxB)
		stepA := mustCreateStep(t, s, ctxA, runA.ID)
		stepB := mustCreateStep(t, s, ctxB, runB.ID)
		tcA := mustCreateToolCall(t, s, ctxA, runA.ID, stepA.ID)
		tcB := mustCreateToolCall(t, s, ctxB, runB.ID, stepB.ID)

		gotA, err := s.ListToolCalls(ctxA, stepA.ID)
		if err != nil {
			t.Fatalf("list tool calls A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != tcA.ID {
			t.Fatalf("ListToolCalls(ctxA) = %d call(s), want exactly tcA", len(gotA))
		}

		gotB, err := s.ListToolCalls(ctxB, stepB.ID)
		if err != nil {
			t.Fatalf("list tool calls B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != tcB.ID {
			t.Fatalf("ListToolCalls(ctxB) = %d call(s), want exactly tcB", len(gotB))
		}
	})

	t.Run("Conversation", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")
		// One shared agent ID, not two distinct ones. Two distinct IDs
		// would let agent_id alone separate the rows regardless of
		// whether the scope predicate does anything at all — the same
		// "two different IDs prove nothing" flaw that already hit this
		// phase once on working memory, this time on conversation
		// history, the entity the whole scope refactor exists to
		// protect. errors.Is-checkable proof this predicate is
		// load-bearing is in the phase report: deleting it from one
		// backend's LoadConversation makes this subtest fail.
		agentID := id.NewAgentID()

		if err := s.SaveConversation(ctxA, agentID, []memory.Message{{Role: "user", Content: "from-a"}}); err != nil {
			t.Fatalf("save conversation A: %v", err)
		}
		if err := s.SaveConversation(ctxB, agentID, []memory.Message{{Role: "user", Content: "from-b"}}); err != nil {
			t.Fatalf("save conversation B: %v", err)
		}

		gotA, err := s.LoadConversation(ctxA, agentID, 0)
		if err != nil {
			t.Fatalf("load conversation A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].Content != "from-a" {
			t.Fatalf("LoadConversation(ctxA, agentID) = %v, want exactly [%q] (scope B's message must not be visible to scope A)", gotA, "from-a")
		}

		gotB, err := s.LoadConversation(ctxB, agentID, 0)
		if err != nil {
			t.Fatalf("load conversation B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].Content != "from-b" {
			t.Fatalf("LoadConversation(ctxB, agentID) = %v, want exactly [%q] (scope A's message must not be visible to scope B)", gotB, "from-b")
		}

		// ClearConversation from B's own scope must only ever touch B's
		// own rows: scope A's history must survive, and B's own history
		// must actually be gone — checking only the first half would let
		// a scope-mismatched no-op on B (i.e. ClearConversation doing
		// nothing at all) pass silently, since a no-op also leaves A
		// untouched. This was previously untested entirely —
		// ClearConversation only appeared in ZeroScopeRejection.
		if err = s.ClearConversation(ctxB, agentID); err != nil {
			t.Fatalf("clear conversation (ctxB): %v", err)
		}
		stillThere, err := s.LoadConversation(ctxA, agentID, 0)
		if err != nil {
			t.Fatalf("reload conversation A after ctxB clear: %v", err)
		}
		if len(stillThere) != 1 || stillThere[0].Content != "from-a" {
			t.Fatalf("conversation A after ctxB's ClearConversation = %v, want unchanged [%q] (must not delete another scope's history)", stillThere, "from-a")
		}
		bGone, err := s.LoadConversation(ctxB, agentID, 0)
		if err != nil {
			t.Fatalf("reload conversation B after ctxB clear: %v", err)
		}
		if len(bGone) != 0 {
			t.Fatalf("conversation B after its own ClearConversation = %v, want empty (clear must actually delete B's own rows, not no-op)", bGone)
		}

		// Clearing from the correct scope removes that scope's own rows.
		if err = s.ClearConversation(ctxA, agentID); err != nil {
			t.Fatalf("clear conversation (ctxA): %v", err)
		}
		afterClear, err := s.LoadConversation(ctxA, agentID, 0)
		if err != nil {
			t.Fatalf("reload conversation A after ctxA clear: %v", err)
		}
		if len(afterClear) != 0 {
			t.Fatalf("conversation A after same-scope ClearConversation = %v, want empty", afterClear)
		}
	})

	t.Run("WorkingMemory", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")
		runA := id.NewAgentRunID()
		runB := id.NewAgentRunID()

		if err := s.SaveWorking(ctxA, runA, "k", "value-a"); err != nil {
			t.Fatalf("save working A: %v", err)
		}
		if err := s.SaveWorking(ctxB, runB, "k", "value-b"); err != nil {
			t.Fatalf("save working B: %v", err)
		}

		gotA, err := s.LoadWorking(ctxA, runA, "k")
		if err != nil {
			t.Fatalf("load working A: %v", err)
		}
		if gotA != "value-a" {
			t.Fatalf("LoadWorking(ctxA, runA) = %v, want %q", gotA, "value-a")
		}

		gotB, err := s.LoadWorking(ctxB, runB, "k")
		if err != nil {
			t.Fatalf("load working B: %v", err)
		}
		if gotB != "value-b" {
			t.Fatalf("LoadWorking(ctxB, runB) = %v, want %q", gotB, "value-b")
		}

		if _, err := s.LoadWorking(ctxB, runA, "k"); !errors.Is(err, cortex.ErrWorkingMemoryNotFound) {
			t.Errorf("LoadWorking(ctxB, runA) = %v, want ErrWorkingMemoryNotFound (cross-scope read must not see runA's value)", err)
		}
	})

	t.Run("Summary", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")
		// Shared agent ID, same reasoning as Conversation above: two
		// distinct IDs would let agent_id alone separate the rows and
		// never exercise the scope predicate at all.
		agentID := id.NewAgentID()

		if err := s.SaveSummary(ctxA, agentID, "summary-a"); err != nil {
			t.Fatalf("save summary A: %v", err)
		}
		if err := s.SaveSummary(ctxB, agentID, "summary-b"); err != nil {
			t.Fatalf("save summary B: %v", err)
		}

		gotA, err := s.LoadSummaries(ctxA, agentID)
		if err != nil {
			t.Fatalf("load summaries A: %v", err)
		}
		if len(gotA) != 1 || gotA[0] != "summary-a" {
			t.Fatalf("LoadSummaries(ctxA, agentID) = %v, want exactly [%q] (scope B's summary must not be visible to scope A)", gotA, "summary-a")
		}

		gotB, err := s.LoadSummaries(ctxB, agentID)
		if err != nil {
			t.Fatalf("load summaries B: %v", err)
		}
		if len(gotB) != 1 || gotB[0] != "summary-b" {
			t.Fatalf("LoadSummaries(ctxB, agentID) = %v, want exactly [%q] (scope A's summary must not be visible to scope B)", gotB, "summary-b")
		}
	})
}

// ──────────────────────────────────────────────────
// Same-identifier cross-scope isolation
// ──────────────────────────────────────────────────

// testSameIdentifierCrossScope is the shape that caught the real bug this
// whole phase exists to close: a run ID (and, by the same logic, a step ID
// or checkpoint's run ID) is a bearer capability, not an isolation
// boundary. A caller in scope B who merely learns scope A's run ID must
// never be able to read, overwrite, or delete scope A's data by reusing
// that identifier under its own scope.
func testSameIdentifierCrossScope(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("WorkingMemory", func(t *testing.T) {
		s := newStore(t)
		runID := id.NewAgentRunID() // one run ID, shared across both scopes below
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		if err := s.SaveWorking(ctxA, runID, "k", "value-a"); err != nil {
			t.Fatalf("save working A: %v", err)
		}
		// The dangerous write: scope B saves under the exact same run ID
		// and key scope A just used. Before the scope-aware unique index
		// existed, this silently overwrote A's row instead of creating a
		// separate one.
		if err := s.SaveWorking(ctxB, runID, "k", "value-b"); err != nil {
			t.Fatalf("save working B: %v", err)
		}

		gotA, err := s.LoadWorking(ctxA, runID, "k")
		if err != nil {
			t.Fatalf("load working A: %v", err)
		}
		if gotA != "value-a" {
			t.Fatalf("LoadWorking(ctxA, runID) = %v, want %q (scope B's write must not clobber scope A's row)", gotA, "value-a")
		}

		gotB, err := s.LoadWorking(ctxB, runID, "k")
		if err != nil {
			t.Fatalf("load working B: %v", err)
		}
		if gotB != "value-b" {
			t.Fatalf("LoadWorking(ctxB, runID) = %v, want %q", gotB, "value-b")
		}

		// Clearing runID's working memory from ctxB must only ever touch
		// B's own row: A's value must still be readable afterward.
		if err = s.ClearWorking(ctxB, runID); err != nil {
			t.Fatalf("clear working (ctxB): %v", err)
		}
		stillThere, err := s.LoadWorking(ctxA, runID, "k")
		if err != nil {
			t.Fatalf("reload working A after ctxB clear: %v", err)
		}
		if stillThere != "value-a" {
			t.Fatalf("value after ctxB's ClearWorking = %v, want %q (must not delete another scope's row)", stillThere, "value-a")
		}

		if err := s.ClearWorking(ctxA, runID); err != nil {
			t.Fatalf("clear working (ctxA): %v", err)
		}
		if _, err := s.LoadWorking(ctxA, runID, "k"); !errors.Is(err, cortex.ErrWorkingMemoryNotFound) {
			t.Errorf("LoadWorking(ctxA) after same-scope ClearWorking = %v, want ErrWorkingMemoryNotFound (row should be gone)", err)
		}
	})

	t.Run("Step", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		// The run itself has to actually exist (postgres FKs
		// cortex_steps.run_id -> cortex_runs.id), so it's created once
		// under ctxA; scope B then reuses that run's ID purely as a
		// bearer capability, the same way a real cross-tenant caller
		// would have learned it from a log line or a client response.
		r := mustCreateRun(t, s, ctxA)
		stepA := mustCreateStep(t, s, ctxA, r.ID)
		stepB := mustCreateStep(t, s, ctxB, r.ID)

		gotA, err := s.ListSteps(ctxA, r.ID)
		if err != nil {
			t.Fatalf("list steps A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != stepA.ID {
			t.Fatalf("ListSteps(ctxA, sharedRunID) = %d step(s), want exactly stepA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListSteps(ctxB, r.ID)
		if err != nil {
			t.Fatalf("list steps B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != stepB.ID {
			t.Fatalf("ListSteps(ctxB, sharedRunID) = %d step(s), want exactly stepB", len(gotB))
		}
	})

	t.Run("ToolCall", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		r := mustCreateRun(t, s, ctxA)
		step := mustCreateStep(t, s, ctxA, r.ID)
		// Scope B reuses both A's run ID and A's step ID.
		tcA := mustCreateToolCall(t, s, ctxA, r.ID, step.ID)
		tcB := mustCreateToolCall(t, s, ctxB, r.ID, step.ID)

		gotA, err := s.ListToolCalls(ctxA, step.ID)
		if err != nil {
			t.Fatalf("list tool calls A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != tcA.ID {
			t.Fatalf("ListToolCalls(ctxA, sharedStepID) = %d call(s), want exactly tcA", len(gotA))
		}

		gotB, err := s.ListToolCalls(ctxB, step.ID)
		if err != nil {
			t.Fatalf("list tool calls B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != tcB.ID {
			t.Fatalf("ListToolCalls(ctxB, sharedStepID) = %d call(s), want exactly tcB", len(gotB))
		}
	})

	t.Run("Checkpoint", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		r := mustCreateRun(t, s, ctxA)
		// Scope B creates its own checkpoint but reuses A's run ID — the
		// same bearer-capability shape as the working-memory regression,
		// applied to checkpoints. sqlite's pre-existing cross-scope
		// checkpoint test used two DIFFERENT run IDs, which never
		// exercises this path.
		cpA := mustCreateCheckpoint(t, s, ctxA, r.ID, r.AgentID)
		cpB := mustCreateCheckpoint(t, s, ctxB, r.ID, r.AgentID)

		if _, err := s.GetCheckpoint(ctxB, cpA.ID); !errors.Is(err, cortex.ErrCheckpointNotFound) {
			t.Errorf("GetCheckpoint(ctxB, cpA.ID) = %v, want ErrCheckpointNotFound", err)
		}
		if _, err := s.GetCheckpoint(ctxA, cpA.ID); err != nil {
			t.Errorf("GetCheckpoint(ctxA, cpA.ID) = %v, want no error", err)
		}

		// A RunID filter naming the shared run must still only surface
		// each scope's own checkpoint, not the other scope's row that
		// happens to share the same run_id.
		gotA, err := s.ListPending(ctxA, &checkpoint.ListFilter{RunID: r.ID.String()})
		if err != nil {
			t.Fatalf("list pending A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != cpA.ID {
			t.Fatalf("ListPending(ctxA, RunID=shared) = %d checkpoint(s), want exactly cpA", len(gotA))
		}
		gotB, err := s.ListPending(ctxB, &checkpoint.ListFilter{RunID: r.ID.String()})
		if err != nil {
			t.Fatalf("list pending B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != cpB.ID {
			t.Fatalf("ListPending(ctxB, RunID=shared) = %d checkpoint(s), want exactly cpB", len(gotB))
		}

		// Resolving from the wrong scope must refuse and leave the other
		// scope's checkpoint untouched, even though both share a run_id.
		if err = s.Resolve(ctxA, cpB.ID, checkpoint.Decision{Approved: true}); !errors.Is(err, cortex.ErrCheckpointNotFound) {
			t.Errorf("Resolve(ctxA, cpB.ID) = %v, want ErrCheckpointNotFound", err)
		}
		stillPending, err := s.GetCheckpoint(ctxB, cpB.ID)
		if err != nil {
			t.Fatalf("reload cpB: %v", err)
		}
		if stillPending.State != "pending" {
			t.Errorf("cpB.State after cross-scope Resolve = %q, want %q (must not resolve)", stillPending.State, "pending")
		}
		if err := s.Resolve(ctxB, cpB.ID, checkpoint.Decision{Approved: true}); err != nil {
			t.Fatalf("Resolve(ctxB, cpB.ID): %v", err)
		}
	})
}

// ──────────────────────────────────────────────────
// Prefix matching
// ──────────────────────────────────────────────────

// testPrefixMatching proves a broader (workspace-only) scope filter
// matches rows stored at a narrower (workspace+project) scope, while a
// completely different workspace matches nothing.
func testPrefixMatching(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Run", func(t *testing.T) {
		s := newStore(t)
		r := mustCreateRun(t, s, ctxWithScope("ws_x", "p1"))

		broad := ctxWithScope("ws_x")
		got, err := s.ListRuns(broad, nil)
		if err != nil {
			t.Fatalf("list runs (broad): %v", err)
		}
		if !containsRunID(got, r.ID) {
			t.Errorf("ListRuns({workspace=ws_x}) didn't return a run scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListRuns(other, nil)
		if err != nil {
			t.Fatalf("list runs (other workspace): %v", err)
		}
		if containsRunID(gotOther, r.ID) {
			t.Errorf("ListRuns({workspace=ws_y}) incorrectly returned a run scoped to {workspace=ws_x, project=p1}")
		}
	})

	t.Run("Checkpoint", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		r := mustCreateRun(t, s, createCtx)
		cp := mustCreateCheckpoint(t, s, createCtx, r.ID, r.AgentID)

		broad := ctxWithScope("ws_x")
		got, err := s.ListPending(broad, nil)
		if err != nil {
			t.Fatalf("list pending (broad): %v", err)
		}
		if !containsCheckpointID(got, cp.ID) {
			t.Errorf("ListPending({workspace=ws_x}) didn't return a checkpoint scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListPending(other, nil)
		if err != nil {
			t.Fatalf("list pending (other workspace): %v", err)
		}
		if containsCheckpointID(gotOther, cp.ID) {
			t.Errorf("ListPending({workspace=ws_y}) incorrectly returned a checkpoint scoped to {workspace=ws_x, project=p1}")
		}
	})
}

func containsRunID(rs []*run.Run, want id.AgentRunID) bool {
	for _, r := range rs {
		if r.ID == want {
			return true
		}
	}
	return false
}

func containsCheckpointID(cps []*checkpoint.Checkpoint, want id.CheckpointID) bool {
	for _, cp := range cps {
		if cp.ID == want {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────
// Scope immutability
// ──────────────────────────────────────────────────

// testScopeImmutability updates a row from a broader-but-still-matching
// scope and asserts the row's own narrower stored scope survives the
// write untouched.
func testScopeImmutability(t *testing.T, newStore func(t *testing.T) store.Store) {
	const want = "workspace=ws_x/project=p1"

	t.Run("Run", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		r := mustCreateRun(t, s, createCtx)

		loaded, err := s.GetRun(createCtx, r.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		loaded.Input = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateRun(updateCtx, loaded); err != nil {
			t.Fatalf("update run: %v", err)
		}

		reloaded, err := s.GetRun(createCtx, r.ID)
		if err != nil {
			t.Fatalf("reload run: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Input != "mutated" {
			t.Errorf("Input after update = %q, want %q", reloaded.Input, "mutated")
		}
	})

	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		agentID := mustCreateAgent(t, s, createCtx)

		loaded, err := s.Get(context.Background(), agentID)
		if err != nil {
			t.Fatalf("get agent: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		// Agent Update takes no scope predicate at all this phase — a
		// plain background context proves nothing about the update path
		// can put the row's scope back even if it wanted to.
		if err = s.Update(context.Background(), loaded); err != nil {
			t.Fatalf("update agent: %v", err)
		}

		reloaded, err := s.Get(context.Background(), agentID)
		if err != nil {
			t.Fatalf("reload agent: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q", reloaded.Description, "mutated")
		}
	})
}

// ──────────────────────────────────────────────────
// scope_extra is never NULL
// ──────────────────────────────────────────────────

// testScopeExtraNeverNull creates rows under a scope with no overflow
// levels (1-3 levels: everything lands in the indexed columns, leaving the
// extra map empty) and proves the create still round-trips. The regression
// this guards is grove writing a nil Go map as SQL NULL against a NOT NULL
// scope_extra column — a live hazard whenever the map is empty rather than
// explicitly initialized before being handed to the model.
func testScopeExtraNeverNull(t *testing.T, newStore func(t *testing.T) store.Store) {
	const want = "workspace=ws_x"
	ctx := ctxWithScope("ws_x")

	t.Run("Run", func(t *testing.T) {
		s := newStore(t)
		r := mustCreateRun(t, s, ctx)
		got, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("get run after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Checkpoint", func(t *testing.T) {
		s := newStore(t)
		r := mustCreateRun(t, s, ctx)
		cp := mustCreateCheckpoint(t, s, ctx, r.ID, r.AgentID)
		got, err := s.GetCheckpoint(ctx, cp.ID)
		if err != nil {
			t.Fatalf("get checkpoint after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Step", func(t *testing.T) {
		s := newStore(t)
		r := mustCreateRun(t, s, ctx)
		st := mustCreateStep(t, s, ctx, r.ID)
		got, err := s.ListSteps(ctx, r.ID)
		if err != nil {
			t.Fatalf("list steps after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if len(got) != 1 || got[0].ID != st.ID || got[0].Scope.Canonical() != want {
			t.Fatalf("steps = %+v, want exactly one step %s scoped %q", got, st.ID, want)
		}
	})

	t.Run("ToolCall", func(t *testing.T) {
		s := newStore(t)
		r := mustCreateRun(t, s, ctx)
		st := mustCreateStep(t, s, ctx, r.ID)
		tc := mustCreateToolCall(t, s, ctx, r.ID, st.ID)
		got, err := s.ListToolCalls(ctx, st.ID)
		if err != nil {
			t.Fatalf("list tool calls after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if len(got) != 1 || got[0].ID != tc.ID || got[0].Scope.Canonical() != want {
			t.Fatalf("tool calls = %+v, want exactly one call %s scoped %q", got, tc.ID, want)
		}
	})

	t.Run("Conversation", func(t *testing.T) {
		s := newStore(t)
		agentID := id.NewAgentID()
		if err := s.SaveConversation(ctx, agentID, []memory.Message{{Role: "user", Content: "hello"}}); err != nil {
			t.Fatalf("save conversation with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		got, err := s.LoadConversation(ctx, agentID, 0)
		if err != nil {
			t.Fatalf("load conversation: %v", err)
		}
		if len(got) != 1 || got[0].Content != "hello" {
			t.Fatalf("conversation = %+v, want exactly one message %q", got, "hello")
		}
	})

	t.Run("WorkingMemory", func(t *testing.T) {
		s := newStore(t)
		runID := id.NewAgentRunID()
		if err := s.SaveWorking(ctx, runID, "k", "v"); err != nil {
			t.Fatalf("save working memory with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		got, err := s.LoadWorking(ctx, runID, "k")
		if err != nil {
			t.Fatalf("load working memory: %v", err)
		}
		if got != "v" {
			t.Errorf("LoadWorking() = %v, want %q", got, "v")
		}
	})

	t.Run("Summary", func(t *testing.T) {
		s := newStore(t)
		agentID := id.NewAgentID()
		if err := s.SaveSummary(ctx, agentID, "summary text"); err != nil {
			t.Fatalf("save summary with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		got, err := s.LoadSummaries(ctx, agentID)
		if err != nil {
			t.Fatalf("load summaries: %v", err)
		}
		if len(got) != 1 || got[0] != "summary text" {
			t.Fatalf("summaries = %+v, want exactly one entry %q", got, "summary text")
		}
	})

	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		agentID := mustCreateAgent(t, s, ctx)
		got, err := s.Get(context.Background(), agentID)
		if err != nil {
			t.Fatalf("get agent after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})
}
