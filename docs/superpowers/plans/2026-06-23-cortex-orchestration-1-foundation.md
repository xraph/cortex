# Cortex Orchestration — Plan 1: Foundation & Persistence

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish the `orchestration/` package's core types and shared Blackboard, the two persisted entities (`OrchestrationConfig`, `OrchestrationRun`), their storage across all three backends (sqlite, postgres, mongo), and engine CRUD — with nothing executing yet.

**Architecture:** A new leaf package `orchestration/` defines strategy-agnostic types (`Orchestrator`, `AgentRunner`, `Result`, `Participant`, `Blackboard`) plus the two entities and their `Store` interfaces. The entities are persisted by mirroring the existing persona/run store pattern (bun/grove models + converters + programmatic migrations) across `store/sqlite`, `store/postgres`, `store/mongo`. The two `Store` interfaces are folded into the composite `store.Store` only after all three backends implement them, keeping the build green at every task boundary. The engine gains thin CRUD pass-throughs.

**Tech Stack:** Go 1.25, `go.jetify.com/typeid/v2` (IDs), `github.com/xraph/grove` + drivers (bun-style ORM/migrations), standard `testing`.

## Global Constraints

- Module path: `github.com/xraph/cortex`. Go version floor: `go 1.25.7`.
- **No `Co-Authored-By` trailers** in any commit message (user global rule).
- IDs use the `id` package's `ID` type (TypeID). New entity IDs are type aliases (`type FooID = id.ID`) with `New*`/`Parse*` helpers — never a fresh struct.
- Entities embed `cortex.Entity` (CreatedAt/UpdatedAt) and carry an `AppID string` for app-scoping.
- Store backends assert `var _ store.Store = (*Store)(nil)` — every composite-interface method must exist on all three backends or `go build` fails.
- Migrations are **programmatic** `migrate.Migration` values registered in each backend's `migrations.go`. Next version string after `20240101000008` is **`20240101000009`**.
- Table names are prefixed `cortex_`. New tables: `cortex_orchestration_configs`, `cortex_orchestration_runs`.
- sqlite/postgres store JSON-shaped fields as serialized strings via `mustJSON(...)` / `unmarshalField(...)`; mongo stores typed values directly via bson tags.
- The repo has **no store-backend unit tests** and no in-memory DB harness. Store tasks are verified by `go build`; real unit tests live in the `orchestration/` package (no DB needed).
- Run `gofmt`/`goimports` discipline: group imports stdlib / third-party / `github.com/xraph/cortex/*`.

---

## File Structure

**Created:**
- `orchestration/orchestrator.go` — `Strategy`/`Status` consts, `Participant`, `Handoff`, `AgentResult`, `RunOpts`, `AgentRunner`, `Orchestrator`, `Result`, `Settings`.
- `orchestration/blackboard.go` — `Blackboard` (shared state + entry log + roster + handoff callback).
- `orchestration/config.go` — `OrchestrationConfig` entity + `ConfigStore` interface + `ConfigListFilter`.
- `orchestration/run.go` — `OrchestrationRun` entity + `RunStore` interface + `RunListFilter`.
- `orchestration/orchestrator_test.go`, `orchestration/blackboard_test.go`, `orchestration/config_test.go`, `orchestration/run_test.go`.
- `store/sqlite/orchestration.go`, `store/postgres/orchestration.go`, `store/mongo/orchestration.go` — CRUD impls.
- `engine/orchestration_crud.go` — engine CRUD pass-throughs.

**Modified:**
- `id/id.go` (+ `id/id_test.go`) — add `orchcfg` prefix + helpers.
- `errors.go` — add `ErrOrchestrationNotFound`, `ErrOrchestrationRunNotFound`.
- `store/{sqlite,postgres,mongo}/models.go` — add model structs + converters.
- `store/{sqlite,postgres,mongo}/migrations.go` — add version `20240101000009`.
- `store/store.go` — fold `ConfigStore` + `RunStore` into the composite interface.

---

## Task 1: OrchestrationConfig ID prefix + error sentinels

**Files:**
- Modify: `id/id.go`
- Modify: `errors.go`
- Test: `id/id_test.go`

**Interfaces:**
- Produces: `id.PrefixOrchestrationConfig` (`Prefix`), `id.OrchestrationConfigID` (alias of `id.ID`), `id.NewOrchestrationConfigID() id.ID`, `id.ParseOrchestrationConfigID(string) (id.ID, error)`; sentinels `cortex.ErrOrchestrationNotFound`, `cortex.ErrOrchestrationRunNotFound`.
- Consumes: nothing.

- [ ] **Step 1: Write the failing test**

Add these cases to the existing tables in `id/id_test.go`. Find the constructor/prefix table (contains `{"OrchestrationID", id.NewOrchestrationID, "orch_"}`) and add a sibling entry; find the parse round-trip table (`{"OrchestrationID", id.NewOrchestrationID, id.ParseOrchestrationID}`) and add a sibling; add one standalone test for prefix isolation:

```go
func TestOrchestrationConfigID(t *testing.T) {
	got := id.NewOrchestrationConfigID()
	if got.Prefix() != id.PrefixOrchestrationConfig {
		t.Fatalf("prefix = %q, want %q", got.Prefix(), id.PrefixOrchestrationConfig)
	}
	if !strings.HasPrefix(got.String(), "orchcfg_") {
		t.Fatalf("string = %q, want orchcfg_ prefix", got.String())
	}
	parsed, err := id.ParseOrchestrationConfigID(got.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.String() != got.String() {
		t.Fatalf("round-trip mismatch: %q != %q", parsed.String(), got.String())
	}
	// orch_ (run) ids must be rejected by the config parser.
	if _, err := id.ParseOrchestrationConfigID(id.NewOrchestrationID().String()); err == nil {
		t.Fatal("expected ParseOrchestrationConfigID to reject an orch_ id")
	}
}
```

Ensure `id/id_test.go` imports `strings` (add to its import block if absent).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./id/ -run TestOrchestrationConfigID -v`
Expected: FAIL — `undefined: id.NewOrchestrationConfigID` (compile error).

- [ ] **Step 3: Write minimal implementation**

In `id/id.go`, add the prefix constant inside the existing `const (...)` Prefix block (next to `PrefixOrchestration`):

```go
	PrefixOrchestrationConfig Prefix = "orchcfg"
```

Add the type alias next to `OrchestrationID`:

```go
// OrchestrationConfigID is a type-safe identifier for orchestration configs (prefix: "orchcfg").
type OrchestrationConfigID = ID
```

Add the constructor next to `NewOrchestrationID`:

```go
// NewOrchestrationConfigID generates a new unique orchestration config ID.
func NewOrchestrationConfigID() ID { return New(PrefixOrchestrationConfig) }
```

Add the parser next to `ParseOrchestrationID`:

```go
// ParseOrchestrationConfigID parses a string and validates the "orchcfg" prefix.
func ParseOrchestrationConfigID(s string) (ID, error) { return ParseWithPrefix(s, PrefixOrchestrationConfig) }
```

In `errors.go`, add inside the existing error `var (...)` block (next to `ErrPersonaNotFound`):

```go
	ErrOrchestrationNotFound    = errors.New("cortex: orchestration not found")
	ErrOrchestrationRunNotFound = errors.New("cortex: orchestration run not found")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./id/ -run TestOrchestrationConfigID -v && go build ./...`
Expected: PASS, and the build succeeds.

- [ ] **Step 5: Commit**

```bash
git add id/id.go id/id_test.go errors.go
git commit -m "feat(id): add orchcfg prefix and orchestration error sentinels"
```

---

## Task 2: Orchestration core types

**Files:**
- Create: `orchestration/orchestrator.go`
- Test: `orchestration/orchestrator_test.go`

**Interfaces:**
- Consumes: `id`, `cortex` (none of Task 1's helpers yet).
- Produces: `orchestration.Strategy*` consts, `Participant`, `Handoff`, `AgentResult`, `RunOpts`, `AgentRunner`, `Orchestrator`, `Result`, `Settings`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/orchestrator_test.go`:

```go
package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestStrategyConstants(t *testing.T) {
	cases := map[string]string{
		orchestration.StrategySequential:   "sequential",
		orchestration.StrategyParallel:     "parallel",
		orchestration.StrategyRouter:       "router",
		orchestration.StrategyHierarchical: "hierarchical",
		orchestration.StrategyDebate:       "debate",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("strategy constant = %q, want %q", got, want)
		}
	}
}

func TestResultConstruction(t *testing.T) {
	r := &orchestration.Result{
		OrchestrationID: id.NewOrchestrationID(),
		Strategy:        orchestration.StrategySequential,
		Output:          "done",
		AgentResults: []orchestration.AgentResult{
			{AgentName: "writer", Output: "draft"},
		},
		Handoffs: []orchestration.Handoff{{From: "writer", To: "editor", Payload: "draft"}},
	}
	if r.Output != "done" || len(r.AgentResults) != 1 || len(r.Handoffs) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -v`
Expected: FAIL — `package github.com/xraph/cortex/orchestration is not in std` / no Go files.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/orchestrator.go`:

```go
// Package orchestration coordinates multiple Cortex agents working together —
// in sequence, in parallel, hierarchically, via a router, or in a debate.
//
// It is a leaf package: it depends only on id, cortex, and llm, and reaches
// host capabilities (running an agent, emitting plugin hooks) through injected
// interfaces and callbacks, never by importing the engine or plugin packages.
package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// Strategy identifiers. A stored OrchestrationConfig.Strategy is one of these.
const (
	StrategySequential   = "sequential"
	StrategyParallel     = "parallel"
	StrategyRouter       = "router"
	StrategyHierarchical = "hierarchical"
	StrategyDebate       = "debate"
)

// Participant is one agent in an orchestration, with metadata used for awareness:
// the roster of participants is surfaced to each agent so it knows who else is
// taking part and in what role.
type Participant struct {
	AgentName string   `json:"agent_name"`
	Role      string   `json:"role,omitempty"`   // e.g. "manager", "worker", "critic", "judge"
	Skills    []string `json:"skills,omitempty"` // advisory; shown in the roster
}

// Handoff records one agent passing work to another.
type Handoff struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload,omitempty"`
}

// RunOpts is the subset of engine run overrides an orchestrator needs when
// invoking an agent. It is mapped to engine.RunOverrides by the host adapter.
type RunOpts struct {
	Model        string
	Temperature  *float64
	MaxSteps     int
	SystemPrompt string
}

// AgentResult is the strategy-facing view of one completed agent run.
type AgentResult struct {
	AgentName string        `json:"agent_name"`
	RunID     id.AgentRunID `json:"run_id,omitempty"`
	Output    string        `json:"output"`
	Err       error         `json:"-"`
}

// AgentRunner is the single host capability an orchestrator depends on: the
// ability to run one named agent and get its result. The engine satisfies it
// via a thin adapter, avoiding an engine⇄orchestration import cycle.
type AgentRunner interface {
	RunAgent(ctx context.Context, appID, agentName, input string, opts *RunOpts) (*AgentResult, error)
}

// Settings carries every strategy's tunables in one struct. Fields a given
// strategy does not use are ignored.
type Settings struct {
	MaxConcurrency int               `json:"max_concurrency,omitempty"` // parallel / hierarchical worker fan-out cap
	Rounds         int               `json:"rounds,omitempty"`          // debate rounds
	Manager        string            `json:"manager,omitempty"`         // hierarchical: manager agent name
	Judge          string            `json:"judge,omitempty"`           // debate: judge agent name
	Aggregator     string            `json:"aggregator,omitempty"`      // parallel: optional aggregator agent name
	RouterAgent    string            `json:"router_agent,omitempty"`    // router: agent that decides (optional)
	RouterRules    map[string]string `json:"router_rules,omitempty"`    // router: keyword→agent static rules
	Model          string            `json:"model,omitempty"`           // model for meta-decisions (router/judge/manager)
}

// Result is the outcome of an orchestration run.
type Result struct {
	OrchestrationID id.OrchestrationID `json:"orchestration_id"`
	Strategy        string             `json:"strategy"`
	Output          string             `json:"output"`
	AgentResults    []AgentResult      `json:"agent_results,omitempty"`
	Handoffs        []Handoff          `json:"handoffs,omitempty"`
	Elapsed         time.Duration      `json:"elapsed"`
	Err             error              `json:"-"`
}
```

> The `Orchestrator` interface is intentionally **not** defined here: it references `*Blackboard`, which does not exist until Task 3. Adding it now would leave the package uncompilable at this task boundary. Task 3 appends it to this same file once `Blackboard` exists.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -v`
Expected: PASS (`TestStrategyConstants`, `TestResultConstruction`). The package compiles cleanly — nothing here references `Blackboard`.

- [ ] **Step 5: Commit**

```bash
git add orchestration/orchestrator.go orchestration/orchestrator_test.go
git commit -m "feat(orchestration): add core strategy types and AgentRunner seam"
```

---

## Task 3: Blackboard (shared state, roster, handoffs)

**Files:**
- Create: `orchestration/blackboard.go`
- Test: `orchestration/blackboard_test.go`

**Interfaces:**
- Consumes: `Participant`, `Handoff` (Task 2).
- Produces: `Blackboard`, `NewBlackboard(orchID, participants, onHandoff) *Blackboard`, methods `Read/Write/Append/Snapshot/Roster/Handoff/Handoffs/Entries`. `HandoffFunc` callback type.

- [ ] **Step 1: Write the failing test**

Create `orchestration/blackboard_test.go`:

```go
package orchestration_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestBlackboardReadWrite(t *testing.T) {
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, nil)
	bb.Write("k", "v")
	got, ok := bb.Read("k")
	if !ok || got != "v" {
		t.Fatalf("Read = %v, %v; want v, true", got, ok)
	}
	if _, ok := bb.Read("missing"); ok {
		t.Fatal("expected missing key to report ok=false")
	}
}

func TestBlackboardSnapshotAndRoster(t *testing.T) {
	parts := []orchestration.Participant{
		{AgentName: "writer", Role: "author"},
		{AgentName: "editor", Role: "critic"},
	}
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), parts, nil)
	bb.Append("writer", "first draft")
	snap := bb.Snapshot()
	if !strings.Contains(snap, "writer") || !strings.Contains(snap, "first draft") {
		t.Fatalf("snapshot missing contribution: %q", snap)
	}
	if !strings.Contains(snap, "editor") {
		t.Fatalf("snapshot missing roster member: %q", snap)
	}
	if len(bb.Roster()) != 2 {
		t.Fatalf("roster len = %d, want 2", len(bb.Roster()))
	}
}

func TestBlackboardHandoffFiresCallback(t *testing.T) {
	var got orchestration.Handoff
	called := 0
	cb := func(_ context.Context, h orchestration.Handoff) {
		called++
		got = h
	}
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, cb)
	bb.Handoff(context.Background(), "writer", "editor", "draft")
	if called != 1 {
		t.Fatalf("callback called %d times, want 1", called)
	}
	if got.From != "writer" || got.To != "editor" || got.Payload != "draft" {
		t.Fatalf("handoff = %+v", got)
	}
	if len(bb.Handoffs()) != 1 {
		t.Fatalf("recorded handoffs = %d, want 1", len(bb.Handoffs()))
	}
}

func TestBlackboardConcurrentAccess(t *testing.T) {
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bb.Write("k", n)
			bb.Append("agent", "msg")
			_, _ = bb.Read("k")
			_ = bb.Snapshot()
		}(i)
	}
	wg.Wait()
	if len(bb.Entries()) != 50 {
		t.Fatalf("entries = %d, want 50", len(bb.Entries()))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestBlackboard -v`
Expected: FAIL — `undefined: orchestration.NewBlackboard`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/blackboard.go`:

```go
package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/xraph/cortex/id"
)

// HandoffFunc is invoked whenever an agent hands off to another. The engine
// wires this to plugin hook emission; tests pass their own.
type HandoffFunc func(ctx context.Context, h Handoff)

// Entry is one timestamp-ordered contribution recorded on the blackboard.
type Entry struct {
	AgentName string
	Content   string
}

// Blackboard is the shared, mutex-guarded state for a single orchestration run.
// Every participant can read/write the value map, append contributions, inspect
// the participant roster (awareness), and record handoffs (communication).
type Blackboard struct {
	orchID   id.OrchestrationID
	mu       sync.RWMutex
	values   map[string]any
	entries  []Entry
	roster   []Participant
	handoffs []Handoff
	onHandoff HandoffFunc
}

// NewBlackboard creates a blackboard for the given orchestration. participants
// seed the roster; onHandoff may be nil.
func NewBlackboard(orchID id.OrchestrationID, participants []Participant, onHandoff HandoffFunc) *Blackboard {
	roster := make([]Participant, len(participants))
	copy(roster, participants)
	return &Blackboard{
		orchID:    orchID,
		values:    make(map[string]any),
		roster:    roster,
		onHandoff: onHandoff,
	}
}

// Read returns the value for key and whether it was present.
func (b *Blackboard) Read(key string) (any, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.values[key]
	return v, ok
}

// Write sets a shared value.
func (b *Blackboard) Write(key string, val any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values[key] = val
}

// Append records a contribution from an agent into the ordered log.
func (b *Blackboard) Append(agentName, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, Entry{AgentName: agentName, Content: content})
}

// Roster returns a copy of the participant roster.
func (b *Blackboard) Roster() []Participant {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Participant, len(b.roster))
	copy(out, b.roster)
	return out
}

// Entries returns a copy of the contribution log.
func (b *Blackboard) Entries() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Entry, len(b.entries))
	copy(out, b.entries)
	return out
}

// Handoffs returns a copy of the recorded handoff log.
func (b *Blackboard) Handoffs() []Handoff {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Handoff, len(b.handoffs))
	copy(out, b.handoffs)
	return out
}

// Handoff records a from→to→payload handoff and fires the callback (if set).
func (b *Blackboard) Handoff(ctx context.Context, from, to, payload string) {
	h := Handoff{From: from, To: to, Payload: payload}
	b.mu.Lock()
	b.handoffs = append(b.handoffs, h)
	cb := b.onHandoff
	b.mu.Unlock()
	if cb != nil {
		cb(ctx, h)
	}
}

// Snapshot renders the roster and prior contributions into a compact text block
// that strategies prepend to an agent's input, giving each agent awareness of
// who else is participating and what has been said so far. Returns "" when empty.
func (b *Blackboard) Snapshot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.roster) == 0 && len(b.entries) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(b.roster) > 0 {
		sb.WriteString("Participants in this collaboration:\n")
		for _, p := range b.roster {
			if p.Role != "" {
				fmt.Fprintf(&sb, "- %s (%s)\n", p.AgentName, p.Role)
			} else {
				fmt.Fprintf(&sb, "- %s\n", p.AgentName)
			}
		}
	}
	if len(b.entries) > 0 {
		sb.WriteString("\nWork so far:\n")
		for _, e := range b.entries {
			fmt.Fprintf(&sb, "[%s]: %s\n", e.AgentName, e.Content)
		}
	}
	return sb.String()
}
```

- [ ] **Step 4: Append the `Orchestrator` interface to `orchestrator.go`**

Now that `Blackboard` exists, add the interface to the **end of `orchestration/orchestrator.go`** (deferred from Task 2):

```go

// Orchestrator is one multi-agent execution strategy. Implementations live in
// Plan 2 (sequential.go, parallel.go, router.go, hierarchical.go, debate.go).
type Orchestrator interface {
	Strategy() string
	Run(ctx context.Context, input string, bb *Blackboard) (*Result, error)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./orchestration/ -race -v`
Expected: PASS for all `TestBlackboard*` and the Task 2 tests; no race detected; package compiles with `Orchestrator` defined.

- [ ] **Step 6: Commit**

```bash
git add orchestration/blackboard.go orchestration/blackboard_test.go orchestration/orchestrator.go
git commit -m "feat(orchestration): add shared Blackboard with roster and handoffs"
```

---

## Task 4: OrchestrationConfig entity + Store interface

**Files:**
- Create: `orchestration/config.go`
- Test: `orchestration/config_test.go`

**Interfaces:**
- Consumes: `Participant`, `Settings` (Task 2); `id.OrchestrationConfigID` (Task 1); `cortex.Entity`.
- Produces: `OrchestrationConfig`, `ConfigStore`, `ConfigListFilter`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/config_test.go`:

```go
package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationConfigFields(t *testing.T) {
	c := &orchestration.OrchestrationConfig{
		Entity:   cortex.NewEntity(),
		ID:       id.NewOrchestrationConfigID(),
		Name:     "research-team",
		AppID:    "app1",
		Strategy: orchestration.StrategyDebate,
		Participants: []orchestration.Participant{
			{AgentName: "optimist", Role: "debater"},
			{AgentName: "skeptic", Role: "debater"},
			{AgentName: "judge", Role: "judge"},
		},
		Settings: orchestration.Settings{Rounds: 2, Judge: "judge"},
	}
	if c.Name != "research-team" || c.Strategy != "debate" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if len(c.Participants) != 3 || c.Settings.Rounds != 2 {
		t.Fatalf("unexpected participants/settings: %+v", c)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestOrchestrationConfigFields -v`
Expected: FAIL — `undefined: orchestration.OrchestrationConfig`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/config.go`:

```go
package orchestration

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// OrchestrationConfig is a stored, named definition of a multi-agent
// orchestration: a strategy plus its participant agents and tunables.
type OrchestrationConfig struct {
	cortex.Entity
	ID           id.OrchestrationConfigID `json:"id"`
	Name         string                   `json:"name"`
	Description  string                   `json:"description,omitempty"`
	AppID        string                   `json:"app_id"`
	Strategy     string                   `json:"strategy"`
	Participants []Participant            `json:"participants"`
	Settings     Settings                 `json:"settings,omitempty"`
	Metadata     map[string]any           `json:"metadata,omitempty"`
}

// ConfigStore defines persistence for orchestration configs.
type ConfigStore interface {
	CreateOrchestration(ctx context.Context, c *OrchestrationConfig) error
	GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*OrchestrationConfig, error)
	GetOrchestrationByName(ctx context.Context, appID, name string) (*OrchestrationConfig, error)
	UpdateOrchestration(ctx context.Context, c *OrchestrationConfig) error
	DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error
	ListOrchestrations(ctx context.Context, filter *ConfigListFilter) ([]*OrchestrationConfig, error)
	CountOrchestrations(ctx context.Context, filter *ConfigListFilter) (int64, error)
}

// ConfigListFilter controls pagination and filtering for orchestration listing.
type ConfigListFilter struct {
	AppID  string
	Search string
	Limit  int
	Offset int
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestOrchestrationConfigFields -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add orchestration/config.go orchestration/config_test.go
git commit -m "feat(orchestration): add OrchestrationConfig entity and ConfigStore"
```

---

## Task 5: OrchestrationRun entity + Store interface

**Files:**
- Create: `orchestration/run.go`
- Test: `orchestration/run_test.go`

**Interfaces:**
- Consumes: `id.OrchestrationID`, `id.OrchestrationConfigID`, `id.AgentRunID`; `cortex.Entity`.
- Produces: `Status*` consts, `OrchestrationRun`, `RunStore`, `RunListFilter`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/run_test.go`:

```go
package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationRunFields(t *testing.T) {
	r := &orchestration.OrchestrationRun{
		Entity:      cortex.NewEntity(),
		ID:          id.NewOrchestrationID(),
		ConfigID:    id.NewOrchestrationConfigID(),
		AppID:       "app1",
		Strategy:    orchestration.StrategySequential,
		Status:      orchestration.StatusRunning,
		Input:       "hello",
		AgentRunIDs: []id.AgentRunID{id.NewAgentRunID()},
	}
	if r.Status != "running" || r.Strategy != "sequential" {
		t.Fatalf("unexpected run: %+v", r)
	}
	if len(r.AgentRunIDs) != 1 {
		t.Fatalf("agent run ids = %d, want 1", len(r.AgentRunIDs))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestOrchestrationRunFields -v`
Expected: FAIL — `undefined: orchestration.OrchestrationRun`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/run.go`:

```go
package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Orchestration run status values.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// OrchestrationRun is the persisted execution record of one orchestration.
type OrchestrationRun struct {
	cortex.Entity
	ID          id.OrchestrationID       `json:"id"`
	ConfigID    id.OrchestrationConfigID `json:"config_id,omitempty"` // empty for programmatic runs
	AppID       string                   `json:"app_id"`
	TenantID    string                   `json:"tenant_id,omitempty"`
	Strategy    string                   `json:"strategy"`
	Status      string                   `json:"status"`
	Input       string                   `json:"input"`
	Output      string                   `json:"output,omitempty"`
	Error       string                   `json:"error,omitempty"`
	AgentRunIDs []id.AgentRunID          `json:"agent_run_ids,omitempty"`
	StartedAt   time.Time                `json:"started_at"`
	CompletedAt *time.Time               `json:"completed_at,omitempty"`
}

// RunStore defines persistence for orchestration run records.
type RunStore interface {
	CreateOrchestrationRun(ctx context.Context, r *OrchestrationRun) error
	GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*OrchestrationRun, error)
	UpdateOrchestrationRun(ctx context.Context, r *OrchestrationRun) error
	ListOrchestrationRuns(ctx context.Context, filter *RunListFilter) ([]*OrchestrationRun, error)
	CountOrchestrationRuns(ctx context.Context, filter *RunListFilter) (int64, error)
}

// RunListFilter controls pagination and filtering for orchestration run listing.
type RunListFilter struct {
	AppID  string
	Status string
	Limit  int
	Offset int
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -v`
Expected: PASS (all orchestration tests).

- [ ] **Step 5: Commit**

```bash
git add orchestration/run.go orchestration/run_test.go
git commit -m "feat(orchestration): add OrchestrationRun entity and RunStore"
```

---

## Task 6: sqlite backend (models, converters, migration, CRUD)

**Files:**
- Modify: `store/sqlite/models.go`
- Modify: `store/sqlite/migrations.go`
- Create: `store/sqlite/orchestration.go`

**Interfaces:**
- Consumes: `orchestration.OrchestrationConfig`, `orchestration.OrchestrationRun`, `*orchestration.ConfigListFilter`, `*orchestration.RunListFilter`; `id.ParseOrchestrationConfigID`, `id.ParseOrchestrationID`; `cortex.ErrOrchestrationNotFound`, `cortex.ErrOrchestrationRunNotFound`.
- Produces: methods on `*sqlite.Store` matching `orchestration.ConfigStore` + `orchestration.RunStore`.

- [ ] **Step 1: Add model structs + converters**

Append to `store/sqlite/models.go` (before the JSON-helper section). The run model stores `AgentRunIDs` and `Participants`/`Settings` as JSON strings:

```go
// ──────────────────────────────────────────────────
// Orchestration config model
// ──────────────────────────────────────────────────

type orchestrationConfigModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_configs"`
	ID              string    `grove:"id,pk"`
	Name            string    `grove:"name,notnull"`
	Description     string    `grove:"description"`
	AppID           string    `grove:"app_id,notnull"`
	Strategy        string    `grove:"strategy"`
	Participants    string    `grove:"participants"`
	Settings        string    `grove:"settings"`
	Metadata        string    `grove:"metadata"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
}

func orchestrationConfigToModel(c *orchestration.OrchestrationConfig) *orchestrationConfigModel {
	return &orchestrationConfigModel{
		ID:           c.ID.String(),
		Name:         c.Name,
		Description:  c.Description,
		AppID:        c.AppID,
		Strategy:     c.Strategy,
		Participants: mustJSON(c.Participants),
		Settings:     mustJSON(c.Settings),
		Metadata:     mustJSON(c.Metadata),
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func orchestrationConfigFromModel(m *orchestrationConfigModel) (*orchestration.OrchestrationConfig, error) {
	cfgID, err := id.ParseOrchestrationConfigID(m.ID)
	if err != nil {
		return nil, err
	}
	c := &orchestration.OrchestrationConfig{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          cfgID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Strategy:    m.Strategy,
	}
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"participants", m.Participants, &c.Participants},
		{"settings", m.Settings, &c.Settings},
		{"metadata", m.Metadata, &c.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// ──────────────────────────────────────────────────
// Orchestration run model
// ──────────────────────────────────────────────────

type orchestrationRunModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_runs"`
	ID              string     `grove:"id,pk"`
	ConfigID        string     `grove:"config_id"`
	AppID           string     `grove:"app_id,notnull"`
	TenantID        string     `grove:"tenant_id"`
	Strategy        string     `grove:"strategy"`
	Status          string     `grove:"status,notnull"`
	Input           string     `grove:"input"`
	Output          string     `grove:"output"`
	Error           string     `grove:"error"`
	AgentRunIDs     string     `grove:"agent_run_ids"`
	StartedAt       time.Time  `grove:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"`
	CreatedAt       time.Time  `grove:"created_at"`
	UpdatedAt       time.Time  `grove:"updated_at"`
}

func orchestrationRunToModel(r *orchestration.OrchestrationRun) *orchestrationRunModel {
	runIDs := make([]string, len(r.AgentRunIDs))
	for i, rid := range r.AgentRunIDs {
		runIDs[i] = rid.String()
	}
	return &orchestrationRunModel{
		ID:          r.ID.String(),
		ConfigID:    r.ConfigID.String(),
		AppID:       r.AppID,
		TenantID:    r.TenantID,
		Strategy:    r.Strategy,
		Status:      r.Status,
		Input:       r.Input,
		Output:      r.Output,
		Error:       r.Error,
		AgentRunIDs: mustJSON(runIDs),
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func orchestrationRunFromModel(m *orchestrationRunModel) (*orchestration.OrchestrationRun, error) {
	runID, err := id.ParseOrchestrationID(m.ID)
	if err != nil {
		return nil, err
	}
	r := &orchestration.OrchestrationRun{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AppID:       m.AppID,
		TenantID:    m.TenantID,
		Strategy:    m.Strategy,
		Status:      m.Status,
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
	}
	if m.ConfigID != "" {
		cfgID, cerr := id.ParseOrchestrationConfigID(m.ConfigID)
		if cerr != nil {
			return nil, cerr
		}
		r.ConfigID = cfgID
	}
	var runIDStrings []string
	if err := unmarshalField("agent_run_ids", m.AgentRunIDs, &runIDStrings); err != nil {
		return nil, err
	}
	for _, s := range runIDStrings {
		rid, perr := id.ParseAgentRunID(s)
		if perr != nil {
			return nil, perr
		}
		r.AgentRunIDs = append(r.AgentRunIDs, rid)
	}
	return r, nil
}
```

Add `"github.com/xraph/cortex/orchestration"` to the import block of `store/sqlite/models.go`.

- [ ] **Step 2: Add the migration**

In `store/sqlite/migrations.go`, add this `&migrate.Migration{...}` as the final entry in the `Migrations.MustRegister(...)` call (after `create_behaviors_personas`):

```go
		&migrate.Migration{
			Name:    "create_orchestrations",
			Version: "20240101000009",
			Comment: "Create cortex_orchestration_configs and cortex_orchestration_runs tables",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_orchestration_configs (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    app_id        TEXT NOT NULL,
    strategy      TEXT NOT NULL DEFAULT '',
    participants  TEXT NOT NULL DEFAULT '[]',
    settings      TEXT NOT NULL DEFAULT '{}',
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_app_name ON cortex_orchestration_configs (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_orchestration_runs (
    id             TEXT PRIMARY KEY,
    config_id      TEXT NOT NULL DEFAULT '',
    app_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT '',
    strategy       TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running',
    input          TEXT NOT NULL DEFAULT '',
    output         TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    agent_run_ids  TEXT NOT NULL DEFAULT '[]',
    started_at     TEXT,
    completed_at   TEXT,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_orchestration_runs_app_status ON cortex_orchestration_runs (app_id, status);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_orchestration_runs;
DROP TABLE IF EXISTS cortex_orchestration_configs;
`)
				return err
			},
		},
```

> Note: `started_at` is declared `TEXT` (nullable) even though the Go field is non-pointer `time.Time`; grove writes the zero/explicit value fine. `completed_at` is a pointer and maps to nullable `TEXT`.

- [ ] **Step 3: Add the CRUD implementation**

Create `store/sqlite/orchestration.go` (mirrors `store/sqlite/persona.go`):

```go
package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if _, err := s.sdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: create orchestration: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	if err := s.sdb.NewSelect(m).Where("id = ?", orchID.String()).Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	err := s.sdb.NewSelect(m).Where("app_id = ?", appID).Where("name = ?", name).Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration by name: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	c.UpdatedAt = time.Now().UTC()
	res, err := s.sdb.NewUpdate(orchestrationConfigToModel(c)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	res, err := s.sdb.NewDelete((*orchestrationConfigModel)(nil)).Where("id = ?", orchID.String()).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	var models []orchestrationConfigModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Search != "" {
			q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list orchestrations: %w", err)
	}
	result := make([]*orchestration.OrchestrationConfig, len(models))
	for i := range models {
		c, convErr := orchestrationConfigFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = c
	}
	return result, nil
}

func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	q := s.sdb.NewSelect((*orchestrationConfigModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Search != "" {
			q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count orchestrations: %w", err)
	}
	return count, nil
}

func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if _, err := s.sdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: create orchestration run: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	m := new(orchestrationRunModel)
	if err := s.sdb.NewSelect(m).Where("id = ?", runID.String()).Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration run: %w", err)
	}
	return orchestrationRunFromModel(m)
}

func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	r.UpdatedAt = time.Now().UTC()
	res, err := s.sdb.NewUpdate(orchestrationRunToModel(r)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration run: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration run rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}
	return nil
}

func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	var models []orchestrationRunModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at DESC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list orchestration runs: %w", err)
	}
	result := make([]*orchestration.OrchestrationRun, len(models))
	for i := range models {
		r, convErr := orchestrationRunFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = r
	}
	return result, nil
}

func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	q := s.sdb.NewSelect((*orchestrationRunModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count orchestration runs: %w", err)
	}
	return count, nil
}
```

> grove's `q.Count(ctx)` returns `(int64, error)` (verified against `store/sqlite/persona.go:CountPersonas`), so `count` is returned directly with no conversion.

- [ ] **Step 4: Verify it builds**

Run: `go build ./store/sqlite/`
Expected: builds with no errors.

- [ ] **Step 5: Commit**

```bash
git add store/sqlite/models.go store/sqlite/migrations.go store/sqlite/orchestration.go
git commit -m "feat(store/sqlite): persist orchestration configs and runs"
```

---

## Task 7: postgres backend (models, converters, migration, CRUD)

**Files:**
- Modify: `store/postgres/models.go`
- Modify: `store/postgres/migrations.go`
- Create: `store/postgres/orchestration.go`

**Interfaces:**
- Consumes: same as Task 6.
- Produces: methods on `*postgres.Store` matching `ConfigStore` + `RunStore`.

> Verified idioms (from `store/postgres/persona.go` + `models.go`): postgres stores JSON fields as `string` with the grove tag suffix `,type:jsonb`, timestamps as `time.Time` with `,notnull,default:current_timestamp`, uses the driver field `s.pgdb`, error-message prefix `cortex:` (no backend suffix), and inline `errors.Is(err, sql.ErrNoRows)` (no `isNoRows` helper). `mustJSON`/`unmarshalField` already exist in `store/postgres/models.go`.

- [ ] **Step 1: Add model structs + converters**

Append to `store/postgres/models.go` (add `"github.com/xraph/cortex/orchestration"` import). The **converter bodies are identical to Task 6**, but the model grove tags differ (`,type:jsonb`, timestamp defaults):

```go
// ──────────────────────────────────────────────────
// Orchestration config model
// ──────────────────────────────────────────────────

type orchestrationConfigModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_configs"`
	ID              string    `grove:"id,pk"`
	Name            string    `grove:"name,notnull"`
	Description     string    `grove:"description"`
	AppID           string    `grove:"app_id,notnull"`
	Strategy        string    `grove:"strategy"`
	Participants    string    `grove:"participants,type:jsonb"`
	Settings        string    `grove:"settings,type:jsonb"`
	Metadata        string    `grove:"metadata,type:jsonb"`
	CreatedAt       time.Time `grove:"created_at,notnull,default:current_timestamp"`
	UpdatedAt       time.Time `grove:"updated_at,notnull,default:current_timestamp"`
}

func orchestrationConfigToModel(c *orchestration.OrchestrationConfig) *orchestrationConfigModel {
	return &orchestrationConfigModel{
		ID:           c.ID.String(),
		Name:         c.Name,
		Description:  c.Description,
		AppID:        c.AppID,
		Strategy:     c.Strategy,
		Participants: mustJSON(c.Participants),
		Settings:     mustJSON(c.Settings),
		Metadata:     mustJSON(c.Metadata),
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func orchestrationConfigFromModel(m *orchestrationConfigModel) (*orchestration.OrchestrationConfig, error) {
	cfgID, err := id.ParseOrchestrationConfigID(m.ID)
	if err != nil {
		return nil, err
	}
	c := &orchestration.OrchestrationConfig{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          cfgID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Strategy:    m.Strategy,
	}
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"participants", m.Participants, &c.Participants},
		{"settings", m.Settings, &c.Settings},
		{"metadata", m.Metadata, &c.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// ──────────────────────────────────────────────────
// Orchestration run model
// ──────────────────────────────────────────────────

type orchestrationRunModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_runs"`
	ID              string     `grove:"id,pk"`
	ConfigID        string     `grove:"config_id"`
	AppID           string     `grove:"app_id,notnull"`
	TenantID        string     `grove:"tenant_id"`
	Strategy        string     `grove:"strategy"`
	Status          string     `grove:"status,notnull"`
	Input           string     `grove:"input"`
	Output          string     `grove:"output"`
	Error           string     `grove:"error"`
	AgentRunIDs     string     `grove:"agent_run_ids,type:jsonb"`
	StartedAt       time.Time  `grove:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"`
	CreatedAt       time.Time  `grove:"created_at,notnull,default:current_timestamp"`
	UpdatedAt       time.Time  `grove:"updated_at,notnull,default:current_timestamp"`
}

func orchestrationRunToModel(r *orchestration.OrchestrationRun) *orchestrationRunModel {
	runIDs := make([]string, len(r.AgentRunIDs))
	for i, rid := range r.AgentRunIDs {
		runIDs[i] = rid.String()
	}
	return &orchestrationRunModel{
		ID:          r.ID.String(),
		ConfigID:    r.ConfigID.String(),
		AppID:       r.AppID,
		TenantID:    r.TenantID,
		Strategy:    r.Strategy,
		Status:      r.Status,
		Input:       r.Input,
		Output:      r.Output,
		Error:       r.Error,
		AgentRunIDs: mustJSON(runIDs),
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func orchestrationRunFromModel(m *orchestrationRunModel) (*orchestration.OrchestrationRun, error) {
	runID, err := id.ParseOrchestrationID(m.ID)
	if err != nil {
		return nil, err
	}
	r := &orchestration.OrchestrationRun{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AppID:       m.AppID,
		TenantID:    m.TenantID,
		Strategy:    m.Strategy,
		Status:      m.Status,
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
	}
	if m.ConfigID != "" {
		cfgID, cerr := id.ParseOrchestrationConfigID(m.ConfigID)
		if cerr != nil {
			return nil, cerr
		}
		r.ConfigID = cfgID
	}
	var runIDStrings []string
	if err := unmarshalField("agent_run_ids", m.AgentRunIDs, &runIDStrings); err != nil {
		return nil, err
	}
	for _, s := range runIDStrings {
		rid, perr := id.ParseAgentRunID(s)
		if perr != nil {
			return nil, perr
		}
		r.AgentRunIDs = append(r.AgentRunIDs, rid)
	}
	return r, nil
}
```

- [ ] **Step 2: Add the migration**

In `store/postgres/migrations.go`, add as the final `&migrate.Migration{...}` in the `g.MustRegister(...)` call. Use postgres column types (`JSONB`, `TIMESTAMPTZ`, `BOOLEAN`):

```go
		&migrate.Migration{
			Name:    "create_orchestrations",
			Version: "20240101000009",
			Comment: "Create cortex_orchestration_configs and cortex_orchestration_runs tables",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_orchestration_configs (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT DEFAULT '',
    app_id        TEXT NOT NULL,
    strategy      TEXT DEFAULT '',
    participants  JSONB DEFAULT '[]',
    settings      JSONB DEFAULT '{}',
    metadata      JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_app_name ON cortex_orchestration_configs (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_orchestration_runs (
    id             TEXT PRIMARY KEY,
    config_id      TEXT DEFAULT '',
    app_id         TEXT NOT NULL,
    tenant_id      TEXT DEFAULT '',
    strategy       TEXT DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running',
    input          TEXT DEFAULT '',
    output         TEXT DEFAULT '',
    error          TEXT DEFAULT '',
    agent_run_ids  JSONB DEFAULT '[]',
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_orchestration_runs_app_status ON cortex_orchestration_runs (app_id, status);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_orchestration_runs;
DROP TABLE IF EXISTS cortex_orchestration_configs;
`)
				return err
			},
		},
```

> If the postgres `participants`/`settings`/`metadata`/`agent_run_ids` model fields are `string` (JSON-encoded via `mustJSON`), `JSONB` columns still accept JSON-string inserts. If you switched to typed model fields in Step 1, keep `JSONB` — grove serializes typed values to JSONB.

- [ ] **Step 3: Add the CRUD implementation**

Create `store/postgres/orchestration.go` (driver `s.pgdb`, prefix `cortex:`, inline `sql.ErrNoRows`):

```go
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if _, err := s.pgdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex: create orchestration: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	if err := s.pgdb.NewSelect(m).Where("id = ?", orchID.String()).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	err := s.pgdb.NewSelect(m).Where("app_id = ?", appID).Where("name = ?", name).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration by name: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	c.UpdatedAt = time.Now().UTC()
	res, err := s.pgdb.NewUpdate(orchestrationConfigToModel(c)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	res, err := s.pgdb.NewDelete((*orchestrationConfigModel)(nil)).Where("id = ?", orchID.String()).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: delete orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	var models []orchestrationConfigModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Search != "" {
			q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list orchestrations: %w", err)
	}
	result := make([]*orchestration.OrchestrationConfig, len(models))
	for i := range models {
		c, convErr := orchestrationConfigFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = c
	}
	return result, nil
}

func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	q := s.pgdb.NewSelect((*orchestrationConfigModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Search != "" {
			q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count orchestrations: %w", err)
	}
	return count, nil
}

func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if _, err := s.pgdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex: create orchestration run: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	m := new(orchestrationRunModel)
	if err := s.pgdb.NewSelect(m).Where("id = ?", runID.String()).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration run: %w", err)
	}
	return orchestrationRunFromModel(m)
}

func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	r.UpdatedAt = time.Now().UTC()
	res, err := s.pgdb.NewUpdate(orchestrationRunToModel(r)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update orchestration run: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update orchestration run rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}
	return nil
}

func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	var models []orchestrationRunModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at DESC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list orchestration runs: %w", err)
	}
	result := make([]*orchestration.OrchestrationRun, len(models))
	for i := range models {
		r, convErr := orchestrationRunFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = r
	}
	return result, nil
}

func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	q := s.pgdb.NewSelect((*orchestrationRunModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count orchestration runs: %w", err)
	}
	return count, nil
}
```

> Confirm the postgres driver field name is `s.pgdb` (per `store/postgres/persona.go`). If `NewUpdate(...).WherePK()` or `res.RowsAffected()` differ in `store/postgres/persona.go`'s `UpdatePersona`/`DeletePersona`, match those exact calls.

- [ ] **Step 4: Verify it builds**

Run: `go build ./store/postgres/`
Expected: builds with no errors.

- [ ] **Step 5: Commit**

```bash
git add store/postgres/models.go store/postgres/migrations.go store/postgres/orchestration.go
git commit -m "feat(store/postgres): persist orchestration configs and runs"
```

---

## Task 8: mongo backend (models, converters, migration, CRUD)

**Files:**
- Modify: `store/mongo/models.go`
- Modify: `store/mongo/migrations.go`
- Create: `store/mongo/orchestration.go`

**Interfaces:**
- Consumes: same as Task 6.
- Produces: methods on `*mongo.Store` matching `ConfigStore` + `RunStore`.

> mongo stores typed values directly (bson tags), not JSON strings — mirror `store/mongo/persona.go` and the `personaModel` in `store/mongo/models.go`.

- [ ] **Step 1: Add model structs + converters**

Append to `store/mongo/models.go` (add `"github.com/xraph/cortex/orchestration"` import):

```go
type orchestrationConfigModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_configs"`
	ID              string                     `grove:"id,pk"        bson:"_id"`
	Name            string                     `grove:"name"         bson:"name"`
	Description     string                     `grove:"description"  bson:"description"`
	AppID           string                     `grove:"app_id"       bson:"app_id"`
	Strategy        string                     `grove:"strategy"     bson:"strategy"`
	Participants    []orchestration.Participant `grove:"participants" bson:"participants,omitempty"`
	Settings        orchestration.Settings     `grove:"settings"     bson:"settings,omitempty"`
	Metadata        map[string]any             `grove:"metadata"     bson:"metadata,omitempty"`
	CreatedAt       time.Time                  `grove:"created_at"   bson:"created_at"`
	UpdatedAt       time.Time                  `grove:"updated_at"   bson:"updated_at"`
}

func orchestrationConfigToModel(c *orchestration.OrchestrationConfig) *orchestrationConfigModel {
	return &orchestrationConfigModel{
		ID:           c.ID.String(),
		Name:         c.Name,
		Description:  c.Description,
		AppID:        c.AppID,
		Strategy:     c.Strategy,
		Participants: c.Participants,
		Settings:     c.Settings,
		Metadata:     c.Metadata,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func orchestrationConfigFromModel(m *orchestrationConfigModel) (*orchestration.OrchestrationConfig, error) {
	cfgID, err := id.ParseOrchestrationConfigID(m.ID)
	if err != nil {
		return nil, err
	}
	return &orchestration.OrchestrationConfig{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           cfgID,
		Name:         m.Name,
		Description:  m.Description,
		AppID:        m.AppID,
		Strategy:     m.Strategy,
		Participants: m.Participants,
		Settings:     m.Settings,
		Metadata:     m.Metadata,
	}, nil
}

type orchestrationRunModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_runs"`
	ID              string     `grove:"id,pk"         bson:"_id"`
	ConfigID        string     `grove:"config_id"     bson:"config_id"`
	AppID           string     `grove:"app_id"        bson:"app_id"`
	TenantID        string     `grove:"tenant_id"     bson:"tenant_id"`
	Strategy        string     `grove:"strategy"      bson:"strategy"`
	Status          string     `grove:"status"        bson:"status"`
	Input           string     `grove:"input"         bson:"input"`
	Output          string     `grove:"output"        bson:"output"`
	Error           string     `grove:"error"         bson:"error"`
	AgentRunIDs     []string   `grove:"agent_run_ids" bson:"agent_run_ids,omitempty"`
	StartedAt       time.Time  `grove:"started_at"    bson:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"  bson:"completed_at,omitempty"`
	CreatedAt       time.Time  `grove:"created_at"    bson:"created_at"`
	UpdatedAt       time.Time  `grove:"updated_at"    bson:"updated_at"`
}

func orchestrationRunToModel(r *orchestration.OrchestrationRun) *orchestrationRunModel {
	runIDs := make([]string, len(r.AgentRunIDs))
	for i, rid := range r.AgentRunIDs {
		runIDs[i] = rid.String()
	}
	return &orchestrationRunModel{
		ID:          r.ID.String(),
		ConfigID:    r.ConfigID.String(),
		AppID:       r.AppID,
		TenantID:    r.TenantID,
		Strategy:    r.Strategy,
		Status:      r.Status,
		Input:       r.Input,
		Output:      r.Output,
		Error:       r.Error,
		AgentRunIDs: runIDs,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func orchestrationRunFromModel(m *orchestrationRunModel) (*orchestration.OrchestrationRun, error) {
	runID, err := id.ParseOrchestrationID(m.ID)
	if err != nil {
		return nil, err
	}
	r := &orchestration.OrchestrationRun{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AppID:       m.AppID,
		TenantID:    m.TenantID,
		Strategy:    m.Strategy,
		Status:      m.Status,
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
	}
	if m.ConfigID != "" {
		cfgID, cerr := id.ParseOrchestrationConfigID(m.ConfigID)
		if cerr != nil {
			return nil, cerr
		}
		r.ConfigID = cfgID
	}
	for _, s := range m.AgentRunIDs {
		rid, perr := id.ParseAgentRunID(s)
		if perr != nil {
			return nil, perr
		}
		r.AgentRunIDs = append(r.AgentRunIDs, rid)
	}
	return r, nil
}
```

- [ ] **Step 2: Add the migration**

In `store/mongo/migrations.go`, add a collection-create migration as the final entry, mirroring the existing mongo migration that calls `mexec.CreateCollection`:

```go
		&migrate.Migration{
			Name:    "create_cortex_orchestrations",
			Version: "20240101000009",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				mexec, ok := exec.(*mongomigrate.Executor)
				if !ok {
					return fmt.Errorf("expected mongomigrate executor, got %T", exec)
				}
				if err := mexec.CreateCollection(ctx, (*orchestrationConfigModel)(nil)); err != nil {
					return err
				}
				return mexec.CreateCollection(ctx, (*orchestrationRunModel)(nil))
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				return nil
			},
		},
```

> Match the exact `Up`/`Down` signature and `mexec` usage of the existing mongo migrations in this file (check the `create_cortex_personas`/`create_cortex_behaviors` entry). If existing mongo migrations also register indexes, add `(app_id, name)` unique + `(app_id, status)` indexes following that idiom; otherwise the `CreateCollection` calls suffice for Plan 1.

- [ ] **Step 3: Add the CRUD implementation**

Create `store/mongo/orchestration.go`. Verified idioms from `store/mongo/persona.go`: driver `s.mdb`; `now()` timestamp helper; `Filter(bson.M{...})` (not `Where`); not-found via `isNoDocuments(err)`; `res.MatchedCount()` / `res.DeletedCount()`; `Sort(bson.D{{Key: ..., Value: 1}})`; `Limit(int64(...))` / `Skip(int64(...))`; `Count(ctx)` returns `int64`. Search uses a regex filter:

```go
package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	t := now()
	c.CreatedAt = t
	c.UpdatedAt = t
	if _, err := s.mdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/mongo: create orchestration: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	var m orchestrationConfigModel
	err := s.mdb.NewFind(&m).Filter(bson.M{"_id": orchID.String()}).Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get orchestration: %w", err)
	}
	return orchestrationConfigFromModel(&m)
}

func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	var m orchestrationConfigModel
	err := s.mdb.NewFind(&m).Filter(bson.M{"app_id": appID, "name": name}).Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get orchestration by name: %w", err)
	}
	return orchestrationConfigFromModel(&m)
}

func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	c.UpdatedAt = now()
	m := orchestrationConfigToModel(c)
	res, err := s.mdb.NewUpdate(m).Filter(bson.M{"_id": m.ID}).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration: %w", err)
	}
	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	res, err := s.mdb.NewDelete((*orchestrationConfigModel)(nil)).Filter(bson.M{"_id": orchID.String()}).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete orchestration: %w", err)
	}
	if res.DeletedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	var models []orchestrationConfigModel
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}
		if filter.Search != "" {
			f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
		}
	}
	q := s.mdb.NewFind(&models).Filter(f).Sort(bson.D{{Key: "created_at", Value: 1}})
	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}
		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list orchestrations: %w", err)
	}
	result := make([]*orchestration.OrchestrationConfig, len(models))
	for i := range models {
		c, convErr := orchestrationConfigFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = c
	}
	return result, nil
}

func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}
		if filter.Search != "" {
			f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
		}
	}
	count, err := s.mdb.NewFind((*orchestrationConfigModel)(nil)).Filter(f).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestrations: %w", err)
	}
	return count, nil
}

func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	t := now()
	r.CreatedAt = t
	r.UpdatedAt = t
	if _, err := s.mdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/mongo: create orchestration run: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	var m orchestrationRunModel
	err := s.mdb.NewFind(&m).Filter(bson.M{"_id": runID.String()}).Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get orchestration run: %w", err)
	}
	return orchestrationRunFromModel(&m)
}

func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	r.UpdatedAt = now()
	m := orchestrationRunToModel(r)
	res, err := s.mdb.NewUpdate(m).Filter(bson.M{"_id": m.ID}).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration run: %w", err)
	}
	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}
	return nil
}

func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	var models []orchestrationRunModel
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}
		if filter.Status != "" {
			f["status"] = filter.Status
		}
	}
	q := s.mdb.NewFind(&models).Filter(f).Sort(bson.D{{Key: "created_at", Value: -1}})
	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}
		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list orchestration runs: %w", err)
	}
	result := make([]*orchestration.OrchestrationRun, len(models))
	for i := range models {
		r, convErr := orchestrationRunFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = r
	}
	return result, nil
}

func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}
		if filter.Status != "" {
			f["status"] = filter.Status
		}
	}
	count, err := s.mdb.NewFind((*orchestrationRunModel)(nil)).Filter(f).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestration runs: %w", err)
	}
	return count, nil
}
```

- [ ] **Step 4: Verify it builds**

Run: `go build ./store/mongo/`
Expected: builds with no errors.

- [ ] **Step 5: Commit**

```bash
git add store/mongo/models.go store/mongo/migrations.go store/mongo/orchestration.go
git commit -m "feat(store/mongo): persist orchestration configs and runs"
```

---

## Task 9: Fold the two Stores into the composite `store.Store`

**Files:**
- Modify: `store/store.go`

**Interfaces:**
- Consumes: `orchestration.ConfigStore`, `orchestration.RunStore`; all three backends' methods (Tasks 6-8).
- Produces: the composite `store.Store` now requires orchestration persistence; the `var _ store.Store = (*Store)(nil)` assertions in each backend now enforce it.

- [ ] **Step 1: Add the embeds**

In `store/store.go`, add the import `"github.com/xraph/cortex/orchestration"` and embed both interfaces in `Store`:

```go
// Store is the composite persistence interface for all Cortex subsystems.
type Store interface {
	agent.Store
	run.Store
	memory.Store
	checkpoint.Store
	skill.Store
	trait.Store
	behavior.Store
	persona.Store
	orchestration.ConfigStore
	orchestration.RunStore

	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}
```

- [ ] **Step 2: Verify the whole module builds**

Run: `go build ./...`
Expected: builds with no errors. If any backend is missing a method, the failing `var _ store.Store = (*Store)(nil)` assertion pinpoints it — return to the relevant Task 6/7/8 file and add the method.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS (orchestration package tests + existing suite). No regressions.

- [ ] **Step 4: Commit**

```bash
git add store/store.go
git commit -m "feat(store): require orchestration persistence in composite Store"
```

---

## Task 10: Engine CRUD pass-throughs

**Files:**
- Create: `engine/orchestration_crud.go`
- Test: `engine/orchestration_crud_test.go`

**Interfaces:**
- Consumes: `e.store` (composite `store.Store`); `orchestration.*` types; `cortex.ErrNoStore`.
- Produces: engine methods `CreateOrchestration`, `GetOrchestration`, `GetOrchestrationByName`, `UpdateOrchestration`, `DeleteOrchestration`, `ListOrchestrations`, `CountOrchestrations`, `GetOrchestrationRun`, `ListOrchestrationRuns`, `CountOrchestrationRuns`. (`RunOrchestration` is Plan 2.)

- [ ] **Step 1: Write the failing test**

Create `engine/orchestration_crud_test.go`. This asserts the no-store guard returns `cortex.ErrNoStore` (no DB needed — an engine with no store):

```go
package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationCRUDNoStore(t *testing.T) {
	e, err := engine.New()
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx := context.Background()

	if err := e.CreateOrchestration(ctx, &orchestration.OrchestrationConfig{}); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("CreateOrchestration err = %v, want ErrNoStore", err)
	}
	if _, err := e.GetOrchestration(ctx, id.NewOrchestrationConfigID()); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("GetOrchestration err = %v, want ErrNoStore", err)
	}
	if _, err := e.ListOrchestrations(ctx, nil); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("ListOrchestrations err = %v, want ErrNoStore", err)
	}
	if _, err := e.GetOrchestrationRun(ctx, id.NewOrchestrationID()); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("GetOrchestrationRun err = %v, want ErrNoStore", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -run TestOrchestrationCRUDNoStore -v`
Expected: FAIL — `e.CreateOrchestration undefined`.

- [ ] **Step 3: Write minimal implementation**

Create `engine/orchestration_crud.go` (mirrors the persona CRUD block in `engine/engine.go`):

```go
package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// CreateOrchestration stores a new orchestration config.
func (e *Engine) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateOrchestration(ctx, c)
}

// GetOrchestration returns an orchestration config by ID.
func (e *Engine) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestration(ctx, orchID)
}

// GetOrchestrationByName returns an orchestration config by app-scoped name.
func (e *Engine) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestrationByName(ctx, appID, name)
}

// UpdateOrchestration updates an existing orchestration config.
func (e *Engine) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateOrchestration(ctx, c)
}

// DeleteOrchestration removes an orchestration config.
func (e *Engine) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteOrchestration(ctx, orchID)
}

// ListOrchestrations lists orchestration configs.
func (e *Engine) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListOrchestrations(ctx, filter)
}

// CountOrchestrations counts orchestration configs.
func (e *Engine) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountOrchestrations(ctx, filter)
}

// GetOrchestrationRun returns an orchestration run record by ID.
func (e *Engine) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestrationRun(ctx, runID)
}

// ListOrchestrationRuns lists orchestration run records.
func (e *Engine) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListOrchestrationRuns(ctx, filter)
}

// CountOrchestrationRuns counts orchestration run records.
func (e *Engine) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountOrchestrationRuns(ctx, filter)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./engine/ -run TestOrchestrationCRUDNoStore -v && go build ./...`
Expected: PASS, build succeeds.

- [ ] **Step 5: Commit**

```bash
git add engine/orchestration_crud.go engine/orchestration_crud_test.go
git commit -m "feat(engine): add orchestration config and run CRUD pass-throughs"
```

---

## Done criteria for Plan 1

- [ ] `go build ./...` succeeds.
- [ ] `go test ./...` passes, including `./orchestration/` (`-race` clean).
- [ ] The `orchestration` package exposes core types, `Blackboard`, and both entities + Store interfaces.
- [ ] All three store backends persist `OrchestrationConfig` and `OrchestrationRun` (compile-guarded by `store.Store`).
- [ ] The engine exposes orchestration CRUD; `RunOrchestration` and the strategies are deliberately deferred to Plan 2.

Next: **Plan 2 — Strategies & Execution** (the five orchestrators, `builder.go`, the `AgentRunner` adapter, `RunOrchestration` with hook emission, integration test).
