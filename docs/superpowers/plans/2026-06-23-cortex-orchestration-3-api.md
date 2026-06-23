# Cortex Orchestration — Plan 3: API & Examples

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose orchestration over HTTP — CRUD for configs, a run endpoint (`POST /v1/orchestrations/:name/run`), and run history reads — wired into the existing Forge API, with the new not-found sentinels mapped to 404, plus a documentation page.

**Architecture:** Mirror the existing `persona_handler.go` pattern exactly: a `registerOrchestrationRoutes(router)` method registering a `/v1` route group with OpenAPI metadata, typed request structs (`path:`/`query:`/`json:` tags), handlers that scope by `cortex.AppFromContext`, and `mapStoreError` for error translation. The run endpoint mirrors `runAgent`, delegating to `engine.RunOrchestration` (Plan 2). Configs are addressed by name (like personas); runs by ID.

**Tech Stack:** Go 1.25, `github.com/xraph/forge` (router, `forge.Context`, OpenAPI option helpers), Fumadocs MDX for docs.

## Global Constraints

- Prerequisites: **Plans 1 and 2 complete and merged** (entities, stores, engine CRUD, `engine.RunOrchestration`).
- No `Co-Authored-By` trailers in commits.
- Route group prefix is **`/v1`** (every existing handler uses `router.Group("/v1", ...)`). The spec/design doc text said `/cortex/...`; the **codebase reality is `/v1`** — follow the code. Final run route: `POST /v1/orchestrations/:name/run`.
- Handlers have signature `func (a *API) name(ctx forge.Context, req *ReqStruct) (*RespType, error)` and end with `ctx.JSON(...)` / `ctx.NoContent(...)`.
- App scoping via `cortex.AppFromContext(ctx.Context())`; path params via `ctx.Param("name")`; errors via `forge.BadRequest(...)` and `mapStoreError(...)`.
- List responses are `ListXResponse{ Items: ... }` structs in `api/responses.go`. Pagination via `defaultLimit(req.Limit)` + `req.Offset`.
- Request structs reuse `orchestration.Participant` and `orchestration.Settings` directly (both already JSON-tagged) — no separate Req mapping types.

---

## File Structure

**Created:**
- `api/orchestration_handler.go` — `registerOrchestrationRoutes` + 8 handlers.
- `docs/content/docs/execution/orchestration.mdx` — documentation page.

**Modified:**
- `api/requests.go` — orchestration request structs.
- `api/responses.go` — orchestration response structs.
- `api/helpers.go` — add the two new sentinels to `isNotFound`.
- `api/api.go` — call `registerOrchestrationRoutes` in `RegisterRoutes`.
- `docs/content/docs/execution/meta.json` — add `"orchestration"` to pages.

---

## Task 1: Request and response structs

**Files:**
- Modify: `api/requests.go`
- Modify: `api/responses.go`

**Interfaces:**
- Consumes: `orchestration.Participant`, `orchestration.Settings`, `orchestration.Config`, `orchestration.Run`.
- Produces: `CreateOrchestrationRequest`, `GetOrchestrationRequest`, `ListOrchestrationsRequest`, `UpdateOrchestrationRequest`, `DeleteOrchestrationRequest`, `RunOrchestrationRequest`, `GetOrchestrationRunRequest`, `ListOrchestrationRunsRequest`; `ListOrchestrationsResponse`, `ListOrchestrationRunsResponse`, `RunOrchestrationResponse`.

- [ ] **Step 1: Add request structs**

Append to `api/requests.go` (ensure `"github.com/xraph/cortex/orchestration"` is imported):

```go
// CreateOrchestrationRequest is the request body for creating an orchestration config.
type CreateOrchestrationRequest struct {
	Name         string                     `json:"name" description:"Unique orchestration name"`
	Description  string                     `json:"description,omitempty"`
	Strategy     string                     `json:"strategy" description:"sequential|parallel|router|hierarchical|debate"`
	Participants []orchestration.Participant `json:"participants" description:"Agents taking part, with optional roles"`
	Settings     orchestration.Settings     `json:"settings,omitempty"`
	Metadata     map[string]any             `json:"metadata,omitempty"`
}

// GetOrchestrationRequest addresses an orchestration config by name.
type GetOrchestrationRequest struct {
	Name string `path:"name" description:"Orchestration name"`
}

// ListOrchestrationsRequest paginates orchestration configs.
type ListOrchestrationsRequest struct {
	Limit  int `query:"limit" description:"Max results (default: 50)"`
	Offset int `query:"offset" description:"Results to skip"`
}

// UpdateOrchestrationRequest is the request body for updating an orchestration config.
type UpdateOrchestrationRequest struct {
	Name         string                      `path:"name" description:"Orchestration name"`
	Description  string                      `json:"description,omitempty"`
	Strategy     string                      `json:"strategy,omitempty"`
	Participants []orchestration.Participant `json:"participants,omitempty"`
	Settings     *orchestration.Settings     `json:"settings,omitempty"`
	Metadata     map[string]any              `json:"metadata,omitempty"`
}

// DeleteOrchestrationRequest addresses an orchestration config by name.
type DeleteOrchestrationRequest struct {
	Name string `path:"name" description:"Orchestration name"`
}

// RunOrchestrationRequest runs a stored orchestration by name.
type RunOrchestrationRequest struct {
	Name  string `path:"name" description:"Orchestration name"`
	Input string `json:"input" description:"Initial input for the orchestration"`
}

// GetOrchestrationRunRequest addresses an orchestration run by ID.
type GetOrchestrationRunRequest struct {
	ID string `path:"id" description:"Orchestration run ID"`
}

// ListOrchestrationRunsRequest paginates and filters orchestration runs.
type ListOrchestrationRunsRequest struct {
	Status string `query:"status" description:"Filter by status: running|completed|failed"`
	Limit  int    `query:"limit"`
	Offset int    `query:"offset"`
}
```

- [ ] **Step 2: Add response structs**

Append to `api/responses.go` (ensure `"github.com/xraph/cortex/orchestration"` is imported):

```go
// ListOrchestrationsResponse wraps a list of orchestration configs.
type ListOrchestrationsResponse struct {
	Items []*orchestration.Config `json:"items"`
}

// ListOrchestrationRunsResponse wraps a list of orchestration runs.
type ListOrchestrationRunsResponse struct {
	Items []*orchestration.Run `json:"items"`
}

// RunOrchestrationResponse summarizes a completed orchestration run.
type RunOrchestrationResponse struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Strategy   string `json:"strategy"`
	Output     string `json:"output"`
	DurationMs int64  `json:"duration_ms"`
}
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./api/`
Expected: builds (structs reference real `orchestration` types).

- [ ] **Step 4: Commit**

```bash
git add api/requests.go api/responses.go
git commit -m "feat(api): add orchestration request and response structs"
```

---

## Task 2: Map new not-found sentinels to 404

**Files:**
- Modify: `api/helpers.go`

**Interfaces:**
- Consumes: `cortex.ErrOrchestrationNotFound`, `cortex.ErrOrchestrationRunNotFound` (Plan 1).
- Produces: `isNotFound` now recognizes both, so `mapStoreError` returns 404 for them.

- [ ] **Step 1: Extend `isNotFound`**

In `api/helpers.go`, add the two sentinels to the `isNotFound` chain:

```go
func isNotFound(err error) bool {
	return errors.Is(err, cortex.ErrAgentNotFound) ||
		errors.Is(err, cortex.ErrSkillNotFound) ||
		errors.Is(err, cortex.ErrTraitNotFound) ||
		errors.Is(err, cortex.ErrBehaviorNotFound) ||
		errors.Is(err, cortex.ErrPersonaNotFound) ||
		errors.Is(err, cortex.ErrRunNotFound) ||
		errors.Is(err, cortex.ErrCheckpointNotFound) ||
		errors.Is(err, cortex.ErrOrchestrationNotFound) ||
		errors.Is(err, cortex.ErrOrchestrationRunNotFound)
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./api/`
Expected: builds.

- [ ] **Step 3: Commit**

```bash
git add api/helpers.go
git commit -m "feat(api): map orchestration not-found errors to 404"
```

---

## Task 3: Orchestration handler + route registration

**Files:**
- Create: `api/orchestration_handler.go`
- Modify: `api/api.go`

**Interfaces:**
- Consumes: engine methods `CreateOrchestration`, `GetOrchestrationByName`, `ListOrchestrations`, `UpdateOrchestration`, `DeleteOrchestration`, `RunOrchestration`, `GetOrchestrationRun`, `ListOrchestrationRuns` (Plans 1-2); the request/response structs (Task 1).
- Produces: `(*API).registerOrchestrationRoutes(router) error` and 8 handlers; `RegisterRoutes` now calls it.

- [ ] **Step 1: Write the handler file**

Create `api/orchestration_handler.go`:

```go
package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (a *API) registerOrchestrationRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("orchestrations"))

	if err := g.POST("/orchestrations", a.createOrchestration,
		forge.WithSummary("Create orchestration"),
		forge.WithDescription("Creates a multi-agent orchestration config (strategy + participants + settings)."),
		forge.WithOperationID("createOrchestration"),
		forge.WithRequestSchema(CreateOrchestrationRequest{}),
		forge.WithCreatedResponse(&orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestrations", a.listOrchestrations,
		forge.WithSummary("List orchestrations"),
		forge.WithOperationID("listOrchestrations"),
		forge.WithRequestSchema(ListOrchestrationsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Orchestration list", []*orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestrations/:name", a.getOrchestration,
		forge.WithSummary("Get orchestration"),
		forge.WithOperationID("getOrchestration"),
		forge.WithResponseSchema(http.StatusOK, "Orchestration details", &orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.PUT("/orchestrations/:name", a.updateOrchestration,
		forge.WithSummary("Update orchestration"),
		forge.WithOperationID("updateOrchestration"),
		forge.WithRequestSchema(UpdateOrchestrationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated orchestration", &orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.DELETE("/orchestrations/:name", a.deleteOrchestration,
		forge.WithSummary("Delete orchestration"),
		forge.WithOperationID("deleteOrchestration"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.POST("/orchestrations/:name/run", a.runOrchestration,
		forge.WithSummary("Run orchestration"),
		forge.WithDescription("Executes a stored orchestration and returns the run summary."),
		forge.WithOperationID("runOrchestration"),
		forge.WithRequestSchema(RunOrchestrationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run summary", &RunOrchestrationResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestration-runs", a.listOrchestrationRuns,
		forge.WithSummary("List orchestration runs"),
		forge.WithOperationID("listOrchestrationRuns"),
		forge.WithRequestSchema(ListOrchestrationRunsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run list", []*orchestration.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestration-runs/:id", a.getOrchestrationRun,
		forge.WithSummary("Get orchestration run"),
		forge.WithOperationID("getOrchestrationRun"),
		forge.WithResponseSchema(http.StatusOK, "Run details", &orchestration.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	return nil
}

var knownStrategies = map[string]bool{
	orchestration.StrategySequential:   true,
	orchestration.StrategyParallel:     true,
	orchestration.StrategyRouter:       true,
	orchestration.StrategyHierarchical: true,
	orchestration.StrategyDebate:       true,
}

func (a *API) createOrchestration(ctx forge.Context, req *CreateOrchestrationRequest) (*orchestration.Config, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}
	if !knownStrategies[req.Strategy] {
		return nil, forge.BadRequest("strategy must be one of: sequential, parallel, router, hierarchical, debate")
	}
	if len(req.Participants) == 0 {
		return nil, forge.BadRequest("at least one participant is required")
	}

	c := &orchestration.Config{
		Entity:       cortex.NewEntity(),
		ID:           id.NewOrchestrationConfigID(),
		Name:         req.Name,
		Description:  req.Description,
		AppID:        cortex.AppFromContext(ctx.Context()),
		Strategy:     req.Strategy,
		Participants: req.Participants,
		Settings:     req.Settings,
		Metadata:     req.Metadata,
	}
	if err := a.eng.CreateOrchestration(ctx.Context(), c); err != nil {
		return nil, fmt.Errorf("create orchestration: %w", err)
	}
	return c, ctx.JSON(http.StatusCreated, c)
}

func (a *API) getOrchestration(ctx forge.Context, _ *GetOrchestrationRequest) (*orchestration.Config, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return c, ctx.JSON(http.StatusOK, c)
}

func (a *API) listOrchestrations(ctx forge.Context, req *ListOrchestrationsRequest) (*ListOrchestrationsResponse, error) {
	items, err := a.eng.ListOrchestrations(ctx.Context(), &orchestration.ConfigListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list orchestrations: %w", err)
	}
	resp := &ListOrchestrationsResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) updateOrchestration(ctx forge.Context, req *UpdateOrchestrationRequest) (*orchestration.Config, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		c.Description = req.Description
	}
	if req.Strategy != "" {
		if !knownStrategies[req.Strategy] {
			return nil, forge.BadRequest("strategy must be one of: sequential, parallel, router, hierarchical, debate")
		}
		c.Strategy = req.Strategy
	}
	if req.Participants != nil {
		c.Participants = req.Participants
	}
	if req.Settings != nil {
		c.Settings = *req.Settings
	}
	if req.Metadata != nil {
		c.Metadata = req.Metadata
	}

	if err := a.eng.UpdateOrchestration(ctx.Context(), c); err != nil {
		return nil, fmt.Errorf("update orchestration: %w", err)
	}
	return c, ctx.JSON(http.StatusOK, c)
}

func (a *API) deleteOrchestration(ctx forge.Context, _ *DeleteOrchestrationRequest) (*struct{}, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteOrchestration(ctx.Context(), c.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}

func (a *API) runOrchestration(ctx forge.Context, req *RunOrchestrationRequest) (*RunOrchestrationResponse, error) {
	if req.Input == "" {
		return nil, forge.BadRequest("input is required")
	}
	appID := cortex.AppFromContext(ctx.Context())
	r, err := a.eng.RunOrchestration(ctx.Context(), appID, req.Name, req.Input)
	if err != nil {
		return nil, mapStoreError(err)
	}

	var durationMs int64
	if r.CompletedAt != nil {
		durationMs = r.CompletedAt.Sub(r.StartedAt).Milliseconds()
	}
	resp := &RunOrchestrationResponse{
		RunID:      r.ID.String(),
		Status:     r.Status,
		Strategy:   r.Strategy,
		Output:     r.Output,
		DurationMs: durationMs,
	}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) listOrchestrationRuns(ctx forge.Context, req *ListOrchestrationRunsRequest) (*ListOrchestrationRunsResponse, error) {
	items, err := a.eng.ListOrchestrationRuns(ctx.Context(), &orchestration.RunListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Status: req.Status,
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list orchestration runs: %w", err)
	}
	resp := &ListOrchestrationRunsResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) getOrchestrationRun(ctx forge.Context, _ *GetOrchestrationRunRequest) (*orchestration.Run, error) {
	runID, err := id.ParseOrchestrationID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest("invalid orchestration run id")
	}
	r, err := a.eng.GetOrchestrationRun(ctx.Context(), runID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return r, ctx.JSON(http.StatusOK, r)
}
```

- [ ] **Step 2: Register the routes**

In `api/api.go`, inside `RegisterRoutes`, add the call before the final `registerConfigRoutes` line:

```go
	if err := a.registerToolRoutes(router); err != nil {
		return err
	}
	if err := a.registerOrchestrationRoutes(router); err != nil {
		return err
	}
	return a.registerConfigRoutes(router)
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./...`
Expected: builds with no errors.

- [ ] **Step 4: Run the suite**

Run: `go test ./...`
Expected: PASS (no API handler unit tests are required here; existing suite stays green).

- [ ] **Step 5: Commit**

```bash
git add api/orchestration_handler.go api/api.go
git commit -m "feat(api): add orchestration CRUD and run endpoints"
```

---

## Task 4: Documentation page

**Files:**
- Create: `docs/content/docs/execution/orchestration.mdx`
- Modify: `docs/content/docs/execution/meta.json`

**Interfaces:**
- Consumes: nothing (docs).
- Produces: a navigable "Orchestration" page under the Execution section.

- [ ] **Step 1: Write the docs page**

Create `docs/content/docs/execution/orchestration.mdx`:

````mdx
---
title: Orchestration
description: Run multiple agents together — sequential, parallel, router, hierarchical, and debate strategies with a shared blackboard and handoffs.
---

Cortex orchestration coordinates **multiple agents working together**. An
orchestration is a stored config — a strategy plus the participant agents and
their settings — that you run by name. During a run, agents share a **blackboard**
(roster + prior contributions) and pass work through recorded **handoffs**.

## Strategies

| Strategy | Behavior |
|----------|----------|
| `sequential` | Agents run in order; each sees the prior outputs and the final answer is the last agent's. |
| `parallel` | All agents run concurrently on the same input; an optional aggregator agent synthesizes one answer. |
| `router` | A static rule map or a router agent picks one agent to handle the input. |
| `hierarchical` | A manager agent delegates subtasks to workers (JSON plan), then synthesizes the result. |
| `debate` | Debater agents argue over N rounds, then a judge agent renders a verdict. |

## Awareness and communication

Every participant sees a **roster** of the others (name + role) and the running
log of contributions via the blackboard snapshot prepended to its input. When one
agent hands off to another, an `AgentHandoff` plugin hook fires (and is recorded
on the run), so audit and metrics extensions observe the collaboration.

## Settings

```json
{
  "max_concurrency": 4,
  "rounds": 2,
  "manager": "lead",
  "judge": "referee",
  "aggregator": "summarizer",
  "router_agent": "dispatcher",
  "router_rules": { "refund": "billing", "bug": "support" },
  "model": "smart"
}
```

Each strategy reads only the settings it needs; the rest are ignored.

## Create and run over HTTP

```bash
# Create a debate orchestration
curl -X POST /v1/orchestrations -H 'Content-Type: application/json' -d '{
  "name": "should-we-ship",
  "strategy": "debate",
  "participants": [
    {"agent_name": "optimist", "role": "debater"},
    {"agent_name": "skeptic", "role": "debater"},
    {"agent_name": "referee", "role": "judge"}
  ],
  "settings": {"rounds": 2, "judge": "referee"}
}'

# Run it
curl -X POST /v1/orchestrations/should-we-ship/run \
  -H 'Content-Type: application/json' \
  -d '{"input": "Should we ship the feature this week?"}'
```

The run returns a summary:

```json
{
  "run_id": "orch_01...",
  "status": "completed",
  "strategy": "debate",
  "output": "...the judge's verdict...",
  "duration_ms": 4213
}
```

Run history is available at `GET /v1/orchestration-runs` and
`GET /v1/orchestration-runs/:id`.

## In Go

```go
// Persist a config once...
cfg := &orchestration.Config{
    ID:       id.NewOrchestrationConfigID(),
    Name:     "research-team",
    AppID:    "my-app",
    Strategy: orchestration.StrategySequential,
    Participants: []orchestration.Participant{
        {AgentName: "researcher", Role: "worker"},
        {AgentName: "writer", Role: "worker"},
    },
}
_ = eng.CreateOrchestration(ctx, cfg)

// ...then run it by name.
run, err := eng.RunOrchestration(ctx, "my-app", "research-team", "Summarize CRDTs")
```
````

- [ ] **Step 2: Add the page to navigation**

Replace the contents of `docs/content/docs/execution/meta.json` with:

```json
{
  "title": "Execution",
  "pages": ["runs", "orchestration", "memory", "checkpoints"]
}
```

- [ ] **Step 3: Verify the MDX is well-formed**

Run: `git diff --stat docs/content/docs/execution/`
Expected: shows the new `orchestration.mdx` and modified `meta.json`. (The docs site is a separate pnpm project; a full Fumadocs build is optional and not required for this task. Visually confirm the front-matter block and fenced code blocks are balanced.)

- [ ] **Step 4: Commit**

```bash
git add docs/content/docs/execution/orchestration.mdx docs/content/docs/execution/meta.json
git commit -m "docs: add orchestration page under Execution"
```

---

## Done criteria for Plan 3

- [ ] `go build ./... && go test ./...` green.
- [ ] CRUD + run + run-history endpoints registered under `/v1` and visible in OpenAPI metadata.
- [ ] `POST /v1/orchestrations/:name/run` executes a stored orchestration via `engine.RunOrchestration` and returns a run summary.
- [ ] Orchestration not-found errors return HTTP 404.
- [ ] Documentation page navigable under Execution.

## Whole-feature done criteria (Plans 1-3)

- [ ] `go build ./... && go test ./... -race` green across the module.
- [ ] Cortex supports multi-agent orchestration: five strategies, shared blackboard (communication), participant roster (awareness), and recorded handoffs — all persisted and exposed over HTTP.
- [ ] The previously dormant `OrchestrationStarted`/`OrchestrationCompleted`/`AgentHandoff` plugin hooks are emitted during real runs.
- [ ] Agent cloning remains the one capability from the original question deliberately left out of scope (tracked as a separate follow-up).
