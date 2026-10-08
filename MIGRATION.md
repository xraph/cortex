# Cortex dashboard migration

You can use this file to track the React dashboard against the live Go services and the recorded legacy inventory. Cortex now exposes dashboard data through `extension/contract`; the Go-rendered dashboard has been removed.

Status: React contracts, plugin and persistent demo implemented and locally reviewed. Legacy dashboard source retired on 2026-10-08. External production integration access remains unverified.

## Initial checkout audit, 2026-10-08

- Primary Cortex checkout: `main` at `06ea111`, two unpublished commits above fetched `origin/main` at `a5218f4`. No other worktrees.
- `core-module-split`, `feat/a2a-messaging`, `fix/a2a-inreplyto-interop` and `fix/ci-module-versions-and-docs-format` have no patches missing from main (`git cherry`), and their fetched remote tips are ancestors of main. No replay or consolidation is needed.
- Existing local commits disconnected templ auto-discovery before this migration. The source remained available then as the parity reference.
- At the initial audit, the dashboard checkout was on main with concurrent edits. Cortex owned its new plugin and narrowly additive integration patches. Migration commits were subsequently pushed to main.

## Legacy surface inventory

The source paths below are a historical inventory of the retired dashboard. The status column records each replacement and its review level. Legacy route dispatch lived in `dashboard/contributor.go`; nav, widgets and settings lived in `dashboard/forge.contributor.yaml`. Git history retains the complete source.

| Source | Columns and form fields | Actions, empty states and other text | Status |
| --- | --- | --- | --- |
| `dashboard/pages/agent_detail.templ` | ID, State, Steps, Created, Action | Back; Edit; Delete; View all profiles; View; EmptyState("play", "No runs yet", "Runs for this agent will appear here") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/agent_form.templ` | name, model, description, enabled, system_prompt, max_steps, max_tokens, temperature, reasoning_loop, persona_ref, tools | Cancel; Save Changes; Create Agent | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/agents.templ` | Name, Model, Status, Persona, Actions | New Agent; View; Edit; Delete; EmptyState("bot", "No agents found", "Create your first agent to get started") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/behavior_detail.templ` | Type, Pattern, Target, Value | Back; Edit; Delete; EmptyState("zap", "No triggers", "Triggers define when this behavior activates"); EmptyState("zap", "No actions", "Actions define what happens when triggered") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/behavior_form.templ` | name, priority, description, requires_skill, requires_trait, triggers, actions, Type, Pattern, Target, Value | Cancel; Save Changes; Create Behavior | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/behaviors.templ` | Name, Description, Priority, Triggers, Actions | New Behavior; View; Edit; Delete; EmptyState("zap", "No behaviors found", "Create your first behavior to define an action pattern") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/chat.templ` |  |  | Real stream, sessions, overrides and preview; browser run/approval/compare reviewed |
| `dashboard/pages/checkpoint_detail.templ` | Decided By, Reason | Back to Checkpoints; Approve; Reject | List/detail/decision; HTTP approve/reject and browser approve/resume pass |
| `dashboard/pages/checkpoints.templ` | ID, Reason, Run, Step, State, Created, Action | Review; EmptyState("shield-check", "No pending checkpoints", "Checkpoints awaiting review will appear here") | List/detail/decision; HTTP approve/reject and browser approve/resume pass |
| `dashboard/pages/helpers.templ` |  | Previous; Next | Shared tables, filters, paging and ZeroState; plugin tests pass |
| `dashboard/pages/knowledge.templ` | Name, Documents, Chunks, Embedding Model, Chunk Strategy | EmptyState("book-open", "No collections found", "Create knowledge collections in Weave to see them here") | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/knowledge_detail.templ` |  | Back | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/memory.templ` |  | Clear Memory; User; Assistant; System; Tool; EmptyState("database", "Select an agent", "Choose an agent to view its conversation memory"); EmptyState("database", "No messages", "No conversation history found for this agent") | Real sessions and memory; HTTP read/clear and restart persistence pass |
| `dashboard/pages/model_detail.templ` |  | Back; Supported | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/models.templ` | Model, Provider, Context, Max Output, Input $/M, Output $/M, Capabilities | EmptyState("sparkles", "No models found", "Models from connected LLM providers will appear here") | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/overview.templ` | Agent, State, Steps, Created, Reason, Run, Step, Action | Review; EmptyState("play", "No runs yet", "Agent runs will appear here"); EmptyState("shield-check", "No pending checkpoints", "All clear") | Real counts, pending/recent rows and slots; browser reviewed |
| `dashboard/pages/persona_detail.templ` | Skill, Proficiency, Trait, Overrides, Strategy, Max Steps, Transition | Back; Edit; Delete; Yes; No | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/persona_form.templ` | name, description, identity, skills, traits, behaviors, cognitive_style, communication_style, Skill Name, Proficiency, Trait Name | Cancel; Save Changes; Create Persona | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/personas.templ` | Name, Description, Skills, Traits, Actions | New Persona; View; Edit; Delete; EmptyState("user-circle", "No personas found", "Create your first persona to define an agent identity") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/playground.templ` | Persona, Skills, Traits, Behaviors | Chat; Run History; Compare; Custom Config | Real stream, sessions, overrides and preview; browser run/approval/compare reviewed |
| `dashboard/pages/playground_prompt_preview.templ` |  |  | Real stream, sessions, overrides and preview; browser run/approval/compare reviewed |
| `dashboard/pages/run_detail.templ` | Tool, Arguments, Result, Error | Back to Runs; View Safety Scans; EmptyState("play", "No steps recorded", "Steps will appear as the run progresses") | Persisted runs/steps/tools and cancellation; HTTP and browser pass |
| `dashboard/pages/runs.templ` | Agent, State, Steps, Tokens, Duration, Created, Action | View; EmptyState("play", "No runs found", "Agent runs will appear here when agents are executed") | Persisted runs/steps/tools and cancellation; HTTP and browser pass |
| `dashboard/pages/safety_profiles.templ` | Name, Description, Status, ID | Active; Disabled; EmptyState("shield-check", "No safety profiles", "Create safety profiles in Shield to enable agent guardrails") | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/safety_scans.templ` | ID, Direction, Decision, Findings, PII, Profile, Duration, Created | EmptyState("scan-line", "No scans found", "Safety scans will appear here as agents process content") | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/pages/skill_detail.templ` | Tool Name, Mastery, Guidance, Prefer When, Source, Inject Mode, Priority | Back; Edit; Delete; EmptyState("wrench", "No tool bindings", "Tools bound to this skill will appear here") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/skill_form.templ` | name, default_proficiency, description, system_prompt_fragment, tools, knowledge, Tool Name, Mastery, Guidance, Prefer When, Source, Inject Mode, Priority | Cancel; Save Changes; Create Skill | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/skills.templ` | Name, Description, Proficiency, Tools, Actions | New Skill; View; Edit; Delete; EmptyState("wrench", "No skills found", "Create your first skill to define a capability") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/tool_detail.templ` | Agent Name, Actions, Skill, Mastery, Guidance, Prefer When | Back; View; EmptyState("bot", "No agent references", "No agents directly reference this tool"); EmptyState("wrench", "No skill bindings", "No skills bind this tool") | Authorized runtime definitions, schema, references and usage; HTTP/tests pass |
| `dashboard/pages/tools.templ` | Name, Source, Agents, Skills, Total Calls, Error Rate | EmptyState("terminal", "No tools found", "Tools referenced by agents or skills will appear here") | Authorized runtime definitions, schema, references and usage; HTTP/tests pass |
| `dashboard/pages/trait_detail.templ` | Target, Value, Condition, Weight | Back; Edit; Delete; EmptyState("zap", "No influences", "Runtime influences will appear here") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/trait_form.templ` | name, category, description, dimensions, influences, Name, Low Label, High Label, Target, Value, Condition, Weight | Cancel; Save Changes; Create Trait | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/pages/traits.templ` | Name, Description, Category, Dimensions, Actions | New Trait; View; Edit; Delete; EmptyState("brain", "No traits found", "Create your first trait to define a personality dimension") | Structured list/detail/editor/delete replacement; nested HTTP round trips and form review pass |
| `dashboard/settings/config.templ` | Default Model, Default Max Steps, Default Max Tokens, Default Temperature, Reasoning Loop |  | Actual runtime values, read-only; browser reviewed; unsafe Save removed |
| `dashboard/widgets/active_checkpoints.templ` | Reason, Step, Action | Review | Overview summaries and React slots; local data reviewed; external widgets need provider |
| `dashboard/widgets/knowledge_stats.templ` |  |  | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/widgets/recent_runs.templ` | Agent, State, Steps, Time |  | Overview summaries and React slots; local data reviewed; external widgets need provider |
| `dashboard/widgets/safety_stats.templ` |  |  | Catalog contract and React views; adapter tests pass; external live access unverified |
| `dashboard/widgets/stats.templ` |  |  | Overview summaries and React slots; local data reviewed; external widgets need provider |

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

## Initial migration evidence

| Check | Result | Evidence |
| --- | --- | --- |
| All 734 playbook lines | Read | `forge-dashboard/packages/plugin/PLAYBOOK.md` |
| Baseline core/engine | Passed | `GOWORK=off go test -mod=readonly ./...`, engine and core packages pass |
| Baseline SQLite/PostgreSQL | Passed | Same full invocation, both store suites pass |
| Baseline MongoDB | Blocked | MongoDB 8.3.2 container exits: allocator compatibility with Linux kernel 7.0.14, requires opt-in for 6.19+ |
| Contract configuration foundation | Passed | Extension `go test -race -mod=readonly ./...`: scoped reads/writes, absent/malformed claims, foreign IDs, omitted versus cleared fields, nested SQLite values, restart identity, delete references and audit refusal |
| Contract runtime operations | Passed | Extension race suite: live engine tokens, session ownership/history/clear, foreign IDs, host overlay permission, checkpoint principal/repeat decision, audit outcome failure and no-LLM refusal |
| Catalog adapters | Passed locally | Real SQLite tests traverse 125 records, exclude foreign IDs, count before paging and surface failures; Nexus metadata preserves decimal strings |
| Cancellation regression | Passed | Root engine race suite (70.675s) and extension race suite (39.175s); provider error cannot overwrite an explicit cancellation |
| React plugin | Passed | `pnpm typecheck`, `pnpm lint`, `pnpm test`: 7 tests; shared plugin: 237 tests |
| Persistent demo | Passed | `GOWORK=off go test -mod=readonly ./...` in dashboard demo (0.911s); real SQLite close/reopen preserves IDs, nested persona and run result |
| Live HTTP workflows | Passed | `python3 demo/verify-cortex.py`: six configuration CRUD round trips, omission/clearing, references, stream/failure/cancel, approve/reject/repeat refusal, memory, overlays, orchestration and saved message principal/recipient |
| Live permission/scope checks | Passed | Reader can read and cannot manage; denied cannot read; isolated tenant cannot read northwind agent/run IDs |
| Browser review | Passed for named flows | All navigation visited; six saved editors reviewed; agent edit/save, streaming chat, approval/resume, prompt preview and independent comparison sessions exercised against Go |
| Responsive review | Passed for named pages | Agent detail, new agent form and playground at desktop and 390px; document width remains 390px; nested form controls retain labels and wrapping |
| Shell bundle | Passed | Vite production bundle succeeds; shared main chunk 1546.36 kB, gzip 403.96 kB; Cortex editor 1.42 kB, gzip 0.80 kB, with CodeMirror in a shared lazy chunk. Shared bundle exceeds Vite's 500 kB warning threshold |
| Full shell build | Blocked by shared source | `DashboardPreview.tsx`: unsupported AppSidebar `scopes` prop and implicit-any `id`; log `/tmp/cortex-dashboard-shell-build-final.log` |
| Full workspace tests | Failed outside Cortex | `pnpm -r --no-bail test`: host setup 8 failures remain; 24 projects pass, including Cortex and Shield. Log `/tmp/cortex-dashboard-workspace-final-tests.log` |
| Sentinel module | Compiles | `GOWORK=off go test -mod=readonly ./...`, both packages have no tests |
| External live catalogs | Unverified | No production Nexus, Weave or Shield credentials/grants configured in this demo; adapter tests do not establish remote access |
| Legacy retirement | Source removed | Removed `dashboard/`, orphaned `sentinel/pages`, local dashboard dependencies, templ Make targets and dashboard CI/release entries. Post-removal verification is recorded below |

## Current implementation

`extension/contract` registers Cortex configuration intents for agents, personas, skills, traits, behaviors and orchestrations. The extension discovers the contributor without a templ dependency. You must supply authenticated `cortex.read`/`cortex.manage` scopes and a durable audit recorder for commands. Names are immutable, IDs and scope are server-owned, and typed pointer patches preserve omissions. Audit records attempts before mutation and outcomes afterwards; an outcome recording failure reports that the command may have run. Audit and domain storage are not one transaction.

Composition references are limited to the exact stored scope. Existing name lookups use descendant matching, so ambiguous parent/child names require special care on execution. External writers are outside the contract mutex and must coordinate reference changes with deletes; store interfaces do not provide name foreign-key constraints.

Runtime contracts now expose scoped run review, cancellation, checkpoint decisions, sessions and memory, clones, orchestration execution/history, A2A inspection/send/inbox and host-only overlays. Chat execution uses a bounded live cursor feed from `Engine.StreamAgent`; the feed holds up to 1024 events per run for ten minutes after completion, with a maximum of 64 retained runs and a five-minute execution timeout. A missing or truncated feed is reported explicitly; persisted run, step and message records remain the durable review surface. External catalog adapters require an explicit access callback and nonempty authorized app/tenant scope. Nexus, Weave and Shield adapters are implemented and tested; the local demo labels its simulated model catalog and reports unavailable knowledge/safety integrations.

## Local review record

You can open the running review at `http://127.0.0.1:5176/@cortex`. The Go demo runs on 8096 with SQLite at `/tmp/cortex-dashboard-review-v3/cortex.db`. Its completion provider is deterministic and local. It exercises the engine, tool authorizer, checkpoint continuation, message bus and persisted state without a remote LLM.

The saved `demo-reviewer` edit survived a Go server restart. Browser runs `arun_01m4e9xm1cerxr9rbhb965259r` and `arun_01m4e9z1n6ejtvc9bpg4kdfet0` cover streaming and approved lookup continuation. Comparison runs `arun_01m4eaxhjxe0b8vyagyzs446hw` and `arun_01m4eay3q8e7hbrbnka8z5a6sy` use separate saved sessions. Completed stream transcripts collapse beside saved conversation history. Secondary actions use shared icon buttons with tooltips and accessible labels; keyboard focus displays the edit tooltip. Save, Send and confirmation decisions retain text.

Screenshots are saved in `/Users/rexraphael/.codex/visualizations/2026/10/08/01a11c7a-218f-7641-a3be-f83bbb062d7a/`: `cortex-playground-desktop.jpg`, `cortex-playground-narrow.jpg`, `cortex-agent-form-narrow.jpg`, `cortex-icons-desktop.jpg` and `cortex-icons-narrow.jpg`. These establish the named pages at the reviewed sizes, not every route or operating system.

Cortex commits: `64463bd` configuration contracts, `40a7ecc` execution contracts, `68435a5` scoped catalogs and diagnostics, `dd9b4b3` cancellation fix, `1c0f8ac` complete catalog summaries. Dashboard commits: `e6885d9` React workspace, `061be99` review actions/summaries and `25d5e13` persistent demo, `468e670` compact comparison and `319bdf1` contextual icons/tooltips.

## Legacy retirement, 2026-10-08

The dedicated retirement change removes 130 files (48,116 lines): the entire `dashboard/` module and the eight orphaned Sentinel page/generated files. The extension and Sentinel module no longer require or replace `github.com/xraph/cortex/dashboard`. Module tidying removes Sentinel's direct templ/ForgeUI dependencies and unused transitive dependencies. The extension still receives templ and ForgeUI indirectly through the published Forge auth package; no Cortex source imports either UI library. CI, release tagging, release installation instructions and `scripts/pin-submodules.sh` no longer name the deleted module. `make all` no longer generates templ, and developer setup no longer installs or checks the templ CLI.

The Cortex source and sibling Forge/Forgery checkouts have no remaining Go imports of the removed packages. The React dashboard and its `extension/contract` APIs remain the supported path. Removing old source does not establish production Nexus, Weave or Shield access; those credentials and grants remain deployment qualification work.

The Sentinel UI hook implementation was disconnected in `06ea111`. Its remaining templates had no callers or working evaluation command. They are deliberately retired, with the following historical inventory retained. Sentinel lifecycle hooks remain available, and automatic evaluation remains explicitly unimplemented.

| Retired Sentinel source | Historical surface | Disposition |
| --- | --- | --- |
| `sentinel/pages/eval_summary.templ` | Agent suite, pass rate, average score, run count, dimension scores and evaluation links; no-evaluation explanation | Dropped orphaned UI; no registered data contributor supplied these values |
| `sentinel/pages/eval_widget.templ` | Total evaluations, average pass rate, recent failures and suite count | Dropped orphaned widget; no registered data contributor supplied these counts |
| `sentinel/pages/run_eval.templ` | Run pass rate, average score, passed/failed/total cases, case name/status/score and evaluation link; Run Evaluation button | Dropped orphaned UI and unwired action; no evaluation command was implemented |
| `sentinel/pages/playground_panel.templ` | Suite selector, Run Evaluation button and result totals | Dropped orphaned UI and unwired action; no evaluation command was implemented |

### Post-removal verification

| Check | Result |
| --- | --- |
| `make f`, followed by `make l` | Passed; zero lint issues across all eight remaining Go modules, including integration-tagged tests |
| Root `go build ./...` and `CORTEX_TEST_MONGO_IMAGE=mongo:7 go test -count=1 ./...` | Passed; MongoDB 7 replica-set, PostgreSQL and SQLite suites executed. The pinned MongoDB 8.3.2 image remains incompatible with this Docker Desktop kernel |
| Build and full tests in A2A remote, gRPC binding, API, extension, Fabriq integration and Sentinel modules | Passed; Sentinel has no test files |
| `inttest`: `go test -tags=integration -count=1 -timeout 10m ./...` | Passed; live PostgreSQL Fabriq learning-loop test executed |
| Persistent dashboard demo build and full tests | Passed against the post-removal Cortex checkout |
| Live HTTP workflow script | Passed against a freshly compiled demo on port 8107 with a separate SQLite database: configuration CRUD/nested patches, references, streams, failure, approval/rejection, cancellation, memory, overlays, orchestration and messaging |
| Cortex React plugin lint, typecheck and tests | Passed; seven tests |
| Shell production build | Passed, including TypeScript; the shared main chunk remains above Vite's 500 kB warning threshold |
| Full dashboard workspace tests | Passed; 5,893 tests across 26 packages/apps. The earlier shared setup-test failures are resolved |
| Browser review | Existing React chat and agent navigation render; agent list reviewed at desktop and 390px, with document width remaining 390px |
| Release wiring | Shell syntax check and isolated release-pin probe passed across all four published submodules; no deleted module references remain |
| Source retirement audit | No `dashboard/`, `sentinel/pages`, `.templ`, generated `_templ.go` files or imports/requires of `github.com/xraph/cortex/dashboard` remain |

Earlier ledger rows describe the original migration review and retain their original limits. External production credentials/grants and Sentinel automatic evaluation remain outside the verified local cutover.
