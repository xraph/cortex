# Cortex assured execution progress

Plan: `2026-10-18-cortex-assured-execution.md`. Implementation baseline: `cd4c746`. Execution is inline in the primary checkout on `main`.

## Ownership and contracts

This chat owns the runtime integrator, persistence, contract and progress roles for the active packet. The optimization chat owns Sentinel recorded evaluation and cortex/optimization. A fresh reviewer inspected S1.1 without editing files. Other implementation packets remain unassigned. Inspect status before each packet; the optimization workstream shares prepared execution and evaluation contracts and must consume these foundation commits.

Baseline inventory: Cortex `cd4c746` is clean; Sentinel `efbad75` has an untracked `_project_files/`; Fabriq `/Users/rexraphael/Work/TwinOS/fabriq` at `5f58c83` has an untracked server binary; forge-dashboard `edfac7f` has unrelated untracked fixtures/output. Those files remain untouched. Docker server 29.8.2 is available.

| Packet | State | Owner | Evidence |
| --- | --- | --- | --- |
| F0 | active | this chat | Architecture read; shared seams and current checkout inventoried; compatibility tests are introduced with S1.1 |
| S1.1 | accepted | this chat | Regression, race, backend, compatibility, lint/build and source review gates passed; focused commit carries this ledger |
| S1.2 | accepted | this chat | Frozen execution, derivation, cumulative budgets, restart and compatibility fences pass focused/race/backend checks and source review |
| S1.3 | accepted | this chat | Publication boundary regressions, race tests, backend round trips, nested API/A2A checks, lint/build and fresh source review passed |
| S1.4 | implemented | this chat | Runtime, backend, API mapping, dashboard contract and A2A checks pass; installed application scope qualification stays in G1 |
| S1.5 | implemented | this chat | Strict delegation, bounded repair, cancellation, failure receipts, race, all-module lint and root build pass |
| S1.6 | active | this chat | Public outcomes, capabilities, audit/metrics and dashboard integration implemented; local installed checks pass; live scanner/provider qualification remains open |
| Stages 2 through 4 | queued | unassigned | No implementation claimed |

## Interface rulings

- S1.1 -> S1.2/S1.6: retain existing completed meaning and add incomplete/stop reason plus unchecked verification metadata. Never infer verified success from a legacy completed row.
- S1.2 -> S2.2/optimization task 1: one prepared execution/provenance contract; no optimizer-specific ReAct copy.
- S1.3 -> S2.6: publication policy owns public content; durable replay must consume only approved events.
- S1.4 -> every store adapter: reject unsupported scopes consistently without changing valid ordered-prefix semantics.
- S1.5 -> S3.1: strict delegation validation is a compatibility repair, later consumed by the typed plan adapter.
- Native skill scripts expect `Task N` headings; these plans use stable `S1.N` packet headings. Extract packet briefs directly into this plan's scratch workspace and preserve the same baseline/test ledger format. No plan renumbering or skill edits are needed.
- User instructions require primary `main`, commit and push. Those explicit instructions override the skill's worktree and repeated push-approval defaults.

## Verification log

S1.1 implements explicit incomplete outcomes and stop reasons across synchronous, streaming, mock and resume paths. Final answers remain unchecked. Terminal storage failure prevents completion hooks and done events. Truncated tool calls never dispatch; unknown finish reasons, malformed responses and streams missing their finish marker stay incomplete. The orchestration adapter rejects incomplete workers, A2A projects them as terminal failures, and the sweeper cleans their orphaned suspension rows.

Checks completed:

- Behavioral regressions failed before the changes, including step exhaustion, truncated output, missing assurance metadata and swallowed terminal-write failures. Additional red cases covered premature stream EOF, malformed responses, mock persistence, resume limits, orchestration propagation and orphan cleanup.
- Engine/run race suites passed on the final code. The full SQLite suite passed both normally and with race detection. Its first race run exceeded three minutes while executing conformance; the serial rerun with a ten-minute per-package timeout passed.
- PostgreSQL and Mongo conformance passed with race detection against Docker. Mongo used `CORTEX_TEST_MONGO_IMAGE=mongo:7` for the documented local kernel incompatibility.
- SQLite and PostgreSQL old-schema upgrade/replay fixtures preserve completed rows without inventing verification. Mongo historical BSON decoding and future JSON enum compatibility pass. The PostgreSQL fixture initially lacked its parent agent; it was corrected and rerun successfully.
- `GOWORK=off make l`, `GOWORK=off make f`, final `GOWORK=off make l`, root build and diff checks passed. A2A remote and gRPC binding race suites passed.
- Fresh source review found premature EOF and incomplete cleanup gaps; both were fixed and reviewed again. No unresolved S1.1 findings remain.

Limits: this is the outcome foundation, not the Stage 1 gate. Public output safety, scope validation, strict delegation plans and dashboard status integration remain queued. Cumulative budgets are implemented in S1.2. Terminal writes and hooks are ordered but do not provide distributed exactly-once delivery; atomic claims and recovery belong to the later coordination work. F0 schemas remain proposed until their owning packet compiles its fixtures.

## Additional interface decisions

- `verification.Status` lives in a lightweight root package; missing values normalize to `unchecked`, and unknown values survive round trips without becoming passed.
- `run.Run` persists `stop_reason`, raw `finish_reason`, `verification_status` and `result_ref` in every backend. Mongo needs no DDL for additive fields. SQL migrations use version `20261018000001` and are safe to replay after partial application.
- `invalid_response`, `provider_unknown` and `execution_error` supplement the planned stop reasons so malformed provider output and generic failures do not masquerade as final answers.
- Synchronous legacy providers may omit a finish reason when they return content. Streaming providers must supply their documented final marker; transport EOF alone proves no completed answer.
- `incomplete` is a terminal stream event. `done` retains execution-completed semantics and carries unchecked verification. A2A uses its existing failed state because its protocol has no incomplete state.
- S1.2 owns the shared prepared-execution API and provenance in `run`; the optimizer consumes that implementation. Sentinel's generic frozen/recorded evaluation remains owned by the optimization chat.


## Prepared execution contract

S1.2 supplies `Engine.PrepareAgent`, `RunPrepared` and `StreamPrepared`. Ordinary model-backed runs use the same preparation and execution path. You can inspect a snapshot through `PreparedAgent.Inspect()` and derive a candidate with `PreparedAgent.Derive(PreparedEdits)`, without reading current agent, skill or persona records again. Inspection returns detached data. Derivation preserves the agent, scope, model, budgets, tool schemas and implementation pins, accepts only existing unlocked section bodies and tool guidance/subsets, and records the baseline digest in `Provenance.DerivedFrom`.

`PreparedRunOptions` carries session ID, execution purpose, provenance and acceptance reference. The engine accepts only parent run, experiment, candidate, environment receipt, trial and harness identity from caller provenance; it derives snapshot and source identities itself. `ExecutionProvenanceFromContext` gives adapters a detached copy. These are host APIs. An optimizer still has to enforce its own edit policy and narrower sampling bounds before deriving a candidate.

The effective `run.Budget` and cumulative `run.Usage` survive suspension and database restart. Resume compares both against the continuation and checks the frozen prompt, configuration and provenance. A caller deadline can narrow the prepared deadline; it cannot disappear on resume. Each resumed tool gets that deadline and a current authorization check immediately before dispatch. Only a claimed approval checkpoint can satisfy a fresh request for approval.

Token accounting has two values: provider-reported usage and the charge used for dispatch limits. Missing usage consumes the remaining configured token allowance and sets `Estimated`. A failed provider attempt is still an attempt. The total limit prevents further dispatch and narrows the output cap, but provider input usage may exceed the remaining allowance in one request. No hard billing ceiling is claimed. Optional zero limits remain disabled for existing callers; the step limit remains finite.

SQL migration `20261018000002` adds `provenance`, `usage` and `budget`. Mongo persists the same fields without DDL. Prepared continuation bytes are base64 encoded so JSONB key ordering cannot change their digest. New prepared pauses use the storage state `paused_prepared`, which current adapters expose as ordinary `paused`. Previous workers only claim literal `paused` and cannot resume or expire these runs. Keep an upgraded sweeper active during mixed-version rollout. An old-only rollback cannot service prepared pauses, and its fixed expiry batch may repeatedly encounter them ahead of legacy work. Drain or reconcile prepared runs before retiring upgraded workers; do not rewrite their state to bypass the fence.

Validation includes red regressions for mutable snapshot copies, continuation downgrade, counter rollback, purpose changes, removed caller deadlines, stale authorization within a resumed batch, external resumes impersonating approval, failed-provider accounting and missing knowledge diagnostics. The full engine/execution/suspension/run suites pass on the final code. Engine and SQLite full race suites passed; the final effective-budget changes also passed focused resume, approval, prepared execution, budget and migration race suites. SQLite was slow under concurrent host load and completed in 447 seconds without a timeout. PostgreSQL and Mongo conformance ran against Docker, then both passed the final budget round-trip checks. Mongo used the documented `mongo:7` override. SQLite and PostgreSQL old-schema fixtures replay both additive migrations without inventing identity or accounting for historical rows.

Formatting, all-module lint, root build and diff checks passed. Final lint also covers the expanded migration fixtures and exact JSON-number preservation in tool schemas. The fresh source reviewer reports no remaining material S1.2 findings. Nexus has no adapter tests in this checkout; the provider-boundary fixtures here use controlled clients, and live provider qualification remains a Stage 1 integration gate. No live-provider or sandbox claim is made by this packet.

## Publication safety contract

S1.3 is implemented and its packet checks pass. `WithPublicationPolicy` selects a versioned host policy, and `WithPublicationGate` installs a checker. Existing `WithSafety` scanners use the same boundary through `safety.ScannerGate`. You get buffered output by default. A checker must explicitly implement `IncrementalGate` to use `checked_segments`; its decisions approve prefixes permanently, so this mode cannot enforce a rule that needs the complete document. Later rejection never retracts an earlier event. `disabled_explicit` skips scanning and reports `unscanned`. A host that requires scanning rejects a disabled snapshot or a missing gate.

The defaults bound input, model output and each tool result to 1 MiB. Stream accounting includes chunk and tool-call structure, including empty deltas. Both held text and approved output remain bounded. Oversized content stops publication. A configured checker error, nil scanner result, unsupported decision, invalid segment boundary or missing required checker stops publication too. Empty redactions are valid. A redaction of effect arguments rejects the call instead of quietly changing the command.

The engine checks input before its first run write and before a started hook. Retrieved knowledge is checked before freezing, then checked again under the execution host's policy without reloading sources. If the current policy would redact frozen knowledge, the run is refused: you need a new snapshot and evaluation. Tool results pass input and output checks before storage, history or completed hooks. Resumed results, direct Dispatch, mock execution, provider errors and finish metadata use the same helpers. Current retrieval flags are merged into run provenance without changing the frozen digest. `PublicationStatus` describes screening only; it does not mean the result is factually verified.

Each stream event observes cancellation, including the first and terminal events. Buffered candidate text stays private until a check succeeds. Ordinary steps and conversation history receive approved text only. There is no durable raw-evidence repository in this packet; Stage 2 owns that restricted storage contract. Host implementations of scanners, providers, stores and hooks remain trusted code and must honor context cancellation and protect their own logs.

### Snapshot and rollout compatibility

New prepared snapshots and continuations use schema 2 with publication policy version 1. Schema-1 prepared artifacts and paused runs are rejected explicitly; the runtime does not infer a missing safety policy. This applies to stored harness snapshots as well as suspensions. Version 1.8.0 workers reject schema-2 continuations before claiming them, and older workers still cannot claim their `paused_prepared` storage state.

Drain or cancel schema-1 pauses on their original compatible workers before upgrading. For a stored schema-1 harness, keep the original artifact immutable. Call `PrepareAgent` against the intended, reviewed source configuration, save the result as a new harness version and run fresh evaluation before acceptance. This creates a new digest and does not carry an acceptance reference forward. It does not promise to recreate historical sources that are no longer available. Do not patch a schema number, rewrite a paused row or transfer an old acceptance result to bypass this check.

Historical schema-0 continuations remain a compatibility path under the current host policy. They have no frozen publication guarantee. Their re-suspensions retain the legacy shape, so keep them on upgraded workers during rollout if they require current publication enforcement.

### Scanner qualification

The installed Shield v1.7.1 engine has placeholder detection layers and returns `allow` without an applied profile receipt. Its current API also cannot select the requested per-run profile. The Cortex adapter now reports these cases as policy unavailable. Adapter tests exercise the installed dependency, cancellation and result conversion; they do not establish effective Shield detection. You can provide a qualified host scanner or gate in the meantime. Effective Shield scanning and live provider qualification remain open integration requirements.

### Source stability follow-up

The optimization source review found that v1.8.0 freezes collected values without verifying that the agent, persona, skills, traits and overlays stayed stable throughout collection. That foundation does not yet satisfy reproducibility-required optimization. The runtime follow-up now records and rechecks the exact agent, persona, skill, trait and overlay records consumed during preparation, including absent overlays. It rejects an observed change or failed revalidation with `ErrPreparationConflict`, returns no snapshot and does not retry implicitly. New provenance records `SourceValidation=read_set_checked` and content digests in `SourceReads`. These checks do not provide a transaction across sources or detect every change-and-revert sequence. The knowledge provider has no revision API, so only its returned content is frozen. Atomic source consistency remains unavailable. Optimization must keep that capability unavailable unless a host atomic snapshot provider supplies the required guarantee. Trial execution must not rebuild a snapshot from current sources. Provider request temperatures are copied per step so provider-side normalization cannot alter later frozen settings. Deterministic regressions first reproduced all seven source-change cases and temperature mutation in both sync and stream execution.

### Publication validation evidence

The regressions were observed failing before their fixes. Final engine, safety, run and suspension tests pass, as do full engine race tests and the final focused race cases for provider-buffer isolation and cancellation before dispatch. PostgreSQL and Mongo ran real Docker round trips alongside SQLite, preserving publication policy and provenance. API and A2A remote race suites pass. After final formatting, all nine modules report zero lint issues, the root builds, and the affected package suites pass again. Fresh source review found no remaining material issues. These checks qualify the runtime boundary; effective Shield detection, live provider behavior and the installed dashboard remain part of the later integration gate.

Source follow-up validation: full engine/run/suspension race suites passed; final formatting, all-module lint, affected tests and root build passed. The prepared provenance fixture passed on SQLite and real Docker PostgreSQL/Mongo without skips. The change is additive within schema 2; old schema-2 artifacts have no source-validation receipt and must not inherit one. Ruling: read-set validation is useful conflict detection, but atomic-source capability stays unavailable until a transactional or host-qualified snapshot provider exists.

## Scope boundary contract

S1.4 centralizes identity validation in `ValidateScope` and `RequireScope`. Invalid scope attachment masks prior authority, context levels are copied, and every built-in store validates its direct context before querying. Duplicate keys, whitespace-only levels, reserved separators and depth beyond three are rejected. Valid prefix listings remain available; writes and agent execution require the exact resource scope. The dashboard scope adapter uses the shared validator, REST maps invalid scope to denial, and A2A rejects invalid resolver identity before reaching the gateway. Public discovery requires an explicit valid host scope.

Stored entity conversion checks canonical/indexed agreement and overflow. Exact writes fence unsafe legacy rows. Suspension decoding now precedes the committed claim transition, preventing an invalid row from stranding a run. `store.ScopeInspector` provides an optional administrative, read-only inventory for every backend, including pending asks. It crosses scopes intentionally and is not mounted as a public route. [Migration guidance](../guides/cortex-scope-migration.md) covers authorization, dry-run mappings, relational consistency, audits, rollback and qualification.

Ruling: ancestor listing remains compatible, but ancestor mutation and execution are intentionally refused. Tests that previously treated a broader context as update authority now assert refusal followed by a successful exact-scope update. An operator must authorize any migration; this implementation does not rewrite historical identity. Old historical BSON fixtures now include the indexed scope columns that real scoped rows contain, while still omitting assurance fields.

S1.4 validation: red regressions reproduced parent-authority fallback, mutable context identity, ancestor updates/deletes/execution, invalid stored rows, unsafe suspension claims and invalid remote identity. Full root/engine and all three backend race suites passed after the fixes, including conformance and rescope. PostgreSQL and Mongo used real Docker instances with no skips; SQLite completed its full race suite in 227 seconds. API, Forge extension/dashboard contract and A2A remote race suites passed. Final formatting, all-nine-module lint, root build, affected tests and diff checks passed. REST evidence here covers error mapping; full installed two-principal HTTP/dashboard workflows remain on S1.6's G1 matrix. No production data was migrated.


## Delegation plan contract

S1.5 requires a complete JSON array before hierarchical workers start. Every non-manager participant is required and receives exactly one nonempty task. Unknown workers, duplicate assignments or JSON keys, extra fields, partial plans, prose wrappers, trailing content and invalid encoding are rejected with `ErrInvalidDelegationPlan` and a typed reason. Plans are limited to 256 KiB, tasks to 64 KiB and teams to 64 workers. Existing configurations that depended on broadcast fallback or omitted workers must produce explicit complete plans.

`Settings.PlanRepairAttempts` defaults to zero and accepts at most one additional manager invocation. The repair receives bounded validation diagnostics as JSON data. Rejected plan text does not enter worker context. Each planning, repair, worker and synthesis invocation keeps its run ID, including failed engine runs, so orchestration persistence can account for the work. Normal runner budgets apply to each call; an aggregate cross-agent budget remains Stage 3 work. A budget error stops repair without dispatching workers.

Workers use a bounded concurrency slot and recheck cancellation before dispatch. Nil results, embedded errors, empty outputs and mismatched identities fail the orchestration. Required worker failure prevents synthesis and leaves the public orchestration output empty while retaining partial run records and handoffs. A failed or missing manager result is also an error. No validation rule converts partial work into success.

Validation: behavioral red tests reproduced malformed-plan broadcast, silently dropped assignments, worker failure concealment and lost incomplete-worker run IDs. Full root/engine/orchestration race suites passed; the final adapter receipt change also passed focused engine and orchestration race checks. Final formatting, all-nine-module lint, root build and diff checks passed. Installed orchestration and UI qualification remain S1.6 work.


## Outcome integration and qualification

S1.6 adds outcome fields to synchronous REST responses, preserves legacy unchecked status on saved-run projections, and carries `cortex.assurance` through A2A task, status-update and gRPC projections. REST maps publication outage, rejection and size limits to explicit 503, 403 and 413 responses. `Engine.RuntimeCapabilities` and `runtime.detail.assurance` distinguish installed publication mechanisms from unavailable contextual verification and atomic source snapshots. A gate's presence is not a health or effectiveness receipt.

`RunTerminated` observers receive content-free outcome metadata after successful persistence. `PublicationRejected` covers failed checks before or during a run, with an empty run ID before creation. Audit records these separately, and fixed metrics count incomplete runs, cancellation, terminal policy failures and rejected checks. They contain no raw candidate text and do not add unbounded labels. Delivery remains best effort; durable audit coordination is not claimed.

Dashboard commit `755b043` in forge-dashboard displays execution, verification and screening separately in chat, run review and run tables. Expired feeds show unknown instead of completed. Inconsistent or unreferenced passed verification cannot render as passed. Agent statistics now label completion rate precisely. The deterministic demo now emits its stream finish reason; the missing marker was detected by an installed HTTP check, correctly producing an incomplete run. The seed test fixture now registers its required tool.

Local installed evidence: the demo's real Cortex engine, Forge contract and disk-backed SQLite passed HTTP CRUD, nested patches, references, streaming, provider errors, approvals/rejections, cancellation, memory, overlays, orchestration and messaging. Reader and denied-principal HTTP checks passed, and a separately authenticated tenant could not read another tenant's run. Restart retained the incomplete run used for UI review. Playwright inspection at 1440x900 and 390x844 showed readable outcome summaries, no page overflow at 390px and a visible keyboard focus ring. The provider is deterministic demonstration code; this does not qualify a remote model or an effective scanner.

Dashboard validation: 41 tests, scoped ESLint, TypeScript checking and the shell production build passed. There are no make l/f targets there; the equivalents were the plugin's lint/format/typecheck/test scripts and the shell build. Demo Go tests ran with race detection; gofmt, go vet and build passed. The demo's sibling dependency requires Go 1.26.9, so local verification used that toolchain and a temporary modfile. No dependency pins or sibling replacements were added to Git.

Runtime validation: full engine/run/audit/metrics race suites passed, along with API, extension/contract, A2A and gRPC suites. Docs type generation, typecheck, lint and production build passed. Final all-nine-module lint, root build and the final gRPC race suite passed after an import-shadow correction and coordinated formatting. CI exposed two Fabriq fixtures without explicit scope; the optimization owner is correcting those fixtures and reports its full race suite/build passing. No production scope rule was relaxed.

Open G1 qualification: effective scanner and live-provider deployment, the remaining installed fault matrix, exact final CI and final source review. Stage 1 is not accepted as a whole. Stages 2 through 4 remain queued; no sandbox isolation, contextual correctness or complete autonomous coordination is claimed.
