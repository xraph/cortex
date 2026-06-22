# Cortex ↔ Fabriq: Plug-n-Play Living Brain — Design

- **Status:** Approved (brainstorming) — ready for implementation planning
- **Date:** 2026-06-22
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex` (all changes live here). `github.com/xraph/fabriq` requires **no changes** (standalone discipline preserved).
- **Related:** Follow-up spec #2 — `weave ↔ fabriq` vectorstore backend (`weave/vectorstore/fabriq`, mirrors `pgvector`), to be written **after** this lands.

---

## 1. Goal

Make **fabriq** a plug-n-play **living brain** for **cortex** agents: an agent built on cortex gains, with one import and one wiring call, fabriq-backed

1. **Recall (read)** — multi-channel knowledge retrieval (vector + full-text + graph, RRF-fused, distillation-aware).
2. **Rich tools** — fabriq's `graph_traverse`, guarded `remember`/write, `map`/`digest`/`resolve`, exposed as native cortex tools.
3. **Learning loop (write-back)** — agent runs/tool-calls flow back into the fabric and become future recall material via fabriq's existing embed + distillation workers.
4. **One-line wiring** — a `vessel`-based engine option that auto-discovers the fabriq facade from DI and enables all of the above (mirrors `knowledge/weave`'s `EngineOption`).

### Non-goals

- Backing cortex's composite `store.Store` (agents/skills/runs/checkpoints — operational persistence, not "brain"). Out of scope.
- `weave ↔ fabriq` integration — separate, later spec (vectorstore-only).
- Any change to fabriq core. The bridge consumes fabriq's public surfaces only.

---

## 2. Background — the three layers

```
cortex   (agent orchestration: personas, runs, checkpoints, plugins)
  │  knowledge.Provider
  ├──────────────◄ fabriq        ← THIS SPEC (direct entity/graph/distill brain + tools + learning loop)
  └──────────────◄ weave         ← already exists (document RAG)
                     │  vectorstore.VectorStore  (memory | pgvector | fabriq*)
weave    (RAG pipeline: load → chunk → embed → retrieve → assemble)
  └──────────────────────────────◄ fabriq        ← spec #2 (*later, vectorstore-only)
fabriq   (data fabric: entities · vectors · search · graph · distillation · live · blobs)
```

The cortex↔fabriq bridge and the weave↔fabriq bridge are **different seams at different layers** and are complementary, not redundant.

### What already exists (verified)

**Cortex**

- `knowledge.Provider` — `Retrieve(ctx, query string, *RetrieveParams) ([]ScoredChunk, error)` + `ListCollections(ctx) ([]CollectionInfo, error)`. Explicitly designed to decouple cortex from RAG engines, "the same way `llm.Client` decouples it from LLM providers."
- `engine.WithKnowledge(provider)` — when set, the engine auto-exposes a `knowledge_search` tool and injects `KnowledgeRef` context into prompts.
- `engine.WithExtension(plugin.Extension)` — registers a plugin; 16 opt-in lifecycle hook interfaces (`RunStarted`, `RunCompleted`, `RunFailed`, `ToolCalled`, `ToolCompleted`, …).
- `knowledge/weave/adapter.go` — the precedent: lives **inside cortex**, imports the external engine, implements `knowledge.Provider`, exposes `EngineOption(c vessel.Vessel) engine.Option` that `vessel.Inject`s the engine from DI and returns `engine.WithKnowledge(...)` (or a no-op when absent).
- **Constraint:** the engine tool system is currently **closed** — `resolveTools(_ []string)` returns only `builtinTools()`, and `executeTool` only dispatches `executeBuiltinTool`. There is no external tool-registration path, so skill `ToolBinding`s are not executable today.

**Fabriq**

- `*fabriq.Fabriq` facade — implements `query.Fabric`, exposes `.Registry() *registry.Registry`.
- `forgeext.Extension` — the Forge extension; **provides `*fabriq.Fabriq` into the vessel DI container** via `vessel.Provide(c, func() (*fabriq.Fabriq, error) {…})` (alias `"fabriq"`). Also exposes `.Fabriq()` and `.Stores()` (`*fabriq.Stores`, incl. `.CAS`).
- `core/agent.Toolkit` — the transport-agnostic brain:
  - `NewToolkit(fab query.Fabric, reg *registry.Registry, emb Embedder, cfg Config) (*Toolkit, error)`
  - `Recall(ctx, RecallRequest) (ContextPack, error)` — multi-channel RRF (vector + search + graph) + altitude/distillation, token-budget packed.
  - `Remember(ctx, RememberRequest) (command.Result, error)` — guarded write (deny-by-default `WritePolicy`), through the command plane (tenant scope + lifecycle hooks apply).
  - `Watch(ctx, query.SubscribeScope) (<-chan query.Delta, error)` — live deltas.
  - `Map`, `Digest`, `Resolve` — distillation navigation.
  - `Tools() []Tool` — a transport-neutral tool catalog (`recall`, `vector_similar`, `search`, `graph_traverse`, `get`, `remember`, `map`, `digest`, `resolve`).
- `agent.Tool` — `{Name string; Description string; InputSchema json.RawMessage; Handler func(ctx, json.RawMessage) (any, error)}`. Maps 1:1 to cortex's `llm.Tool` + a handler. `forgeext/agentmcp` already adapts this catalog to MCP — the same conversion the bridge needs.

---

## 3. Architecture

All new code lives in cortex. One cohesive bridge package + one small core enhancement.

```
github.com/xraph/cortex/
├── engine/                          # PHASE 0 — pluggable tool registry (core change)
│   ├── options.go                   #   + WithTool(def llm.Tool, h ToolHandler) Option
│   └── react.go / tools.go          #   resolveTools + executeTool consult the registry
└── integrations/fabriq/             # NEW bridge package (imports github.com/xraph/fabriq)
    ├── provider.go                  #   Adapter: knowledge.Provider over Toolkit.Recall   (Phase 1)
    ├── render.go                    #   ContextItem → chunk Content renderer (pluggable)  (Phase 1)
    ├── tools.go                     #   Toolkit.Tools() → cortex tools + handlers          (Phase 3)
    ├── plugin.go                    #   learning-loop plugin.Extension                     (Phase 4)
    ├── config.go                    #   Option funcs (embedder, entities, budget, policy…)
    └── wire.go                      #   EngineOption / EngineOptions(vessel) bundlers       (Phase 2)
```

### Component boundaries

| Unit | Does | Uses | Depends on |
|---|---|---|---|
| `engine.WithTool` registry | Lets any caller register an executable tool | — | cortex `llm.Tool` |
| `fabriq.Adapter` (provider) | `knowledge.Provider` over `Toolkit.Recall` | a `*agent.Toolkit` + render fn + config | fabriq `core/agent`, cortex `knowledge` |
| `fabriq` tools | Adapt `Toolkit.Tools()` → cortex tools | the same `*agent.Toolkit` | cortex `engine`/`llm`, fabriq `core/agent` |
| `fabriq.Plugin` (learning loop) | Persist run/tool activity into the fabric | the same `*agent.Toolkit` (`Remember`) | cortex `plugin`, fabriq `core/agent` |
| `wire.go` | Discover fabriq from DI, build Toolkit, return engine options | `vessel`, the above units | all of the above |

The `*agent.Toolkit` is the single shared seam: the provider, the tools, and the plugin are three faces of the same toolkit instance.

**Package naming:** the bridge lives in directory `integrations/fabriq` but its Go package name must **not** be `fabriq` — that would collide with the imported `github.com/xraph/fabriq` root package (used as `fabriq.Fabriq` in `wire.go`). Use `package fabriqbrain` (directory stays `integrations/fabriq`), so hosts import it as `fabriqbrain "github.com/xraph/cortex/integrations/fabriq"`. The host example below uses that alias.

---

## 4. Phase specs

Each phase is independently shippable. The implementation plan stages them; the design captures all.

### Phase 0 — Cortex core: pluggable tool registry

Add an external tool-registration path so fabriq's tools (and skill `ToolBinding`s generally) can execute.

```go
// engine/options.go
type ToolHandler func(ctx context.Context, arguments string) (string, error)

// WithTool registers an executable tool with the engine. The def is advertised
// to the LLM via resolveTools; the handler runs in executeTool.
func WithTool(def llm.Tool, h ToolHandler) Option
```

- New `Engine` field: `tools map[string]registeredTool` where `registeredTool{def llm.Tool; handler ToolHandler}`.
- `resolveTools` appends registered defs to `builtinTools()` (current behaviour — return-all — is preserved; name-filtering by an agent's bound tools is a future improvement, out of scope here).
- `executeTool`: after `executeBuiltinTool` reports not-handled, look up `e.tools[tc.Name]` and invoke the handler; unknown tool → existing error path unchanged.
- Backward compatible: engines that never call `WithTool` behave exactly as today.

### Phase 1 — `knowledge.Provider` adapter (recall)

```go
// integrations/fabriq/provider.go
type Adapter struct {
    tk     *agent.Toolkit
    render func(it agent.ContextItem) string
    cfg    providerConfig   // default entities, default budget, K
}

var _ knowledge.Provider = (*Adapter)(nil)

func New(tk *agent.Toolkit, opts ...Option) *Adapter
func (a *Adapter) Retrieve(ctx, query string, p *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error)
func (a *Adapter) ListCollections(ctx) ([]knowledge.CollectionInfo, error)
```

**`Retrieve` flow**

1. Apply tenant scope (see §5).
2. `entities`: `[p.Collection]` if set, else `cfg.DefaultEntities` (default = all registry entities carrying a vector or search index).
3. `budget`: `cfg.Budget` (default e.g. 4096). Required because `Recall` rejects a non-positive budget; `RetrieveParams` carries no budget. Optionally scaled from `TopK`.
4. `k`: `p.TopK` if > 0, else `cfg.K`.
5. `pack, err := tk.Recall(ctx, agent.RecallRequest{Query: query, Budget: budget, Entities: entities, K: k})`.
6. Map each `ContextItem` → `ScoredChunk`, **filter** by `p.MinScore`, **cap** to `p.TopK`.

**`ContextItem` → `ScoredChunk` mapping**

| ScoredChunk field | From |
|---|---|
| `Content` | `render(item)` — default `string(item.Row)`; pluggable (digest/summary items can resolve richer text via `Toolkit.Digest`/`Resolve`). |
| `Score` | `item.Score` (fused RRF score) |
| `Source` | `strings.Join(item.Source, "+")` (contributing channels: `vector`/`search`/`graph`) |
| `DocumentID` | `item.ID` |
| `CollectionID` | `item.Entity` |
| `Metadata` | `{"entity": item.Entity, "channels": …, "tokens": …}` |

**`ListCollections`** — enumerate recall-able registry entities → `CollectionInfo{ID: entity, Name: entity, …}`. Counts best-effort (may be 0); `EmbeddingModel` from the configured embedder when known.

Recall already blends vector + search + graph + distillation internally, so the provider stays thin: the brain's richness lives in fabriq, not in the adapter.

### Phase 2 — One-line DI wiring

Mirror `knowledge/weave`'s pattern, extended to the full brain.

```go
// integrations/fabriq/wire.go

// EngineOption wires ONLY the knowledge provider (parity with weave.EngineOption).
// Returns a no-op option when no fabriq facade is in the container.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option

// EngineOptions wires the FULL brain: knowledge provider + rich tools + learning-loop plugin.
// Returns nil (safe to spread) when no fabriq facade is present.
func EngineOptions(c vessel.Vessel, opts ...Option) []engine.Option
```

Internals:
1. `f, err := vessel.Inject[*fabriq.Fabriq](c)`; on error → no-op / nil.
2. Build the toolkit: `agent.NewToolkit(f, f.Registry(), embedder, toolkitCfg)`. `embedder` comes from a bridge `WithEmbedder(agent.Embedder)` option — it **must** match the embedder fabriq's index/embed worker used (same model + dims), or recall's vector channel mismatches. If nil, recall degrades gracefully to search + graph only (documented).
3. Wire CAS from `f`'s stores into the toolkit config (so `digest`/`resolve` can fetch summary text), as `agentmcp` does.
4. Return `[WithKnowledge(adapter), WithTool(...)×N, WithExtension(plugin)]`.

Host usage:

```go
eng, _ := engine.New(append(
    []engine.Option{engine.WithStore(store), engine.WithLLM(client)},
    fabriqbrain.EngineOptions(container, fabriqbrain.WithEmbedder(emb))...,
)...)
```

An optional cortex Forge-extension bundler (so a Forge host enables the brain by registering one extension) is a **stretch** within this phase; the `vessel` `EngineOptions` is the primary, documented surface.

### Phase 3 — Rich tools

Mechanical adaptation of fabriq's existing tool catalog through the Phase-0 registry.

```go
// integrations/fabriq/tools.go
func toolOptions(tk *agent.Toolkit, allow toolSet) []engine.Option {
    var opts []engine.Option
    for _, t := range tk.Tools() {     // recall, vector_similar, search, graph_traverse, get, remember, map, digest, resolve
        if !allow(t.Name) { continue }
        def := llm.Tool{Name: t.Name, Description: t.Description, Parameters: schemaOf(t.InputSchema)}
        h := func(ctx context.Context, args string) (string, error) {
            out, err := t.Handler(ctx, json.RawMessage(args))
            if err != nil { return "", err }
            b, _ := json.Marshal(out)
            return string(b), nil
        }
        opts = append(opts, engine.WithTool(def, h))
    }
    return opts
}
```

- `recall` overlaps the engine's auto `knowledge_search`; default **skip** `recall` in the custom set (configurable) to avoid duplicate tools.
- Writes (`remember`) are governed by fabriq's deny-by-default `WritePolicy` (from the toolkit config). Default allowlist is empty → `remember` is advertised but rejects writes until the host opts in entities/ops.
- `watch` is **not** a request/response tool; it is exposed as recall material / streaming via fabriq's own surfaces, not registered as an LLM tool here.

### Phase 4 — Learning loop (write-back)

A cortex `plugin.Extension` that turns agent activity into future recall material, reusing fabriq's existing embed + distillation workers (no new indexing machinery).

```go
// integrations/fabriq/plugin.go
type Plugin struct {
    tk      *agent.Toolkit
    entity  string                 // fabriq entity to write memory rows into (default "agent_memory")
    inflight sync.Map              // runID -> input, captured at OnRunStarted
}

func (p *Plugin) Name() string { return "fabriq-brain" }

// opt-in hooks:
func (p *Plugin) OnRunStarted(ctx, agentID, runID, input string) error   // stash input
func (p *Plugin) OnRunCompleted(ctx, agentID, runID, output string, d time.Duration) error // write {input, output, …}
func (p *Plugin) OnRunFailed(ctx, agentID, runID, err error) error       // optional: write failure
func (p *Plugin) OnToolCompleted(ctx, runID, tool, result string, d time.Duration) error    // optional: tool trace
```

- On `OnRunCompleted`, write via `tk.Remember(ctx, RememberRequest{Entity: p.entity, Op: "create", Payload: {agentID, runID, input, output, tenant, app, ts}})`.
- The host must (a) register `p.entity` in fabriq's registry **with a vector index** so the `proj:embed` worker vectorizes new rows, and (b) include `{p.entity: {create}}` in the toolkit's `WritePolicy`. Both are documented host requirements.
- Closed loop: run completes → memory row written → embed worker vectorizes → distillation rolls up → next `Recall` surfaces it. Distillation keeps the memory corpus summarised so recall stays within budget as history grows.
- Failure isolation: plugin write errors are logged, never fail the agent run (hooks return errors but the bridge swallows non-critical ones per cortex plugin conventions — confirm dispatch semantics during impl).

---

## 5. Cross-cutting — multi-tenancy

Both cortex (`cortex.WithTenant`/`WithApp`) and fabriq/weave ("tenant-scoped isolation via Forge scope context") derive tenancy from **Forge scope context**. The `ctx` cortex hands the provider/tools/plugin therefore already carries the scope fabriq reads — translation is expected to be a **no-op** in the common case.

To stay safe if the scope keys differ, the bridge exposes:

```go
func WithTenantMapper(fn func(ctx context.Context) context.Context) Option   // default: identity
```

Every entry point (`Retrieve`, tool handlers, plugin hooks) runs `ctx = cfg.tenant(ctx)` before touching the toolkit. The default identity mapper is correct when both sides share Forge scope; the override exists for divergence.

---

## 6. Data flow

**Recall (read path)**

```
cortex agent step → knowledge_search tool → Adapter.Retrieve(ctx, q, params)
  → tenant scope → Toolkit.Recall (vector ⊕ search ⊕ graph → RRF → hydrate → altitude → pack)
  → []ContextItem → render+filter+cap → []ScoredChunk → injected into the LLM prompt
```

**Learning loop (write path)**

```
cortex run completes → Plugin.OnRunCompleted → Toolkit.Remember (guarded create on agent_memory)
  → command plane (tenant + lifecycle hooks) → event stream
  → fabriq proj:embed worker (vectorize) + distillation worker (roll up)
  → recall-able on the next Retrieve
```

---

## 7. Error handling

- **No embedder:** recall runs search + graph only; a warning is surfaced in `ContextPack.Warnings`; provider still returns results. Documented as a degraded mode.
- **Channel failures:** the toolkit is lenient by default (`Config.Strict=false`) — per-channel failures become warnings, not errors. The bridge defaults to lenient; `WithStrict` opt-in available.
- **Guarded write denied:** `Remember` returns a typed `WriteError{Code:"not_allowed"}`; the plugin logs and continues (never breaks the run). Tool-path `remember` returns the structured error to the LLM.
- **No fabriq in DI:** `EngineOption`/`EngineOptions` return a no-op/nil — wiring is always safe to include unconditionally (weave parity).
- **Version conflicts / not-found** on writes: surfaced as typed `WriteError` codes (`version_conflict`, `not_found`).

---

## 8. Testing strategy

- **Phase 0 (engine):** unit — register a tool; `resolveTools` includes it; `executeTool` dispatches it; unknown tool still errors; no-registration path unchanged.
- **Phase 1 (provider):** unit against a fake toolkit/fabric (`fabriqtest` fakes / `fabriqtest.NewFabric`): mapping correctness, `MinScore` filter, `TopK` cap, `Collection` routing, empty-result handling, `ListCollections` enumeration.
- **Phase 2 (wiring):** unit — a `vessel` container with a provided `*fabriq.Fabriq` yields non-nil options; an empty container yields no-op/nil. Embedder-nil degraded path.
- **Phase 3 (tools):** unit — every `agent.Tool` round-trips to an `llm.Tool` + handler; handler marshals results; integrates with `engine.executeTool`. `recall` skipped by default.
- **Phase 4 (plugin):** unit — `OnRunStarted`→`OnRunCompleted` issues the expected `Remember`; tenant propagation; write-denied is swallowed; failure hook optional.
- **E2E (integration-gated, Docker/testcontainers):** full loop — agent run → memory written → embed/distill → subsequent recall surfaces the memory. Mirrors fabriq's existing integration-gating conventions.

---

## 9. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Embedder mismatch (bridge vs fabriq index) | Host passes the **same** embedder via `WithEmbedder`; document the invariant; degrade to search+graph if absent. |
| Raw JSON rows are noisy context for the LLM | Pluggable `render`; prefer distillation altitude (`Config.Altitude`) so summaries surface over raw rows when budget is tight. |
| Tool-name collision (`recall` vs `knowledge_search`) | Default-skip `recall` in the custom tool set; configurable. |
| Learning-loop write amplification / cost | Writes go to one `agent_memory` entity; distillation bounds the corpus; hooks are opt-in (run-level by default, tool-level off). |
| Cortex plugin hook error semantics | Confirm dispatch behaviour during impl; bridge swallows non-critical write errors so the agent run never fails on memory persistence. |

---

## 10. Sequencing

1. **This spec** → implementation plan → Phases 0–4 (cortex↔fabriq brain).
2. **After it lands** → spec #2: `weave/vectorstore/fabriq` (vectorstore-only adapter mirroring `pgvector`), so weave's document pipeline persists into the same fabric and fabriq's direct brain covers document chunks too.
