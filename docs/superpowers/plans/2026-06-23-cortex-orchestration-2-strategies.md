# Cortex Orchestration — Plan 2: Strategies & Execution

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the five orchestration strategies (sequential, parallel, router, hierarchical, debate), a builder/factory, an in-package `Service` that runs a stored config end-to-end (persisting an `OrchestrationRun` and firing plugin hooks), and the engine glue (`AgentRunner` adapter + `RunOrchestration`).

**Architecture:** Each strategy implements `orchestration.Orchestrator` and drives agents only through the `AgentRunner` seam — **including meta-decisions**: the router, hierarchical-manager, and debate-judge are ordinary participant agents run via `AgentRunner`, not a raw `llm.Client`. This keeps `orchestration` dependency-free of `llm` and makes every strategy unit-testable with a fake runner. A `Service` (in-package) loads a config, builds the orchestrator, creates/updates the `OrchestrationRun`, wires the `Blackboard` handoff callback to a `HookEmitter`, and fires start/complete hooks. The engine's `RunOrchestration` is a thin wrapper that injects real dependencies (the run adapter, the composite store, and a plugin-registry-backed `HookEmitter`).

**Tech Stack:** Go 1.25, standard library only for orchestration (`context`, `sync`, `strings`, `encoding/json`, `time`). Tests use fakes — no DB, no real LLM.

## Global Constraints

- Prerequisite: **Plan 1 is complete and merged** (core types, `Blackboard`, entities, stores, engine CRUD all exist).
- No `Co-Authored-By` trailers in commits.
- `orchestration` imports only `context`, `sync`, `strings`, `encoding/json`, `time`, `github.com/xraph/cortex`, and `github.com/xraph/cortex/id`. **It must not import `engine`, `plugin`, `store`, or `llm`** (no import cycle; meta-decisions go through `AgentRunner`).
- Meta-decisions are agent-based: `Settings.RouterAgent` / `Settings.Manager` / `Settings.Judge` name participant agents run via `AgentRunner`. `Settings.RouterRules` provides an LLM-free static-routing alternative.
- Strategy `Run` must honor `ctx` cancellation (it propagates through `AgentRunner.RunAgent(ctx, ...)`).
- Concurrency in `parallel`/`hierarchical` is bounded by `Settings.MaxConcurrency` (default 4 when ≤ 0). Use stdlib (`sync.WaitGroup` + a semaphore channel), not a third-party errgroup.
- Existing plugin emitters (verified): `Registry.EmitOrchestrationStarted(ctx, orchID, strategy)`, `Registry.EmitOrchestrationCompleted(ctx, orchID, elapsed)`, `Registry.EmitAgentHandoff(ctx, orchID, from, to, payload)`.
- Engine internals (verified): `e.RunAgent(ctx, appID, agentName, input, *RunOverrides) (*run.Run, error)`; fields `e.store store.Store`, `e.extensions *plugin.Registry`. `run.Run` has `.ID id.AgentRunID`, `.Output string`, `.Error string`, `.State run.State`.

---

## File Structure

**Created (in `orchestration/`):**
- `strategy.go` — shared helpers (`runOptsFromSettings`, `composeInput`, `findParticipant`, `nonManagerParticipants`).
- `sequential.go`, `parallel.go`, `router.go`, `hierarchical.go`, `debate.go` — the five `Orchestrator` implementations.
- `builder.go` — `Build(strategy, runner, appID, participants, settings) (Orchestrator, error)`.
- `service.go` — `HookEmitter` interface + `Service` + `NewService` + `Service.Run`.
- Tests: `sequential_test.go`, `parallel_test.go`, `router_test.go`, `hierarchical_test.go`, `debate_test.go`, `builder_test.go`, `service_test.go`, plus a shared `fakes_test.go`.

**Created (in `engine/`):**
- `engine/orchestration.go` — `agentRunnerAdapter`, `registryHookEmitter`, `RunOrchestration`.
- `engine/orchestration_run_test.go`.

**Modified:**
- `orchestration/blackboard.go` — add `OrchestrationID()` getter.

---

## Task 1: Blackboard getter + shared strategy helpers + test fakes

**Files:**
- Modify: `orchestration/blackboard.go`
- Create: `orchestration/strategy.go`
- Create: `orchestration/fakes_test.go`

**Interfaces:**
- Consumes: Plan 1 types (`Blackboard`, `Participant`, `Settings`, `RunOpts`, `AgentRunner`, `AgentResult`).
- Produces: `(*Blackboard).OrchestrationID() id.OrchestrationID`; helpers `runOptsFromSettings`, `composeInput`, `findParticipant`, `nonManagerParticipants`; test-only `fakeRunner`.

- [ ] **Step 1: Add the `OrchestrationID` getter**

Append to `orchestration/blackboard.go`:

```go
// OrchestrationID returns the ID of the orchestration this blackboard belongs to.
func (b *Blackboard) OrchestrationID() id.OrchestrationID {
	return b.orchID
}
```

- [ ] **Step 2: Create shared helpers**

Create `orchestration/strategy.go`:

```go
package orchestration

const defaultMaxConcurrency = 4

// runOptsFromSettings derives per-agent run options from orchestration settings.
// Returns nil when no overrides apply (the agent's own config is used).
func runOptsFromSettings(s Settings) *RunOpts {
	if s.Model == "" {
		return nil
	}
	return &RunOpts{Model: s.Model}
}

// composeInput prepends the blackboard snapshot (roster + prior work) to a task,
// giving the agent awareness of collaborators and context. Returns task unchanged
// when there is no snapshot.
func composeInput(task, snapshot string) string {
	if snapshot == "" {
		return task
	}
	return snapshot + "\n\nYour task: " + task
}

// findParticipant returns the participant with the given agent name.
func findParticipant(parts []Participant, name string) (Participant, bool) {
	for _, p := range parts {
		if p.AgentName == name {
			return p, true
		}
	}
	return Participant{}, false
}

// nonManagerParticipants returns participants excluding the named manager.
func nonManagerParticipants(parts []Participant, manager string) []Participant {
	out := make([]Participant, 0, len(parts))
	for _, p := range parts {
		if p.AgentName == manager {
			continue
		}
		out = append(out, p)
	}
	return out
}

// boundedConcurrency returns a sane worker cap.
func boundedConcurrency(n int) int {
	if n <= 0 {
		return defaultMaxConcurrency
	}
	return n
}
```

- [ ] **Step 3: Create the shared test fake**

Create `orchestration/fakes_test.go`:

```go
package orchestration

import (
	"context"
	"sync"
)

// fakeRunner records calls and returns canned outputs keyed by agent name.
// A nil/missing entry echoes the input. respond may be set for dynamic replies.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	outputs map[string]string
	respond func(agentName, input string) string
}

type fakeCall struct {
	AgentName string
	Input     string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]string{}}
}

func (f *fakeRunner) RunAgent(_ context.Context, _ /*appID*/ string, agentName, input string, _ *RunOpts) (*AgentResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{AgentName: agentName, Input: input})
	f.mu.Unlock()

	out := input // default: echo
	if f.respond != nil {
		out = f.respond(agentName, input)
	} else if v, ok := f.outputs[agentName]; ok {
		out = v
	}
	return &AgentResult{AgentName: agentName, Output: out}, nil
}

func (f *fakeRunner) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.AgentName
	}
	return names
}
```

> This fake is `package orchestration` (internal test) so it can access unexported helpers if needed. The strategy tests below are also `package orchestration`.

- [ ] **Step 4: Verify it builds and the package still tests green**

Run: `go test ./orchestration/ -run TestBlackboard -v && go vet ./orchestration/`
Expected: existing Plan 1 tests PASS; package compiles with the new helpers and fake.

- [ ] **Step 5: Commit**

```bash
git add orchestration/blackboard.go orchestration/strategy.go orchestration/fakes_test.go
git commit -m "feat(orchestration): add blackboard getter, strategy helpers, test fake"
```

---

## Task 2: Sequential strategy

**Files:**
- Create: `orchestration/sequential.go`
- Test: `orchestration/sequential_test.go`

**Interfaces:**
- Consumes: `AgentRunner`, `Participant`, `Settings`, `Blackboard`, helpers from Task 1.
- Produces: `newSequential(runner, appID, parts, settings) *sequential` implementing `Orchestrator`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/sequential_test.go`:

```go
package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestSequentialRunsInOrderAndChains(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"writer": "DRAFT", "editor": "EDITED"}
	parts := []Participant{{AgentName: "writer"}, {AgentName: "editor"}}
	o := newSequential(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "start", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.callNames(); len(got) != 2 || got[0] != "writer" || got[1] != "editor" {
		t.Fatalf("call order = %v, want [writer editor]", got)
	}
	if res.Output != "EDITED" {
		t.Fatalf("output = %q, want EDITED", res.Output)
	}
	if len(res.AgentResults) != 2 {
		t.Fatalf("agent results = %d, want 2", len(res.AgentResults))
	}
	if len(res.Handoffs) != 1 || res.Handoffs[0].From != "writer" || res.Handoffs[0].To != "editor" {
		t.Fatalf("handoffs = %+v, want one writer→editor", res.Handoffs)
	}
}

func TestSequentialSecondAgentSeesFirstOutput(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, input string) string { return agent + ":" + input }
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newSequential(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	_, err := o.Run(context.Background(), "X", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// b's input must contain a's output ("a:X") via the blackboard snapshot.
	calls := runner.calls
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if !strings.Contains(calls[1].Input, "a:X") {
		t.Fatalf("second agent input %q does not contain first output", calls[1].Input)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestSequential -v`
Expected: FAIL — `undefined: newSequential`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/sequential.go`:

```go
package orchestration

import "context"

type sequential struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newSequential(runner AgentRunner, appID string, parts []Participant, settings Settings) *sequential {
	return &sequential{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *sequential) Strategy() string { return StrategySequential }

func (o *sequential) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategySequential}
	opts := runOptsFromSettings(o.settings)

	last := input
	for i, p := range o.parts {
		agentInput := composeInput(input, bb.Snapshot())
		ar, err := o.runner.RunAgent(ctx, o.appID, p.AgentName, agentInput, opts)
		if err != nil {
			res.AgentResults = append(res.AgentResults, AgentResult{AgentName: p.AgentName, Err: err})
			res.Err = err
			return res, err
		}
		bb.Append(p.AgentName, ar.Output)
		if i > 0 {
			bb.Handoff(ctx, o.parts[i-1].AgentName, p.AgentName, last)
		}
		res.AgentResults = append(res.AgentResults, *ar)
		last = ar.Output
	}
	res.Output = last
	res.Handoffs = bb.Handoffs()
	return res, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestSequential -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add orchestration/sequential.go orchestration/sequential_test.go
git commit -m "feat(orchestration): add sequential strategy"
```

---

## Task 3: Parallel strategy

**Files:**
- Create: `orchestration/parallel.go`
- Test: `orchestration/parallel_test.go`

**Interfaces:**
- Consumes: Task 1 helpers; `AgentRunner`.
- Produces: `newParallel(runner, appID, parts, settings) *parallel` implementing `Orchestrator`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/parallel_test.go`:

```go
package orchestration

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestParallelRunsAllAndConcatenates(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "c": "CC"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}, {AgentName: "c"}}
	o := newParallel(runner, "app1", parts, Settings{MaxConcurrency: 2})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := runner.callNames()
	sort.Strings(got)
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("called = %v, want a,b,c", got)
	}
	if len(res.AgentResults) != 3 {
		t.Fatalf("results = %d, want 3", len(res.AgentResults))
	}
	for _, want := range []string{"AA", "BB", "CC"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output %q missing %q", res.Output, want)
		}
	}
}

func TestParallelAggregatorSynthesizes(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "boss": "FINAL"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newParallel(runner, "app1", parts, Settings{Aggregator: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "FINAL" {
		t.Fatalf("output = %q, want FINAL (aggregator)", res.Output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestParallel -v`
Expected: FAIL — `undefined: newParallel`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/parallel.go`:

```go
package orchestration

import (
	"context"
	"strings"
	"sync"
)

type parallel struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newParallel(runner AgentRunner, appID string, parts []Participant, settings Settings) *parallel {
	return &parallel{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *parallel) Strategy() string { return StrategyParallel }

func (o *parallel) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyParallel}
	opts := runOptsFromSettings(o.settings)
	snapshot := bb.Snapshot()

	results := make([]AgentResult, len(o.parts))
	sem := make(chan struct{}, boundedConcurrency(o.settings.MaxConcurrency))
	var wg sync.WaitGroup

	for i, p := range o.parts {
		wg.Add(1)
		go func(i int, p Participant) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ar, err := o.runner.RunAgent(ctx, o.appID, p.AgentName, composeInput(input, snapshot), opts)
			if err != nil {
				results[i] = AgentResult{AgentName: p.AgentName, Err: err}
				return
			}
			results[i] = *ar
		}(i, p)
	}
	wg.Wait()

	var firstErr error
	var parts []string
	for _, r := range results {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			if firstErr == nil {
				firstErr = r.Err
			}
			continue
		}
		bb.Append(r.AgentName, r.Output)
		parts = append(parts, r.Output)
	}

	// Optional aggregator agent synthesizes a single answer from all outputs.
	if o.settings.Aggregator != "" {
		ar, err := o.runner.RunAgent(ctx, o.appID, o.settings.Aggregator, composeInput(input, bb.Snapshot()), opts)
		if err == nil {
			res.AgentResults = append(res.AgentResults, *ar)
			res.Output = ar.Output
			res.Handoffs = bb.Handoffs()
			return res, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	res.Output = strings.Join(parts, "\n\n")
	res.Handoffs = bb.Handoffs()
	res.Err = firstErr
	return res, firstErr
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestParallel -race -v`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add orchestration/parallel.go orchestration/parallel_test.go
git commit -m "feat(orchestration): add parallel strategy with optional aggregator"
```

---

## Task 4: Router strategy

**Files:**
- Create: `orchestration/router.go`
- Test: `orchestration/router_test.go`

**Interfaces:**
- Consumes: Task 1 helpers; `AgentRunner`.
- Produces: `newRouter(runner, appID, parts, settings) *router` implementing `Orchestrator`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/router_test.go`:

```go
package orchestration

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestRouterStaticRules(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"billing": "BILL", "support": "SUPP"}
	parts := []Participant{{AgentName: "billing"}, {AgentName: "support"}}
	o := newRouter(runner, "app1", parts, Settings{
		RouterRules: map[string]string{"refund": "billing", "broken": "support"},
	})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "I need a refund please", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "BILL" {
		t.Fatalf("output = %q, want BILL (routed to billing)", res.Output)
	}
	if got := runner.callNames(); len(got) != 1 || got[0] != "billing" {
		t.Fatalf("calls = %v, want [billing]", got)
	}
}

func TestRouterAgentDecides(t *testing.T) {
	runner := newFakeRunner()
	// router agent returns the chosen agent name; chosen agent returns its output.
	runner.outputs = map[string]string{"dispatcher": "support", "support": "SUPP"}
	parts := []Participant{{AgentName: "billing"}, {AgentName: "support"}}
	o := newRouter(runner, "app1", parts, Settings{RouterAgent: "dispatcher"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "my app is broken", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "SUPP" {
		t.Fatalf("output = %q, want SUPP", res.Output)
	}
	if len(res.Handoffs) != 1 || res.Handoffs[0].From != "dispatcher" || res.Handoffs[0].To != "support" {
		t.Fatalf("handoffs = %+v, want dispatcher→support", res.Handoffs)
	}
}

func TestRouterFallsBackToFirst(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newRouter(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "anything", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "AA" {
		t.Fatalf("output = %q, want AA (first participant)", res.Output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestRouter -v`
Expected: FAIL — `undefined: newRouter`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/router.go`:

```go
package orchestration

import (
	"context"
	"strings"
)

type router struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newRouter(runner AgentRunner, appID string, parts []Participant, settings Settings) *router {
	return &router{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *router) Strategy() string { return StrategyRouter }

func (o *router) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyRouter}
	if len(o.parts) == 0 {
		return res, nil
	}
	opts := runOptsFromSettings(o.settings)

	chosen := o.parts[0].AgentName
	routedBy := ""

	switch {
	case len(o.settings.RouterRules) > 0:
		lower := strings.ToLower(input)
		for keyword, agentName := range o.settings.RouterRules {
			if strings.Contains(lower, strings.ToLower(keyword)) {
				if _, ok := findParticipant(o.parts, agentName); ok {
					chosen = agentName
				}
				break
			}
		}
	case o.settings.RouterAgent != "":
		prompt := buildRoutingPrompt(o.parts, input)
		ar, err := o.runner.RunAgent(ctx, o.appID, o.settings.RouterAgent, prompt, opts)
		if err != nil {
			res.Err = err
			return res, err
		}
		res.AgentResults = append(res.AgentResults, *ar)
		if name, ok := matchParticipant(o.parts, ar.Output); ok {
			chosen = name
		}
		routedBy = o.settings.RouterAgent
	}

	ar, err := o.runner.RunAgent(ctx, o.appID, chosen, composeInput(input, bb.Snapshot()), opts)
	if err != nil {
		res.Err = err
		return res, err
	}
	bb.Append(chosen, ar.Output)
	if routedBy != "" {
		bb.Handoff(ctx, routedBy, chosen, input)
	}
	res.AgentResults = append(res.AgentResults, *ar)
	res.Output = ar.Output
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func buildRoutingPrompt(parts []Participant, input string) string {
	var sb strings.Builder
	sb.WriteString("You are a router. Choose the single best agent to handle the request.\n")
	sb.WriteString("Respond with ONLY the agent name, nothing else.\n\nAgents:\n")
	for _, p := range parts {
		if p.Role != "" {
			sb.WriteString("- " + p.AgentName + " (" + p.Role + ")\n")
		} else {
			sb.WriteString("- " + p.AgentName + "\n")
		}
	}
	sb.WriteString("\nRequest: " + input)
	return sb.String()
}

// matchParticipant finds the participant whose name appears in the router output.
func matchParticipant(parts []Participant, output string) (string, bool) {
	trimmed := strings.TrimSpace(output)
	// exact match first
	if _, ok := findParticipant(parts, trimmed); ok {
		return trimmed, true
	}
	// substring match (router may add prose)
	lower := strings.ToLower(output)
	for _, p := range parts {
		if strings.Contains(lower, strings.ToLower(p.AgentName)) {
			return p.AgentName, true
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestRouter -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add orchestration/router.go orchestration/router_test.go
git commit -m "feat(orchestration): add router strategy (rules or router-agent)"
```

---

## Task 5: Hierarchical strategy

**Files:**
- Create: `orchestration/hierarchical.go`
- Test: `orchestration/hierarchical_test.go`

**Interfaces:**
- Consumes: Task 1 helpers; `AgentRunner`.
- Produces: `newHierarchical(runner, appID, parts, settings) *hierarchical` implementing `Orchestrator`.

**Behavior:** the manager agent (`Settings.Manager`, or the participant whose `Role == "manager"`, else the first participant) is run to produce a delegation plan — a JSON array `[{"agent":"...","task":"..."}]`. Each named worker runs its task (bounded concurrency). If the plan does not parse, every non-manager participant runs the original input (graceful fallback). Finally the manager runs once more to synthesize a final answer from the workers' outputs.

- [ ] **Step 1: Write the failing test**

Create `orchestration/hierarchical_test.go`:

```go
package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestHierarchicalDelegatesPerPlan(t *testing.T) {
	runner := newFakeRunner()
	plan := `[{"agent":"researcher","task":"gather facts"},{"agent":"writer","task":"compose"}]`
	calls := 0
	runner.respond = func(agent, input string) string {
		switch agent {
		case "boss":
			calls++
			if calls == 1 {
				return plan // first call: the delegation plan
			}
			return "SYNTHESIS" // second call: synthesis
		case "researcher":
			return "FACTS"
		case "writer":
			return "ESSAY"
		}
		return input
	}
	parts := []Participant{
		{AgentName: "boss", Role: "manager"},
		{AgentName: "researcher", Role: "worker"},
		{AgentName: "writer", Role: "worker"},
	}
	o := newHierarchical(runner, "app1", parts, Settings{Manager: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "write a report", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "SYNTHESIS" {
		t.Fatalf("output = %q, want SYNTHESIS", res.Output)
	}
	names := strings.Join(runner.callNames(), ",")
	for _, want := range []string{"boss", "researcher", "writer"} {
		if !strings.Contains(names, want) {
			t.Fatalf("calls %q missing %q", names, want)
		}
	}
	// at least one manager→worker handoff recorded
	if len(res.Handoffs) == 0 {
		t.Fatalf("expected handoffs, got none")
	}
}

func TestHierarchicalFallbackWhenPlanInvalid(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, input string) string {
		if agent == "boss" {
			return "not json"
		}
		return agent + "-done"
	}
	parts := []Participant{
		{AgentName: "boss", Role: "manager"},
		{AgentName: "w1", Role: "worker"},
		{AgentName: "w2", Role: "worker"},
	}
	o := newHierarchical(runner, "app1", parts, Settings{Manager: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "do it", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// both workers must have run despite the invalid plan
	names := strings.Join(runner.callNames(), ",")
	if !strings.Contains(names, "w1") || !strings.Contains(names, "w2") {
		t.Fatalf("fallback did not run all workers: %q", names)
	}
	_ = res
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestHierarchical -v`
Expected: FAIL — `undefined: newHierarchical`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/hierarchical.go`:

```go
package orchestration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

type hierarchical struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newHierarchical(runner AgentRunner, appID string, parts []Participant, settings Settings) *hierarchical {
	return &hierarchical{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *hierarchical) Strategy() string { return StrategyHierarchical }

type delegation struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

func (o *hierarchical) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyHierarchical}
	opts := runOptsFromSettings(o.settings)

	manager := o.resolveManager()
	if manager == "" {
		return res, nil
	}
	workers := nonManagerParticipants(o.parts, manager)

	// 1. Manager produces a delegation plan.
	planOut, err := o.runner.RunAgent(ctx, o.appID, manager, buildPlanPrompt(workers, input), opts)
	if err != nil {
		res.Err = err
		return res, err
	}
	res.AgentResults = append(res.AgentResults, *planOut)
	bb.Append(manager, planOut.Output)

	tasks := parsePlan(planOut.Output, workers, input)

	// 2. Workers execute their tasks (bounded concurrency).
	workerResults := make([]AgentResult, len(tasks))
	sem := make(chan struct{}, boundedConcurrency(o.settings.MaxConcurrency))
	var wg sync.WaitGroup
	for i, d := range tasks {
		wg.Add(1)
		go func(i int, d delegation) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ar, werr := o.runner.RunAgent(ctx, o.appID, d.Agent, composeInput(d.Task, bb.Snapshot()), opts)
			if werr != nil {
				workerResults[i] = AgentResult{AgentName: d.Agent, Err: werr}
				return
			}
			workerResults[i] = *ar
		}(i, d)
	}
	wg.Wait()

	for i, r := range workerResults {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			continue
		}
		bb.Append(r.AgentName, r.Output)
		bb.Handoff(ctx, manager, tasks[i].Agent, tasks[i].Task)
	}

	// 3. Manager synthesizes the final answer from the workers' contributions.
	synth, err := o.runner.RunAgent(ctx, o.appID, manager, buildSynthesisPrompt(input, bb.Snapshot()), opts)
	if err != nil {
		res.Err = err
		res.Handoffs = bb.Handoffs()
		return res, err
	}
	res.AgentResults = append(res.AgentResults, *synth)
	res.Output = synth.Output
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func (o *hierarchical) resolveManager() string {
	if o.settings.Manager != "" {
		return o.settings.Manager
	}
	for _, p := range o.parts {
		if strings.EqualFold(p.Role, "manager") {
			return p.AgentName
		}
	}
	if len(o.parts) > 0 {
		return o.parts[0].AgentName
	}
	return ""
}

// parsePlan extracts a delegation list from the manager output, keeping only
// tasks addressed to known workers. Falls back to assigning the original input
// to every worker when the plan is missing or unparseable.
func parsePlan(output string, workers []Participant, input string) []delegation {
	var plan []delegation
	if start := strings.Index(output, "["); start >= 0 {
		if end := strings.LastIndex(output, "]"); end > start {
			_ = json.Unmarshal([]byte(output[start:end+1]), &plan)
		}
	}
	var valid []delegation
	for _, d := range plan {
		if d.Agent == "" || d.Task == "" {
			continue
		}
		if _, ok := findParticipant(workers, d.Agent); ok {
			valid = append(valid, d)
		}
	}
	if len(valid) > 0 {
		return valid
	}
	fallback := make([]delegation, len(workers))
	for i, w := range workers {
		fallback[i] = delegation{Agent: w.AgentName, Task: input}
	}
	return fallback
}

func buildPlanPrompt(workers []Participant, input string) string {
	var sb strings.Builder
	sb.WriteString("You are a manager. Break the task into subtasks for your team.\n")
	sb.WriteString(`Respond ONLY with a JSON array like [{"agent":"name","task":"..."}].` + "\n\nTeam:\n")
	for _, w := range workers {
		if w.Role != "" {
			sb.WriteString("- " + w.AgentName + " (" + w.Role + ")\n")
		} else {
			sb.WriteString("- " + w.AgentName + "\n")
		}
	}
	sb.WriteString("\nTask: " + input)
	return sb.String()
}

func buildSynthesisPrompt(input, snapshot string) string {
	return snapshot + "\n\nUsing your team's work above, produce the final answer to: " + input
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestHierarchical -race -v`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add orchestration/hierarchical.go orchestration/hierarchical_test.go
git commit -m "feat(orchestration): add hierarchical manager/worker strategy"
```

---

## Task 6: Debate strategy

**Files:**
- Create: `orchestration/debate.go`
- Test: `orchestration/debate_test.go`

**Interfaces:**
- Consumes: Task 1 helpers; `AgentRunner`.
- Produces: `newDebate(runner, appID, parts, settings) *debate` implementing `Orchestrator`.

**Behavior:** the debater participants (everyone except the judge) each respond for `Settings.Rounds` rounds (default 1), every round seeing the prior arguments via the blackboard. After the rounds, the judge agent (`Settings.Judge`, or `Role == "judge"`) produces the final verdict. If no judge is configured, the output is the last debater's final argument.

- [ ] **Step 1: Write the failing test**

Create `orchestration/debate_test.go`:

```go
package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestDebateRunsRoundsThenJudges(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{
		"optimist": "PRO",
		"skeptic":  "CON",
		"judge":    "VERDICT",
	}
	parts := []Participant{
		{AgentName: "optimist", Role: "debater"},
		{AgentName: "skeptic", Role: "debater"},
		{AgentName: "judge", Role: "judge"},
	}
	o := newDebate(runner, "app1", parts, Settings{Rounds: 2, Judge: "judge"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "is X good?", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "VERDICT" {
		t.Fatalf("output = %q, want VERDICT", res.Output)
	}
	// 2 debaters × 2 rounds = 4 debater calls + 1 judge call = 5
	names := runner.callNames()
	if len(names) != 5 {
		t.Fatalf("calls = %d (%v), want 5", len(names), names)
	}
	if names[len(names)-1] != "judge" {
		t.Fatalf("last call = %q, want judge", names[len(names)-1])
	}
}

func TestDebateNoJudgeUsesLastArgument(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, _ string) string { return agent + "-arg" }
	parts := []Participant{{AgentName: "a", Role: "debater"}, {AgentName: "b", Role: "debater"}}
	o := newDebate(runner, "app1", parts, Settings{Rounds: 1})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "topic", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "-arg") {
		t.Fatalf("output = %q, want a debater argument", res.Output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestDebate -v`
Expected: FAIL — `undefined: newDebate`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/debate.go`:

```go
package orchestration

import (
	"context"
	"strconv"
	"strings"
)

type debate struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newDebate(runner AgentRunner, appID string, parts []Participant, settings Settings) *debate {
	return &debate{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *debate) Strategy() string { return StrategyDebate }

func (o *debate) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyDebate}
	opts := runOptsFromSettings(o.settings)

	judge := o.resolveJudge()
	debaters := o.debaters(judge)
	rounds := o.settings.Rounds
	if rounds <= 0 {
		rounds = 1
	}

	last := ""
	for r := 0; r < rounds; r++ {
		for _, d := range debaters {
			prompt := buildDebatePrompt(input, bb.Snapshot(), r+1)
			ar, err := o.runner.RunAgent(ctx, o.appID, d.AgentName, prompt, opts)
			if err != nil {
				res.Err = err
				return res, err
			}
			bb.Append(d.AgentName, ar.Output)
			res.AgentResults = append(res.AgentResults, *ar)
			last = ar.Output
		}
	}

	if judge != "" {
		ar, err := o.runner.RunAgent(ctx, o.appID, judge, buildJudgePrompt(input, bb.Snapshot()), opts)
		if err != nil {
			res.Err = err
			res.Handoffs = bb.Handoffs()
			return res, err
		}
		res.AgentResults = append(res.AgentResults, *ar)
		res.Output = ar.Output
		res.Handoffs = bb.Handoffs()
		return res, nil
	}

	res.Output = last
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func (o *debate) resolveJudge() string {
	if o.settings.Judge != "" {
		return o.settings.Judge
	}
	for _, p := range o.parts {
		if strings.EqualFold(p.Role, "judge") {
			return p.AgentName
		}
	}
	return ""
}

func (o *debate) debaters(judge string) []Participant {
	out := make([]Participant, 0, len(o.parts))
	for _, p := range o.parts {
		if p.AgentName == judge {
			continue
		}
		out = append(out, p)
	}
	return out
}

func buildDebatePrompt(input, snapshot string, round int) string {
	var sb strings.Builder
	sb.WriteString("Debate round " + strconv.Itoa(round) + ".\n")
	if snapshot != "" {
		sb.WriteString(snapshot + "\n\n")
	}
	sb.WriteString("Argue your position on: " + input)
	return sb.String()
}

func buildJudgePrompt(input, snapshot string) string {
	return snapshot + "\n\nAs the judge, weigh the arguments above and give a final verdict on: " + input
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestDebate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add orchestration/debate.go orchestration/debate_test.go
git commit -m "feat(orchestration): add debate strategy with judge"
```

---

## Task 7: Builder / factory

**Files:**
- Create: `orchestration/builder.go`
- Test: `orchestration/builder_test.go`

**Interfaces:**
- Consumes: all five `new*` constructors; `Strategy*` consts.
- Produces: `Build(strategy string, runner AgentRunner, appID string, parts []Participant, settings Settings) (Orchestrator, error)`; sentinel `ErrUnknownStrategy`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/builder_test.go`:

```go
package orchestration

import (
	"errors"
	"testing"
)

func TestBuildAllStrategies(t *testing.T) {
	runner := newFakeRunner()
	parts := []Participant{{AgentName: "a"}}
	for _, strat := range []string{
		StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate,
	} {
		o, err := Build(strat, runner, "app1", parts, Settings{})
		if err != nil {
			t.Fatalf("Build(%q): %v", strat, err)
		}
		if o.Strategy() != strat {
			t.Fatalf("Build(%q).Strategy() = %q", strat, o.Strategy())
		}
	}
}

func TestBuildUnknownStrategy(t *testing.T) {
	_, err := Build("nope", newFakeRunner(), "app1", nil, Settings{})
	if !errors.Is(err, ErrUnknownStrategy) {
		t.Fatalf("err = %v, want ErrUnknownStrategy", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestBuild -v`
Expected: FAIL — `undefined: Build`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/builder.go`:

```go
package orchestration

import (
	"errors"
	"fmt"
)

// ErrUnknownStrategy is returned by Build for an unrecognized strategy name.
var ErrUnknownStrategy = errors.New("orchestration: unknown strategy")

// Build constructs the Orchestrator for a strategy name. appID is the app scope
// passed to every agent run; parts and settings come from the OrchestrationConfig.
func Build(strategy string, runner AgentRunner, appID string, parts []Participant, settings Settings) (Orchestrator, error) {
	switch strategy {
	case StrategySequential:
		return newSequential(runner, appID, parts, settings), nil
	case StrategyParallel:
		return newParallel(runner, appID, parts, settings), nil
	case StrategyRouter:
		return newRouter(runner, appID, parts, settings), nil
	case StrategyHierarchical:
		return newHierarchical(runner, appID, parts, settings), nil
	case StrategyDebate:
		return newDebate(runner, appID, parts, settings), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestBuild -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add orchestration/builder.go orchestration/builder_test.go
git commit -m "feat(orchestration): add strategy builder/factory"
```

---

## Task 8: Service (load → run → persist → emit)

**Files:**
- Create: `orchestration/service.go`
- Test: `orchestration/service_test.go`

**Interfaces:**
- Consumes: `Build`, `ConfigStore`, `RunStore`, `AgentRunner`, `Blackboard`, entities, `Status*`.
- Produces: `HookEmitter` interface; `Service`; `NewService(runner, configs, runs, hooks) *Service`; `(*Service).Run(ctx, appID, name, input) (*OrchestrationRun, error)`.

- [ ] **Step 1: Write the failing test**

Create `orchestration/service_test.go`. The fakes for `ConfigStore`/`RunStore`/`HookEmitter` live here:

```go
package orchestration

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// --- fakes ---

type fakeConfigStore struct{ byName map[string]*OrchestrationConfig }

func (f *fakeConfigStore) CreateOrchestration(context.Context, *OrchestrationConfig) error { return nil }
func (f *fakeConfigStore) GetOrchestration(context.Context, id.OrchestrationConfigID) (*OrchestrationConfig, error) {
	return nil, cortex.ErrOrchestrationNotFound
}
func (f *fakeConfigStore) GetOrchestrationByName(_ context.Context, _, name string) (*OrchestrationConfig, error) {
	if c, ok := f.byName[name]; ok {
		return c, nil
	}
	return nil, cortex.ErrOrchestrationNotFound
}
func (f *fakeConfigStore) UpdateOrchestration(context.Context, *OrchestrationConfig) error { return nil }
func (f *fakeConfigStore) DeleteOrchestration(context.Context, id.OrchestrationConfigID) error {
	return nil
}
func (f *fakeConfigStore) ListOrchestrations(context.Context, *ConfigListFilter) ([]*OrchestrationConfig, error) {
	return nil, nil
}
func (f *fakeConfigStore) CountOrchestrations(context.Context, *ConfigListFilter) (int64, error) {
	return 0, nil
}

type fakeRunStore struct {
	created *OrchestrationRun
	updated *OrchestrationRun
}

func (f *fakeRunStore) CreateOrchestrationRun(_ context.Context, r *OrchestrationRun) error {
	f.created = r
	return nil
}
func (f *fakeRunStore) GetOrchestrationRun(context.Context, id.OrchestrationID) (*OrchestrationRun, error) {
	return f.created, nil
}
func (f *fakeRunStore) UpdateOrchestrationRun(_ context.Context, r *OrchestrationRun) error {
	f.updated = r
	return nil
}
func (f *fakeRunStore) ListOrchestrationRuns(context.Context, *RunListFilter) ([]*OrchestrationRun, error) {
	return nil, nil
}
func (f *fakeRunStore) CountOrchestrationRuns(context.Context, *RunListFilter) (int64, error) {
	return 0, nil
}

type recordingHooks struct {
	started, completed int
	handoffs           int
}

func (h *recordingHooks) OrchestrationStarted(context.Context, id.OrchestrationID, string) { h.started++ }
func (h *recordingHooks) OrchestrationCompleted(context.Context, id.OrchestrationID, time.Duration) {
	h.completed++
}
func (h *recordingHooks) AgentHandoff(context.Context, id.OrchestrationID, string, string, string) {
	h.handoffs++
}

// --- test ---

func TestServiceRunSequentialPersistsAndEmits(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB"}
	cfg := &OrchestrationConfig{
		ID:       id.NewOrchestrationConfigID(),
		Name:     "team",
		AppID:    "app1",
		Strategy: StrategySequential,
		Participants: []Participant{{AgentName: "a"}, {AgentName: "b"}},
	}
	configs := &fakeConfigStore{byName: map[string]*OrchestrationConfig{"team": cfg}}
	runs := &fakeRunStore{}
	hooks := &recordingHooks{}
	svc := NewService(runner, configs, runs, hooks)

	out, err := svc.Run(context.Background(), "app1", "team", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if out.Output != "BB" {
		t.Fatalf("output = %q, want BB", out.Output)
	}
	if out.ConfigID.String() != cfg.ID.String() {
		t.Fatalf("config id not linked")
	}
	if hooks.started != 1 || hooks.completed != 1 {
		t.Fatalf("hooks started=%d completed=%d, want 1/1", hooks.started, hooks.completed)
	}
	if hooks.handoffs != 1 {
		t.Fatalf("handoffs = %d, want 1", hooks.handoffs)
	}
	if runs.created == nil || runs.updated == nil {
		t.Fatalf("run not persisted (create=%v update=%v)", runs.created, runs.updated)
	}
	if runs.updated.Status != StatusCompleted {
		t.Fatalf("persisted status = %q, want completed", runs.updated.Status)
	}
}

func TestServiceRunUnknownConfig(t *testing.T) {
	svc := NewService(newFakeRunner(), &fakeConfigStore{byName: map[string]*OrchestrationConfig{}}, &fakeRunStore{}, &recordingHooks{})
	_, err := svc.Run(context.Background(), "app1", "missing", "go")
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./orchestration/ -run TestService -v`
Expected: FAIL — `undefined: NewService`.

- [ ] **Step 3: Write minimal implementation**

Create `orchestration/service.go`:

```go
package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// HookEmitter receives orchestration lifecycle events. The engine adapts the
// plugin.Registry to this interface; tests pass a recorder.
type HookEmitter interface {
	OrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string)
	OrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration)
	AgentHandoff(ctx context.Context, orchID id.OrchestrationID, from, to, payload string)
}

// noopHooks is used when no emitter is supplied.
type noopHooks struct{}

func (noopHooks) OrchestrationStarted(context.Context, id.OrchestrationID, string)            {}
func (noopHooks) OrchestrationCompleted(context.Context, id.OrchestrationID, time.Duration)   {}
func (noopHooks) AgentHandoff(context.Context, id.OrchestrationID, string, string, string)    {}

// Service runs stored orchestration configs end to end.
type Service struct {
	runner  AgentRunner
	configs ConfigStore
	runs    RunStore
	hooks   HookEmitter
}

// NewService builds a Service. hooks may be nil (a no-op emitter is used).
func NewService(runner AgentRunner, configs ConfigStore, runs RunStore, hooks HookEmitter) *Service {
	if hooks == nil {
		hooks = noopHooks{}
	}
	return &Service{runner: runner, configs: configs, runs: runs, hooks: hooks}
}

// Run loads the named config, executes the strategy, persists an OrchestrationRun,
// and fires lifecycle hooks. The returned run reflects the final state.
func (s *Service) Run(ctx context.Context, appID, name, input string) (*OrchestrationRun, error) {
	cfg, err := s.configs.GetOrchestrationByName(ctx, appID, name)
	if err != nil {
		return nil, err
	}

	orch, err := Build(cfg.Strategy, s.runner, cfg.AppID, cfg.Participants, cfg.Settings)
	if err != nil {
		return nil, err
	}

	started := nowUTC()
	rec := &OrchestrationRun{
		ID:        id.NewOrchestrationID(),
		ConfigID:  cfg.ID,
		AppID:     cfg.AppID,
		TenantID:  cortexTenant(ctx),
		Strategy:  cfg.Strategy,
		Status:    StatusRunning,
		Input:     input,
		StartedAt: started,
	}
	if err := s.runs.CreateOrchestrationRun(ctx, rec); err != nil {
		return nil, err
	}
	orchID := rec.ID
	s.hooks.OrchestrationStarted(ctx, orchID, cfg.Strategy)

	bb := NewBlackboard(orchID, cfg.Participants, func(hctx context.Context, h Handoff) {
		s.hooks.AgentHandoff(hctx, orchID, h.From, h.To, h.Payload)
	})

	result, runErr := orch.Run(ctx, input, bb)

	completed := nowUTC()
	rec.CompletedAt = &completed
	if result != nil {
		rec.Output = result.Output
		for _, ar := range result.AgentResults {
			if !ar.RunID.IsNil() {
				rec.AgentRunIDs = append(rec.AgentRunIDs, ar.RunID)
			}
		}
	}
	if runErr != nil {
		rec.Status = StatusFailed
		rec.Error = runErr.Error()
	} else {
		rec.Status = StatusCompleted
	}
	if err := s.runs.UpdateOrchestrationRun(ctx, rec); err != nil {
		return nil, err
	}
	s.hooks.OrchestrationCompleted(ctx, orchID, completed.Sub(started))

	return rec, runErr
}
```

This needs two tiny local helpers — add them to `orchestration/strategy.go`:

```go
import (
	"context"
	"time"

	"github.com/xraph/cortex"
)

func nowUTC() time.Time { return time.Now().UTC() }

func cortexTenant(ctx context.Context) string { return cortex.TenantFromContext(ctx) }
```

> Merge these imports into `strategy.go`'s existing import block (it currently has none; add the block). `cortex.TenantFromContext` is verified in `scope.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./orchestration/ -run TestService -v`
Expected: PASS — both service tests, including hook counts and persisted status.

- [ ] **Step 5: Commit**

```bash
git add orchestration/service.go orchestration/strategy.go orchestration/service_test.go
git commit -m "feat(orchestration): add Service that runs, persists, and emits hooks"
```

---

## Task 9: Engine wiring — adapter, hook emitter, RunOrchestration

**Files:**
- Create: `engine/orchestration.go`
- Test: `engine/orchestration_run_test.go`

**Interfaces:**
- Consumes: `orchestration.AgentRunner`, `orchestration.HookEmitter`, `orchestration.NewService`, `orchestration.RunOpts`/`AgentResult`; engine `e.RunAgent`, `e.store`, `e.extensions`, `RunOverrides`; `run.Run`.
- Produces: `(*Engine).RunOrchestration(ctx, appID, name, input) (*orchestration.OrchestrationRun, error)`.

- [ ] **Step 1: Write the failing test**

Create `engine/orchestration_run_test.go`:

```go
package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
)

func TestRunOrchestrationNoStore(t *testing.T) {
	e, err := engine.New()
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	_, err = e.RunOrchestration(context.Background(), "app1", "team", "go")
	if !errors.Is(err, cortex.ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
}
```

> Deeper end-to-end behavior (strategy execution, persistence, hooks) is already covered by `orchestration.Service` tests in Task 8 using fakes. This test verifies the engine guard and that wiring compiles.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -run TestRunOrchestrationNoStore -v`
Expected: FAIL — `e.RunOrchestration undefined`.

- [ ] **Step 3: Write minimal implementation**

Create `engine/orchestration.go`:

```go
package engine

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// agentRunnerAdapter adapts the engine's RunAgent to orchestration.AgentRunner.
type agentRunnerAdapter struct{ eng *Engine }

func (a agentRunnerAdapter) RunAgent(ctx context.Context, appID, agentName, input string, opts *orchestration.RunOpts) (*orchestration.AgentResult, error) {
	r, err := a.eng.RunAgent(ctx, appID, agentName, input, mapRunOpts(opts))
	if err != nil {
		return nil, err
	}
	return &orchestration.AgentResult{
		AgentName: agentName,
		RunID:     r.ID,
		Output:    r.Output,
	}, nil
}

func mapRunOpts(o *orchestration.RunOpts) *RunOverrides {
	if o == nil {
		return nil
	}
	return &RunOverrides{
		Model:        o.Model,
		Temperature:  o.Temperature,
		MaxSteps:     o.MaxSteps,
		SystemPrompt: o.SystemPrompt,
	}
}

// registryHookEmitter adapts the plugin Registry to orchestration.HookEmitter.
type registryHookEmitter struct{ eng *Engine }

func (h registryHookEmitter) OrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitOrchestrationStarted(ctx, orchID, strategy)
	}
}

func (h registryHookEmitter) OrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitOrchestrationCompleted(ctx, orchID, elapsed)
	}
}

func (h registryHookEmitter) AgentHandoff(ctx context.Context, orchID id.OrchestrationID, from, to, payload string) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitAgentHandoff(ctx, orchID, from, to, payload)
	}
}

// RunOrchestration loads a stored orchestration config by name and executes it,
// persisting an OrchestrationRun and firing orchestration lifecycle hooks.
func (e *Engine) RunOrchestration(ctx context.Context, appID, name, input string) (*orchestration.OrchestrationRun, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	svc := orchestration.NewService(
		agentRunnerAdapter{eng: e},
		e.store,
		e.store,
		registryHookEmitter{eng: e},
	)
	return svc.Run(ctx, appID, name, input)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./engine/ -run TestRunOrchestrationNoStore -v && go build ./...`
Expected: PASS, full build succeeds.

- [ ] **Step 5: Run the whole suite**

Run: `go test ./... -race`
Expected: PASS across the module; orchestration package race-clean.

- [ ] **Step 6: Commit**

```bash
git add engine/orchestration.go engine/orchestration_run_test.go
git commit -m "feat(engine): add RunOrchestration with runner adapter and hook emission"
```

---

## Done criteria for Plan 2

- [ ] All five strategies implemented and unit-tested with a fake `AgentRunner` (`-race` clean).
- [ ] `Build` factory covers all strategies + unknown-strategy error.
- [ ] `Service` loads a config, runs it, persists an `OrchestrationRun` (running → completed/failed), links agent run IDs, and fires `OrchestrationStarted`/`Completed`/`AgentHandoff` — verified with fakes.
- [ ] `engine.RunOrchestration` wires real dependencies; the dormant plugin hooks are now actually emitted.
- [ ] `go build ./... && go test ./... -race` green.

Next: **Plan 3 — API & Examples** (HTTP CRUD + `POST /cortex/orchestrations/:name/run`, `isNotFound` wiring, a runnable Go example, and a fumadocs page).
