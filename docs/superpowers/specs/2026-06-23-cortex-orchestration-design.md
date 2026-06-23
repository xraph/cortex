# Cortex Orchestration — Multi-Agent Layer (Phase 10) — Design

- **Status:** Approved (brainstorming) — ready for implementation planning
- **Date:** 2026-06-23
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex` (all changes live here).
- **Related:** Implements Phase 10 of `_project_files/CORTEX-DESIGN.md`. Activates the dormant orchestration plugin hooks (`OrchestrationStarted`, `OrchestrationCompleted`, `AgentHandoff`) and the reserved `id.OrchestrationID`.

---

## 1. Goal

Give cortex a **multi-agent orchestration layer**: the ability to run multiple agents together — in sequence, in parallel, hierarchically (manager → workers), via a router, or in a debate — with the agents able to **communicate** (shared blackboard + explicit handoffs) and stay **aware** of each other (participant roster). This is the capability gap surfaced by the question "does cortex support multi-agents, cloning, sub-agents, agent communication and awareness running together?" — today the answer is no; this spec makes it yes for everything except agent cloning (out of scope, see §11).

Concretely, after this lands:

1. **Five working strategies** — sequential, parallel, hierarchical, router, debate.
2. **Communication + awareness** — a shared, scoped `Blackboard` every participant can read/write, plus explicit `from→to→payload` handoffs that fire the `AgentHandoff` hook. A `Roster` of participants (name/role/skills) gives each agent awareness of who else is in the orchestration.
3. **Both definition styles** — programmatic Go constructors (`orchestration.Build(...)` / strategy `new*` factories) AND stored, named `OrchestrationConfig` entities with CRUD and a `POST /v1/orchestrations/:name/run` endpoint (route group is `/v1`, matching every existing handler — the design-doc's `/cortex` prefix does not reflect the code).
4. **Full persistence** — an `OrchestrationRun` execution record (mirrors `run/`) and an `OrchestrationConfig` definition (mirrors `persona/`), persisted across the three real store backends: **sqlite, postgres, mongo**.

### Non-goals

- **Agent cloning / forking** — no clone endpoint or template-copy mechanism. Tracked separately (§11).
- **A pub/sub message bus** — agents are request/response (`RunAgent`). True mid-run agent-to-agent chatter would require interruptible, long-lived agents; rejected in brainstorming as a larger rework. The blackboard + handoff model covers communication and awareness without it.
- **New reasoning loops** — orchestration drives existing agent execution; it does not add ReAct/Plan-Execute variants.

---

## 2. Background — where this fits

```
cortex engine
  ├── RunAgent(appID, name, input, overrides) → *run.Run        ← single-agent (exists today)
  └── RunOrchestration(appID, name, input)    → *OrchestrationRun ← NEW (this spec)
         │
         └── orchestration/  (strategies drive RunAgent via the AgentRunner seam)
                 Blackboard (shared state) · Roster (awareness) · Handoffs (comms)
```

Facts the design builds on (verified in code):

- `id.OrchestrationID` exists (`orch_` prefix) — [id/id.go](../../../id/id.go).
- Plugin hooks `OrchestrationStarted` / `OrchestrationCompleted` / `AgentHandoff` and their emitters exist but are **never called** — [plugin/plugin.go:113](../../../plugin/plugin.go), [plugin/registry.go:317](../../../plugin/registry.go). This spec is their first caller.
- Engine exposes `RunAgent(ctx, appID, agentName, input, *RunOverrides) (*run.Run, error)` — the primitive every strategy drives.
- Entities follow a fixed pattern: embed `cortex.Entity`, carry `AppID`, expose a `Store` interface + `ListFilter`, fold into the composite `store.Store`. Persona is the reference (`persona/persona.go`).
- The API `registerOrchestrationRoutes` method does **not** exist yet (added in Plan 3); every existing handler registers under the `/v1` route group, so the run endpoint is `POST /v1/orchestrations/:name/run`.

---

## 3. Architecture — the `AgentRunner` seam

The engine must not import `orchestration` while `orchestration` imports `engine` (cycle). Resolution (chosen approach **A** in brainstorming):

`orchestration` defines a narrow interface it depends on; the engine satisfies it via a thin adapter.

```go
// orchestration/orchestrator.go
package orchestration

// AgentRunner is the single capability orchestrators need from the host:
// the ability to run one agent and get its result. The engine satisfies it.
type AgentRunner interface {
    RunAgent(ctx context.Context, appID, agentName, input string, opts *RunOpts) (*AgentResult, error)
}

// RunOpts mirrors the subset of engine.RunOverrides orchestration needs.
type RunOpts struct {
    Model       string
    Temperature *float64
    MaxSteps    int
    SystemPrompt string
}

// AgentResult is the strategy-facing view of a completed agent run.
type AgentResult struct {
    AgentName string
    RunID     id.AgentRunID
    Output    string
    Err       error
}
```

The engine provides an adapter (in `engine/`, not `orchestration/`) that wraps `e.RunAgent` and maps `*run.Run` → `*AgentResult`. Because `orchestration` imports only `id` and `cortex` (leaf packages) — reaching the plugin hooks through an injected callback rather than importing `plugin` (see §5), and routing every decision through `AgentRunner` rather than importing `llm` — there is no cycle. Every strategy is unit-testable with a fake `AgentRunner` alone, with no real LLM or store.

> **Meta-decision refinement (vs original spec):** router/hierarchical/debate make their LLM-driven decisions through **dedicated participant agents** (a router agent, a manager agent, a judge agent) run via the same `AgentRunner`, not a raw `llm.Client`. This keeps decisions consistent with the agent model, removes the `llm` import, and makes the decisions unit-testable with a fake runner. The router additionally supports an LLM-free static rule map.

---

## 4. Package layout

```
orchestration/
  orchestrator.go      # Orchestrator interface, AgentRunner, RunOpts, AgentResult, Result, Participant
  blackboard.go        # Blackboard: shared scoped state + ordered entry log + Roster + handoff helper
  config.go            # OrchestrationConfig entity + Store interface + ListFilter (stored form)
  run.go               # OrchestrationRun execution record + Store interface + ListFilter
  builder.go           # programmatic constructors: NewSequential / NewParallel / NewRouter / NewHierarchical / NewDebate
  sequential.go        # strategy: ordered chain
  parallel.go          # strategy: concurrent fan-out + merge
  router.go            # strategy: pick ONE participant (LLM or rule)
  hierarchical.go      # strategy: manager delegates sub-tasks to workers
  debate.go            # strategy: N agents, R rounds, judge aggregates
  *_test.go            # per-strategy unit tests (fake runner + MockClient)
```

Store implementations and wiring land in existing packages. Migrations are **programmatic grove migrations** (a `migrate.Migration` with a `Version` string registered in each backend's `migrations.go`) — there are no raw `.sql` files:

```
store/store.go                  # fold the two Stores into the composite interface
store/sqlite/orchestration.go   # sqlite CRUD impl
store/sqlite/models.go          # add orchestration models + converters (extend)
store/sqlite/migrations.go      # add migration version "20240101000009" (extend)
store/postgres/orchestration.go # postgres CRUD impl
store/postgres/models.go        # add orchestration models + converters (extend)
store/postgres/migrations.go    # add migration version "20240101000009" (extend)
store/mongo/orchestration.go    # mongo CRUD impl
store/mongo/models.go           # add orchestration models + converters (extend)
store/mongo/migrations.go       # add collection-create migration (extend)
engine/orchestration.go         # RunOrchestration + CRUD methods + AgentRunner adapter + hook emission
api/orchestration_handler.go    # CRUD + run handlers
api/requests.go                 # orchestration request structs (extend)
id/id.go                        # add PrefixOrchestrationConfig ("orchcfg") + constructors/parsers
errors.go                       # add ErrOrchestrationNotFound, ErrOrchestrationRunNotFound
```

---

## 5. Core types

### Orchestrator

```go
// Orchestrator is one multi-agent execution strategy.
type Orchestrator interface {
    Strategy() string // "sequential" | "parallel" | "router" | "hierarchical" | "debate"
    Run(ctx context.Context, input string, bb *Blackboard) (*Result, error)
}

// Result is the outcome of an orchestration run.
type Result struct {
    OrchestrationID id.OrchestrationID
    Strategy        string
    Output          string         // final synthesized output
    AgentResults    []AgentResult  // per-agent results, in execution order
    Handoffs        []Handoff      // recorded handoff log
    Elapsed         time.Duration
    Err             error
}

// Participant is one agent in an orchestration, with metadata for awareness.
type Participant struct {
    AgentName string
    Role      string   // e.g. "manager", "worker", "critic", "judge"
    Skills    []string // advisory; surfaced to other agents via the roster
}
```

### Blackboard — communication + awareness

```go
// Blackboard is the shared, mutex-guarded state for one orchestration run.
type Blackboard struct {
    // values: arbitrary shared key/value state agents read and write
    // entries: ordered append-only log of contributions (for replay/debug)
    // roster:  participants, giving each agent awareness of the others
    // handoffs: recorded from→to→payload events
}

func (b *Blackboard) Read(key string) (any, bool)
func (b *Blackboard) Write(key string, val any)
func (b *Blackboard) Append(agentName, content string)        // contribution log
func (b *Blackboard) Snapshot() string                        // rendered context injected into agent input
func (b *Blackboard) Roster() []Participant                   // awareness
func (b *Blackboard) Handoff(ctx, from, to, payload string)   // records + fires AgentHandoff hook
```

`Snapshot()` renders the roster + prior contributions into a compact text block that strategies prepend to an agent's input, so each agent "sees" who else is participating and what they have said. This is how awareness and communication reach the agent without changing `RunAgent`'s signature.

Hook emission is injected, not imported: the `Blackboard` holds a `func(ctx, from, to, payload)` handoff callback wired by the engine to `Registry.EmitAgentHandoff`. Keeps `orchestration` free of an `engine` import.

### Entities

`OrchestrationConfig` (definition, stored) and `OrchestrationRun` (execution record, stored) both follow the persona pattern exactly:

```go
type OrchestrationConfig struct {
    cortex.Entity
    ID           id.OrchestrationConfigID `json:"id"`
    Name         string                   `json:"name"`
    Description  string                   `json:"description,omitempty"`
    AppID        string                   `json:"app_id"`
    Strategy     string                   `json:"strategy"`          // one of the five
    Participants []Participant            `json:"participants"`
    Settings     Settings                 `json:"settings,omitempty"` // rounds, max_concurrency, judge, router rules, manager
    Metadata     map[string]any           `json:"metadata,omitempty"`
}

type OrchestrationRun struct {
    cortex.Entity
    ID           id.OrchestrationID `json:"id"`
    ConfigID     id.OrchestrationConfigID `json:"config_id,omitempty"` // empty for programmatic runs
    AppID        string             `json:"app_id"`
    TenantID     string             `json:"tenant_id,omitempty"`
    Strategy     string             `json:"strategy"`
    Status       string             `json:"status"`   // running | completed | failed
    Input        string             `json:"input"`
    Output       string             `json:"output,omitempty"`
    Error        string             `json:"error,omitempty"`
    AgentRunIDs  []id.AgentRunID    `json:"agent_run_ids,omitempty"` // links to underlying run.Run records
    StartedAt    time.Time          `json:"started_at"`
    CompletedAt  *time.Time         `json:"completed_at,omitempty"`
}
```

`Settings` is a single struct carrying every strategy's tunables (rounds, max concurrency, judge agent name, router rules / router-agent, manager agent name). Unused fields are simply ignored by strategies that do not need them — avoids a per-strategy config-type explosion.

---

## 6. The five strategies

All strategies receive an `AgentRunner`, a `[]Participant`, and a `Settings`. All drive `runner.RunAgent` and record handoffs/contributions on the blackboard. Meta-decisions are made by dedicated participant agents (see refinement note in §3).

| Strategy | Behavior | Meta-decision | Handoffs recorded |
|---|---|---|---|
| **Sequential** | Run participants in order. Each agent's input = original input + blackboard snapshot (prior outputs). Final output = last agent's output. | none | between each consecutive pair |
| **Parallel** | All participants run concurrently on the same input (bounded by `Settings.MaxConcurrency`, stdlib semaphore). Outputs appended to blackboard. Optional aggregator agent synthesizes; else outputs concatenated. | optional aggregator agent | none (fan-out) |
| **Router** | A routing decision selects exactly ONE participant to handle the input — via a static `RouterRules` map (keyword→agent) or a `RouterAgent` that names the choice. | router agent or rules | router→chosen |
| **Hierarchical** | A "manager" participant produces a JSON delegation plan (sub-task → worker). Workers execute (bounded concurrency). Manager runs again to compose the final answer; invalid plans fall back to all-workers-on-input. | manager agent plan | manager→each worker |
| **Debate** | N participants respond over `Settings.Rounds` rounds; each round each agent sees others' prior arguments via the blackboard. A "judge" participant aggregates a final verdict (else last argument). | judge agent | round transitions + judge |

Robustness rules common to all:

- A participant error is captured in its `AgentResult.Err`; strategy-level behavior on error is configurable per strategy (sequential aborts by default; parallel/debate collect and continue; the `Result.Err` is set if the strategy cannot produce an output).
- Context cancellation propagates to in-flight `RunAgent` calls.
- Hard caps: `MaxConcurrency` (parallel/hierarchical workers), `Rounds` (debate), and a participant count sanity limit — logged, never silently truncated.

---

## 7. Persistence

- **New IDs:** add `PrefixOrchestrationConfig = "orchcfg"` with `NewOrchestrationConfigID` / `ParseOrchestrationConfigID` to `id/id.go` (mirrors existing constructors). `OrchestrationID` (`orch_`) already exists and is reused for run records.
- **Store interfaces:** `OrchestrationConfig.Store` (Create/Get/GetByName/Update/Delete/List/Count) and `OrchestrationRun.Store` (Create/Get/Update/List/Count) — both shapes copied from persona/run. Folded into `store.Store`.
- **Migration:** a programmatic grove migration, version `20240101000009`, registered in each backend's `migrations.go` (sqlite/postgres add a `migrate.Migration` creating tables `cortex_orchestration_configs` and `cortex_orchestration_runs`; mongo adds a collection-create migration). App-scoped; JSON/JSONB columns for participants/settings/agent_run_ids; indexed on `(app_id, name)` and `(app_id, status)`.
- **Implementations:** sqlite, postgres, and mongo each get an `orchestration.go` plus model structs + converters in `models.go`. Correctness is guarded at compile time by the existing `var _ store.Store = (*Store)(nil)` assertion in each backend — adding the two Stores to the composite forces all three backends to implement them or the build fails.

---

## 8. Engine + API wiring

**Engine** (`engine/orchestration.go`):

- `RunOrchestration(ctx, appID, name, input string) (*OrchestrationRun, error)` — loads the stored `OrchestrationConfig`, builds the matching `Orchestrator`, creates an `OrchestrationRun` (status `running`), wires a `Blackboard` whose handoff callback calls `e.extensions.EmitAgentHandoff`, fires `EmitOrchestrationStarted` before and `EmitOrchestrationCompleted` after, persists the final record, and links underlying `AgentRunID`s.
- CRUD methods for `OrchestrationConfig` mirroring the persona methods (`CreateOrchestration`, `GetOrchestration`, `GetOrchestrationByName`, `UpdateOrchestration`, `DeleteOrchestration`, `ListOrchestrations`, `CountOrchestrations`) and read methods for `OrchestrationRun` (`GetOrchestrationRun`, `ListOrchestrationRuns`, `CountOrchestrationRuns`).
- An unexported `agentRunnerAdapter` wrapping `e.RunAgent` → `orchestration.AgentRunner`.

**API** (`api/orchestration_handler.go`), registered through the existing `registerOrchestrationRoutes` slot:

```
POST   /v1/orchestrations                 createOrchestration
GET    /v1/orchestrations                 listOrchestrations
GET    /v1/orchestrations/:name           getOrchestration
PUT    /v1/orchestrations/:name           updateOrchestration
DELETE /v1/orchestrations/:name           deleteOrchestration
POST   /v1/orchestrations/:name/run       runOrchestration
GET    /v1/orchestration-runs             listOrchestrationRuns
GET    /v1/orchestration-runs/:id         getOrchestrationRun
```

Request structs added to `api/requests.go` with `path:`/`query:`/`json:` tags, following the existing handler pattern.

---

## 9. Testing

The repo currently has **no store-backend unit tests** — the store layer is guarded by compile-time `var _ store.Store` interface assertions, and the meaningful logic lives in the orchestration package, which is fully testable without a database. Testing posture follows that reality:

- **Strategy unit tests** — each of the five with a deterministic fake `AgentRunner` (records calls, returns canned outputs keyed by agent name, including the router/manager/judge decision agents). Assert: agent invocation order, blackboard contents, handoff log, final output. No DB, no LLM.
- **Blackboard tests** — concurrent read/write safety, snapshot rendering, roster, handoff callback fires. No DB.
- **Core type / ID tests** — `orchcfg` prefix round-trips; `Result`/`Participant` construction. Extends `id/id_test.go`.
- **Store backends** — guarded at compile time by the existing `var _ store.Store = (*Store)(nil)` assertion in each backend (a missing method fails `go build`). No new DB test harness is introduced (the repo has none to follow).
- **Service/engine integration** — the in-package `Service` is exercised with a fake `AgentRunner`, fake config/run stores, and a recording `HookEmitter`, asserting hooks fire, the run record transitions running→completed/failed, and agent run IDs are linked. The engine's `RunOrchestration` is a thin wrapper verified by a no-store guard test plus `go build`.
- **API** — handler tests for create + run, asserting the run endpoint resolves a stored config and returns an `OrchestrationRun`.
- **Backwards compatibility** — existing single-agent `RunAgent` path untouched; full suite green (`go build ./... && go test ./...`).

---

## 10. Build sequence (for the implementation plan)

**Plan 1 — Foundation & Persistence:**
1. IDs (`orchcfg` prefix) + `errors.go` sentinels.
2. `orchestration/` core types (`orchestrator.go`, `blackboard.go`) + tests.
3. Entities (`config.go`, `run.go`) + store interfaces + fold into `store.Store`.
4. Store implementations + programmatic migrations: sqlite → postgres → mongo (compile-time `store.Store` guard forces all three).
5. Engine CRUD methods.

**Plan 2 — Strategies & Execution:**
6. Strategies in order of dependency: sequential → parallel → router → hierarchical → debate, each with tests, plus `builder.go` constructors.
7. Engine: `AgentRunner` adapter, `RunOrchestration`, hook emission, integration test.

**Plan 3 — API & Examples:**
8. API: handler + request structs + route registration.
9. Example under `_examples/multi-agent/`, docs.

---

## 11. Out of scope / follow-ups

- **Agent cloning / forking** — the one remaining capability from the original question. Separate spec: a clone endpoint / template-copy on `agent.Config` (and possibly `Persona`). Not blocked by this work.
- **Message-bus comms** — if true concurrent agent-to-agent messaging is later needed, it builds on the blackboard but requires interruptible agents.
- **`weave`/`fabriq` brain interplay** — orchestration is store-agnostic; no interaction with the fabriq-brain spec (`2026-06-22-cortex-fabriq-brain-design.md`).
