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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/trait"
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

	// ClaimSuspension is the one store method whose whole reason for
	// existing is what happens when two callers race it, so it gets its
	// own group rather than riding along in the six scope groups. It runs
	// against all three backends because the three implementations are
	// genuinely different -- a conditional UPDATE with a rowcount check
	// inside a transaction on postgres and sqlite, a FindOneAndUpdate on
	// mongo -- and proving the sqlite one says nothing about the mongo
	// one, which is the least like the others.
	t.Run("ClaimSuspensionConcurrency", func(t *testing.T) { testClaimSuspensionConcurrency(t, newStore) })

	// Expiry is part of the claim's own condition rather than a check the
	// caller runs afterwards, so it is the store contract that has to
	// prove it, and it has to prove it on each backend: postgres and
	// sqlite fold the deadline into the UPDATE's predicate, mongo checks
	// it in front of the FindOneAndUpdate because it has no cross-
	// collection filter to fold it into.
	t.Run("ClaimSuspensionExpiry", func(t *testing.T) { testClaimSuspensionExpiry(t, newStore) })
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

// mustCreateAgent creates an agent under ctx and returns its ID. The
// helper persists a real agent row — not a throwaway fixture — because
// postgres's cortex_runs.agent_id and cortex_checkpoints.agent_id are
// genuine foreign keys, so every run/checkpoint fixture below needs a row
// that actually exists regardless of backend. It requires a scoped
// context like every other create: the agent store is fully scope-guarded
// now (Create/Get/GetByName/Update/Delete/List/CountAgents all reject a
// zero scope and filter on it), the same as runs, checkpoints, steps,
// tool calls, and working memory.
func mustCreateAgent(t *testing.T, s store.Store, ctx context.Context, name string) id.AgentID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	agentID := id.NewAgentID()
	if name == "" {
		name = "conformance-" + agentID.String()
	}
	cfg := &agent.Config{
		ID:   agentID,
		Name: name,
	}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("fixture: create agent: %v", err)
	}
	return agentID
}

// mustCreateSkill creates a skill under ctx and returns its ID. It
// requires a scoped context like every other create: the skill store is
// fully scope-guarded now, the same as agents.
func mustCreateSkill(t *testing.T, s store.Store, ctx context.Context, name string) id.SkillID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	skillID := id.NewSkillID()
	if name == "" {
		name = "conformance-" + skillID.String()
	}
	sk := &skill.Skill{
		ID:   skillID,
		Name: name,
	}
	if err := s.CreateSkill(ctx, sk); err != nil {
		t.Fatalf("fixture: create skill: %v", err)
	}
	return skillID
}

// mustCreateTrait creates a trait under ctx and returns its ID. It
// requires a scoped context like every other create: the trait store is
// fully scope-guarded now, the same as agents.
func mustCreateTrait(t *testing.T, s store.Store, ctx context.Context, name string) id.TraitID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	traitID := id.NewTraitID()
	if name == "" {
		name = "conformance-" + traitID.String()
	}
	tr := &trait.Trait{
		ID:   traitID,
		Name: name,
	}
	if err := s.CreateTrait(ctx, tr); err != nil {
		t.Fatalf("fixture: create trait: %v", err)
	}
	return traitID
}

// mustCreateBehavior creates a behavior under ctx and returns its ID. It
// requires a scoped context like every other create: the behavior store
// is fully scope-guarded now, the same as agents.
func mustCreateBehavior(t *testing.T, s store.Store, ctx context.Context, name string) id.BehaviorID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	behaviorID := id.NewBehaviorID()
	if name == "" {
		name = "conformance-" + behaviorID.String()
	}
	b := &behavior.Behavior{
		ID:   behaviorID,
		Name: name,
	}
	if err := s.CreateBehavior(ctx, b); err != nil {
		t.Fatalf("fixture: create behavior: %v", err)
	}
	return behaviorID
}

// mustCreatePersona creates a persona under ctx and returns its ID. It
// requires a scoped context like every other create: the persona store is
// fully scope-guarded now, the same as agents.
func mustCreatePersona(t *testing.T, s store.Store, ctx context.Context, name string) id.PersonaID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	personaID := id.NewPersonaID()
	if name == "" {
		name = "conformance-" + personaID.String()
	}
	p := &persona.Persona{
		ID:   personaID,
		Name: name,
	}
	if err := s.CreatePersona(ctx, p); err != nil {
		t.Fatalf("fixture: create persona: %v", err)
	}
	return personaID
}

// mustCreateSession creates a session under ctx and returns its ID. It
// takes agentID rather than creating its own agent, the same way
// mustCreateCheckpoint takes a runID: the caller decides whether that
// agent is shared across scopes, which several Session subtests below
// depend on.
func mustCreateSession(t *testing.T, s store.Store, ctx context.Context, agentID id.AgentID, title string) id.SessionID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	sid := id.NewSessionID()
	if err := s.CreateSession(ctx, &session.Session{ID: sid, AgentID: agentID, Title: title}); err != nil {
		t.Fatalf("fixture: create session: %v", err)
	}
	return sid
}

// mustCreateOrchestration creates an orchestration config under ctx and
// returns its ID.
func mustCreateOrchestration(t *testing.T, s store.Store, ctx context.Context, name string) id.OrchestrationConfigID { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	orchID := id.NewOrchestrationConfigID()
	if name == "" {
		name = "conformance-" + orchID.String()
	}
	c := &orchestration.Config{
		ID:   orchID,
		Name: name,
	}
	if err := s.CreateOrchestration(ctx, c); err != nil {
		t.Fatalf("fixture: create orchestration: %v", err)
	}
	return orchID
}

// newOrchestrationRun builds an unpersisted orchestration run fixture.
// ConfigID is deliberately left empty: cortex_orchestration_runs.config_id
// carries no foreign key on any backend (unlike cortex_runs.agent_id), and
// Run.ConfigID's own doc comment says empty means "programmatic run", so a
// bare fixture needs no config to exist first.
func newOrchestrationRun() *orchestration.Run {
	return &orchestration.Run{
		ID:       id.NewOrchestrationID(),
		Strategy: orchestration.StrategySequential,
		Status:   orchestration.StatusRunning,
		Input:    "input",
	}
}

// mustCreateOrchestrationRun creates an orchestration run under ctx and
// returns the stored run (with its server-assigned scope/timestamps). It
// requires a scoped context like every other create: the orchestration
// run store is fully scope-guarded now, the same as agent runs.
func mustCreateOrchestrationRun(t *testing.T, s store.Store, ctx context.Context) *orchestration.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	r := newOrchestrationRun()
	if err := s.CreateOrchestrationRun(ctx, r); err != nil {
		t.Fatalf("fixture: create orchestration run: %v", err)
	}
	return r
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
	agentID := mustCreateAgent(t, s, ctx, "")
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

// newSuspension builds an unpersisted suspension fixture for runID. The
// continuation carries real content -- a message, a system prompt, a step
// index, a token count -- so a backend that silently drops or mangles the
// JSON/BSON round-trip fails a read-back assertion instead of passing on
// two empty structs comparing equal.
func newSuspension(runID id.AgentRunID) *suspension.Suspension {
	return &suspension.Suspension{
		ID:     id.NewSuspensionID(),
		RunID:  runID,
		Reason: suspension.ReasonApproval,
		Pending: []suspension.PendingCall{
			{ID: "call_conformance", Name: "conformance-tool", Arguments: `{"query":"x"}`},
		},
		Cont: suspension.Continuation{
			Messages:        []llm.Message{{Role: "user", Content: "hello"}},
			SystemPrompt:    "you are a conformance fixture",
			StepIndex:       2,
			TokensUsed:      17,
			NewMessagesFrom: 1,
			SessionID:       id.NewSessionID(),
		},
	}
}

// mustPauseRun creates a run under ctx and moves it to paused, which is
// the only state ClaimSuspension will transition out of.
func mustPauseRun(t *testing.T, s store.Store, ctx context.Context) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	r := mustCreateRun(t, s, ctx)
	r.State = run.StatePaused
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("fixture: pause run: %v", err)
	}
	return r
}

// mustSuspendRun creates a paused run under ctx and the one suspension
// that belongs to it, which is the fixture every claim assertion needs.
// expiresAt is applied verbatim, so a caller can hand it a past deadline
// to make the row visible to ListExpired or nil to keep it out.
func mustSuspendRun(t *testing.T, s store.Store, ctx context.Context, expiresAt *time.Time) (*run.Run, *suspension.Suspension) { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	r := mustPauseRun(t, s, ctx)
	susp := newSuspension(r.ID)
	susp.ExpiresAt = expiresAt
	if err := s.CreateSuspension(ctx, susp); err != nil {
		t.Fatalf("fixture: create suspension: %v", err)
	}
	return r, susp
}

func containsSuspensionID(ss []*suspension.Suspension, want id.SuspensionID) bool {
	for _, s := range ss {
		if s.ID == want {
			return true
		}
	}
	return false
}

// pastDeadline is a deadline every ListExpired call in this suite is
// already past, so an expiry fixture never depends on wall-clock timing.
func pastDeadline() *time.Time {
	t := time.Now().UTC().Add(-time.Hour)
	return &t
}

// futureDeadline is pastDeadline's mirror: far enough out that a claim
// under it can never race the clock. The two exist as a pair so an
// expiry test's two fixtures differ by the deadline and nothing else.
func futureDeadline() *time.Time {
	t := time.Now().UTC().Add(time.Hour)
	return &t
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
		sessionID := id.NewSessionID()
		if err := s.SaveConversation(ctx, agentID, sessionID, []memory.Message{{Role: "user", Content: "hi"}}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("SaveConversation with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.LoadConversation(ctx, agentID, sessionID, 0); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("LoadConversation with no scope = %v, want ErrNoScope", err)
		}
		if err := s.ClearConversation(ctx, agentID, sessionID); !errors.Is(err, cortex.ErrNoScope) {
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

	// Agent, Skill, Trait, and Behavior are all converted now: every
	// method rejects a zero scope, same as runs, checkpoints, steps, tool
	// calls, and working memory above.
	//
	// The two subtests below still cover personas and orchestration
	// configs — the subsystems that remain unconverted. Neither store
	// checks scope at all right now, so both are expected to fail:
	// Create/GetByName/List all succeed against a scopeless context
	// instead of returning cortex.ErrNoScope. That is the correct
	// starting state for a suite meant to go green as each subsystem is
	// converted in a later task.

	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		cfg := &agent.Config{ID: id.NewAgentID(), Name: "assistant"}
		if err := s.Create(ctx, cfg); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("Create with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetByName(ctx, "assistant"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.List(ctx, &agent.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("List with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		sk := &skill.Skill{ID: id.NewSkillID(), Name: "researcher"}
		if err := s.CreateSkill(ctx, sk); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateSkill with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetSkillByName(ctx, "researcher"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetSkillByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListSkills(ctx, &skill.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListSkills with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		tr := &trait.Trait{ID: id.NewTraitID(), Name: "cautious"}
		if err := s.CreateTrait(ctx, tr); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateTrait with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetTraitByName(ctx, "cautious"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetTraitByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListTraits(ctx, &trait.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListTraits with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		b := &behavior.Behavior{ID: id.NewBehaviorID(), Name: "always-explain"}
		if err := s.CreateBehavior(ctx, b); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateBehavior with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetBehaviorByName(ctx, "always-explain"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetBehaviorByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListBehaviors(ctx, &behavior.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListBehaviors with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		p := &persona.Persona{ID: id.NewPersonaID(), Name: "support-rep"}
		if err := s.CreatePersona(ctx, p); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreatePersona with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetPersonaByName(ctx, "support-rep"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetPersonaByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListPersonas(ctx, &persona.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListPersonas with no scope = %v, want ErrNoScope", err)
		}
	})

	// Session is new this phase: store.Store now embeds session.Store, and
	// all three backends implement it, so this subtest exercises the same
	// Create/Get/List zero-scope assertions as every other
	// entity above.
	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		sess := &session.Session{ID: id.NewSessionID(), AgentID: id.NewAgentID(), Title: "chat"}
		if err := s.CreateSession(ctx, sess); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateSession with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetSession(ctx, sess.ID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetSession with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListSessions(ctx, &session.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListSessions with no scope = %v, want ErrNoScope", err)
		}
	})

	// Suspension is new this release. ClaimSuspension gets its guard
	// asserted here alongside the plain reads because it is the one
	// method that WRITES on the way to returning something: a claim that
	// skipped the guard would flip a run's state from an unscoped
	// context, which no other assertion in this file would catch.
	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		susp := newSuspension(id.NewAgentRunID())
		if err := s.CreateSuspension(ctx, susp); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateSuspension with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetSuspension(ctx, susp.RunID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetSuspension with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ClaimSuspension(ctx, susp.RunID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ClaimSuspension with no scope = %v, want ErrNoScope", err)
		}
		if err := s.DeleteSuspension(ctx, susp.RunID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("DeleteSuspension with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListExpired(ctx, time.Now().UTC(), 10); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListExpired with no scope = %v, want ErrNoScope", err)
		}
	})

	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		c := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "escalation-flow"}
		if err := s.CreateOrchestration(ctx, c); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateOrchestration with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetOrchestrationByName(ctx, "escalation-flow"); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetOrchestrationByName with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListOrchestrations(ctx, &orchestration.ConfigListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListOrchestrations with no scope = %v, want ErrNoScope", err)
		}
	})

	// OrchestrationRun covers RunStore, which testZeroScopeRejection's
	// original six subtests never touched — every prior Orchestration
	// assertion here exercised ConfigStore only. cortex_orchestration_runs
	// gained the same five scope columns as configs; this is the coverage
	// that proves its guards actually reject a zero scope instead of just
	// existing unexercised.
	t.Run("OrchestrationRun", func(t *testing.T) {
		s := newStore(t)
		r := newOrchestrationRun()
		if err := s.CreateOrchestrationRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CreateOrchestrationRun with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.GetOrchestrationRun(ctx, r.ID); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("GetOrchestrationRun with no scope = %v, want ErrNoScope", err)
		}
		if err := s.UpdateOrchestrationRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("UpdateOrchestrationRun with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.ListOrchestrationRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("ListOrchestrationRuns with no scope = %v, want ErrNoScope", err)
		}
		if _, err := s.CountOrchestrationRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
			t.Errorf("CountOrchestrationRuns with no scope = %v, want ErrNoScope", err)
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
		// Distinct scope tokens from the rest of this function's
		// ws_a/ws_b. This subtest now also creates sessions
		// (SaveConversation requires one), and the later "Session"
		// subtest's ListSessions(ctxA, &session.ListFilter{}) call is
		// deliberately unfiltered by agent. Each newStore(t) call now
		// truncates/clears every table this suite touches (including
		// cortex_sessions) before handing back the backend, so a stray
		// session left at literal "ws_a" wouldn't actually survive into
		// that later subtest's own newStore(t) call anymore -- but the
		// rename costs nothing and stands as a second line of defense
		// against the same class of cross-subtest leak the truncate
		// fix was itself written to close.
		ctxA := ctxWithScope("ws_conv_a")
		ctxB := ctxWithScope("ws_conv_b")
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
		sidA := mustCreateSession(t, s, ctxA, agentID, "session-a")
		sidB := mustCreateSession(t, s, ctxB, agentID, "session-b")

		if err := s.SaveConversation(ctxA, agentID, sidA, []memory.Message{{Role: "user", Content: "from-a"}}); err != nil {
			t.Fatalf("save conversation A: %v", err)
		}
		if err := s.SaveConversation(ctxB, agentID, sidB, []memory.Message{{Role: "user", Content: "from-b"}}); err != nil {
			t.Fatalf("save conversation B: %v", err)
		}

		gotA, err := s.LoadConversation(ctxA, agentID, sidA, 0)
		if err != nil {
			t.Fatalf("load conversation A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].Content != "from-a" {
			t.Fatalf("LoadConversation(ctxA, agentID) = %v, want exactly [%q] (scope B's message must not be visible to scope A)", gotA, "from-a")
		}

		gotB, err := s.LoadConversation(ctxB, agentID, sidB, 0)
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
		if err = s.ClearConversation(ctxB, agentID, sidB); err != nil {
			t.Fatalf("clear conversation (ctxB): %v", err)
		}
		stillThere, err := s.LoadConversation(ctxA, agentID, sidA, 0)
		if err != nil {
			t.Fatalf("reload conversation A after ctxB clear: %v", err)
		}
		if len(stillThere) != 1 || stillThere[0].Content != "from-a" {
			t.Fatalf("conversation A after ctxB's ClearConversation = %v, want unchanged [%q] (must not delete another scope's history)", stillThere, "from-a")
		}
		bGone, err := s.LoadConversation(ctxB, agentID, sidB, 0)
		if err != nil {
			t.Fatalf("reload conversation B after ctxB clear: %v", err)
		}
		if len(bGone) != 0 {
			t.Fatalf("conversation B after its own ClearConversation = %v, want empty (clear must actually delete B's own rows, not no-op)", bGone)
		}

		// Clearing from the correct scope removes that scope's own rows.
		if err = s.ClearConversation(ctxA, agentID, sidA); err != nil {
			t.Fatalf("clear conversation (ctxA): %v", err)
		}
		afterClear, err := s.LoadConversation(ctxA, agentID, sidA, 0)
		if err != nil {
			t.Fatalf("reload conversation A after ctxA clear: %v", err)
		}
		if len(afterClear) != 0 {
			t.Fatalf("conversation A after same-scope ClearConversation = %v, want empty", afterClear)
		}
	})

	// ConversationLoadReturnsNewestMessages is the store-level proof for
	// fix round 2's Finding 2: LoadConversation used to order
	// created_at ASC and LIMIT off the front, so once a conversation
	// grew past the limit it stayed frozen on its oldest prefix forever
	// -- new turns kept being written but were never read back into
	// context, on every backend already past that limit whether or not
	// it was ever touched by the duplicate-row backfill. LoadConversation
	// must instead take the newest `limit` rows and hand them back in
	// chronological order, so a caller always sees the recent end of the
	// conversation, not a stale prefix.
	t.Run("ConversationLoadReturnsNewestMessages", func(t *testing.T) {
		s := newStore(t)
		agentCtx := ctxWithScope("ws_newest")
		agentID := id.NewAgentID()
		sessionID := mustCreateSession(t, s, agentCtx, agentID, "newest")

		const total = 150
		messages := make([]memory.Message, total)
		for i := range messages {
			messages[i] = memory.Message{Role: "user", Content: fmt.Sprintf("msg-%03d", i)}
		}
		if err := s.SaveConversation(agentCtx, agentID, sessionID, messages); err != nil {
			t.Fatalf("save conversation (%d messages): %v", total, err)
		}

		const limit = 100
		rows, err := s.LoadConversation(agentCtx, agentID, sessionID, limit)
		if err != nil {
			t.Fatalf("load conversation: %v", err)
		}
		if len(rows) != limit {
			t.Fatalf("LoadConversation(limit=%d) returned %d row(s), want %d", limit, len(rows), limit)
		}

		// The last `limit` messages written (msg-050 .. msg-149), in the
		// same chronological order they were written -- not the first
		// `limit` (msg-000 .. msg-099), which is what the pre-fix
		// ASC-then-LIMIT query would have returned.
		wantFirst := total - limit // 50
		for i, msg := range rows {
			want := fmt.Sprintf("msg-%03d", wantFirst+i)
			if msg.Content != want {
				t.Fatalf("row %d = %q, want %q (LoadConversation must return the NEWEST %d messages in chronological order, not the oldest %d)",
					i, msg.Content, want, limit, limit)
			}
		}
	})

	// SessionMessageCounters is the store-level proof for the counter
	// invariant this phase exists to guarantee: message_count always
	// equals the number of message ROWS a session's SaveConversation
	// calls have written, never the number of calls. Two SaveConversation
	// calls with different batch sizes (3, then 2) would leave
	// message_count at 2 if the counter were naively incremented once per
	// call instead of by len(messages); this catches that shape directly,
	// on every backend, since each hand-writes its own transaction.
	t.Run("SessionMessageCounters", func(t *testing.T) {
		s := newStore(t)
		agentCtx := ctxWithScope("ws_counters")
		agentID := id.NewAgentID()
		sessionID := mustCreateSession(t, s, agentCtx, agentID, "counters")

		if err := s.SaveConversation(agentCtx, agentID, sessionID, []memory.Message{
			{Role: "user", Content: "one"},
			{Role: "assistant", Content: "two"},
			{Role: "user", Content: "three"},
		}); err != nil {
			t.Fatalf("save conversation (batch 1, 3 messages): %v", err)
		}
		if err := s.SaveConversation(agentCtx, agentID, sessionID, []memory.Message{
			{Role: "assistant", Content: "four"},
			{Role: "user", Content: "five"},
		}); err != nil {
			t.Fatalf("save conversation (batch 2, 2 messages): %v", err)
		}

		rows, err := s.LoadConversation(agentCtx, agentID, sessionID, 0)
		if err != nil {
			t.Fatalf("load conversation: %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("LoadConversation returned %d row(s), want 5 (setup assumption broken, not the thing under test)", len(rows))
		}

		got, err := s.GetSession(agentCtx, sessionID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if got.MessageCount != 5 {
			t.Errorf("session.MessageCount = %d, want 5 (the number of message rows written across both calls, not the number of SaveConversation calls, which was 2)", got.MessageCount)
		}
		if got.LastMessage != "five" {
			t.Errorf("session.LastMessage = %q, want %q (the last row written by the most recent SaveConversation call)", got.LastMessage, "five")
		}

		if clearErr := s.ClearConversation(agentCtx, agentID, sessionID); clearErr != nil {
			t.Fatalf("clear conversation: %v", clearErr)
		}
		afterClear, err := s.GetSession(agentCtx, sessionID)
		if err != nil {
			t.Fatalf("get session after clear: %v", err)
		}
		if afterClear.MessageCount != 0 {
			t.Errorf("session.MessageCount after ClearConversation = %d, want 0", afterClear.MessageCount)
		}
		if afterClear.LastMessage != "" {
			t.Errorf("session.LastMessage after ClearConversation = %q, want empty", afterClear.LastMessage)
		}
	})

	// ConversationRejectsCrossAgentSession is the store-level proof for
	// fix round 1's Finding 1: the session UPDATE inside
	// SaveConversation/ClearConversation used to match on id+scope_canon
	// alone, with no agent_id predicate, while the message-row
	// insert/delete in the same call DOES filter on agent_id. That gap
	// let a caller who supplied the right session id but the wrong
	// agent id write/delete zero message rows (agent_id doesn't match
	// the real rows) while still resetting the session's own
	// message_count/last_message -- exactly the counter drift the
	// design forbids, reachable from api/memory_handler.go's
	// caller-supplied ?session_id= before that handler validated
	// ownership. Both calls must now fail outright (ErrSessionNotFound)
	// and leave the victim's session and rows untouched.
	t.Run("ConversationRejectsCrossAgentSession", func(t *testing.T) {
		s := newStore(t)
		agentCtx := ctxWithScope("ws_cross_agent")
		victimAgent := id.NewAgentID()
		attackerAgent := id.NewAgentID()
		victimSession := mustCreateSession(t, s, agentCtx, victimAgent, "victim-session")

		if err := s.SaveConversation(agentCtx, victimAgent, victimSession, []memory.Message{
			{Role: "user", Content: "victim's real message"},
		}); err != nil {
			t.Fatalf("save conversation for the victim agent: %v", err)
		}

		before, err := s.GetSession(agentCtx, victimSession)
		if err != nil {
			t.Fatalf("get session before cross-agent attempt: %v", err)
		}
		if before.MessageCount != 1 || before.LastMessage != "victim's real message" {
			t.Fatalf("setup assumption broken: MessageCount=%d LastMessage=%q, want 1 and %q",
				before.MessageCount, before.LastMessage, "victim's real message")
		}

		// attackerAgent supplies the victim's real session id -- the
		// shape of a caller-supplied ?session_id= that doesn't belong to
		// the agent named in the request path.
		if saveErr := s.SaveConversation(agentCtx, attackerAgent, victimSession, []memory.Message{
			{Role: "user", Content: "smuggled in under the wrong agent"},
		}); !errors.Is(saveErr, cortex.ErrSessionNotFound) {
			t.Errorf("SaveConversation(attackerAgent, victimSession) = %v, want ErrSessionNotFound", saveErr)
		}
		if clearErr := s.ClearConversation(agentCtx, attackerAgent, victimSession); !errors.Is(clearErr, cortex.ErrSessionNotFound) {
			t.Errorf("ClearConversation(attackerAgent, victimSession) = %v, want ErrSessionNotFound", clearErr)
		}

		after, err := s.GetSession(agentCtx, victimSession)
		if err != nil {
			t.Fatalf("get session after cross-agent attempt: %v", err)
		}
		if after.MessageCount != before.MessageCount {
			t.Errorf("session.MessageCount after a rejected cross-agent call = %d, want unchanged %d", after.MessageCount, before.MessageCount)
		}
		if after.LastMessage != before.LastMessage {
			t.Errorf("session.LastMessage after a rejected cross-agent call = %q, want unchanged %q", after.LastMessage, before.LastMessage)
		}

		rows, err := s.LoadConversation(agentCtx, victimAgent, victimSession, 0)
		if err != nil {
			t.Fatalf("load conversation after cross-agent attempt: %v", err)
		}
		if len(rows) != 1 {
			t.Errorf("conversation rows after a rejected cross-agent call = %d, want unchanged 1 (the attacker's message must not have been inserted, and the victim's must survive)", len(rows))
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

	// Agent gets the same distinct-identifier cross-scope shape as
	// Run/Checkpoint above: two rows with DIFFERENT names, one per scope,
	// and a listing scoped to each side that should see only its own row.
	// Unlike the five subtests that follow, this one is expected to pass:
	// List/GetByName are scope-guarded now.
	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		agentA := mustCreateAgent(t, s, ctxA, "agent-a")
		agentB := mustCreateAgent(t, s, ctxB, "agent-b")

		gotA, err := s.List(ctxA, &agent.ListFilter{})
		if err != nil {
			t.Fatalf("list agents A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != agentA {
			t.Fatalf("List(ctxA) = %d agent(s), want exactly agentA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.List(ctxB, &agent.ListFilter{})
		if err != nil {
			t.Fatalf("list agents B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != agentB {
			t.Fatalf("List(ctxB) = %d agent(s), want exactly agentB", len(gotB))
		}

		if _, err := s.GetByName(ctxB, "agent-a"); !errors.Is(err, cortex.ErrAgentNotFound) {
			t.Errorf("GetByName(ctxB, %q) = %v, want ErrAgentNotFound", "agent-a", err)
		}
	})

	// Skill, Trait, and Behavior get the same distinct-identifier
	// cross-scope shape as Agent above. Unlike Orchestration below,
	// these three (and Persona, further below) are expected to pass:
	// List/GetByName are scope-guarded now.

	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		skillA := mustCreateSkill(t, s, ctxA, "skill-a")
		skillB := mustCreateSkill(t, s, ctxB, "skill-b")

		gotA, err := s.ListSkills(ctxA, &skill.ListFilter{})
		if err != nil {
			t.Fatalf("list skills A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != skillA {
			t.Fatalf("ListSkills(ctxA) = %d skill(s), want exactly skillA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListSkills(ctxB, &skill.ListFilter{})
		if err != nil {
			t.Fatalf("list skills B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != skillB {
			t.Fatalf("ListSkills(ctxB) = %d skill(s), want exactly skillB", len(gotB))
		}

		if _, err := s.GetSkillByName(ctxB, "skill-a"); !errors.Is(err, cortex.ErrSkillNotFound) {
			t.Errorf("GetSkillByName(ctxB, %q) = %v, want ErrSkillNotFound", "skill-a", err)
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		traitA := mustCreateTrait(t, s, ctxA, "trait-a")
		traitB := mustCreateTrait(t, s, ctxB, "trait-b")

		gotA, err := s.ListTraits(ctxA, &trait.ListFilter{})
		if err != nil {
			t.Fatalf("list traits A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != traitA {
			t.Fatalf("ListTraits(ctxA) = %d trait(s), want exactly traitA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListTraits(ctxB, &trait.ListFilter{})
		if err != nil {
			t.Fatalf("list traits B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != traitB {
			t.Fatalf("ListTraits(ctxB) = %d trait(s), want exactly traitB", len(gotB))
		}

		if _, err := s.GetTraitByName(ctxB, "trait-a"); !errors.Is(err, cortex.ErrTraitNotFound) {
			t.Errorf("GetTraitByName(ctxB, %q) = %v, want ErrTraitNotFound", "trait-a", err)
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		behaviorA := mustCreateBehavior(t, s, ctxA, "behavior-a")
		behaviorB := mustCreateBehavior(t, s, ctxB, "behavior-b")

		gotA, err := s.ListBehaviors(ctxA, &behavior.ListFilter{})
		if err != nil {
			t.Fatalf("list behaviors A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != behaviorA {
			t.Fatalf("ListBehaviors(ctxA) = %d behavior(s), want exactly behaviorA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListBehaviors(ctxB, &behavior.ListFilter{})
		if err != nil {
			t.Fatalf("list behaviors B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != behaviorB {
			t.Fatalf("ListBehaviors(ctxB) = %d behavior(s), want exactly behaviorB", len(gotB))
		}

		if _, err := s.GetBehaviorByName(ctxB, "behavior-a"); !errors.Is(err, cortex.ErrBehaviorNotFound) {
			t.Errorf("GetBehaviorByName(ctxB, %q) = %v, want ErrBehaviorNotFound", "behavior-a", err)
		}
	})

	// Persona gets the same distinct-identifier cross-scope shape as
	// Agent/Skill/Trait/Behavior above: List/GetPersonaByName are
	// scope-guarded now, so this is expected to pass. Orchestration
	// below is the one entity left that isn't -- neither store filters
	// by scope yet, so its List/Get returns rows from both scopes and it
	// is expected to fail.

	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		personaA := mustCreatePersona(t, s, ctxA, "persona-a")
		personaB := mustCreatePersona(t, s, ctxB, "persona-b")

		gotA, err := s.ListPersonas(ctxA, &persona.ListFilter{})
		if err != nil {
			t.Fatalf("list personas A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != personaA {
			t.Fatalf("ListPersonas(ctxA) = %d persona(s), want exactly personaA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListPersonas(ctxB, &persona.ListFilter{})
		if err != nil {
			t.Fatalf("list personas B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != personaB {
			t.Fatalf("ListPersonas(ctxB) = %d persona(s), want exactly personaB", len(gotB))
		}

		if _, err := s.GetPersonaByName(ctxB, "persona-a"); !errors.Is(err, cortex.ErrPersonaNotFound) {
			t.Errorf("GetPersonaByName(ctxB, %q) = %v, want ErrPersonaNotFound", "persona-a", err)
		}
	})

	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		// One agent shared by both sessions: only the scope differs
		// between sidA and sidB, so a predicate that filtered on
		// agent_id instead of scope would still pass this test by
		// accident.
		agentID := mustCreateAgent(t, s, ctxA, "shared-agent")
		sidA := mustCreateSession(t, s, ctxA, agentID, "thread-a")
		sidB := mustCreateSession(t, s, ctxB, agentID, "thread-b")

		gotA, err := s.ListSessions(ctxA, &session.ListFilter{})
		if err != nil {
			t.Fatalf("list sessions A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != sidA {
			t.Fatalf("ListSessions(ctxA) = %d session(s), want exactly sidA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListSessions(ctxB, &session.ListFilter{})
		if err != nil {
			t.Fatalf("list sessions B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != sidB {
			t.Fatalf("ListSessions(ctxB) = %d session(s), want exactly sidB", len(gotB))
		}

		if _, err := s.GetSession(ctxB, sidA); !errors.Is(err, cortex.ErrSessionNotFound) {
			t.Errorf("GetSession(ctxB, sidA) = %v, want ErrSessionNotFound", err)
		}
	})

	// SessionDeleteRemovesMessages proves deleting a session leaves no
	// orphaned messages behind. Every backend gets there differently --
	// postgres/sqlite delete the message rows explicitly inside
	// DeleteSession's own transaction rather than through a database
	// FOREIGN KEY (cortex_memories.session_id is shared by
	// working/summary memory too, which never sets it at all, so a real
	// FK there would reject their writes outright), mongo does the same
	// inside its own multi-document transaction since it has no FK
	// concept at all -- but this test doesn't care which route a given
	// backend took, only that all three land on the same observable
	// result.
	t.Run("SessionDeleteRemovesMessages", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		agentID := mustCreateAgent(t, s, ctx, "")
		sid := mustCreateSession(t, s, ctx, agentID, "thread")

		if err := s.SaveConversation(ctx, agentID, sid, []memory.Message{{Role: "user", Content: "hello"}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := s.DeleteSession(ctx, sid); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		msgs, err := s.LoadConversation(ctx, agentID, sid, 100)
		if err != nil {
			t.Fatalf("load after delete: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("deleting a session left %d orphaned messages", len(msgs))
		}
	})

	// A suspension is keyed one-per-run, and a run only ever exists in one
	// scope, so the two fixtures here necessarily sit on two different
	// runs -- the same shape the Run subtest above uses. The scope
	// predicate is still what the assertions turn on: ListExpired is
	// filtered by nothing but scope, and the cross-read at the end asks
	// for scope A's run by id from scope B, which only a scope predicate
	// on the suspension read can refuse.
	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA, suspA := mustSuspendRun(t, s, ctxA, pastDeadline())
		_, suspB := mustSuspendRun(t, s, ctxB, pastDeadline())

		gotA, err := s.ListExpired(ctxA, time.Now().UTC(), 100)
		if err != nil {
			t.Fatalf("list expired A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != suspA.ID {
			t.Fatalf("ListExpired(ctxA) = %d suspension(s), want exactly suspA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListExpired(ctxB, time.Now().UTC(), 100)
		if err != nil {
			t.Fatalf("list expired B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != suspB.ID {
			t.Fatalf("ListExpired(ctxB) = %d suspension(s), want exactly suspB", len(gotB))
		}

		if _, err := s.GetSuspension(ctxB, runA.ID); !errors.Is(err, cortex.ErrNotSuspended) {
			t.Errorf("GetSuspension(ctxB, runA.ID) = %v, want ErrNotSuspended", err)
		}
	})

	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		orchA := mustCreateOrchestration(t, s, ctxA, "orchestration-a")
		orchB := mustCreateOrchestration(t, s, ctxB, "orchestration-b")

		gotA, err := s.ListOrchestrations(ctxA, &orchestration.ConfigListFilter{})
		if err != nil {
			t.Fatalf("list orchestrations A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != orchA {
			t.Fatalf("ListOrchestrations(ctxA) = %d orchestration(s), want exactly orchA (predicate isn't filtering)", len(gotA))
		}

		gotB, err := s.ListOrchestrations(ctxB, &orchestration.ConfigListFilter{})
		if err != nil {
			t.Fatalf("list orchestrations B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != orchB {
			t.Fatalf("ListOrchestrations(ctxB) = %d orchestration(s), want exactly orchB", len(gotB))
		}

		if _, err := s.GetOrchestrationByName(ctxB, "orchestration-a"); !errors.Is(err, cortex.ErrOrchestrationNotFound) {
			t.Errorf("GetOrchestrationByName(ctxB, %q) = %v, want ErrOrchestrationNotFound", "orchestration-a", err)
		}
	})

	// OrchestrationRun covers RunStore, same gap as ZeroScopeRejection's
	// OrchestrationRun subtest above: every other Orchestration assertion
	// in this group exercises ConfigStore only.
	t.Run("OrchestrationRun", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA := mustCreateOrchestrationRun(t, s, ctxA)
		runB := mustCreateOrchestrationRun(t, s, ctxB)

		gotA, err := s.ListOrchestrationRuns(ctxA, nil)
		if err != nil {
			t.Fatalf("list orchestration runs A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].ID != runA.ID {
			t.Fatalf("ListOrchestrationRuns(ctxA) = %d run(s), want exactly runA (predicate isn't filtering)", len(gotA))
		}
		count, err := s.CountOrchestrationRuns(ctxA, nil)
		if err != nil {
			t.Fatalf("count orchestration runs A: %v", err)
		}
		if count != 1 {
			t.Fatalf("CountOrchestrationRuns(ctxA) = %d, want 1", count)
		}

		gotB, err := s.ListOrchestrationRuns(ctxB, nil)
		if err != nil {
			t.Fatalf("list orchestration runs B: %v", err)
		}
		if len(gotB) != 1 || gotB[0].ID != runB.ID {
			t.Fatalf("ListOrchestrationRuns(ctxB) = %d run(s), want exactly runB", len(gotB))
		}

		if _, err := s.GetOrchestrationRun(ctxB, runA.ID); !errors.Is(err, cortex.ErrOrchestrationRunNotFound) {
			t.Errorf("GetOrchestrationRun(ctxB, runA.ID) = %v, want ErrOrchestrationRunNotFound", err)
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

	// Agent is the one entity below that IS scope-guarded: the same name
	// reused across two scopes must resolve to two distinct rows, proving
	// UNIQUE (scope_canon, name) — not the retired UNIQUE (app_id, name)
	// — is what the store enforces now. Before this task, the second
	// mustCreateAgent call below failed outright with
	// cortex.ErrAlreadyExists, colliding on the old app_id-keyed index
	// before the fixture could even get two rows on the books to compare.
	// It no longer does: two different scopes can each use "assistant".
	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreateAgent(t, s, ctxA, "assistant")
		mustCreateAgent(t, s, ctxB, "assistant")

		gotA, err := s.GetByName(ctxA, "assistant")
		if err != nil {
			t.Fatalf("scope A cannot read its own agent: %v", err)
		}
		gotB, err := s.GetByName(ctxB, "assistant")
		if err != nil {
			t.Fatalf("scope B cannot read its own agent: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's agent merely by knowing (or guessing) its id -- the
		// same bearer-capability threat the run/checkpoint/step subtests
		// above exercise for their own identifiers. GetByName above only
		// proves the name-lookup predicate; Get/Update/Delete take the id
		// directly and need their own predicate proven independently.
		_, getErr := s.Get(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrAgentNotFound) {
			t.Errorf("Get(ctxB, agentA.ID) = %v, want ErrAgentNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.Update(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrAgentNotFound) {
			t.Errorf("Update(ctxB, agentA) = %v, want ErrAgentNotFound", updateErr)
		}
		stillA, err := s.Get(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload agentA after cross-scope Update attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("Update(ctxB, agentA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.Delete(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrAgentNotFound) {
			t.Errorf("Delete(ctxB, agentA.ID) = %v, want ErrAgentNotFound", deleteErr)
		}
		_, reloadErr := s.Get(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("agentA missing after cross-scope Delete attempt: %v (must not delete another scope's row)", reloadErr)
		}
	})

	// Skill, Trait, and Behavior get the same treatment as Agent above:
	// the same name reused across two scopes must resolve to two
	// distinct rows, proving UNIQUE (scope_canon, name) — not the retired
	// UNIQUE (app_id, name) — is what each store enforces now, and
	// Get/Update/Delete must refuse a scope-B caller who only knows (or
	// guesses) scope-A's id.

	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreateSkill(t, s, ctxA, "researcher")
		mustCreateSkill(t, s, ctxB, "researcher")

		gotA, err := s.GetSkillByName(ctxA, "researcher")
		if err != nil {
			t.Fatalf("scope A cannot read its own skill: %v", err)
		}
		gotB, err := s.GetSkillByName(ctxB, "researcher")
		if err != nil {
			t.Fatalf("scope B cannot read its own skill: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's skill merely by knowing (or guessing) its id.
		// GetSkillByName above only proves the name-lookup predicate;
		// GetSkill/UpdateSkill/DeleteSkill take the id directly and need
		// their own predicate proven independently.
		_, getErr := s.GetSkill(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrSkillNotFound) {
			t.Errorf("GetSkill(ctxB, skillA.ID) = %v, want ErrSkillNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.UpdateSkill(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrSkillNotFound) {
			t.Errorf("UpdateSkill(ctxB, skillA) = %v, want ErrSkillNotFound", updateErr)
		}
		stillA, err := s.GetSkill(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload skillA after cross-scope UpdateSkill attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("UpdateSkill(ctxB, skillA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.DeleteSkill(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrSkillNotFound) {
			t.Errorf("DeleteSkill(ctxB, skillA.ID) = %v, want ErrSkillNotFound", deleteErr)
		}
		_, reloadErr := s.GetSkill(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("skillA missing after cross-scope DeleteSkill attempt: %v (must not delete another scope's row)", reloadErr)
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreateTrait(t, s, ctxA, "cautious")
		mustCreateTrait(t, s, ctxB, "cautious")

		gotA, err := s.GetTraitByName(ctxA, "cautious")
		if err != nil {
			t.Fatalf("scope A cannot read its own trait: %v", err)
		}
		gotB, err := s.GetTraitByName(ctxB, "cautious")
		if err != nil {
			t.Fatalf("scope B cannot read its own trait: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's trait merely by knowing (or guessing) its id.
		// GetTraitByName above only proves the name-lookup predicate;
		// GetTrait/UpdateTrait/DeleteTrait take the id directly and need
		// their own predicate proven independently.
		_, getErr := s.GetTrait(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrTraitNotFound) {
			t.Errorf("GetTrait(ctxB, traitA.ID) = %v, want ErrTraitNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.UpdateTrait(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrTraitNotFound) {
			t.Errorf("UpdateTrait(ctxB, traitA) = %v, want ErrTraitNotFound", updateErr)
		}
		stillA, err := s.GetTrait(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload traitA after cross-scope UpdateTrait attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("UpdateTrait(ctxB, traitA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.DeleteTrait(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrTraitNotFound) {
			t.Errorf("DeleteTrait(ctxB, traitA.ID) = %v, want ErrTraitNotFound", deleteErr)
		}
		_, reloadErr := s.GetTrait(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("traitA missing after cross-scope DeleteTrait attempt: %v (must not delete another scope's row)", reloadErr)
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreateBehavior(t, s, ctxA, "always-explain")
		mustCreateBehavior(t, s, ctxB, "always-explain")

		gotA, err := s.GetBehaviorByName(ctxA, "always-explain")
		if err != nil {
			t.Fatalf("scope A cannot read its own behavior: %v", err)
		}
		gotB, err := s.GetBehaviorByName(ctxB, "always-explain")
		if err != nil {
			t.Fatalf("scope B cannot read its own behavior: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's behavior merely by knowing (or guessing) its id.
		// GetBehaviorByName above only proves the name-lookup predicate;
		// GetBehavior/UpdateBehavior/DeleteBehavior take the id directly
		// and need their own predicate proven independently.
		_, getErr := s.GetBehavior(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrBehaviorNotFound) {
			t.Errorf("GetBehavior(ctxB, behaviorA.ID) = %v, want ErrBehaviorNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.UpdateBehavior(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrBehaviorNotFound) {
			t.Errorf("UpdateBehavior(ctxB, behaviorA) = %v, want ErrBehaviorNotFound", updateErr)
		}
		stillA, err := s.GetBehavior(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload behaviorA after cross-scope UpdateBehavior attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("UpdateBehavior(ctxB, behaviorA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.DeleteBehavior(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrBehaviorNotFound) {
			t.Errorf("DeleteBehavior(ctxB, behaviorA.ID) = %v, want ErrBehaviorNotFound", deleteErr)
		}
		_, reloadErr := s.GetBehavior(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("behaviorA missing after cross-scope DeleteBehavior attempt: %v (must not delete another scope's row)", reloadErr)
		}
	})

	// Persona gets the same treatment as Agent/Skill/Trait/Behavior
	// above: the same name reused across two scopes must resolve to two
	// distinct rows, proving UNIQUE (scope_canon, name) — not the
	// retired UNIQUE (app_id, name) — is what the store enforces now,
	// and Get/Update/Delete must refuse a scope-B caller who only knows
	// (or guesses) scope-A's id.
	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreatePersona(t, s, ctxA, "support-rep")
		mustCreatePersona(t, s, ctxB, "support-rep")

		gotA, err := s.GetPersonaByName(ctxA, "support-rep")
		if err != nil {
			t.Fatalf("scope A cannot read its own persona: %v", err)
		}
		gotB, err := s.GetPersonaByName(ctxB, "support-rep")
		if err != nil {
			t.Fatalf("scope B cannot read its own persona: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's persona merely by knowing (or guessing) its id (or
		// name). GetPersonaByName above only proves the name-lookup
		// predicate; Get/Update/Delete need their own predicate proven
		// independently, and each check re-reads from scope A afterward
		// to prove no mutation and no deletion happened.
		_, getErr := s.GetPersona(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrPersonaNotFound) {
			t.Errorf("GetPersona(ctxB, personaA.ID) = %v, want ErrPersonaNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.UpdatePersona(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrPersonaNotFound) {
			t.Errorf("UpdatePersona(ctxB, personaA) = %v, want ErrPersonaNotFound", updateErr)
		}
		stillA, err := s.GetPersona(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload personaA after cross-scope UpdatePersona attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("UpdatePersona(ctxB, personaA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.DeletePersona(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrPersonaNotFound) {
			t.Errorf("DeletePersona(ctxB, personaA.ID) = %v, want ErrPersonaNotFound", deleteErr)
		}
		_, reloadErr := s.GetPersona(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("personaA missing after cross-scope DeletePersona attempt: %v (must not delete another scope's row)", reloadErr)
		}
	})

	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		// One agent, one session, read from a second scope. Separating
		// the fixtures by agent instead would mean agent_id alone
		// distinguishes the rows and the scope predicate is never
		// exercised.
		agentA := mustCreateAgent(t, s, ctxA, "shared-name")
		sidA := mustCreateSession(t, s, ctxA, agentA, "thread")

		if _, err := s.GetSession(ctxB, sidA); !errors.Is(err, cortex.ErrSessionNotFound) {
			t.Errorf("GetSession from scope B = %v, want ErrSessionNotFound", err)
		}
		if err := s.DeleteSession(ctxB, sidA); !errors.Is(err, cortex.ErrSessionNotFound) {
			t.Errorf("DeleteSession from scope B = %v, want ErrSessionNotFound", err)
		}
		if _, err := s.GetSession(ctxA, sidA); err != nil {
			t.Errorf("scope A lost its own session after B's attempts: %v", err)
		}
	})

	// Suspension has no reusable name to collide on -- its identity is a
	// run id, and run ids are globally unique -- so this subtest proves
	// the other half of the same contract: a scope-B caller who knows (or
	// guesses) a scope-A run id must not be able to read, claim, or
	// delete scope A's suspension. Claim matters most of the three: it is
	// the only one that would also flip scope A's run state as a side
	// effect, so the run is re-read afterward and must still be paused.
	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		runA, suspA := mustSuspendRun(t, s, ctxA, nil)

		if _, err := s.GetSuspension(ctxB, runA.ID); !errors.Is(err, cortex.ErrNotSuspended) {
			t.Errorf("GetSuspension from scope B = %v, want ErrNotSuspended", err)
		}
		if _, err := s.ClaimSuspension(ctxB, runA.ID); !errors.Is(err, cortex.ErrNotSuspended) {
			t.Errorf("ClaimSuspension from scope B = %v, want ErrNotSuspended", err)
		}
		if err := s.DeleteSuspension(ctxB, runA.ID); !errors.Is(err, cortex.ErrNotSuspended) {
			t.Errorf("DeleteSuspension from scope B = %v, want ErrNotSuspended", err)
		}

		stillA, err := s.GetSuspension(ctxA, runA.ID)
		if err != nil {
			t.Fatalf("scope A lost its own suspension after B's attempts: %v", err)
		}
		if stillA.ID != suspA.ID {
			t.Errorf("GetSuspension(ctxA) = %s, want %s", stillA.ID, suspA.ID)
		}
		reloadedRun, err := s.GetRun(ctxA, runA.ID)
		if err != nil {
			t.Fatalf("reload runA after cross-scope ClaimSuspension attempt: %v", err)
		}
		if reloadedRun.State != run.StatePaused {
			t.Errorf("runA state = %q after a cross-scope claim, want %q (the claim must be a no-op)", reloadedRun.State, run.StatePaused)
		}
	})

	// Orchestration gets the same treatment as Agent/Skill/Trait/Behavior/
	// Persona above: the same name reused across two scopes must resolve
	// to two distinct rows, proving UNIQUE (scope_canon, name) is what the
	// store enforces now, and Get/Update/Delete must refuse a scope-B
	// caller who only knows (or guesses) scope-A's id. Fix round 1 dropped
	// AppID from Config entirely (it never disambiguated anything once the
	// index went scope-keyed — a fixed app_id could only turn a hit into a
	// miss), so GetOrchestrationByName no longer takes one either.
	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		ctxA := ctxWithScope("ws_a")
		ctxB := ctxWithScope("ws_b")

		mustCreateOrchestration(t, s, ctxA, "escalation-flow")
		mustCreateOrchestration(t, s, ctxB, "escalation-flow")

		gotA, err := s.GetOrchestrationByName(ctxA, "escalation-flow")
		if err != nil {
			t.Fatalf("scope A cannot read its own orchestration: %v", err)
		}
		gotB, err := s.GetOrchestrationByName(ctxB, "escalation-flow")
		if err != nil {
			t.Fatalf("scope B cannot read its own orchestration: %v", err)
		}
		if gotA.ID == gotB.ID {
			t.Fatal("both scopes resolved to the same row; the scope predicate is not applied")
		}

		// A caller in scope B must not be able to read, mutate, or delete
		// scope A's orchestration merely by knowing (or guessing) its id.
		// GetOrchestrationByName above only proves the name-lookup
		// predicate; Get/Update/Delete need their own predicate proven
		// independently, and each check re-reads from scope A afterward
		// to prove no mutation and no deletion happened.
		_, getErr := s.GetOrchestration(ctxB, gotA.ID)
		if !errors.Is(getErr, cortex.ErrOrchestrationNotFound) {
			t.Errorf("GetOrchestration(ctxB, orchA.ID) = %v, want ErrOrchestrationNotFound", getErr)
		}

		gotA.Description = "mutated-from-B"
		updateErr := s.UpdateOrchestration(ctxB, gotA)
		if !errors.Is(updateErr, cortex.ErrOrchestrationNotFound) {
			t.Errorf("UpdateOrchestration(ctxB, orchA) = %v, want ErrOrchestrationNotFound", updateErr)
		}
		stillA, err := s.GetOrchestration(ctxA, gotA.ID)
		if err != nil {
			t.Fatalf("reload orchA after cross-scope UpdateOrchestration attempt: %v", err)
		}
		if stillA.Description == "mutated-from-B" {
			t.Error("UpdateOrchestration(ctxB, orchA) mutated scope A's row; a cross-scope update must be a no-op")
		}

		deleteErr := s.DeleteOrchestration(ctxB, gotA.ID)
		if !errors.Is(deleteErr, cortex.ErrOrchestrationNotFound) {
			t.Errorf("DeleteOrchestration(ctxB, orchA.ID) = %v, want ErrOrchestrationNotFound", deleteErr)
		}
		_, reloadErr := s.GetOrchestration(ctxA, gotA.ID)
		if reloadErr != nil {
			t.Errorf("orchA missing after cross-scope DeleteOrchestration attempt: %v (must not delete another scope's row)", reloadErr)
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

	// Agent already had a Scope field going into this task (Phase 0), but
	// this subtest itself did not — List's scope predicate went untested
	// against a broader-than-stored filter until now.
	t.Run("Agent", func(t *testing.T) {
		s := newStore(t)
		agentID := mustCreateAgent(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.List(broad, nil)
		if err != nil {
			t.Fatalf("list agents (broad): %v", err)
		}
		if !containsAgentID(got, agentID) {
			t.Errorf("List({workspace=ws_x}) didn't return an agent scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.List(other, nil)
		if err != nil {
			t.Fatalf("list agents (other workspace): %v", err)
		}
		if containsAgentID(gotOther, agentID) {
			t.Errorf("List({workspace=ws_y}) incorrectly returned an agent scoped to {workspace=ws_x, project=p1}")
		}
	})

	// Skill, Trait, and Behavior lost their Scope field only this task
	// (Task 6), so their List predicate against a broader-than-stored
	// filter went untested until now, the same as Agent's did going into
	// this task.
	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		skillID := mustCreateSkill(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListSkills(broad, nil)
		if err != nil {
			t.Fatalf("list skills (broad): %v", err)
		}
		if !containsSkillID(got, skillID) {
			t.Errorf("ListSkills({workspace=ws_x}) didn't return a skill scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListSkills(other, nil)
		if err != nil {
			t.Fatalf("list skills (other workspace): %v", err)
		}
		if containsSkillID(gotOther, skillID) {
			t.Errorf("ListSkills({workspace=ws_y}) incorrectly returned a skill scoped to {workspace=ws_x, project=p1}")
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		traitID := mustCreateTrait(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListTraits(broad, nil)
		if err != nil {
			t.Fatalf("list traits (broad): %v", err)
		}
		if !containsTraitID(got, traitID) {
			t.Errorf("ListTraits({workspace=ws_x}) didn't return a trait scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListTraits(other, nil)
		if err != nil {
			t.Fatalf("list traits (other workspace): %v", err)
		}
		if containsTraitID(gotOther, traitID) {
			t.Errorf("ListTraits({workspace=ws_y}) incorrectly returned a trait scoped to {workspace=ws_x, project=p1}")
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		behaviorID := mustCreateBehavior(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListBehaviors(broad, nil)
		if err != nil {
			t.Fatalf("list behaviors (broad): %v", err)
		}
		if !containsBehaviorID(got, behaviorID) {
			t.Errorf("ListBehaviors({workspace=ws_x}) didn't return a behavior scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListBehaviors(other, nil)
		if err != nil {
			t.Fatalf("list behaviors (other workspace): %v", err)
		}
		if containsBehaviorID(gotOther, behaviorID) {
			t.Errorf("ListBehaviors({workspace=ws_y}) incorrectly returned a behavior scoped to {workspace=ws_x, project=p1}")
		}
	})

	// Persona lost its Scope field only this task (Task 7), so its List
	// predicate against a broader-than-stored filter went untested until
	// now, the same as Skill/Trait/Behavior's did going into Task 6.
	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		personaID := mustCreatePersona(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListPersonas(broad, nil)
		if err != nil {
			t.Fatalf("list personas (broad): %v", err)
		}
		if !containsPersonaID(got, personaID) {
			t.Errorf("ListPersonas({workspace=ws_x}) didn't return a persona scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListPersonas(other, nil)
		if err != nil {
			t.Fatalf("list personas (other workspace): %v", err)
		}
		if containsPersonaID(gotOther, personaID) {
			t.Errorf("ListPersonas({workspace=ws_y}) incorrectly returned a persona scoped to {workspace=ws_x, project=p1}")
		}
	})

	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		agentID := mustCreateAgent(t, s, createCtx, "")
		sid := mustCreateSession(t, s, createCtx, agentID, "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListSessions(broad, &session.ListFilter{})
		if err != nil {
			t.Fatalf("list sessions (broad): %v", err)
		}
		if !containsSessionID(got, sid) {
			t.Errorf("ListSessions({workspace=ws_x}) didn't return a session scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListSessions(other, &session.ListFilter{})
		if err != nil {
			t.Fatalf("list sessions (other workspace): %v", err)
		}
		if containsSessionID(gotOther, sid) {
			t.Errorf("ListSessions({workspace=ws_y}) incorrectly returned a session scoped to {workspace=ws_x, project=p1}")
		}
	})

	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		_, susp := mustSuspendRun(t, s, createCtx, pastDeadline())

		broad := ctxWithScope("ws_x")
		got, err := s.ListExpired(broad, time.Now().UTC(), 100)
		if err != nil {
			t.Fatalf("list expired (broad): %v", err)
		}
		if !containsSuspensionID(got, susp.ID) {
			t.Errorf("ListExpired({workspace=ws_x}) didn't return a suspension scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListExpired(other, time.Now().UTC(), 100)
		if err != nil {
			t.Fatalf("list expired (other workspace): %v", err)
		}
		if containsSuspensionID(gotOther, susp.ID) {
			t.Errorf("ListExpired({workspace=ws_y}) incorrectly returned a suspension scoped to {workspace=ws_x, project=p1}")
		}
	})

	// Orchestration is the last entity converted in this phase, so its
	// List predicate against a broader-than-stored filter went untested
	// until now, the same as Persona's did going into Task 7.
	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		orchID := mustCreateOrchestration(t, s, ctxWithScope("ws_x", "p1"), "")

		broad := ctxWithScope("ws_x")
		got, err := s.ListOrchestrations(broad, nil)
		if err != nil {
			t.Fatalf("list orchestrations (broad): %v", err)
		}
		if !containsOrchestrationID(got, orchID) {
			t.Errorf("ListOrchestrations({workspace=ws_x}) didn't return an orchestration scoped to {workspace=ws_x, project=p1} (prefix matching broken)")
		}

		other := ctxWithScope("ws_y")
		gotOther, err := s.ListOrchestrations(other, nil)
		if err != nil {
			t.Fatalf("list orchestrations (other workspace): %v", err)
		}
		if containsOrchestrationID(gotOther, orchID) {
			t.Errorf("ListOrchestrations({workspace=ws_y}) incorrectly returned an orchestration scoped to {workspace=ws_x, project=p1}")
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

func containsAgentID(cfgs []*agent.Config, want id.AgentID) bool {
	for _, c := range cfgs {
		if c.ID == want {
			return true
		}
	}
	return false
}

func containsSkillID(sks []*skill.Skill, want id.SkillID) bool {
	for _, sk := range sks {
		if sk.ID == want {
			return true
		}
	}
	return false
}

func containsTraitID(trs []*trait.Trait, want id.TraitID) bool {
	for _, tr := range trs {
		if tr.ID == want {
			return true
		}
	}
	return false
}

func containsBehaviorID(bs []*behavior.Behavior, want id.BehaviorID) bool {
	for _, b := range bs {
		if b.ID == want {
			return true
		}
	}
	return false
}

func containsPersonaID(ps []*persona.Persona, want id.PersonaID) bool {
	for _, p := range ps {
		if p.ID == want {
			return true
		}
	}
	return false
}

func containsOrchestrationID(cfgs []*orchestration.Config, want id.OrchestrationConfigID) bool {
	for _, c := range cfgs {
		if c.ID == want {
			return true
		}
	}
	return false
}

func containsSessionID(sessions []*session.Session, want id.SessionID) bool {
	for _, sess := range sessions {
		if sess.ID == want {
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
		agentID := mustCreateAgent(t, s, createCtx, "")

		loaded, err := s.Get(createCtx, agentID)
		if err != nil {
			t.Fatalf("get agent: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.Update(updateCtx, loaded); err != nil {
			t.Fatalf("update agent: %v", err)
		}

		reloaded, err := s.Get(createCtx, agentID)
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

	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		skillID := mustCreateSkill(t, s, createCtx, "")

		loaded, err := s.GetSkill(createCtx, skillID)
		if err != nil {
			t.Fatalf("get skill: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateSkill(updateCtx, loaded); err != nil {
			t.Fatalf("update skill: %v", err)
		}

		reloaded, err := s.GetSkill(createCtx, skillID)
		if err != nil {
			t.Fatalf("reload skill: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q", reloaded.Description, "mutated")
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		traitID := mustCreateTrait(t, s, createCtx, "")

		loaded, err := s.GetTrait(createCtx, traitID)
		if err != nil {
			t.Fatalf("get trait: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateTrait(updateCtx, loaded); err != nil {
			t.Fatalf("update trait: %v", err)
		}

		reloaded, err := s.GetTrait(createCtx, traitID)
		if err != nil {
			t.Fatalf("reload trait: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q", reloaded.Description, "mutated")
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		behaviorID := mustCreateBehavior(t, s, createCtx, "")

		loaded, err := s.GetBehavior(createCtx, behaviorID)
		if err != nil {
			t.Fatalf("get behavior: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateBehavior(updateCtx, loaded); err != nil {
			t.Fatalf("update behavior: %v", err)
		}

		reloaded, err := s.GetBehavior(createCtx, behaviorID)
		if err != nil {
			t.Fatalf("reload behavior: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q", reloaded.Description, "mutated")
		}
	})

	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		personaID := mustCreatePersona(t, s, createCtx, "")

		loaded, err := s.GetPersona(createCtx, personaID)
		if err != nil {
			t.Fatalf("get persona: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdatePersona(updateCtx, loaded); err != nil {
			t.Fatalf("update persona: %v", err)
		}

		reloaded, err := s.GetPersona(createCtx, personaID)
		if err != nil {
			t.Fatalf("reload persona: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q (the update must actually land, not silently no-op)", reloaded.Description, "mutated")
		}
	})

	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		agentID := mustCreateAgent(t, s, createCtx, "")
		sid := mustCreateSession(t, s, createCtx, agentID, "original-title")

		loaded, err := s.GetSession(createCtx, sid)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Title = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateSession(updateCtx, loaded); err != nil {
			t.Fatalf("update session: %v", err)
		}

		reloaded, err := s.GetSession(createCtx, sid)
		if err != nil {
			t.Fatalf("reload session: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Title != "mutated" {
			t.Errorf("Title after update = %q, want %q (the update must actually land, not silently no-op)", reloaded.Title, "mutated")
		}
	})

	// Suspension has no Update method, so the mutating call this group
	// has to prove immutable against is ClaimSuspension: it writes, and
	// it can be authorized by a context broader than the row's own stored
	// scope. The claim must land on the RUN and leave the suspension's
	// stored scope exactly where CreateSuspension put it.
	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		r, susp := mustSuspendRun(t, s, createCtx, nil)

		loaded, err := s.GetSuspension(createCtx, r.ID)
		if err != nil {
			t.Fatalf("get suspension: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}

		// A broader context (workspace only, no project) still
		// authorizes the claim via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		claimCtx := ctxWithScope("ws_x")
		claimed, err := s.ClaimSuspension(claimCtx, r.ID)
		if err != nil {
			t.Fatalf("claim suspension from a broader scope: %v", err)
		}
		if claimed.ID != susp.ID {
			t.Errorf("ClaimSuspension returned %s, want %s", claimed.ID, susp.ID)
		}

		reloaded, err := s.GetSuspension(createCtx, r.ID)
		if err != nil {
			t.Fatalf("reload suspension: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after claim = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		reloadedRun, err := s.GetRun(createCtx, r.ID)
		if err != nil {
			t.Fatalf("reload run: %v", err)
		}
		if reloadedRun.State != run.StateRunning {
			t.Errorf("run state after claim = %q, want %q (the claim must actually land, not silently no-op)", reloadedRun.State, run.StateRunning)
		}
		if reloadedRun.Scope.Canonical() != want {
			t.Errorf("run scope after claim = %q, want %q (scope must be immutable)", reloadedRun.Scope.Canonical(), want)
		}
	})

	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		orchID := mustCreateOrchestration(t, s, createCtx, "")

		loaded, err := s.GetOrchestration(createCtx, orchID)
		if err != nil {
			t.Fatalf("get orchestration: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Description = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateOrchestration(updateCtx, loaded); err != nil {
			t.Fatalf("update orchestration: %v", err)
		}

		reloaded, err := s.GetOrchestration(createCtx, orchID)
		if err != nil {
			t.Fatalf("reload orchestration: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Description != "mutated" {
			t.Errorf("Description after update = %q, want %q (the update must actually land, not silently no-op)", reloaded.Description, "mutated")
		}
	})

	// OrchestrationRun covers RunStore, same gap noted at
	// ZeroScopeRejection/OrchestrationRun above.
	t.Run("OrchestrationRun", func(t *testing.T) {
		s := newStore(t)
		createCtx := ctxWithScope("ws_x", "p1")
		r := mustCreateOrchestrationRun(t, s, createCtx)

		loaded, err := s.GetOrchestrationRun(createCtx, r.ID)
		if err != nil {
			t.Fatalf("get orchestration run: %v", err)
		}
		if got := loaded.Scope.Canonical(); got != want {
			t.Fatalf("scope after create = %q, want %q", got, want)
		}
		loaded.Input = "mutated"

		// A broader context (workspace only, no project) still
		// authorizes the update via prefix matching, but must not
		// collapse the row's own stored scope down to the broader one.
		updateCtx := ctxWithScope("ws_x")
		if err = s.UpdateOrchestrationRun(updateCtx, loaded); err != nil {
			t.Fatalf("update orchestration run: %v", err)
		}

		reloaded, err := s.GetOrchestrationRun(createCtx, r.ID)
		if err != nil {
			t.Fatalf("reload orchestration run: %v", err)
		}
		if reloaded.Scope.Canonical() != want {
			t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
		}
		if reloaded.Input != "mutated" {
			t.Errorf("Input after update = %q, want %q (the update must actually land, not silently no-op)", reloaded.Input, "mutated")
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
		sessionID := mustCreateSession(t, s, ctx, agentID, "")
		if err := s.SaveConversation(ctx, agentID, sessionID, []memory.Message{{Role: "user", Content: "hello"}}); err != nil {
			t.Fatalf("save conversation with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		got, err := s.LoadConversation(ctx, agentID, sessionID, 0)
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
		agentID := mustCreateAgent(t, s, ctx, "")
		got, err := s.Get(ctx, agentID)
		if err != nil {
			t.Fatalf("get agent after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Skill", func(t *testing.T) {
		s := newStore(t)
		skillID := mustCreateSkill(t, s, ctx, "")
		got, err := s.GetSkill(ctx, skillID)
		if err != nil {
			t.Fatalf("get skill after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Trait", func(t *testing.T) {
		s := newStore(t)
		traitID := mustCreateTrait(t, s, ctx, "")
		got, err := s.GetTrait(ctx, traitID)
		if err != nil {
			t.Fatalf("get trait after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Behavior", func(t *testing.T) {
		s := newStore(t)
		behaviorID := mustCreateBehavior(t, s, ctx, "")
		got, err := s.GetBehavior(ctx, behaviorID)
		if err != nil {
			t.Fatalf("get behavior after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Persona", func(t *testing.T) {
		s := newStore(t)
		personaID := mustCreatePersona(t, s, ctx, "")
		got, err := s.GetPersona(ctx, personaID)
		if err != nil {
			t.Fatalf("get persona after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Session", func(t *testing.T) {
		s := newStore(t)
		agentID := mustCreateAgent(t, s, ctx, "")
		sid := mustCreateSession(t, s, ctx, agentID, "")
		got, err := s.GetSession(ctx, sid)
		if err != nil {
			t.Fatalf("get session after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})

	t.Run("Suspension", func(t *testing.T) {
		s := newStore(t)
		r, susp := mustSuspendRun(t, s, ctx, nil)
		got, err := s.GetSuspension(ctx, r.ID)
		if err != nil {
			t.Fatalf("get suspension after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
		// The round-trip is asserted here rather than in its own subtest
		// because this is the one place a suspension is written and read
		// back with nothing else going on: a backend that mangled the
		// continuation encoding would otherwise pass every scope
		// assertion in this file with two empty structs comparing equal.
		if got.ID != susp.ID || got.Reason != susp.Reason {
			t.Errorf("round-trip = (%s, %q), want (%s, %q)", got.ID, got.Reason, susp.ID, susp.Reason)
		}
		if len(got.Pending) != 1 || got.Pending[0].Name != susp.Pending[0].Name || got.Pending[0].Arguments != susp.Pending[0].Arguments {
			t.Errorf("pending calls round-tripped as %+v, want %+v", got.Pending, susp.Pending)
		}
		if got.Cont.SystemPrompt != susp.Cont.SystemPrompt || got.Cont.StepIndex != susp.Cont.StepIndex || got.Cont.TokensUsed != susp.Cont.TokensUsed {
			t.Errorf("continuation round-tripped as %+v, want %+v", got.Cont, susp.Cont)
		}
		// A backend that drops these two hands a resume a continuation
		// that re-saves the history it loaded, into whatever session the
		// resume happens to resolve.
		if got.Cont.NewMessagesFrom != susp.Cont.NewMessagesFrom || got.Cont.SessionID != susp.Cont.SessionID {
			t.Errorf("continuation boundary/session round-tripped as (%d, %q), want (%d, %q)",
				got.Cont.NewMessagesFrom, got.Cont.SessionID, susp.Cont.NewMessagesFrom, susp.Cont.SessionID)
		}
		if len(got.Cont.Messages) != 1 || got.Cont.Messages[0].Content != susp.Cont.Messages[0].Content {
			t.Errorf("continuation messages round-tripped as %+v, want %+v", got.Cont.Messages, susp.Cont.Messages)
		}
	})

	t.Run("Orchestration", func(t *testing.T) {
		s := newStore(t)
		orchID := mustCreateOrchestration(t, s, ctx, "")
		got, err := s.GetOrchestration(ctx, orchID)
		if err != nil {
			t.Fatalf("get orchestration after create with no overflow levels: %v (scope_extra NOT NULL hazard?)", err)
		}
		if got.Scope.Canonical() != want {
			t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
		}
	})
}

// ──────────────────────────────────────────────────
// ClaimSuspension concurrency
// ──────────────────────────────────────────────────

// testClaimSuspensionConcurrency proves the claim is genuinely atomic:
// the paused-to-running transition and the suspension read happen as one
// operation, so two concurrent resumes cannot both proceed.
//
// Both subtests race against the SAME suspension. Racing two suspensions
// that cannot collide would pass against a read-then-write implementation
// too, which is to say it would prove nothing at all.
func testClaimSuspensionConcurrency(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("ClaimSuspension_ConcurrentDoubleClaimWinsOnce", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		r, susp := mustSuspendRun(t, s, ctx, nil)

		const claimers = 2
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			claims = make([]*suspension.Suspension, claimers)
			errs   = make([]error, claimers)
		)
		for i := range claimers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Both goroutines block here and are released at once, so
				// they contend for the same paused run rather than
				// arriving comfortably one after the other.
				<-start
				claims[i], errs[i] = s.ClaimSuspension(ctx, r.ID)
			}(i)
		}
		close(start)
		wg.Wait()

		var wins, losses int
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
				if claims[i] == nil || claims[i].ID != susp.ID {
					t.Errorf("winning claim returned %v, want suspension %s", claims[i], susp.ID)
				}
			case errors.Is(err, cortex.ErrNotSuspended):
				losses++
				if claims[i] != nil {
					t.Errorf("losing claim returned a suspension alongside ErrNotSuspended: %v", claims[i])
				}
			default:
				t.Fatalf("claim %d returned an unexpected error: %v", i, err)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("concurrent claims: %d won, %d lost, want exactly 1 and 1 (the claim is not atomic)", wins, losses)
		}

		reloaded, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("reload run after the race: %v", err)
		}
		if reloaded.State != run.StateRunning {
			t.Errorf("run state after the race = %q, want %q", reloaded.State, run.StateRunning)
		}
	})

	// The sequential half of the same contract. The concurrent subtest
	// above can only observe whichever interleaving the scheduler happens
	// to produce; this one pins the outcome down with no scheduling
	// involved, so a backend that let a second claim through would fail
	// deterministically rather than only under load.
	t.Run("ClaimSuspension_SecondClaimAfterWinReturnsNotSuspended", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		r, susp := mustSuspendRun(t, s, ctx, nil)

		first, err := s.ClaimSuspension(ctx, r.ID)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if first.ID != susp.ID {
			t.Fatalf("first claim returned %s, want %s", first.ID, susp.ID)
		}

		second, err := s.ClaimSuspension(ctx, r.ID)
		if !errors.Is(err, cortex.ErrNotSuspended) {
			t.Fatalf("second claim = (%v, %v), want ErrNotSuspended", second, err)
		}
		if second != nil {
			t.Errorf("second claim returned a suspension alongside ErrNotSuspended: %v", second)
		}

		// The suspension row itself survives a claim: deleting it is
		// DeleteSuspension's job, once the resume has fully completed.
		if _, err := s.GetSuspension(ctx, r.ID); err != nil {
			t.Errorf("suspension gone after a claim: %v (ClaimSuspension must not delete it)", err)
		}
	})
}

// ──────────────────────────────────────────────────
// ClaimSuspension expiry
// ──────────────────────────────────────────────────

// testClaimSuspensionExpiry proves the deadline is part of the claim
// itself, not something a caller checks after the fact.
//
// The distinction is the whole point of the group. A claim that
// transitioned the run first and reported the expiry second would leave
// the run running with a suspension nobody can claim any more, because a
// claim only ever matches a paused run. That is a wedged run: strictly
// worse than the expired-but-paused row the expiry sweep exists to clean
// up. So every subtest below asserts the run's state as well as the
// error, since the error alone cannot tell the two implementations
// apart.
//
// The three fixtures differ only in the suspension's deadline (past,
// future, absent), which is the thing under test.
func testClaimSuspensionExpiry(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("ClaimSuspension_ExpiredRefusesAndLeavesTheRunPaused", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		r, susp := mustSuspendRun(t, s, ctx, pastDeadline())

		claimed, err := s.ClaimSuspension(ctx, r.ID)
		if !errors.Is(err, cortex.ErrSuspensionExpired) {
			t.Fatalf("ClaimSuspension on an expired suspension = (%v, %v), want ErrSuspensionExpired", claimed, err)
		}
		if claimed != nil {
			t.Errorf("expired claim returned a suspension alongside the error: %v", claimed)
		}

		reloaded, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("reload run after the expired claim: %v", err)
		}
		if reloaded.State != run.StatePaused {
			t.Errorf("run state after a refused claim = %q, want %q; the run is wedged in a state no later claim can match", reloaded.State, run.StatePaused)
		}

		// The row has to survive for the expiry sweep to find. Deleting
		// it here would make the refusal unrecoverable in the other
		// direction.
		still, err := s.GetSuspension(ctx, r.ID)
		if err != nil {
			t.Fatalf("suspension gone after a refused claim: %v", err)
		}
		if still.ID != susp.ID {
			t.Errorf("GetSuspension after a refused claim = %s, want %s", still.ID, susp.ID)
		}
	})

	t.Run("ClaimSuspension_DeadlineStillAheadClaimsNormally", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		r, susp := mustSuspendRun(t, s, ctx, futureDeadline())

		claimed, err := s.ClaimSuspension(ctx, r.ID)
		if err != nil {
			t.Fatalf("ClaimSuspension with the deadline still ahead: %v", err)
		}
		if claimed.ID != susp.ID {
			t.Fatalf("claim returned %s, want %s", claimed.ID, susp.ID)
		}

		reloaded, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("reload run after the claim: %v", err)
		}
		if reloaded.State != run.StateRunning {
			t.Errorf("run state after a successful claim = %q, want %q", reloaded.State, run.StateRunning)
		}
	})

	// The other half of the classification. A paused run with no
	// suspension at all must read as ErrNotSuspended, or a caller
	// handling expiry (dropping the resume, telling a user it is too
	// late) would apply that handling to a run that was simply never
	// suspended.
	t.Run("ClaimSuspension_NoSuspensionIsNotSuspendedRatherThanExpired", func(t *testing.T) {
		s := newStore(t)
		ctx := ctxWithScope("ws_x")
		r := mustPauseRun(t, s, ctx)

		claimed, err := s.ClaimSuspension(ctx, r.ID)
		if !errors.Is(err, cortex.ErrNotSuspended) {
			t.Fatalf("ClaimSuspension on a run with no suspension = (%v, %v), want ErrNotSuspended", claimed, err)
		}

		reloaded, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("reload run after the failed claim: %v", err)
		}
		if reloaded.State != run.StatePaused {
			t.Errorf("run state after a claim that found no suspension = %q, want %q", reloaded.State, run.StatePaused)
		}
	})
}
