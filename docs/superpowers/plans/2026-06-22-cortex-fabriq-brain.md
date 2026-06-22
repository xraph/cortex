# Cortex ↔ Fabriq Plug-n-Play Brain — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make fabriq a plug-n-play living brain for cortex agents — recall, rich tools, a learning loop, and one-line wiring — all implemented inside the cortex repo.

**Architecture:** A small core enhancement to cortex's `engine` (a pluggable tool registry) plus one new package `integrations/fabriq` (Go package name `fabriqbrain`) that adapts fabriq's `core/agent.Toolkit` to cortex's `knowledge.Provider`, tool, and `plugin.Extension` seams. fabriq is consumed only through its public surfaces and is not modified.

**Tech Stack:** Go; cortex (`github.com/xraph/cortex`); fabriq (`github.com/xraph/fabriq`) `core/agent` toolkit; `vessel` DI; stdlib `testing` (no testify — matches cortex conventions).

## Global Constraints

- **Language/tests:** Go. Tests use the standard library `testing` package only — no `testify`, no other assertion libs (cortex has no test deps today). Table-driven where natural; assert with `if got != want { t.Fatalf(...) }`.
- **Package name:** the new package directory is `integrations/fabriq` but its Go package name is `fabriqbrain` (NOT `fabriq` — that collides with the imported `github.com/xraph/fabriq` root package used as `fabriq.Fabriq`).
- **fabriq is read-only:** make NO changes to `github.com/xraph/fabriq`. Consume its public API only (`core/agent`, `core/command`, `fabriqtest`, the `*fabriq.Fabriq` facade).
- **Decoupling:** new types depend on narrow local interfaces (`recaller`, `rememberer`, `toolLister`), never the concrete `*agent.Toolkit`, so units test with fakes. `*agent.Toolkit` satisfies all three.
- **No-op safety:** DI wiring helpers MUST return a no-op option / nil slice when no fabriq facade is in the container (parity with `knowledge/weave`'s `EngineOption`).
- **Commits:** one commit per task minimum; commit after each green test cycle. Work happens on branch `feat/fabriq-brain` (already created; the design spec is committed there).
- **Module versions (from fabriq go.mod):** fabriq pulls `github.com/xraph/forge v1.8.0`, `grove v1.5.7`, `shield v1.5.1`, `trove v1.5.1`, `vessel v1.0.2`. cortex already deps `grove v1.5.7`, `weave v1.5.3`, `vessel`, `go-utils`.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `engine/options.go` (modify) | Add `ToolHandler`, `WithTool` | 1 |
| `engine/engine.go` (modify) | Add `tools []registeredTool` field | 1 |
| `engine/react.go` (modify) | `resolveTools`/`executeTool` consult registry | 1 |
| `engine/tools_registry_test.go` (create) | Tool registry tests | 1 |
| `go.mod` / `go.sum` (modify) | Add fabriq dependency (+ local replace) | 2 |
| `integrations/fabriq/config.go` (create) | `config` + `Option` funcs (shared) | 2 |
| `integrations/fabriq/provider.go` (create) | `Adapter` → `knowledge.Provider` (Retrieve, ListCollections) | 3, 4 |
| `integrations/fabriq/render.go` (create) | `ContextItem` → chunk content renderer | 3 |
| `integrations/fabriq/provider_test.go` (create) | Provider tests with a fake recaller | 3, 4 |
| `integrations/fabriq/tools.go` (create) | `toolLister` → cortex tool options | 5 |
| `integrations/fabriq/tools_test.go` (create) | Tool-adaptation tests | 5 |
| `integrations/fabriq/plugin.go` (create) | learning-loop `plugin.Extension` | 6 |
| `integrations/fabriq/plugin_test.go` (create) | Plugin tests with a fake rememberer | 6 |
| `integrations/fabriq/wire.go` (create) | `EngineOption` / `EngineOptions` (vessel) | 7 |
| `integrations/fabriq/wire_test.go` (create) | Wiring tests (present/absent facade) | 7 |
| `integrations/fabriq/doc.go` (create) | Package doc + usage example | 8 |
| `docs/content/docs/integrations/fabriq.mdx` (create) | User-facing integration doc | 8 |

---

## Task 1: Cortex engine — pluggable tool registry (Phase 0)

**Files:**
- Modify: `engine/engine.go` (the `Engine` struct, ~line 28-36)
- Modify: `engine/options.go` (append new option)
- Modify: `engine/react.go` (`resolveTools` ~line 499, `executeTool` ~line 504)
- Test: `engine/tools_registry_test.go` (create)

**Interfaces:**
- Consumes: cortex `llm.Tool{Name string; Description string; Parameters any}`, `llm.ToolCall{ID, Name, Arguments string}`, existing `engine.Option = func(*Engine) error`, package-private `jsonResult(key, value string) string` (engine/tools.go).
- Produces:
  - `type ToolHandler func(ctx context.Context, arguments string) (string, error)`
  - `func WithTool(def llm.Tool, h ToolHandler) Option`
  - Engine field `tools []registeredTool` where `type registeredTool struct { def llm.Tool; handler ToolHandler }`
  - Registered tools appear in `resolveTools(...)` output and dispatch through `executeTool`.

- [ ] **Step 1: Write the failing test**

Create `engine/tools_registry_test.go`:

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/llm"
)

func echoTool() (llm.Tool, ToolHandler) {
	def := llm.Tool{
		Name:        "echo",
		Description: "echoes its arguments",
		Parameters:  map[string]any{"type": "object"},
	}
	h := func(_ context.Context, args string) (string, error) {
		return "echoed:" + args, nil
	}
	return def, h
}

func TestWithTool_AdvertisedInResolveTools(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := e.resolveTools(nil)
	var found bool
	for _, tl := range tools {
		if tl.Name == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveTools did not include registered tool %q; got %d tools", "echo", len(tools))
	}
}

func TestWithTool_DispatchedInExecuteTool(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := e.executeTool(context.Background(), llm.ToolCall{Name: "echo", Arguments: `{"x":1}`})
	if got != `echoed:{"x":1}` {
		t.Fatalf("executeTool = %q, want %q", got, `echoed:{"x":1}`)
	}
}

func TestExecuteTool_UnknownStillErrors(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := e.executeTool(context.Background(), llm.ToolCall{Name: "nope"})
	if !strings.Contains(got, "unknown tool") {
		t.Fatalf("executeTool = %q, want it to contain %q", got, "unknown tool")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./engine/ -run TestWithTool -v`
Expected: compile failure — `WithTool`, `ToolHandler`, `e.tools` undefined.

- [ ] **Step 3: Add the registry field to the Engine struct**

In `engine/engine.go`, add to the `Engine` struct (after `pendingExts []plugin.Extension`):

```go
	tools       []registeredTool
```

And add the type (place it just above the `Engine` struct definition or near it):

```go
// registeredTool pairs an externally-registered tool definition with its handler.
type registeredTool struct {
	def     llm.Tool
	handler ToolHandler
}
```

- [ ] **Step 4: Add the option**

In `engine/options.go`, append:

```go
// ToolHandler executes a registered tool. arguments is the raw JSON argument
// string from the LLM tool call; the return string is the tool result fed back
// to the model.
type ToolHandler func(ctx context.Context, arguments string) (string, error)

// WithTool registers an externally-provided executable tool. The def is
// advertised to the LLM (resolveTools); the handler runs when the model calls
// it (executeTool). Registering tools with the same name appends both; the
// first match wins at dispatch.
func WithTool(def llm.Tool, h ToolHandler) Option {
	return func(e *Engine) error {
		e.tools = append(e.tools, registeredTool{def: def, handler: h})
		return nil
	}
}
```

- [ ] **Step 5: Wire resolveTools and executeTool**

In `engine/react.go`, replace `resolveTools`:

```go
// resolveTools converts tool name references to llm.Tool definitions.
func (e *Engine) resolveTools(_ []string) []llm.Tool {
	tools := e.builtinTools()
	for _, rt := range e.tools {
		tools = append(tools, rt.def)
	}
	return tools
}
```

And replace `executeTool`:

```go
// executeTool executes a tool call and returns the result.
func (e *Engine) executeTool(ctx context.Context, tc llm.ToolCall) string {
	if result, handled := e.executeBuiltinTool(ctx, tc.Name, tc.Arguments); handled {
		return result
	}
	for _, rt := range e.tools {
		if rt.def.Name == tc.Name {
			out, err := rt.handler(ctx, tc.Arguments)
			if err != nil {
				return jsonResult("error", err.Error())
			}
			return out
		}
	}
	return jsonResult("error", fmt.Sprintf("unknown tool %q", tc.Name))
}
```

(`fmt` is already imported in react.go.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./engine/ -run 'TestWithTool|TestExecuteTool' -v`
Expected: PASS (3 tests).

- [ ] **Step 7: Verify the whole package still builds and vets**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go build ./engine/... && go vet ./engine/...`
Expected: no output, exit 0.

- [ ] **Step 8: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add engine/engine.go engine/options.go engine/react.go engine/tools_registry_test.go
git commit -m "feat(engine): pluggable tool registry (WithTool)"
```

---

## Task 2: Add fabriq dependency + bridge package config (Phase 1 setup)

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `integrations/fabriq/config.go`

**Interfaces:**
- Consumes: fabriq `core/agent` (`agent.Embedder`, `agent.WritePolicy`, `agent.ContextItem`), `core/command` (`command.Op`, `command.OpCreate`).
- Produces:
  - `type Option func(*config)`
  - `type config struct { embedder agent.Embedder; entities []string; budget int; memoryEntity string; writePolicy agent.WritePolicy; tenant func(context.Context) context.Context; render func(agent.ContextItem) string }`
  - Options: `WithEmbedder`, `WithEntities`, `WithBudget`, `WithMemoryEntity`, `WithWritePolicy`, `WithTenantMapper`, `WithRenderer`
  - `func defaultConfig() config` (budget 4096, memoryEntity `"agent_memory"`, identity tenant mapper, nil render → defaults applied later)

- [ ] **Step 1: Add the fabriq module dependency**

fabriq is local and may be unpushed, so use a local `replace`. Run:

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
go mod edit -require=github.com/xraph/fabriq@v0.0.0-00010101000000-000000000000
go mod edit -replace=github.com/xraph/fabriq=../../../TwinOS/fabriq
```

Verify the relative path resolves: `ls ../../../TwinOS/fabriq/go.mod` should print the path. If it does not, recompute the relative path from the cortex repo root to `/Users/rexraphael/Work/TwinOS/fabriq` and redo the `-replace`.

- [ ] **Step 2: Tidy and confirm the dependency resolves**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go mod tidy`
Expected: completes without error and adds fabriq + its transitive deps to `go.sum`.

**If `go mod tidy` fails** resolving a transitive xraph dep (e.g. `shield`, `trove`, `forge`) because fabriq depends on an unreleased version (fabriq's own go.mod uses a local `replace` for `shield`): add a matching local `replace` in cortex for that module pointing at its checkout under `/Users/rexraphael/Work/xraph/...`, then re-run `go mod tidy`. Record any added replaces in the commit message.

- [ ] **Step 3: Write the failing test for config defaults**

Create `integrations/fabriq/config.go` is the target of this test; first write `integrations/fabriq/config_test.go`:

```go
package fabriqbrain

import (
	"context"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if c.budget != 4096 {
		t.Fatalf("default budget = %d, want 4096", c.budget)
	}
	if c.memoryEntity != "agent_memory" {
		t.Fatalf("default memoryEntity = %q, want %q", c.memoryEntity, "agent_memory")
	}
	if c.tenant == nil {
		t.Fatalf("default tenant mapper is nil; want identity")
	}
	ctx := context.Background()
	if c.tenant(ctx) != ctx {
		t.Fatalf("default tenant mapper is not identity")
	}
}

func TestOptionsApply(t *testing.T) {
	c := defaultConfig()
	for _, o := range []Option{
		WithEntities("doc", "note"),
		WithBudget(1000),
		WithMemoryEntity("mem"),
	} {
		o(&c)
	}
	if len(c.entities) != 2 || c.entities[0] != "doc" {
		t.Fatalf("entities = %v, want [doc note]", c.entities)
	}
	if c.budget != 1000 {
		t.Fatalf("budget = %d, want 1000", c.budget)
	}
	if c.memoryEntity != "mem" {
		t.Fatalf("memoryEntity = %q, want mem", c.memoryEntity)
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestDefaultConfig -v`
Expected: compile failure — package `fabriqbrain` / `defaultConfig` not found.

- [ ] **Step 5: Write config.go**

Create `integrations/fabriq/config.go`:

```go
// Package fabriqbrain adapts the fabriq agent toolkit to cortex as a
// plug-n-play brain: a knowledge.Provider for recall, rich tools, and a
// learning-loop plugin. The package directory is integrations/fabriq; the
// package name is fabriqbrain to avoid colliding with github.com/xraph/fabriq.
package fabriqbrain

import (
	"context"

	"github.com/xraph/fabriq/core/agent"
)

// config holds bridge configuration shared by the provider, tools, and plugin.
type config struct {
	embedder     agent.Embedder
	entities     []string
	budget       int
	memoryEntity string
	writePolicy  agent.WritePolicy
	tenant       func(context.Context) context.Context
	render       func(agent.ContextItem) string
}

// Option configures the bridge.
type Option func(*config)

func defaultConfig() config {
	return config{
		budget:       4096,
		memoryEntity: "agent_memory",
		tenant:       func(ctx context.Context) context.Context { return ctx },
	}
}

func applyOptions(opts []Option) config {
	c := defaultConfig()
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithEmbedder supplies the embedding model for recall's vector channel. It
// MUST match the embedder fabriq's index/embed worker used (same model + dims);
// otherwise the vector channel mismatches. When nil, recall degrades to
// full-text + graph channels only.
func WithEmbedder(e agent.Embedder) Option { return func(c *config) { c.embedder = e } }

// WithEntities sets the fabriq entity types recall searches and ListCollections
// reports. Required for recall to return results (fabriq's Recall needs at least
// one entity).
func WithEntities(entities ...string) Option {
	return func(c *config) { c.entities = append([]string(nil), entities...) }
}

// WithBudget sets the token budget for each recall (default 4096).
func WithBudget(n int) Option { return func(c *config) { c.budget = n } }

// WithMemoryEntity sets the fabriq entity the learning-loop plugin writes agent
// activity into (default "agent_memory").
func WithMemoryEntity(entity string) Option { return func(c *config) { c.memoryEntity = entity } }

// WithWritePolicy sets the guarded-write allowlist used by the remember tool and
// the learning-loop plugin. Empty = no writes permitted (deny-by-default).
func WithWritePolicy(p agent.WritePolicy) Option { return func(c *config) { c.writePolicy = p } }

// WithTenantMapper overrides how a cortex request context is translated into the
// scope fabriq reads. Default is identity (correct when both share Forge scope).
func WithTenantMapper(fn func(context.Context) context.Context) Option {
	return func(c *config) {
		if fn != nil {
			c.tenant = fn
		}
	}
}

// WithRenderer overrides how a recalled ContextItem becomes chunk text. Default
// renders the row JSON verbatim.
func WithRenderer(fn func(agent.ContextItem) string) Option {
	return func(c *config) { c.render = fn }
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run 'TestDefaultConfig|TestOptionsApply' -v`
Expected: PASS (2 tests).

- [ ] **Step 7: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add go.mod go.sum integrations/fabriq/config.go integrations/fabriq/config_test.go
git commit -m "feat(fabriqbrain): add fabriq dependency + bridge config"
```

---

## Task 3: knowledge.Provider — Retrieve + content renderer (Phase 1)

**Files:**
- Create: `integrations/fabriq/render.go`
- Create: `integrations/fabriq/provider.go`
- Test: `integrations/fabriq/provider_test.go`

**Interfaces:**
- Consumes: `config` (Task 2); fabriq `agent.RecallRequest{Query string; Budget int; Entities []string; K int}`, `agent.ContextPack{Items []agent.ContextItem}`, `agent.ContextItem{Entity, ID string; Row json.RawMessage; Score float64; Source []string; Tokens int}`; cortex `knowledge.Provider`, `knowledge.ScoredChunk{Content string; Score float64; Source, DocumentID, CollectionID string; Metadata map[string]string}`, `knowledge.RetrieveParams{Collection string; TopK int; MinScore float64}`.
- Produces:
  - `type recaller interface { Recall(ctx context.Context, req agent.RecallRequest) (agent.ContextPack, error) }`
  - `type Adapter struct { rec recaller; cfg config }` — implements `knowledge.Provider`
  - `func NewProvider(rec recaller, opts ...Option) *Adapter`
  - `func (a *Adapter) Retrieve(ctx, query string, p *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error)`
  - `func defaultRender(it agent.ContextItem) string`

- [ ] **Step 1: Write the failing test**

Create `integrations/fabriq/provider_test.go`:

```go
package fabriqbrain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/fabriq/core/agent"
)

// fakeRecaller returns a canned ContextPack and records the request it saw.
type fakeRecaller struct {
	pack agent.ContextPack
	got  agent.RecallRequest
	err  error
}

func (f *fakeRecaller) Recall(_ context.Context, req agent.RecallRequest) (agent.ContextPack, error) {
	f.got = req
	return f.pack, f.err
}

func item(entity, id string, score float64, row string, sources ...string) agent.ContextItem {
	return agent.ContextItem{Entity: entity, ID: id, Row: json.RawMessage(row), Score: score, Source: sources}
}

func TestRetrieve_MapsItemsToChunks(t *testing.T) {
	rec := &fakeRecaller{pack: agent.ContextPack{Items: []agent.ContextItem{
		item("doc", "d1", 0.9, `{"t":"hello"}`, "vector", "graph"),
	}}}
	a := NewProvider(rec, WithEntities("doc"), WithBudget(1234))

	chunks, err := a.Retrieve(context.Background(), "hi", &knowledge.RetrieveParams{TopK: 5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Content != `{"t":"hello"}` {
		t.Fatalf("Content = %q", c.Content)
	}
	if c.Score != 0.9 {
		t.Fatalf("Score = %v, want 0.9", c.Score)
	}
	if c.DocumentID != "d1" || c.CollectionID != "doc" {
		t.Fatalf("DocumentID/CollectionID = %q/%q", c.DocumentID, c.CollectionID)
	}
	if c.Source != "vector+graph" {
		t.Fatalf("Source = %q, want vector+graph", c.Source)
	}
	if c.Metadata["entity"] != "doc" {
		t.Fatalf("Metadata[entity] = %q", c.Metadata["entity"])
	}
	// Recall request was built from config + params.
	if rec.got.Query != "hi" || rec.got.Budget != 1234 || rec.got.K != 5 {
		t.Fatalf("recall req = %+v", rec.got)
	}
	if len(rec.got.Entities) != 1 || rec.got.Entities[0] != "doc" {
		t.Fatalf("recall entities = %v", rec.got.Entities)
	}
}

func TestRetrieve_CollectionOverridesEntities(t *testing.T) {
	rec := &fakeRecaller{}
	a := NewProvider(rec, WithEntities("doc", "note"))
	_, err := a.Retrieve(context.Background(), "q", &knowledge.RetrieveParams{Collection: "note"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(rec.got.Entities) != 1 || rec.got.Entities[0] != "note" {
		t.Fatalf("entities = %v, want [note]", rec.got.Entities)
	}
}

func TestRetrieve_FiltersByMinScoreAndCapsTopK(t *testing.T) {
	rec := &fakeRecaller{pack: agent.ContextPack{Items: []agent.ContextItem{
		item("doc", "a", 0.95, `{}`),
		item("doc", "b", 0.40, `{}`),
		item("doc", "c", 0.80, `{}`),
	}}}
	a := NewProvider(rec, WithEntities("doc"))
	chunks, err := a.Retrieve(context.Background(), "q", &knowledge.RetrieveParams{TopK: 1, MinScore: 0.5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// b is dropped (0.40 < 0.5); then capped to TopK=1 → only the first survivor.
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if chunks[0].DocumentID != "a" {
		t.Fatalf("kept %q, want a", chunks[0].DocumentID)
	}
}

func TestRetrieve_NilParamsUsesDefaults(t *testing.T) {
	rec := &fakeRecaller{}
	a := NewProvider(rec, WithEntities("doc"))
	if _, err := a.Retrieve(context.Background(), "q", nil); err != nil {
		t.Fatalf("Retrieve nil params: %v", err)
	}
	if rec.got.Budget != 4096 {
		t.Fatalf("budget = %d, want default 4096", rec.got.Budget)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestRetrieve -v`
Expected: compile failure — `NewProvider`, `Adapter` undefined.

- [ ] **Step 3: Write render.go**

Create `integrations/fabriq/render.go`:

```go
package fabriqbrain

import "github.com/xraph/fabriq/core/agent"

// defaultRender renders a recalled item as its row JSON verbatim. Hosts that
// want summary/altitude text can override via WithRenderer.
func defaultRender(it agent.ContextItem) string {
	return string(it.Row)
}
```

- [ ] **Step 4: Write provider.go (Retrieve only)**

Create `integrations/fabriq/provider.go`:

```go
package fabriqbrain

import (
	"context"
	"strconv"
	"strings"

	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/fabriq/core/agent"
)

// recaller is the narrow slice of *agent.Toolkit the provider needs.
type recaller interface {
	Recall(ctx context.Context, req agent.RecallRequest) (agent.ContextPack, error)
}

// Adapter implements cortex's knowledge.Provider over fabriq's recall pipeline.
type Adapter struct {
	rec recaller
	cfg config
}

var _ knowledge.Provider = (*Adapter)(nil)

// NewProvider builds a knowledge.Provider over a fabriq recaller (a
// *agent.Toolkit satisfies recaller).
func NewProvider(rec recaller, opts ...Option) *Adapter {
	c := applyOptions(opts)
	if c.render == nil {
		c.render = defaultRender
	}
	return &Adapter{rec: rec, cfg: c}
}

// Retrieve runs fabriq recall and maps the context pack to scored chunks.
func (a *Adapter) Retrieve(ctx context.Context, query string, p *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	ctx = a.cfg.tenant(ctx)

	entities := a.cfg.entities
	topK := 0
	minScore := 0.0
	if p != nil {
		if p.Collection != "" {
			entities = []string{p.Collection}
		}
		topK = p.TopK
		minScore = p.MinScore
	}

	req := agent.RecallRequest{
		Query:    query,
		Budget:   a.cfg.budget,
		Entities: entities,
		K:        topK,
	}
	pack, err := a.rec.Recall(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make([]knowledge.ScoredChunk, 0, len(pack.Items))
	for _, it := range pack.Items {
		if minScore > 0 && it.Score < minScore {
			continue
		}
		out = append(out, knowledge.ScoredChunk{
			Content:      a.cfg.render(it),
			Score:        it.Score,
			Source:       strings.Join(it.Source, "+"),
			DocumentID:   it.ID,
			CollectionID: it.Entity,
			Metadata: map[string]string{
				"entity":   it.Entity,
				"channels": strings.Join(it.Source, "+"),
				"tokens":   strconv.Itoa(it.Tokens),
			},
		})
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestRetrieve -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/render.go integrations/fabriq/provider.go integrations/fabriq/provider_test.go
git commit -m "feat(fabriqbrain): knowledge.Provider Retrieve over fabriq recall"
```

---

## Task 4: knowledge.Provider — ListCollections (Phase 1)

**Files:**
- Modify: `integrations/fabriq/provider.go`
- Test: `integrations/fabriq/provider_test.go` (append)

**Interfaces:**
- Consumes: `config.entities` (Task 2); cortex `knowledge.CollectionInfo{ID, Name string; DocumentCount, ChunkCount int64; EmbeddingModel string}`.
- Produces: `func (a *Adapter) ListCollections(ctx context.Context) ([]knowledge.CollectionInfo, error)`

- [ ] **Step 1: Write the failing test (append to provider_test.go)**

```go
func TestListCollections_FromConfiguredEntities(t *testing.T) {
	a := NewProvider(&fakeRecaller{}, WithEntities("doc", "note"))
	cols, err := a.ListCollections(context.Background())
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("got %d collections, want 2", len(cols))
	}
	if cols[0].ID != "doc" || cols[0].Name != "doc" {
		t.Fatalf("collection[0] = %+v", cols[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestListCollections -v`
Expected: FAIL — `ListCollections` undefined.

- [ ] **Step 3: Implement ListCollections (append to provider.go)**

```go
// ListCollections reports the configured recall-able entities as collections.
// Counts are best-effort (0) — fabriq entities are not document collections.
func (a *Adapter) ListCollections(_ context.Context) ([]knowledge.CollectionInfo, error) {
	out := make([]knowledge.CollectionInfo, 0, len(a.cfg.entities))
	for _, e := range a.cfg.entities {
		out = append(out, knowledge.CollectionInfo{ID: e, Name: e})
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -v`
Expected: PASS (all provider + config tests).

- [ ] **Step 5: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/provider.go integrations/fabriq/provider_test.go
git commit -m "feat(fabriqbrain): knowledge.Provider ListCollections"
```

---

## Task 5: Rich tools — adapt fabriq's tool catalog to cortex tools (Phase 3)

**Files:**
- Create: `integrations/fabriq/tools.go`
- Test: `integrations/fabriq/tools_test.go`

**Interfaces:**
- Consumes: `config` (Task 2); cortex `engine.Option`, `engine.WithTool`, `engine.ToolHandler` (Task 1), `llm.Tool`; fabriq `agent.Tool{Name, Description string; InputSchema json.RawMessage; Handler func(ctx, json.RawMessage) (any, error)}`.
- Produces:
  - `type toolLister interface { Tools() []agent.Tool }`
  - `func toolOptions(tl toolLister, c config) []engine.Option`
  - Default behavior: skip the `recall` tool (the engine's auto `knowledge_search` already covers it).

- [ ] **Step 1: Write the failing test**

Create `integrations/fabriq/tools_test.go`:

```go
package fabriqbrain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/fabriq/core/agent"
)

type fakeToolLister struct{ tools []agent.Tool }

func (f fakeToolLister) Tools() []agent.Tool { return f.tools }

func newEngineWith(t *testing.T, opts ...engine.Option) *engine.Engine {
	t.Helper()
	e, err := engine.New(opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

func TestToolOptions_SkipsRecallAndAdaptsHandlers(t *testing.T) {
	called := ""
	tl := fakeToolLister{tools: []agent.Tool{
		{Name: "recall", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }},
		{Name: "graph_traverse", Description: "walk edges", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(_ context.Context, args json.RawMessage) (any, error) {
				called = string(args)
				return map[string]any{"ok": true}, nil
			}},
	}}

	opts := toolOptions(tl, defaultConfig())
	if len(opts) != 1 {
		t.Fatalf("got %d tool options, want 1 (recall skipped)", len(opts))
	}

	// Apply to a real engine and dispatch the registered tool.
	eng := newEngineWith(t, opts...)
	got := eng.Dispatch(context.Background(), "graph_traverse", `{"from":"n1"}`)
	if called != `{"from":"n1"}` {
		t.Fatalf("handler got args %q", called)
	}
	if got != `{"ok":true}` {
		t.Fatalf("tool result = %q, want {\"ok\":true}", got)
	}
}
```

This test uses the local `newEngineWith` helper (above) and a new exported `engine.Dispatch` method (added in Step 3) so the bridge test can build an engine and run a tool without the full ReAct loop.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestToolOptions -v`
Expected: compile failure — `toolOptions`, `newEngineWith`, `dispatchForTest` undefined.

- [ ] **Step 3: Add an exported tool-dispatch method to the engine package**

The bridge test drives a tool directly (no ReAct loop), so expose the existing `executeTool` path. Create `engine/dispatch.go`:

```go
package engine

import (
	"context"

	"github.com/xraph/cortex/llm"
)

// Dispatch executes a registered or built-in tool by name and returns its raw
// result string. It is the same path the ReAct loop uses (executeTool), exposed
// for hosts and tests that drive tools directly.
func (e *Engine) Dispatch(ctx context.Context, name, arguments string) string {
	return e.executeTool(ctx, llm.ToolCall{Name: name, Arguments: arguments})
}
```

This is also generally useful for hosts that want to invoke a fabriq tool outside an agent run.

- [ ] **Step 4: Write tools.go**

Create `integrations/fabriq/tools.go`:

```go
package fabriqbrain

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/fabriq/core/agent"
)

// toolLister is the narrow slice of *agent.Toolkit the tool adapter needs.
type toolLister interface {
	Tools() []agent.Tool
}

// skippedTools are not registered as cortex tools. recall overlaps the engine's
// auto knowledge_search tool.
var skippedTools = map[string]bool{"recall": true}

// toolOptions adapts fabriq's tool catalog to cortex engine options. The tenant
// mapper from config is applied before each handler runs.
func toolOptions(tl toolLister, c config) []engine.Option {
	var opts []engine.Option
	for _, t := range tl.Tools() {
		if skippedTools[t.Name] {
			continue
		}
		t := t // capture
		def := llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schemaOf(t.InputSchema),
		}
		handler := func(ctx context.Context, args string) (string, error) {
			ctx = c.tenant(ctx)
			out, err := t.Handler(ctx, json.RawMessage(args))
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
		opts = append(opts, engine.WithTool(def, handler))
	}
	return opts
}

// schemaOf decodes a JSON-schema RawMessage into a generic value for llm.Tool
// (whose Parameters is `any`). On failure it passes the raw bytes through.
func schemaOf(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestToolOptions -v && go test ./engine/ -v`
Expected: PASS (bridge tool test + engine tests, including the new `Dispatch`).

- [ ] **Step 6: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add engine/dispatch.go integrations/fabriq/tools.go integrations/fabriq/tools_test.go
git commit -m "feat(fabriqbrain): adapt fabriq tool catalog to cortex tools"
```

---

## Task 6: Learning-loop plugin (Phase 4)

**Files:**
- Create: `integrations/fabriq/plugin.go`
- Test: `integrations/fabriq/plugin_test.go`

**Interfaces:**
- Consumes: `config` (Task 2); cortex `plugin.Extension` (`Name() string`), hooks `plugin.RunStarted`/`RunCompleted`, `id.AgentID`, `id.AgentRunID`; fabriq `agent.RememberRequest{Entity, Op, AggID string; Payload json.RawMessage}`, `command.Result`.
- Produces:
  - `type rememberer interface { Remember(ctx context.Context, req agent.RememberRequest) (command.Result, error) }`
  - `type Plugin struct { rem rememberer; cfg config; inflight sync.Map }`
  - `func NewPlugin(rem rememberer, opts ...Option) *Plugin`
  - `func (p *Plugin) Name() string` → `"fabriq-brain"`
  - `func (p *Plugin) OnRunStarted(ctx, agentID id.AgentID, runID id.AgentRunID, input string) error`
  - `func (p *Plugin) OnRunCompleted(ctx, agentID id.AgentID, runID id.AgentRunID, output string, elapsed time.Duration) error`

- [ ] **Step 1: Write the failing test**

Create `integrations/fabriq/plugin_test.go`:

```go
package fabriqbrain

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
)

type fakeRememberer struct {
	reqs []agent.RememberRequest
	err  error
}

func (f *fakeRememberer) Remember(_ context.Context, req agent.RememberRequest) (command.Result, error) {
	f.reqs = append(f.reqs, req)
	return command.Result{}, f.err
}

func TestPlugin_ImplementsExtensionAndHooks(t *testing.T) {
	var _ plugin.Extension = (*Plugin)(nil)
	var _ plugin.RunStarted = (*Plugin)(nil)
	var _ plugin.RunCompleted = (*Plugin)(nil)
}

func TestPlugin_WritesMemoryOnRunCompleted(t *testing.T) {
	rem := &fakeRememberer{}
	p := NewPlugin(rem, WithMemoryEntity("agent_memory"))

	agentID := id.AgentID{}
	runID := id.AgentRunID{}
	ctx := context.Background()

	if err := p.OnRunStarted(ctx, agentID, runID, "what is fabriq?"); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}
	if err := p.OnRunCompleted(ctx, agentID, runID, "a data fabric", 2*time.Second); err != nil {
		t.Fatalf("OnRunCompleted: %v", err)
	}

	if len(rem.reqs) != 1 {
		t.Fatalf("got %d Remember calls, want 1", len(rem.reqs))
	}
	req := rem.reqs[0]
	if req.Entity != "agent_memory" || req.Op != "create" {
		t.Fatalf("req = %+v, want entity=agent_memory op=create", req)
	}
	var payload map[string]any
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["input"] != "what is fabriq?" || payload["output"] != "a data fabric" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestPlugin_SwallowsWriteErrors(t *testing.T) {
	rem := &fakeRememberer{err: context.DeadlineExceeded}
	p := NewPlugin(rem)
	// A write failure must NOT propagate as a hook error (the registry logs it,
	// but returning nil keeps run completion clean and matches our intent).
	if err := p.OnRunCompleted(context.Background(), id.AgentID{}, id.AgentRunID{}, "out", 0); err != nil {
		t.Fatalf("OnRunCompleted returned error %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestPlugin -v`
Expected: compile failure — `Plugin`, `NewPlugin` undefined.

- [ ] **Step 3: Write plugin.go**

Create `integrations/fabriq/plugin.go`:

```go
package fabriqbrain

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
)

// rememberer is the narrow slice of *agent.Toolkit the plugin needs.
type rememberer interface {
	Remember(ctx context.Context, req agent.RememberRequest) (command.Result, error)
}

// Plugin is a cortex extension that writes agent run activity back into the
// fabric so fabriq's embed + distillation workers turn it into future recall
// material. It implements plugin.RunStarted and plugin.RunCompleted.
type Plugin struct {
	rem      rememberer
	cfg      config
	inflight sync.Map // runID string -> input string
}

// NewPlugin builds the learning-loop plugin over a fabriq rememberer (a
// *agent.Toolkit satisfies rememberer).
func NewPlugin(rem rememberer, opts ...Option) *Plugin {
	return &Plugin{rem: rem, cfg: applyOptions(opts)}
}

// Name identifies the extension.
func (p *Plugin) Name() string { return "fabriq-brain" }

// OnRunStarted stashes the run input so OnRunCompleted can persist the full Q/A.
func (p *Plugin) OnRunStarted(_ context.Context, _ id.AgentID, runID id.AgentRunID, input string) error {
	p.inflight.Store(runID.String(), input)
	return nil
}

// OnRunCompleted writes a memory row for the finished run. Write failures are
// swallowed (logged by the registry) so memory persistence never fails a run.
func (p *Plugin) OnRunCompleted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, output string, elapsed time.Duration) error {
	ctx = p.cfg.tenant(ctx)

	input, _ := p.inflight.LoadAndDelete(runID.String())
	in, _ := input.(string)

	payload, err := json.Marshal(map[string]any{
		"agentId":   agentID.String(),
		"runId":     runID.String(),
		"input":     in,
		"output":    output,
		"elapsedMs": elapsed.Milliseconds(),
	})
	if err != nil {
		return nil
	}

	_, _ = p.rem.Remember(ctx, agent.RememberRequest{
		Entity:  p.cfg.memoryEntity,
		Op:      "create",
		Payload: payload,
	})
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestPlugin -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/plugin.go integrations/fabriq/plugin_test.go
git commit -m "feat(fabriqbrain): learning-loop plugin writes run activity to the fabric"
```

---

## Task 7: One-line DI wiring (Phase 2)

**Files:**
- Create: `integrations/fabriq/wire.go`
- Test: `integrations/fabriq/wire_test.go`

**Interfaces:**
- Consumes: `config`, `NewProvider`, `toolOptions`, `NewPlugin` (Tasks 2–6); cortex `engine.Option`, `engine.WithKnowledge`, `engine.WithExtension`; `vessel.Vessel`, `vessel.Inject`; fabriq `*fabriq.Fabriq` (facade; has `.Registry()` and satisfies `query.Fabric`), `agent.NewToolkit`, `agent.Config`, `agent.Toolkit`.
- Produces:
  - `func buildToolkit(f *fabriq.Fabriq, c config) (*agent.Toolkit, error)`
  - `func EngineOption(c vessel.Vessel, opts ...Option) engine.Option` — knowledge provider only (weave parity)
  - `func EngineOptions(c vessel.Vessel, opts ...Option) []engine.Option` — full brain (knowledge + tools + plugin)

- [ ] **Step 1: Write the failing test**

Create `integrations/fabriq/wire_test.go`:

```go
package fabriqbrain

import (
	"testing"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/vessel"
)

func TestEngineOptions_NoFabriqFacadeIsNoop(t *testing.T) {
	c := vessel.New() // empty container: no *fabriq.Fabriq provided
	opts := EngineOptions(c)
	if opts != nil {
		t.Fatalf("EngineOptions with no facade = %v, want nil", opts)
	}
	// EngineOption must be a safe no-op that applies cleanly.
	e, err := engine.New(EngineOption(c))
	if err != nil {
		t.Fatalf("engine.New with no-op EngineOption: %v", err)
	}
	if e.Knowledge() != nil {
		t.Fatalf("knowledge should be nil when no facade present")
	}
}
```

(If `vessel.New()` is not the correct constructor, use the same empty-container pattern cortex's existing weave wiring tests use; check `github.com/xraph/vessel` for the constructor — likely `vessel.New()`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestEngineOptions -v`
Expected: compile failure — `EngineOption`, `EngineOptions` undefined.

- [ ] **Step 3: Write wire.go**

Create `integrations/fabriq/wire.go`:

```go
package fabriqbrain

import (
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"

	"github.com/xraph/fabriq"
	"github.com/xraph/fabriq/core/agent"
)

// buildToolkit constructs a fabriq agent toolkit from the facade and config.
// NOTE: *fabriq.Fabriq exposes Registry() but not CAS (CAS lives on
// forgeext.Extension, which is not what we inject). The digest/resolve tools run
// with a nil CAS — fabriq's toolkit supports that (no CAS-backed summary text).
// Wiring CAS would require injecting the Forge extension instead; out of scope.
func buildToolkit(f *fabriq.Fabriq, c config) (*agent.Toolkit, error) {
	return agent.NewToolkit(f, f.Registry(), c.embedder, agent.Config{Write: c.writePolicy})
}

// EngineOption wires ONLY the knowledge provider (parity with
// weave.EngineOption). Returns a no-op option when no fabriq facade is in the
// container.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	f, err := vessel.Inject[*fabriq.Fabriq](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	cfg := applyOptions(opts)
	tk, err := buildToolkit(f, cfg)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithKnowledge(NewProvider(tk, opts...))
}

// EngineOptions wires the FULL brain: knowledge provider + rich tools +
// learning-loop plugin. Returns nil (safe to spread) when no fabriq facade is
// present.
func EngineOptions(c vessel.Vessel, opts ...Option) []engine.Option {
	f, err := vessel.Inject[*fabriq.Fabriq](c)
	if err != nil {
		return nil
	}
	cfg := applyOptions(opts)
	tk, err := buildToolkit(f, cfg)
	if err != nil {
		return nil
	}

	out := []engine.Option{engine.WithKnowledge(NewProvider(tk, opts...))}
	out = append(out, toolOptions(tk, cfg)...)
	out = append(out, engine.WithExtension(NewPlugin(tk, opts...)))
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestEngineOptions -v`
Expected: PASS.

If `vessel.Inject` on an empty container panics rather than returning an error, wrap the inject in the same defensive pattern weave uses (it relies on the returned error). Confirm against `knowledge/weave/adapter.go` (already verified: it uses `if err != nil`).

- [ ] **Step 5: Full package build + vet + test**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go build ./... && go vet ./integrations/... ./engine/... && go test ./integrations/... ./engine/...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/wire.go integrations/fabriq/wire_test.go
git commit -m "feat(fabriqbrain): vessel-based EngineOption/EngineOptions wiring"
```

---

## Task 8: Package doc + integration guide (docs)

**Files:**
- Create: `integrations/fabriq/doc.go`
- Create: `docs/content/docs/integrations/fabriq.mdx`

**Interfaces:**
- Consumes: the public surface produced by Tasks 2–7 (`EngineOption`, `EngineOptions`, the `With*` options).
- Produces: documentation only (no code behavior).

- [ ] **Step 1: Write the package doc**

Create `integrations/fabriq/doc.go`:

```go
// Package fabriqbrain makes fabriq a plug-n-play living brain for cortex agents.
//
// It adapts fabriq's core/agent toolkit to three cortex seams:
//
//   - knowledge.Provider — multi-channel recall (vector + full-text + graph,
//     RRF-fused, distillation-aware) exposed as the engine's knowledge_search.
//   - rich tools — fabriq's graph_traverse, remember, map/digest/resolve
//     registered as cortex tools (engine.WithTool).
//   - learning loop — a plugin.Extension that writes agent run activity into
//     the fabric so fabriq's embed + distillation workers turn it into future
//     recall material.
//
// Wiring (auto-discovers the fabriq facade from the DI container):
//
//	eng, _ := engine.New(append(
//	    []engine.Option{engine.WithStore(store), engine.WithLLM(client)},
//	    fabriqbrain.EngineOptions(container,
//	        fabriqbrain.WithEmbedder(emb),
//	        fabriqbrain.WithEntities("doc", "note", "agent_memory"),
//	        fabriqbrain.WithWritePolicy(agent.WritePolicy{
//	            Allow: map[string][]command.Op{"agent_memory": {command.OpCreate}},
//	        }),
//	    )...,
//	)...)
//
// Host requirements for the learning loop: register the memory entity (default
// "agent_memory") in fabriq's registry WITH a vector index so the proj:embed
// worker vectorizes new rows, and allow {memoryEntity: {create}} in the write
// policy.
//
// The package directory is integrations/fabriq; the package name is fabriqbrain
// to avoid colliding with github.com/xraph/fabriq.
package fabriqbrain
```

- [ ] **Step 2: Write the user-facing integration doc**

Create `docs/content/docs/integrations/fabriq.mdx`. Mirror the structure of an existing page under `docs/content/docs/` (check the directory for an example and copy its frontmatter shape — `title`, `description`). Content:

```mdx
---
title: Fabriq Brain
description: Plug fabriq in as a living brain for cortex agents — recall, rich tools, and a learning loop.
---

## Overview

`fabriqbrain` (`github.com/xraph/cortex/integrations/fabriq`) adapts the
[fabriq](https://github.com/xraph/fabriq) data fabric to cortex as a
plug-n-play brain. With one wiring call your agents gain fabriq-backed:

- **Recall** — multi-channel retrieval (vector + full-text + graph, RRF-fused,
  distillation-aware) via the engine's `knowledge_search` tool.
- **Rich tools** — `graph_traverse`, guarded `remember`, and the distillation
  tools `map`/`digest`/`resolve`, registered as native cortex tools.
- **Learning loop** — a plugin that writes each run back into the fabric, where
  fabriq's embed + distillation workers turn it into future recall material.

## Wiring

```go
import (
	"github.com/xraph/cortex/engine"
	fabriqbrain "github.com/xraph/cortex/integrations/fabriq"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
)

eng, err := engine.New(append(
	[]engine.Option{engine.WithStore(store), engine.WithLLM(client)},
	fabriqbrain.EngineOptions(container,
		fabriqbrain.WithEmbedder(emb),
		fabriqbrain.WithEntities("doc", "note", "agent_memory"),
		fabriqbrain.WithWritePolicy(agent.WritePolicy{
			Allow: map[string][]command.Op{"agent_memory": {command.OpCreate}},
		}),
	)...,
)...)
```

`EngineOptions` auto-discovers the `*fabriq.Fabriq` facade from the DI
container (provided by fabriq's Forge extension). If no facade is present it
returns `nil`, so the call is always safe to include. Use `EngineOption`
(singular) to wire only the knowledge provider, mirroring the weave adapter.

## Options

| Option | Purpose |
|---|---|
| `WithEmbedder(agent.Embedder)` | Embedding model for recall's vector channel. Must match the embedder fabriq indexed with. |
| `WithEntities(...string)` | Entity types recall searches and `ListCollections` reports. |
| `WithBudget(int)` | Token budget per recall (default 4096). |
| `WithMemoryEntity(string)` | Entity the learning loop writes to (default `agent_memory`). |
| `WithWritePolicy(agent.WritePolicy)` | Allowlist for the `remember` tool and learning-loop writes (deny-by-default). |
| `WithTenantMapper(func(ctx) ctx)` | Translate cortex request context to fabriq scope (default identity). |
| `WithRenderer(func(agent.ContextItem) string)` | Override how a recalled row becomes chunk text. |

## Learning loop requirements

For run activity to become recall material, the host must:

1. Register the memory entity (default `agent_memory`) in fabriq's registry
   **with a vector index** so the `proj:embed` worker vectorizes new rows.
2. Allow `{memoryEntity: {create}}` in the write policy.

Distillation rolls the growing memory corpus into summaries, so recall stays
within budget as history accumulates.
```

- [ ] **Step 3: Build to confirm doc.go compiles**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go build ./integrations/...`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/doc.go docs/content/docs/integrations/fabriq.mdx
git commit -m "docs(fabriqbrain): package doc + integration guide"
```

---

## Task 9 (optional, integration-gated): E2E learning loop

**Files:**
- Create: `integrations/fabriq/e2e_integration_test.go` (build tag `//go:build integration`)

**Interfaces:**
- Consumes: fabriq `fabriqtest` container helpers (`StartPostgres`, etc.), `fabriqtest.NewFabric`/`NewWorld`, the full `*agent.Toolkit`; a real or stub `agent.Embedder`.
- Produces: a test proving run → memory write → embed/distill → subsequent recall surfaces the memory.

> This task requires Docker/testcontainers and the same integration-gating fabriq uses. It is OPTIONAL for the first merge — the unit tests above fully cover the bridge logic with fakes. Implement only if you want end-to-end proof against a real fabric. Mirror fabriq's `core/agent` distillation E2E tests for setup. Gate with `//go:build integration` and run via `go test -tags integration ./integrations/fabriq/`.

- [ ] **Step 1: Decide whether to implement now or defer.** If deferring, note it in the PR description and skip to the self-review. If implementing, model the test on fabriq's `core/agent/distill_e2e_test.go` for world/toolkit setup and assert that a recall after a plugin write returns the written memory row.

---

## Final verification

- [ ] **Run the full bridge + engine suite:**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go build ./... && go vet ./engine/... ./integrations/... && go test ./engine/... ./integrations/...`
Expected: all green.

- [ ] **Confirm fabriq is unchanged:**

Run: `cd /Users/rexraphael/Work/TwinOS/fabriq && git status --porcelain`
Expected: no new changes attributable to this work (fabriq must remain untouched).

---

## Plan ↔ Spec coverage check

| Spec section | Task(s) |
|---|---|
| Phase 0 — engine tool registry | 1 |
| Phase 1 — knowledge.Provider (Retrieve + mapping) | 3 |
| Phase 1 — ListCollections | 4 |
| Phase 2 — vessel wiring (EngineOption/EngineOptions) | 7 |
| Phase 3 — rich tools from Tools() | 5 |
| Phase 4 — learning-loop plugin | 6 |
| Cross-cutting — tenancy mapper | 2 (config) applied in 3, 5, 6 |
| Content rendering (pluggable) | 3 (render.go) |
| Package naming (fabriqbrain) | 2 |
| fabriq dependency wiring + risk | 2 |
| Testing strategy (unit with fakes) | 1, 3, 4, 5, 6, 7; E2E in 9 |
| Docs | 8 |
| Weave follow-up | Out of scope (separate spec) |
```
