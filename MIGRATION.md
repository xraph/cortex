# Cortex dashboard migration

You can use this file to track the React replacement against the live Go services and the legacy dashboard. Keep the legacy package until every required row below has implementation and live review evidence.

Status: inventory and implementation in progress. Nothing here establishes rollout readiness.

## Checkout audit, 2026-10-08

- Primary Cortex checkout: `main` at `06ea111`, two unpublished commits above fetched `origin/main` at `a5218f4`. No other worktrees.
- `core-module-split`, `feat/a2a-messaging`, `fix/a2a-inreplyto-interop` and `fix/ci-module-versions-and-docs-format` have no patches missing from main (`git cherry`), and their fetched remote tips are ancestors of main. No replay or consolidation is needed.
- Existing local commits disconnected templ auto-discovery before this migration. The source remains available as the parity reference.
- Dashboard checkout is on main with concurrent edits. Cortex owns its new plugin and narrowly additive integration patches. No push.

## Legacy surface inventory

Every source below is retained. Items begin as pending and need the evidence ledger below before retirement. Route dispatch lives in `dashboard/contributor.go`; nav, widgets and settings begin in `dashboard/forge.contributor.yaml`.

| Source | Columns and form fields | Actions, empty states and other text | Status |
| --- | --- | --- | --- |
| `dashboard/pages/agent_detail.templ` | ID, State, Steps, Created, Action | Back; Edit; Delete; View all profiles; View; EmptyState("play", "No runs yet", "Runs for this agent will appear here") | Pending |
| `dashboard/pages/agent_form.templ` | name, model, description, enabled, system_prompt, max_steps, max_tokens, temperature, reasoning_loop, persona_ref, tools | Cancel; Save Changes; Create Agent | Pending |
| `dashboard/pages/agents.templ` | Name, Model, Status, Persona, Actions | New Agent; View; Edit; Delete; EmptyState("bot", "No agents found", "Create your first agent to get started") | Pending |
| `dashboard/pages/behavior_detail.templ` | Type, Pattern, Target, Value | Back; Edit; Delete; EmptyState("zap", "No triggers", "Triggers define when this behavior activates"); EmptyState("zap", "No actions", "Actions define what happens when triggered") | Pending |
| `dashboard/pages/behavior_form.templ` | name, priority, description, requires_skill, requires_trait, triggers, actions, Type, Pattern, Target, Value | Cancel; Save Changes; Create Behavior | Pending |
| `dashboard/pages/behaviors.templ` | Name, Description, Priority, Triggers, Actions | New Behavior; View; Edit; Delete; EmptyState("zap", "No behaviors found", "Create your first behavior to define an action pattern") | Pending |
| `dashboard/pages/chat.templ` |  |  | Pending |
| `dashboard/pages/checkpoint_detail.templ` | Decided By, Reason | Back to Checkpoints; Approve; Reject | Pending |
| `dashboard/pages/checkpoints.templ` | ID, Reason, Run, Step, State, Created, Action | Review; EmptyState("shield-check", "No pending checkpoints", "Checkpoints awaiting review will appear here") | Pending |
| `dashboard/pages/helpers.templ` |  | Previous; Next | Pending |
| `dashboard/pages/knowledge.templ` | Name, Documents, Chunks, Embedding Model, Chunk Strategy | EmptyState("book-open", "No collections found", "Create knowledge collections in Weave to see them here") | Pending |
| `dashboard/pages/knowledge_detail.templ` |  | Back | Pending |
| `dashboard/pages/memory.templ` |  | Clear Memory; User; Assistant; System; Tool; EmptyState("database", "Select an agent", "Choose an agent to view its conversation memory"); EmptyState("database", "No messages", "No conversation history found for this agent") | Pending |
| `dashboard/pages/model_detail.templ` |  | Back; Supported | Pending |
| `dashboard/pages/models.templ` | Model, Provider, Context, Max Output, Input $/M, Output $/M, Capabilities | EmptyState("sparkles", "No models found", "Models from connected LLM providers will appear here") | Pending |
| `dashboard/pages/overview.templ` | Agent, State, Steps, Created, Reason, Run, Step, Action | Review; EmptyState("play", "No runs yet", "Agent runs will appear here"); EmptyState("shield-check", "No pending checkpoints", "All clear") | Pending |
| `dashboard/pages/persona_detail.templ` | Skill, Proficiency, Trait, Overrides, Strategy, Max Steps, Transition | Back; Edit; Delete; Yes; No | Pending |
| `dashboard/pages/persona_form.templ` | name, description, identity, skills, traits, behaviors, cognitive_style, communication_style, Skill Name, Proficiency, Trait Name | Cancel; Save Changes; Create Persona | Pending |
| `dashboard/pages/personas.templ` | Name, Description, Skills, Traits, Actions | New Persona; View; Edit; Delete; EmptyState("user-circle", "No personas found", "Create your first persona to define an agent identity") | Pending |
| `dashboard/pages/playground.templ` | Persona, Skills, Traits, Behaviors | Chat; Run History; Compare; Custom Config | Pending |
| `dashboard/pages/playground_prompt_preview.templ` |  |  | Pending |
| `dashboard/pages/run_detail.templ` | Tool, Arguments, Result, Error | Back to Runs; View Safety Scans; EmptyState("play", "No steps recorded", "Steps will appear as the run progresses") | Pending |
| `dashboard/pages/runs.templ` | Agent, State, Steps, Tokens, Duration, Created, Action | View; EmptyState("play", "No runs found", "Agent runs will appear here when agents are executed") | Pending |
| `dashboard/pages/safety_profiles.templ` | Name, Description, Status, ID | Active; Disabled; EmptyState("shield-check", "No safety profiles", "Create safety profiles in Shield to enable agent guardrails") | Pending |
| `dashboard/pages/safety_scans.templ` | ID, Direction, Decision, Findings, PII, Profile, Duration, Created | EmptyState("scan-line", "No scans found", "Safety scans will appear here as agents process content") | Pending |
| `dashboard/pages/skill_detail.templ` | Tool Name, Mastery, Guidance, Prefer When, Source, Inject Mode, Priority | Back; Edit; Delete; EmptyState("wrench", "No tool bindings", "Tools bound to this skill will appear here") | Pending |
| `dashboard/pages/skill_form.templ` | name, default_proficiency, description, system_prompt_fragment, tools, knowledge, Tool Name, Mastery, Guidance, Prefer When, Source, Inject Mode, Priority | Cancel; Save Changes; Create Skill | Pending |
| `dashboard/pages/skills.templ` | Name, Description, Proficiency, Tools, Actions | New Skill; View; Edit; Delete; EmptyState("wrench", "No skills found", "Create your first skill to define a capability") | Pending |
| `dashboard/pages/tool_detail.templ` | Agent Name, Actions, Skill, Mastery, Guidance, Prefer When | Back; View; EmptyState("bot", "No agent references", "No agents directly reference this tool"); EmptyState("wrench", "No skill bindings", "No skills bind this tool") | Pending |
| `dashboard/pages/tools.templ` | Name, Source, Agents, Skills, Total Calls, Error Rate | EmptyState("terminal", "No tools found", "Tools referenced by agents or skills will appear here") | Pending |
| `dashboard/pages/trait_detail.templ` | Target, Value, Condition, Weight | Back; Edit; Delete; EmptyState("zap", "No influences", "Runtime influences will appear here") | Pending |
| `dashboard/pages/trait_form.templ` | name, category, description, dimensions, influences, Name, Low Label, High Label, Target, Value, Condition, Weight | Cancel; Save Changes; Create Trait | Pending |
| `dashboard/pages/traits.templ` | Name, Description, Category, Dimensions, Actions | New Trait; View; Edit; Delete; EmptyState("brain", "No traits found", "Create your first trait to define a personality dimension") | Pending |
| `dashboard/settings/config.templ` | Default Model, Default Max Steps, Default Max Tokens, Default Temperature, Reasoning Loop |  | Pending |
| `dashboard/widgets/active_checkpoints.templ` | Reason, Step, Action | Review | Pending |
| `dashboard/widgets/knowledge_stats.templ` |  |  | Pending |
| `dashboard/widgets/recent_runs.templ` | Agent, State, Steps, Time |  | Pending |
| `dashboard/widgets/safety_stats.templ` |  |  | Pending |
| `dashboard/widgets/stats.templ` |  |  | Pending |

### Routes, filters, details and integrations

- Overview: seven entity counts, recent runs with agent/state/steps/created, pending checkpoints with reason/run/step/review, plugin widgets. Counts must fail visibly when the store fails.
- Agents/personas/skills/traits/behaviors: search and offset/limit paging; list/detail/create/edit/delete. Details include identity/timestamps, descriptions, prompt text, composition references, nested values and usage. Agent detail also includes recent runs, total/success rate/last run, safety links and plugin sections.
- Runs: agent and state filters, paging, duration; detail includes input/output/error, steps, tool arguments/results/errors, persona, safety scans and plugin contributions. Cancellation must call `Engine.CancelRun`, never update the state directly.
- Checkpoints: pending list with paging, detail and approve/reject with reason. Decider identity comes from the principal, not user input. `Engine.ResolveCheckpoint` owns the suspension claim and continuation.
- Memory: agent picker, default-session history, role/content/timestamp and clear. New UI must identify a real session and confirm destructive clearing.
- Tools: search/paging; configuration references in agents and skills, source label, call count/error rate, detail guidance/mastery/prefer-when and schema. Legacy discovery is reference-derived, not proof of registered execution. `api/tool_handler.go` schema route is a stub.
- Models: search/provider/paging; ID/name/provider/context/max output/input and output prices plus chat/streaming/embeddings/images/vision/tools/JSON/audio/thinking/batch capability flags. Provider health is separate from configured model metadata.
- Knowledge: search, collection identity/name/documents/chunks/embedding model/chunk strategy, detail statistics and overview widget. Use authorized provider adapters; do not repeat the old unscoped Weave store reads.
- Safety: profiles and run-filtered scans, totals/blocked/flagged/allowed/block rate, direction/decision/findings/PII/profile/duration/time, overview widget and run links. Old adapter scans only the latest 100 rows and swallows errors. No complete-result claim without a scoped provider.
- Chat: agent picker/history, send/stream, token/step/tool/checkpoint rendering, stop, clear and approve/reject. Playground adds model/temperature/max steps/max tokens/reasoning/prompt/persona/skills/traits/behaviors/tools overrides, prompt preview, run history, compare panes and plugin panels/tabs. Engine-backed commands must retain session and principal.
- Settings: model/max steps/max tokens/temperature/reasoning loop, shutdown timeout/run concurrency and plugin names. `Engine.UpdateConfig` mutates process memory without synchronization or persistence. Keep runtime configuration read-only until a safe persistent configuration service exists; do not imply settings survive restart.
- Plugin contribution interfaces: widgets/settings/pages/nav, agent/persona/run detail sections, chat toolbar/message actions, playground panel/tab. `sentinel` formerly supplied eval summary/widget/run detail/playground panel. Commit `06ea111` removed that UI implementation; `sentinel/plugin.go` auto-eval remains TODO. New data contributions need explicit contract registration and React slots, not templ execution.

## Domain capabilities beyond templ

- `engine/clone.go`: agent and persona cloning.
- `session/session.go`: scoped threads, stable IDs, default thread, counters maintained with messages, immutable migration provenance.
- `prompt/overlay.go`: ordered scoped prompt patches, additions/removals and optional model/temperature/token overrides. Scope inheritance is host-controlled; do not delegate unrestricted overlays to tenants.
- `engine/resume.go`: externally executed tool results and human replies resume through atomic suspension claims. Never bypass approval reasons with a generic resume action.
- `engine/orchestration*.go`: persisted multi-agent configurations, participants/settings, runs/results and execution.
- `engine/a2a_*.go`: opt-in message bus, conversations/messages/inbox and sends. Availability depends on a constructed bus, not configuration text.
- `audit_hook/extension.go`: real lifecycle hooks require a recorder supplied by the host. Extension initialization does not construct one. Contract command audit also needs a durable sink for production.

## Evidence ledger

| Check | Result | Evidence |
| --- | --- | --- |
| All 734 playbook lines | Read | `forge-dashboard/packages/plugin/PLAYBOOK.md` |
| Baseline core/engine | Passed | `GOWORK=off go test -mod=readonly ./...`, engine and core packages pass |
| Baseline SQLite/PostgreSQL | Passed | Same full invocation, both store suites pass |
| Baseline MongoDB | Blocked | MongoDB 8.3.2 container exits: allocator compatibility with Linux kernel 7.0.14, requires opt-in for 6.19+ |
| Contract configuration foundation | Passed | Extension `go test -race -mod=readonly ./...`: scoped reads/writes, absent/malformed claims, foreign IDs, omitted versus cleared fields, nested SQLite values, restart identity, delete references and audit refusal |
| React/demo | Pending | Implementation not verified yet |
| Browser workflows/restart/live refresh | Pending | No live review yet |
| Legacy retirement | Blocked | Required migration and live evidence outstanding |

## Current implementation

`extension/contract` registers Cortex configuration intents for agents, personas, skills, traits, behaviors and orchestrations. The extension discovers the contributor without a templ dependency. You must supply authenticated `cortex.read`/`cortex.manage` scopes and a durable audit recorder for commands. Names are immutable, IDs and scope are server-owned, and typed pointer patches preserve omissions. Audit records attempts before mutation and outcomes afterwards; an outcome recording failure reports that the command may have run. Audit and domain storage are not one transaction.

Composition references are limited to the exact stored scope. Existing name lookups use descendant matching, so ambiguous parent/child names require special care on execution. External writers are outside the contract mutex and must coordinate reference changes with deletes; store interfaces do not provide name foreign-key constraints.
