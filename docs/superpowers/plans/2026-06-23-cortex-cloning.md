# Cortex Agent & Persona Cloning — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add agent and persona cloning — pure deep-copy clone functions, engine `CloneAgent`/`ClonePersona` with unique-name resolution, and `POST /v1/{agents,personas}/:name/clone` endpoints.

**Architecture:** The deep copy is a JSON round-trip (marshal source → unmarshal into a fresh struct), then reset `Entity`/`ID`/`Name` — "save and reload as a new entity". Pure clone functions live in the `agent`/`persona` packages and are unit-tested for independence without a store. The engine loads the source, resolves a unique name, clones, and persists. Thin Forge handlers expose it.

**Tech Stack:** Go 1.25, `encoding/json` (deep copy), `github.com/xraph/forge` (HTTP), existing `id`/`cortex`/`store` packages.

## Global Constraints

- Module `github.com/xraph/cortex`, Go 1.25.7. No `Co-Authored-By` trailers.
- Deep copy is a JSON round-trip; then `Entity = cortex.NewEntity()`, `ID = newID`, `Name = newName`. All other fields (incl. `AppID`, `Enabled`, `PersonaRef`) preserved.
- Name resolution: provided+free → use it; provided+taken → `cortex.ErrAlreadyExists`; omitted → `"<source>-copy"`, `"<source>-copy-2"`, … first free (safety cap 1000).
- Agent clones keep the same `PersonaRef` (shallow). Runtime history (runs/memory/checkpoints) is never copied — it is keyed by the source ID, not a field on the entity.
- Errors reuse existing sentinels: source-not-found (`cortex.ErrAgentNotFound`/`ErrPersonaNotFound`) → 404; name-taken (`cortex.ErrAlreadyExists`) → 409. Both already handled by `mapStoreError` in `api/helpers.go`.
- Routes register under the existing `/v1` group via `registerAgentRoutes`/`registerPersonaRoutes`.
- **Quality gate (must pass at the end):** `make f` (`gofmt -s -w .` + `goimports -w -local github.com/xraph/cortex .`) leaves the tree clean; `make l` (`golangci-lint run ./...`) reports **0 issues**; `go build ./...` and `go test -race ./...` green.
- Imports must be `goimports`-grouped with `-local github.com/xraph/cortex` (stdlib, third-party, then `github.com/xraph/cortex/*`).

---

## File Structure

**Created:**
- `agent/clone.go` — `CloneConfig(src *Config, newID id.AgentID, newName string) (*Config, error)`.
- `agent/clone_test.go` — independence + reset test.
- `persona/clone.go` — `ClonePersona(src *Persona, newID id.PersonaID, newName string) (*Persona, error)`.
- `persona/clone_test.go` — independence + reset test.
- `engine/clone.go` — `resolveCloneName` + `CloneAgent` + `ClonePersona`.
- `engine/clone_test.go` — `resolveCloneName` cases + no-store guard.

**Modified:**
- `api/requests.go` — `CloneAgentRequest`, `ClonePersonaRequest`.
- `api/agent_handler.go` — `cloneAgent` handler + route.
- `api/persona_handler.go` — `clonePersona` handler + route.

---

## Task 1: `agent.CloneConfig`

**Files:**
- Create: `agent/clone.go`
- Test: `agent/clone_test.go`

**Interfaces:**
- Consumes: `agent.Config`, `cortex.NewEntity`, `id.AgentID`.
- Produces: `agent.CloneConfig(src *Config, newID id.AgentID, newName string) (*Config, error)`.

- [ ] **Step 1: Write the failing test**

Create `agent/clone_test.go`:

```go
package agent_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

func TestCloneConfigResetsIdentityAndDeepCopies(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &agent.Config{
		Entity:       cortex.Entity{CreatedAt: old, UpdatedAt: old},
		ID:           id.NewAgentID(),
		Name:         "original",
		AppID:        "app1",
		SystemPrompt: "sp",
		Model:        "smart",
		Tools:        []string{"t1", "t2"},
		Guardrails:   map[string]any{"budget": "10"},
		Metadata:     map[string]any{"m": "n"},
		InlineSkills: []string{"s1"},
		Enabled:      true,
		PersonaRef:   "p1",
	}

	newID := id.NewAgentID()
	clone, err := agent.CloneConfig(src, newID, "copy")
	if err != nil {
		t.Fatalf("CloneConfig: %v", err)
	}

	// Identity reset.
	if clone.ID.String() != newID.String() {
		t.Errorf("ID = %q, want %q", clone.ID, newID)
	}
	if clone.Name != "copy" {
		t.Errorf("Name = %q, want copy", clone.Name)
	}
	if clone.CreatedAt.Equal(old) || clone.UpdatedAt.Equal(old) {
		t.Errorf("timestamps not reset: created=%v updated=%v", clone.CreatedAt, clone.UpdatedAt)
	}

	// Config preserved.
	if clone.AppID != "app1" || clone.SystemPrompt != "sp" || clone.Model != "smart" ||
		!clone.Enabled || clone.PersonaRef != "p1" {
		t.Errorf("preserved fields wrong: %+v", clone)
	}

	// Deep-copy independence: mutate clone, source must not change.
	clone.Tools[0] = "MUT"
	clone.Guardrails["budget"] = "MUT"
	clone.Metadata["m"] = "MUT"
	clone.InlineSkills[0] = "MUT"
	if src.Tools[0] != "t1" {
		t.Error("Tools slice aliased to source")
	}
	if src.Guardrails["budget"] != "10" {
		t.Error("Guardrails map aliased to source")
	}
	if src.Metadata["m"] != "n" {
		t.Error("Metadata map aliased to source")
	}
	if src.InlineSkills[0] != "s1" {
		t.Error("InlineSkills slice aliased to source")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/ -run TestCloneConfig -v`
Expected: FAIL — `undefined: agent.CloneConfig`.

- [ ] **Step 3: Write minimal implementation**

Create `agent/clone.go`:

```go
package agent

import (
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// CloneConfig returns an independent deep copy of src as a new agent with the
// given ID and name and fresh timestamps. All other configuration (including
// AppID, Enabled, and PersonaRef) is preserved. Runtime history is not a field
// on Config and is therefore never copied. The deep copy is a JSON round-trip,
// so all nested slices and maps are independent of the source.
func CloneConfig(src *Config, newID id.AgentID, newName string) (*Config, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("clone agent config: marshal: %w", err)
	}
	clone := new(Config)
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("clone agent config: unmarshal: %w", err)
	}
	clone.Entity = cortex.NewEntity()
	clone.ID = newID
	clone.Name = newName
	return clone, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/ -run TestCloneConfig -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add agent/clone.go agent/clone_test.go
git commit -m "feat(agent): add CloneConfig deep-copy"
```

---

## Task 2: `persona.ClonePersona`

**Files:**
- Create: `persona/clone.go`
- Test: `persona/clone_test.go`

**Interfaces:**
- Consumes: `persona.Persona`, `persona.SkillAssignment`, `persona.TraitAssignment`, `cortex.NewEntity`, `id.PersonaID`.
- Produces: `persona.ClonePersona(src *Persona, newID id.PersonaID, newName string) (*Persona, error)`.

- [ ] **Step 1: Write the failing test**

Create `persona/clone_test.go`:

```go
package persona_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

func TestClonePersonaResetsIdentityAndDeepCopies(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &persona.Persona{
		Entity:   cortex.Entity{CreatedAt: old, UpdatedAt: old},
		ID:       id.NewPersonaID(),
		Name:     "original",
		AppID:    "app1",
		Identity: "I am a helper",
		Skills:   []persona.SkillAssignment{{SkillName: "research"}},
		Traits: []persona.TraitAssignment{
			{TraitName: "openness", DimensionValues: map[string]float64{"curiosity": 0.8}},
		},
		Behaviors: []string{"greet"},
		Metadata:  map[string]any{"m": "n"},
	}

	newID := id.NewPersonaID()
	clone, err := persona.ClonePersona(src, newID, "copy")
	if err != nil {
		t.Fatalf("ClonePersona: %v", err)
	}

	// Identity reset.
	if clone.ID.String() != newID.String() {
		t.Errorf("ID = %q, want %q", clone.ID, newID)
	}
	if clone.Name != "copy" {
		t.Errorf("Name = %q, want copy", clone.Name)
	}
	if clone.CreatedAt.Equal(old) || clone.UpdatedAt.Equal(old) {
		t.Errorf("timestamps not reset")
	}

	// Preserved.
	if clone.AppID != "app1" || clone.Identity != "I am a helper" {
		t.Errorf("preserved fields wrong: %+v", clone)
	}

	// Deep-copy independence.
	clone.Skills[0].SkillName = "MUT"
	clone.Traits[0].DimensionValues["curiosity"] = 0.1
	clone.Behaviors[0] = "MUT"
	clone.Metadata["m"] = "MUT"
	if src.Skills[0].SkillName != "research" {
		t.Error("Skills slice aliased to source")
	}
	if src.Traits[0].DimensionValues["curiosity"] != 0.8 {
		t.Error("Traits DimensionValues map aliased to source")
	}
	if src.Behaviors[0] != "greet" {
		t.Error("Behaviors slice aliased to source")
	}
	if src.Metadata["m"] != "n" {
		t.Error("Metadata map aliased to source")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./persona/ -run TestClonePersona -v`
Expected: FAIL — `undefined: persona.ClonePersona`.

- [ ] **Step 3: Write minimal implementation**

Create `persona/clone.go`:

```go
package persona

import (
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ClonePersona returns an independent deep copy of src as a new persona with the
// given ID and name and fresh timestamps. All other fields (including AppID and
// the full skill/trait/behavior/style composition) are preserved. The deep copy
// is a JSON round-trip, so all nested slices and maps are independent of src.
func ClonePersona(src *Persona, newID id.PersonaID, newName string) (*Persona, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("clone persona: marshal: %w", err)
	}
	clone := new(Persona)
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("clone persona: unmarshal: %w", err)
	}
	clone.Entity = cortex.NewEntity()
	clone.ID = newID
	clone.Name = newName
	return clone, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./persona/ -run TestClonePersona -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add persona/clone.go persona/clone_test.go
git commit -m "feat(persona): add ClonePersona deep-copy"
```

---

## Task 3: Engine `resolveCloneName`, `CloneAgent`, `ClonePersona`

**Files:**
- Create: `engine/clone.go`
- Test: `engine/clone_test.go`

**Interfaces:**
- Consumes: `agent.CloneConfig` (Task 1), `persona.ClonePersona` (Task 2); engine `e.store` (with `GetByName`, `Create`, `GetPersonaByName`, `CreatePersona`); `cortex.ErrNoStore`/`ErrAlreadyExists`/`ErrAgentNotFound`/`ErrPersonaNotFound`; `id.NewAgentID`/`NewPersonaID`.
- Produces: `(*Engine).CloneAgent(ctx, appID, sourceName, newName) (*agent.Config, error)`, `(*Engine).ClonePersona(ctx, appID, sourceName, newName) (*persona.Persona, error)`, unexported `resolveCloneName`.

- [ ] **Step 1: Write the failing test**

Create `engine/clone_test.go`. The `resolveCloneName` test is internal (`package engine`); the no-store test is external (`package engine_test`). Put each in its own file section — but a Go file has one package clause, so create `engine/clone_test.go` as `package engine` for `resolveCloneName`, and add the no-store assertions to the existing external test pattern by using the exported methods from within the same internal test (an internal test can call exported methods too):

```go
package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
)

func TestResolveCloneName(t *testing.T) {
	ctx := context.Background()

	// Desired name, free → returned as-is.
	got, err := resolveCloneName(ctx, "want", "src", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err != nil || got != "want" {
		t.Fatalf("desired-free: got %q, %v; want want,nil", got, err)
	}

	// Desired name, taken → ErrAlreadyExists.
	_, err = resolveCloneName(ctx, "want", "src", func(context.Context, string) (bool, error) {
		return true, nil
	})
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("desired-taken: err = %v, want ErrAlreadyExists", err)
	}

	// Empty desired, "src-copy" taken → "src-copy-2".
	taken := map[string]bool{"src-copy": true}
	got, err = resolveCloneName(ctx, "", "src", func(_ context.Context, n string) (bool, error) {
		return taken[n], nil
	})
	if err != nil || got != "src-copy-2" {
		t.Fatalf("auto-fallback: got %q, %v; want src-copy-2,nil", got, err)
	}

	// Empty desired, all free → "src-copy".
	got, err = resolveCloneName(ctx, "", "src", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err != nil || got != "src-copy" {
		t.Fatalf("auto-first: got %q, %v; want src-copy,nil", got, err)
	}
}

func TestCloneNoStore(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := e.CloneAgent(ctx, "app1", "x", ""); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("CloneAgent err = %v, want ErrNoStore", err)
	}
	if _, err := e.ClonePersona(ctx, "app1", "x", ""); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("ClonePersona err = %v, want ErrNoStore", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -run 'TestResolveCloneName|TestCloneNoStore' -v`
Expected: FAIL — `undefined: resolveCloneName` / `e.CloneAgent undefined`.

- [ ] **Step 3: Write minimal implementation**

Create `engine/clone.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

// maxCloneNameAttempts caps auto-generated name probing to avoid an unbounded loop.
const maxCloneNameAttempts = 1000

// resolveCloneName returns a free name for a clone. If desired is non-empty it is
// used when free and rejected with ErrAlreadyExists when taken. If desired is
// empty, "<source>-copy", "<source>-copy-2", … is tried until a free name is found.
// exists reports whether a name is already in use (true), free (false), or errors.
func resolveCloneName(ctx context.Context, desired, source string, exists func(context.Context, string) (bool, error)) (string, error) {
	if desired != "" {
		taken, err := exists(ctx, desired)
		if err != nil {
			return "", err
		}
		if taken {
			return "", cortex.ErrAlreadyExists
		}
		return desired, nil
	}

	base := source + "-copy"
	candidate := base
	for i := 1; i <= maxCloneNameAttempts; i++ {
		taken, err := exists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i+1)
	}
	return "", fmt.Errorf("clone: no free name for %q after %d attempts", source, maxCloneNameAttempts)
}

// CloneAgent creates an independent copy of an existing agent in the same app.
func (e *Engine) CloneAgent(ctx context.Context, appID, sourceName, newName string) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	src, err := e.store.GetByName(ctx, appID, sourceName)
	if err != nil {
		return nil, err
	}
	name, err := resolveCloneName(ctx, newName, sourceName, func(c context.Context, n string) (bool, error) {
		_, gerr := e.store.GetByName(c, appID, n)
		if gerr == nil {
			return true, nil
		}
		if errors.Is(gerr, cortex.ErrAgentNotFound) {
			return false, nil
		}
		return false, gerr
	})
	if err != nil {
		return nil, err
	}
	clone, err := agent.CloneConfig(src, id.NewAgentID(), name)
	if err != nil {
		return nil, err
	}
	if err := e.store.Create(ctx, clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// ClonePersona creates an independent copy of an existing persona in the same app.
func (e *Engine) ClonePersona(ctx context.Context, appID, sourceName, newName string) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	src, err := e.store.GetPersonaByName(ctx, appID, sourceName)
	if err != nil {
		return nil, err
	}
	name, err := resolveCloneName(ctx, newName, sourceName, func(c context.Context, n string) (bool, error) {
		_, gerr := e.store.GetPersonaByName(c, appID, n)
		if gerr == nil {
			return true, nil
		}
		if errors.Is(gerr, cortex.ErrPersonaNotFound) {
			return false, nil
		}
		return false, gerr
	})
	if err != nil {
		return nil, err
	}
	clone, err := persona.ClonePersona(src, id.NewPersonaID(), name)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePersona(ctx, clone); err != nil {
		return nil, err
	}
	return clone, nil
}
```

> Verify the store method names against `agent.Store` / `persona.Store`: agents use `GetByName(ctx, appID, name)` and `Create(ctx, *Config)`; personas use `GetPersonaByName(ctx, appID, name)` and `CreatePersona(ctx, *Persona)`. These are reachable on `e.store` (the composite `store.Store`). If a name differs, match the interface.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./engine/ -run 'TestResolveCloneName|TestCloneNoStore' -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add engine/clone.go engine/clone_test.go
git commit -m "feat(engine): add CloneAgent and ClonePersona with unique-name resolution"
```

---

## Task 4: API clone endpoints

**Files:**
- Modify: `api/requests.go`
- Modify: `api/agent_handler.go`
- Modify: `api/persona_handler.go`

**Interfaces:**
- Consumes: `(*Engine).CloneAgent`/`ClonePersona` (Task 3); `mapStoreError`, `cortex.AppFromContext`; Forge router helpers.
- Produces: `CloneAgentRequest`, `ClonePersonaRequest`; routes `POST /v1/agents/:name/clone`, `POST /v1/personas/:name/clone`.

- [ ] **Step 1: Add request structs**

Append to `api/requests.go`:

```go
// CloneAgentRequest is the request for cloning an agent.
type CloneAgentRequest struct {
	Name    string `path:"name" description:"Source agent name"`
	NewName string `json:"new_name,omitempty" description:"Name for the clone; auto-generated if omitted"`
}

// ClonePersonaRequest is the request for cloning a persona.
type ClonePersonaRequest struct {
	Name    string `path:"name" description:"Source persona name"`
	NewName string `json:"new_name,omitempty" description:"Name for the clone; auto-generated if omitted"`
}
```

- [ ] **Step 2: Add the agent clone handler + route**

In `api/agent_handler.go`, register the route inside `registerAgentRoutes` (next to the existing `/agents/:name/run` registration):

```go
	if err := g.POST("/agents/:name/clone", a.cloneAgent,
		forge.WithSummary("Clone agent"),
		forge.WithDescription("Creates an independent copy of an agent with a fresh ID and name."),
		forge.WithOperationID("cloneAgent"),
		forge.WithRequestSchema(CloneAgentRequest{}),
		forge.WithCreatedResponse(&agent.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}
```

Add the handler (place it near `runAgent`):

```go
func (a *API) cloneAgent(ctx forge.Context, req *CloneAgentRequest) (*agent.Config, error) {
	appID := cortex.AppFromContext(ctx.Context())
	clone, err := a.eng.CloneAgent(ctx.Context(), appID, req.Name, req.NewName)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return clone, ctx.JSON(http.StatusCreated, clone)
}
```

> `api/agent_handler.go` already imports `fmt`, `net/http`, `github.com/xraph/forge`, `github.com/xraph/cortex`, and `github.com/xraph/cortex/agent` (used by the existing agent handlers). If any are missing, `goimports` will add them in the gate.

- [ ] **Step 3: Add the persona clone handler + route**

In `api/persona_handler.go`, register the route inside `registerPersonaRoutes`:

```go
	if err := g.POST("/personas/:name/clone", a.clonePersona,
		forge.WithSummary("Clone persona"),
		forge.WithDescription("Creates an independent copy of a persona with a fresh ID and name."),
		forge.WithOperationID("clonePersona"),
		forge.WithRequestSchema(ClonePersonaRequest{}),
		forge.WithCreatedResponse(&persona.Persona{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register persona routes: %w", err)
	}
```

Add the handler:

```go
func (a *API) clonePersona(ctx forge.Context, req *ClonePersonaRequest) (*persona.Persona, error) {
	appID := cortex.AppFromContext(ctx.Context())
	clone, err := a.eng.ClonePersona(ctx.Context(), appID, req.Name, req.NewName)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return clone, ctx.JSON(http.StatusCreated, clone)
}
```

- [ ] **Step 4: Verify build + suite**

Run: `go build ./... && go test ./... 2>&1 | tail -5`
Expected: build clean; suite green (no API handler unit tests are required, mirroring the rest of `api/`).

- [ ] **Step 5: Commit**

```bash
git add api/requests.go api/agent_handler.go api/persona_handler.go
git commit -m "feat(api): add agent and persona clone endpoints"
```

---

## Task 5: Quality gate (`make f` / `make l`)

**Files:** none (formatting + lint only; fix in-place if anything is flagged).

- [ ] **Step 1: Format**

Run: `make f`
Then: `git status --porcelain` — if `make f` changed any tracked file, review and `git commit -am "style: gofmt/goimports clone code"` (only the clone files should appear; if an unrelated file changed, inspect before committing).

- [ ] **Step 2: Lint (whole module)**

Run: `make l`
Expected: `0 issues.` If golangci-lint flags anything in the new files, fix it and re-run until clean. (Common: an unchecked error, a stutter name, an unused parameter — address per the linter's suggestion.)

- [ ] **Step 3: Full verification**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: all green.

- [ ] **Step 4: Commit any lint fixes**

```bash
git add -A -- agent/ persona/ engine/ api/
git commit -m "fix(lint): clear findings in clone code" # only if there were fixes
```

> Stage clone files explicitly (`agent/ persona/ engine/ api/`), NOT `git add -A` repo-wide, to avoid sweeping untracked files.

---

## Done criteria

- [ ] `CloneConfig` / `ClonePersona` deep-copy (JSON round-trip), reset ID/Name/timestamps, preserve all else — independence tests green.
- [ ] `resolveCloneName` handles provided-free / provided-taken (409) / auto-fallback sequence.
- [ ] `CloneAgent` / `ClonePersona` load → resolve name → clone → persist; no-store guard returns `ErrNoStore`.
- [ ] `POST /v1/agents/:name/clone` and `POST /v1/personas/:name/clone` return 201 with the new entity; 404 on missing source, 409 on taken name.
- [ ] **`make f` clean, `make l` = 0 issues**, `go build`, `go vet`, `go test -race ./...` all green.
