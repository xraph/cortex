# Cortex↔Fabriq Brain — Learning-Loop Host Wiring + E2E (Tasks 10–12)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the learning loop turnkey (a host helper for the memory entity + write policy) and prove the whole write→vectorize→recall loop end-to-end against a real fabriq.

**Builds on:** the merged `integrations/fabriq` package (Go package `fabriqbrain`) on cortex `main`. Branch: `feat/fabriq-brain-learning-loop`.

## Global Constraints

- Go; tests use stdlib `testing` only (no testify). Integration test fixtures may import `fabriqtest`, `fabriq`, `agent`, `command`, `registry`, `postgres`, `migrations`, `tenant` — not assertion libs.
- Package directory `integrations/fabriq`, Go package name `fabriqbrain`.
- fabriq is consumed READ-ONLY.
- Commit on `feat/fabriq-brain-learning-loop` (not `main`). One commit per task.
- The memory entity is a fabriq DYNAMIC entity with exactly two domain columns: `content` (ColText, NotNull — the embed target) and `meta` (ColJSON — structured run fields). fabriq auto-injects `id`/`tenant_id`/`version`.
- The learning-loop plugin writes `RememberRequest.Payload = {"content": <string>, "meta": {…}}` so it maps cleanly to those two columns.
- E2E embedder MUST be 768-dim (the `fabriq_embeddings` table is `vector(768)`), matching the toolkit's default `VectorDims` (768).

---

## Task 10: Memory entity helper + plugin payload `{content, meta}`

**Files:**
- Create: `integrations/fabriq/memory.go`
- Create: `integrations/fabriq/memory_test.go`
- Modify: `integrations/fabriq/plugin.go` (OnRunCompleted, OnRunFailed payloads)
- Modify: `integrations/fabriq/plugin_test.go` (assert new payload shape)

**Interfaces:**
- Consumes: fabriq `registry.EntitySpec`, `registry.DynamicSchema`, `registry.DynamicColumn`, `registry.ColText`, `registry.ColJSON`, `registry.KindAggregate`, `registry.EmbedSpec` (from `github.com/xraph/fabriq/core/registry`); `agent.WritePolicy`, `command.Op`, `command.OpCreate`.
- Produces:
  - `func MemorySpec(entity string) registry.EntitySpec`
  - `func MemoryWritePolicy(entity string) agent.WritePolicy`
  - Plugin payloads change to `{content, meta}`.

- [ ] **Step 1: Write the failing test for the helper**

Create `integrations/fabriq/memory_test.go`:

```go
package fabriqbrain

import (
	"testing"

	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/registry"
)

func TestMemorySpec_DynamicContentMetaEmbedded(t *testing.T) {
	spec := MemorySpec("agent_memory")
	if spec.Name != "agent_memory" {
		t.Fatalf("Name = %q", spec.Name)
	}
	if spec.Kind != registry.KindAggregate {
		t.Fatalf("Kind = %v, want KindAggregate", spec.Kind)
	}
	if spec.Schema == nil {
		t.Fatalf("Schema is nil; want a dynamic schema")
	}
	cols := map[string]registry.ColumnType{}
	for _, c := range spec.Schema.Columns {
		cols[c.Name] = c.Type
	}
	if cols["content"] != registry.ColText {
		t.Fatalf("content column type = %v, want ColText", cols["content"])
	}
	if cols["meta"] != registry.ColJSON {
		t.Fatalf("meta column type = %v, want ColJSON", cols["meta"])
	}
	if spec.Embed == nil || len(spec.Embed.Fields) != 1 || spec.Embed.Fields[0] != "content" {
		t.Fatalf("Embed = %+v, want Fields [content]", spec.Embed)
	}
}

func TestMemoryWritePolicy_AllowsCreateOnEntity(t *testing.T) {
	p := MemoryWritePolicy("agent_memory")
	ops := p.Allow["agent_memory"]
	if len(ops) != 1 || ops[0] != command.OpCreate {
		t.Fatalf("Allow[agent_memory] = %v, want [OpCreate]", ops)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run 'TestMemorySpec|TestMemoryWritePolicy' -v`
Expected: compile failure — `MemorySpec`, `MemoryWritePolicy` undefined.

- [ ] **Step 3: Write memory.go**

Create `integrations/fabriq/memory.go`:

```go
package fabriqbrain

import (
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/registry"
)

// MemorySpec returns the fabriq registry spec for the learning-loop memory
// entity: a dynamic entity with a `content` text column (the embedding target)
// and a `meta` JSON column (structured run fields), with `content` vector-indexed.
//
// Register it before opening fabriq, e.g.:
//
//	reg.MustRegister(fabriqbrain.MemorySpec("agent_memory"))
//
// The physical table is NOT auto-created by fabriq.Open — provision it via a
// migration or postgres.Adapter.EnsureDynamic before serving writes.
func MemorySpec(entity string) registry.EntitySpec {
	return registry.EntitySpec{
		Name: entity,
		Kind: registry.KindAggregate,
		Schema: &registry.DynamicSchema{
			Table: entity, // table name == entity name (ddl-validated by fabriq)
			Columns: []registry.DynamicColumn{
				{Name: "content", Type: registry.ColText, NotNull: true},
				{Name: "meta", Type: registry.ColJSON},
			},
		},
		Embed: &registry.EmbedSpec{Fields: []string{"content"}},
	}
}

// MemoryWritePolicy returns the deny-by-default write allowlist the learning-loop
// plugin needs: create on the memory entity. Pass it via WithWritePolicy.
func MemoryWritePolicy(entity string) agent.WritePolicy {
	return agent.WritePolicy{
		Allow: map[string][]command.Op{entity: {command.OpCreate}},
	}
}
```

- [ ] **Step 4: Run helper tests to verify they pass**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run 'TestMemorySpec|TestMemoryWritePolicy' -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Update the plugin to write `{content, meta}` (TDD: update tests first)**

In `integrations/fabriq/plugin_test.go`, change the payload assertions:
- In `TestPlugin_WritesMemoryOnRunCompleted`, after unmarshalling `payload`, replace the input/output assertions with:
```go
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "what is fabriq?") || !strings.Contains(content, "a data fabric") {
		t.Fatalf("content = %q, want it to contain the input and output", content)
	}
	meta, ok := payload["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing or not an object: %v", payload["meta"])
	}
	if meta["input"] != "what is fabriq?" || meta["output"] != "a data fabric" {
		t.Fatalf("meta = %v", meta)
	}
```
  Add `"strings"` to the test imports.
- In `TestPlugin_OnRunFailedCleansUpAndRecords`, replace the `payload["failed"]`/`payload["error"]` assertions with reads from the `meta` object:
```go
	meta, ok := payload["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing: %v", payload["meta"])
	}
	if meta["failed"] != true || meta["error"] != context.DeadlineExceeded.Error() {
		t.Fatalf("meta = %v", meta)
	}
```

- [ ] **Step 6: Run plugin tests to verify they now fail**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -run TestPlugin -v`
Expected: FAIL — the plugin still writes the flat payload, so `content`/`meta` are absent.

- [ ] **Step 7: Update plugin.go payloads**

In `integrations/fabriq/plugin.go`, `OnRunCompleted` — replace the marshal block with:
```go
	content := strings.TrimSpace(in + "\n" + output)
	payload, err := json.Marshal(map[string]any{
		"content": content,
		"meta": map[string]any{
			"kind":      "completed",
			"agentId":   agentID.String(),
			"runId":     runID.String(),
			"input":     in,
			"output":    output,
			"elapsedMs": elapsed.Milliseconds(),
		},
	})
```
And `OnRunFailed` — replace the marshal block with:
```go
	content := strings.TrimSpace(in + "\n" + errStr)
	payload, err := json.Marshal(map[string]any{
		"content": content,
		"meta": map[string]any{
			"kind":    "failed",
			"agentId": agentID.String(),
			"runId":   runID.String(),
			"input":   in,
			"error":   errStr,
			"failed":  true,
		},
	})
```
Add `"strings"` to the plugin.go imports. Leave the logging and the `Remember(... Entity: p.cfg.memoryEntity, Op: "create", Payload: payload ...)` calls unchanged.

- [ ] **Step 8: Run the full package test set to verify green**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test ./integrations/fabriq/ -v && go build ./...`
Expected: PASS (all `fabriqbrain` tests, including the updated plugin tests and the new memory tests); build clean.

- [ ] **Step 9: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/memory.go integrations/fabriq/memory_test.go integrations/fabriq/plugin.go integrations/fabriq/plugin_test.go
git commit -m "feat(fabriqbrain): memory entity helper + {content,meta} plugin payload"
```

---

## Task 11: Docs — turnkey host wiring

**Files:**
- Modify: `integrations/fabriq/doc.go`
- Modify: `docs/content/docs/integrations/fabriq.mdx`

**Interfaces:** documentation only; references `MemorySpec`, `MemoryWritePolicy` from Task 10.

- [ ] **Step 1: Update the package doc**

In `integrations/fabriq/doc.go`, replace the "Host requirements for the learning loop" paragraph with a turnkey version that uses the new helpers:

```go
// Host setup for the learning loop (turnkey):
//
//	// 1. Register the memory entity (dynamic, content vector-indexed).
//	reg.MustRegister(fabriqbrain.MemorySpec("agent_memory"))
//	// 2. Provision its table once (migration, or postgres.Adapter.EnsureDynamic
//	//    in setup) — fabriq.Open does not auto-create dynamic tables.
//	// 3. Allow writes to it.
//	opts := fabriqbrain.EngineOptions(container,
//	    fabriqbrain.WithEmbedder(emb),
//	    fabriqbrain.WithEntities("agent_memory"),
//	    fabriqbrain.WithWritePolicy(fabriqbrain.MemoryWritePolicy("agent_memory")),
//	)
//
// The plugin writes {content, meta} rows; fabriq's embed worker vectorizes
// `content` and distillation rolls them up, so future recall surfaces them.
```

- [ ] **Step 2: Update the MDX guide**

In `docs/content/docs/integrations/fabriq.mdx`, replace the "Learning loop requirements" section with a turnkey "Learning loop setup" section showing `MemorySpec` / `MemoryWritePolicy`, the `EnsureDynamic`/migration note, and that the plugin persists `{content, meta}` (content is vector-indexed). Keep the frontmatter and the rest of the page intact.

- [ ] **Step 3: Build to confirm doc.go compiles**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go build ./integrations/...`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/doc.go docs/content/docs/integrations/fabriq.mdx
git commit -m "docs(fabriqbrain): turnkey learning-loop host wiring (MemorySpec/MemoryWritePolicy)"
```

---

## Task 12: E2E integration test (write → vectorize → recall)

**Files:**
- Create: `integrations/fabriq/e2e_integration_test.go` (build tag `//go:build integration`)

**Interfaces:** consumes the public `fabriqbrain` surface (`MemorySpec`, `MemoryWritePolicy`, `NewPlugin`, `NewProvider`, `WithEntities`, `WithWritePolicy`) + fabriq (`fabriq.Open`, `agent.NewToolkit`, `agent.NewIndexer`, `postgres.Open`, `migrations.OpenOrchestrator`, `fabriqtest.StartPostgres`/`CreateAppRole`, `registry`, `tenant`, `command`).

**Reference template (read it):** `/Users/rexraphael/Work/TwinOS/fabriq/dynamic_projection_e2e_integration_test.go` — copy its EnsureDynamic-as-owner-before-CreateAppRole setup. Also `/Users/rexraphael/Work/TwinOS/fabriq/e2e_integration_test.go` for the migrate step, and `/Users/rexraphael/Work/TwinOS/fabriq/core/agent/recall_test.go` for the toolkit/recall shape.

This task is Docker-gated. Build it so it compiles under the `integration` tag; run it if Docker is available.

- [ ] **Step 1: Write the E2E test**

Create `integrations/fabriq/e2e_integration_test.go`:

```go
//go:build integration

package fabriqbrain

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/xraph/fabriq"
	"github.com/xraph/fabriq/adapters/postgres"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/registry"
	"github.com/xraph/fabriq/core/tenant"
	"github.com/xraph/fabriq/fabriqtest"
	"github.com/xraph/fabriq/migrations"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
)

// hashEmbedder is a deterministic 768-dim embedder: equal text → equal vector,
// so a query equal to a stored row's content is its own nearest neighbour.
type hashEmbedder struct{}

func (hashEmbedder) Dims() int { return 768 }
func (hashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, 768)
		h := sha256.Sum256([]byte(t))
		for j := 0; j < 768; j++ {
			vec[j] = float32(h[j%32]) / 255.0
		}
		out[i] = vec
	}
	return out, nil
}

func TestE2E_LearningLoop_WriteVectorizeRecall(t *testing.T) {
	ctx := context.Background()
	const entity = "agent_memory"

	superDSN := fabriqtest.StartPostgres(t)

	// Registry with the memory entity.
	reg := registry.New()
	reg.MustRegister(MemorySpec(entity))
	if err := reg.Validate(); err != nil {
		t.Fatalf("registry validate: %v", err)
	}

	// Migrate fabriq's internal schema (creates fabriq_embeddings/pgvector).
	orch, closeFn, err := migrations.OpenOrchestrator(ctx, superDSN)
	if err != nil {
		t.Fatalf("OpenOrchestrator: %v", err)
	}
	if _, err := orch.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = closeFn()

	// Create the dynamic table as the schema owner BEFORE the app role exists,
	// so DEFAULT PRIVILEGES cover it (mirrors fabriq's dynamic E2E tests).
	owner, err := postgres.Open(ctx, superDSN, reg)
	if err != nil {
		t.Fatalf("postgres.Open (owner): %v", err)
	}
	ent, ok := reg.Get(entity)
	if !ok {
		t.Fatalf("entity %q not registered", entity)
	}
	if err := owner.EnsureDynamic(ctx, ent); err != nil {
		t.Fatalf("EnsureDynamic: %v", err)
	}
	_ = owner.Close()

	// Open fabriq with the app role (Postgres only — no Redis needed).
	appDSN := fabriqtest.CreateAppRole(t, superDSN)
	f, _, err := fabriq.Open(ctx, reg, fabriq.Config{
		Postgres: fabriq.PostgresConfig{DSN: appDSN},
	})
	if err != nil {
		t.Fatalf("fabriq.Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	tctx, err := tenant.WithTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	emb := hashEmbedder{}

	// Build a write-enabled toolkit and the learning-loop plugin over the real fabric.
	tk, err := agent.NewToolkit(f, f.Registry(), emb, agent.Config{
		VectorDims: 768,
		Write:      MemoryWritePolicy(entity),
	})
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	plugin := NewPlugin(tk, WithMemoryEntity(entity))

	// Simulate an agent run: started → completed. The plugin writes the memory row.
	runID := id.AgentRunID{}
	if err := plugin.OnRunStarted(tctx, id.AgentID{}, runID, "the coolant pump failed at station seven"); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}
	if err := plugin.OnRunCompleted(tctx, id.AgentID{}, runID, "replace the pump seal and restart", 2*time.Second); err != nil {
		t.Fatalf("OnRunCompleted: %v", err)
	}

	// Vectorize the written row(s) directly (no Redis embed worker needed).
	ix, err := agent.NewIndexer(f, f.Registry(), emb)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	n, err := ix.Reindex(tctx, entity)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n == 0 {
		t.Fatalf("Reindex indexed 0 rows; expected the memory row")
	}

	// Recall through the knowledge provider. The query text equals the stored
	// content so the hash vectors match: the memory must surface.
	provider := NewProvider(tk, WithEntities(entity))
	chunks, err := provider.Retrieve(tctx,
		"the coolant pump failed at station seven\nreplace the pump seal and restart",
		&knowledge.RetrieveParams{TopK: 5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("recall returned 0 chunks; the learning loop did not surface the memory")
	}
	top := chunks[0]
	if !contains(top.Content, "replace the pump seal") {
		t.Fatalf("top recalled chunk content = %q, want it to contain the run output", top.Content)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || index(s, sub) >= 0)
}
func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

(If `strings` is acceptable in the test, use `strings.Contains` instead of the hand-rolled `contains`/`index` helpers — they exist only to avoid an extra import if you prefer; either is fine. Prefer `strings.Contains` for clarity.)

- [ ] **Step 2: Verify it compiles under the integration tag**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go vet -tags integration ./integrations/fabriq/ && go build -tags integration ./integrations/fabriq/`
Expected: exit 0 (compiles). If a fabriq symbol differs from this skeleton (e.g. `fabriq.Open`'s return arity, `PostgresConfig` field name, `migrations.OpenOrchestrator` signature), READ the reference template and the actual fabriq source and adapt — the APIs in the template file are authoritative. Report any signature mismatch you fixed.

- [ ] **Step 3: Run it if Docker is available**

Run: `cd /Users/rexraphael/Work/xraph/forgery/cortex && go test -tags integration ./integrations/fabriq/ -run TestE2E_LearningLoop -v`
Expected: PASS if Docker/testcontainers is available. If Docker is NOT available, the test will fail to start a container — report that it compiles and is correctly gated, and that runtime verification needs Docker. Do NOT mark the task failed solely because Docker is unavailable in this environment.

- [ ] **Step 4: Commit**

```bash
cd /Users/rexraphael/Work/xraph/forgery/cortex
git add integrations/fabriq/e2e_integration_test.go
git commit -m "test(fabriqbrain): E2E learning loop write→vectorize→recall (integration-gated)"
```

---

## Plan ↔ Goal coverage

| Goal | Task |
|---|---|
| Turnkey host wiring (memory entity spec + write policy) | 10 (helper), 11 (docs) |
| Plugin writes a schema-stable `{content, meta}` payload | 10 |
| E2E proof of write→vectorize→recall | 12 |
