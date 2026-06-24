# Cortex Agent & Persona Cloning — Design

- **Status:** Approved (brainstorming) — ready for implementation planning
- **Date:** 2026-06-23
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex` (all changes live here).
- **Related:** Follow-up to the orchestration feature (`2026-06-23-cortex-orchestration-design.md`). Cloning was the one capability from the original "multi-agent / cloning / sub-agents / communication" question deliberately deferred there.

---

## 1. Goal

Let a caller **clone an existing agent or persona** into a new, independent entity in the same app — copying all configuration, assigning a fresh ID and name with clean timestamps, and carrying **no runtime history**. This makes "duplicate this agent and tweak it" a one-call operation instead of a manual re-create.

After this lands:

1. **Agent cloning** — `engine.CloneAgent` + `POST /v1/agents/:name/clone`.
2. **Persona cloning** — `engine.ClonePersona` + `POST /v1/personas/:name/clone`.
3. **Smart naming** — caller may pass a new name; if omitted, a unique `"<source>-copy"` name is generated.

### Non-goals

- **Deep persona cloning when cloning an agent** — an agent clone keeps the same `PersonaRef` (shallow; both agents share the one persona). Decided in brainstorming.
- **Cloning runtime history** — runs, memory, and checkpoints are keyed by the source's ID; a fresh ID inherently starts empty. We do not copy them.
- **Bulk or cross-app cloning** — one entity at a time, same app.

---

## 2. Background — what gets copied

`agent.Config` (in `agent/config.go`) carries scalar fields plus ref types that must be **deep-copied** so the clone is independent: `Tools []string`, `Guardrails map[string]any`, `Metadata map[string]any`, `InlineSkills/InlineTraits/InlineBehaviors []string`.

`persona.Persona` (in `persona/persona.go`) carries `Skills []SkillAssignment` (`SkillName` + scalar `Proficiency`), `Traits []TraitAssignment` (each with `DimensionValues map[string]float64` — a nested map), `Behaviors []string`, `CognitiveStyle` (contains `Phases []Phase`), `CommunicationStyle` (all scalars), `Perception` (contains `AttentionFilters []AttentionFilter`), and `Metadata map[string]any`.

Hand-copying every nested slice/map is error-prone and breaks silently when a new nested field is added. Both entities are already fully JSON-tagged and are persisted via JSON/JSONB. Therefore the clone uses a **JSON round-trip** (`json.Marshal` the source, `json.Unmarshal` into a fresh value) for the deep copy, then overwrites the identity fields. This is exactly the "save and reload as a new entity" semantics of a clone, and it deep-copies all current and future nested fields for free.

Runtime history is not a field on these entities — it lives in separate stores keyed by the source ID — so the round-trip never touches it.

---

## 3. Architecture — small, independently testable units

```
agent/clone.go     CloneConfig(src *agent.Config, newID id.AgentID, newName string) (*agent.Config, error)
persona/clone.go   ClonePersona(src *persona.Persona, newID id.PersonaID, newName string) (*persona.Persona, error)
engine/clone.go    (*Engine).CloneAgent / (*Engine).ClonePersona + resolveCloneName helper
api/…              POST /v1/agents/:name/clone, POST /v1/personas/:name/clone + request structs
```

### Pure clone functions (no store)

`CloneConfig` / `ClonePersona` are pure: JSON round-trip the source into a new struct, then set `Entity = cortex.NewEntity()` (fresh timestamps), `ID = newID`, `Name = newName`. `AppID`, `Enabled`, and every config/persona field are preserved. They return an error only to surface a (practically impossible) JSON failure; callers propagate it. Living in the entity's own package keeps the deep-copy logic next to the type it copies.

### Engine methods

```go
func (e *Engine) CloneAgent(ctx context.Context, appID, sourceName, newName string) (*agent.Config, error)
func (e *Engine) ClonePersona(ctx context.Context, appID, sourceName, newName string) (*persona.Persona, error)
```

Each: guard `e.store == nil` → `cortex.ErrNoStore`; load the source by name (`GetByName` / `GetPersonaByName`) — not found propagates the existing `Err*NotFound`; resolve a unique name (below); call the pure clone fn with a freshly generated ID; `Create` the clone; return it.

### Name resolution

`resolveCloneName(ctx, desired, source string, exists func(ctx, name string) (bool, error)) (string, error)`:

- `desired != ""` and free → `desired`.
- `desired != ""` and taken → `cortex.ErrAlreadyExists` (maps to 409).
- `desired == ""` → try `"<source>-copy"`, then `"<source>-copy-2"`, `"<source>-copy-3"`, … returning the first free name. A safety cap (e.g. 1000 attempts) returns an error rather than looping forever.

`exists` is injected (the engine passes a closure over `GetByName`/`GetPersonaByName` that returns `(true,nil)` on found, `(false,nil)` on `Err*NotFound`, and `(false,err)` on any other store error), so `resolveCloneName` is unit-testable with a fake.

---

## 4. API

Two routes, mirroring the existing `runAgent`-style action handlers, added to the existing `/v1` agent and persona route groups:

```
POST /v1/agents/:name/clone     cloneAgent     body: {"new_name": "..."}  (optional) → 201 agent.Config
POST /v1/personas/:name/clone   clonePersona   body: {"new_name": "..."}  (optional) → 201 persona.Persona
```

Request structs (in `api/requests.go`):

```go
type CloneAgentRequest struct {
    Name    string `path:"name" description:"Source agent name"`
    NewName string `json:"new_name,omitempty" description:"Name for the clone; auto-generated if omitted"`
}
type ClonePersonaRequest struct {
    Name    string `path:"name" description:"Source persona name"`
    NewName string `json:"new_name,omitempty"`
}
```

Handlers scope by `cortex.AppFromContext`, call the engine clone method, and return the new entity with `ctx.JSON(http.StatusCreated, …)`. Errors route through the existing `mapStoreError`: source-not-found → 404 (`Err*NotFound` already in `isNotFound`), name-taken → 409 (`ErrAlreadyExists` already in `isConflict`). Routes register inside the existing `registerAgentRoutes` / `registerPersonaRoutes`.

---

## 5. Testing

The repo has no DB/store test harness; the meaningful logic is pure and tested without one:

- **`CloneConfig` / `ClonePersona` independence tests** — clone a populated source, then mutate the clone's slices and maps (`Tools`, `Guardrails`, `Metadata`, `InlineSkills`; persona `Skills`, `Traits[].DimensionValues`, `CognitiveStyle.Phases`, `Perception.AttentionFilters`, `Metadata`) and assert the source is unchanged. Assert `ID`/`Name` are the new values, `CreatedAt`/`UpdatedAt` are fresh (not the source's), and `AppID`/`Enabled`/representative config fields are preserved.
- **`resolveCloneName` tests** — desired free → desired; desired taken → `ErrAlreadyExists`; empty desired with `<source>-copy` taken → `<source>-copy-2`; safety-cap behavior.
- **Engine no-store guard** — `CloneAgent`/`ClonePersona` on a store-less engine return `cortex.ErrNoStore`.
- **Backwards compatibility** — existing suite stays green.

---

## 6. Quality gates (project requirements)

- **`make f`** (`gofmt -s -w .` + `goimports -w -local github.com/xraph/cortex .`) must leave the tree clean.
- **`make l`** (`golangci-lint run ./...`, whole module) must report **0 issues**.
- `go build ./...` and `go test -race ./...` green.
- No `Co-Authored-By` trailers in commits.

---

## 7. Build sequence (for the plan)

1. `agent/clone.go` `CloneConfig` + independence test.
2. `persona/clone.go` `ClonePersona` + independence test.
3. `engine/clone.go` `resolveCloneName` (+ test) and `CloneAgent`/`ClonePersona` (+ no-store test).
4. API request structs + clone handlers + route registration.
5. `make f` / `make l` / `go test -race ./...` gate; docs touch if warranted.
