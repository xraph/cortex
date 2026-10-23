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
| S2.1 | implemented | this chat | Evidence, report, verifier and all backend contracts pass race tests; runtime activation remains separate |
| S2.2 | active | this chat | Atomic result/outbox storage, guarded commit API, leased jobs, budgeted dispatch and evaluator executor implemented; experimental tool-free runtime gate implemented; qualified host adapters, tools/resume, scheduling and durable public delivery remain open |
| S2.4 broker/runtime | broker, worker, transport, protected providers, input provisioning, configured assembly and recovery implemented; deployment pending | this chat | Race and Linux fixtures cover staging, capture, attestation, input delivery and independent service discovery; standalone broker command, complete installation and live qualification remain open |
| S3.1 | active | this chat | Bounded graphs, immutable backend revisions, dependency evidence, effect/source manifests and pure strategy adapters implemented; runtime integration remains open |
| S2.3 and Stage 4 | active | optimization owner | Trial execution and recorded Sentinel optimization adapters are committed in `7f96edf` and `c6b5483`; remaining integration and qualification work is tracked in the optimization progress ledger |
| S2.5 through S2.7 and S3.2 through S3.6 | queued | shared owners | Production verification rollout remains disabled pending G1 |

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

Open G1 qualification: effective scanner and live-provider deployment, the remaining installed fault matrix and final source review. Stage 1 is not accepted as a whole. Pure Stage 2 contracts are in development; no sandbox isolation, contextual correctness or complete autonomous coordination is claimed.


## Atomic store capture follow-up

The optional `store.PreparationSnapshotter` now supplies a read-only source view for `engine.WithAtomicPreparation()`. PostgreSQL uses repeatable-read, SQLite uses a serializable transaction request, and MongoDB uses snapshot read concern with majority commit concern. The view binds the exact capture scope, serializes reads and expires when its callback returns. Collection or commit failure returns no prepared artifact. The provider callback runs once without implicit retries.

Successful captures record `SourceValidation=atomic_store_snapshot` and a `SourceSnapshot` receipt containing capture ID, provider, isolation and canonical scope. The receipt identifies this collection, not a reusable source revision. All source readers route through the captured view. Publication checks run after it closes. Default preparation keeps diagnostic read-set validation. Nonempty skill knowledge references fail atomic preparation until an external-source capture contract exists; model/tool pins, live authorization and evaluation isolation remain separate requirements.

Behavioral red tests reproduced ordinary reads being labelled usable, live knowledge being accepted and unsupported stores falling back. Engine tests now cover mixed source generations, absent or mismatched receipts, repeated callbacks, transaction failure and closure before publication. The shared store test commits agent/persona/skill/trait/overlay edits on a separate connection during capture, then checks old values, absent overlays, scope narrowing, callback expiry and cancellation. It passed with race detection on SQLite and real Docker PostgreSQL/MongoDB without skips. Final formatting, the atomic engine and all-backend receipt regressions, API race suite, root build and docs checks passed. Full affected race suites passed: engine, run, suspension, SQLite, PostgreSQL and MongoDB. The first final lint pass encountered the optimization owner's intentionally incomplete trial API. After that slice was complete, coordinated formatting and all-nine-module lint passed with zero issues. Root build, diff checks and the final atomic/cancellation engine tests passed.

CI follow-up: run 39149191516 passed the repaired Fabriq module but exposed a streaming cancellation race and docs generation ordering. The event-send select could choose a ready consumer after cancellation. Publication sends now recheck cancellation after delivery, and streaming checks again immediately before provider dispatch. The original regression failed three times in 100 repetitions before the fix and passed 200 afterward. Docs type checking now generates MDX collections after `next typegen`, whose asynchronous MDX initialization could leave the generated module incomplete when its process exits. Local type checking passes; fresh CI remains required.


Final review fix pass: the reviewer found cancellation could occur inside a step hook or authorization callback after the loop's earlier check. New regressions reproduced provider dispatch and normal/resumed tool effects after cancellation. The shared tool dispatcher now checks context before any handler, and the synchronous model path rechecks immediately before dispatch. All three regressions plus streaming cancellation passed 50 repetitions with race detection. The reviewer session then failed with a usage-limit error before a final report. Its concrete cancellation finding is fixed, but the whole-stage source-review gate remains open. This does not close G1.


Committed atomic store capture as `0e4de20` and the docs-generation fix as `bbc81c9`. After the cancellation fix, the full engine/run/suspension race suites passed again (engine 130 seconds). The final fresh-review gate remains blocked by the review session's usage-limit failure; installed live-provider/scanner qualification is still open. Stage 2 pure evidence and verifier development may proceed under the plan's early-development allowance, without enabling runtime verification gates.


## Evidence and verification contracts

S2.1 supplies version-1 evidence references, acceptance contracts, reports and typed verifier registrations. Evidence IDs use `evi`, contracts use `vcontract` and reports use `vreport`. Required criteria are all-of. Optional scores cannot override them; missing checks, incompatible bindings, unknown verifier revisions and conflicting outcomes remain inconclusive. Freshness binds the exact scope, run, candidate, evidence, prepared execution, principal, authority, policy, tools, sources, environment and dataset. Contracts can bound both report age and source observation age. Invalid UTF-8, unsupported schemas, duplicate criterion IDs and nonfinite scores fail validation.

The host supplies capture identity and time. Registered schema, test, state and human adapters receive typed requests containing immutable manifest references, without model-supplied commands or callbacks. The wrapper freezes inputs, checks current host authority before each check and after evaluation, enforces the deadline, stamps observation time and rejects cancellation after callbacks. Human decisions name their actor, decision revision, exact binding digest and expiry. A sealed report proves integrity only; authenticated host paths must control report creation and storage.

SQLite, PostgreSQL and MongoDB expose optional `evidence.Storage`, `evidence.TombstoneReader` and `verification.Store` capabilities. The composite store interface stays compatible. Shared validation sits in `store/internal/assurance`; backend drivers own exact-scope queries and atomic inserts or tombstone replacement. SQL migration `20261018000003` adds immutable assurance records. MongoDB uses equivalent identity, run, digest and scope indexes. Each backend preserves canonical JSON text so database JSON transformations cannot change the recorded representation.

Built-in evidence storage accepts at most one MiB of bytes. Larger artifacts require an injected repository. `evidence.NewRepository` requires an explicit host authorizer and byte bound, verifies content and metadata on every read, and rechecks access after retrieval. Deletion atomically removes bytes while retaining the reference, authenticated actor, time and reason. Identical retries are idempotent; conflicting deletion reasons and attempts to resurrect deleted evidence fail. Retention and sensitivity enforcement belong to the required host authorizer. No background retention worker or public download route is claimed.

Tests first failed for missing packages and backend capabilities. Behavioral red cases also reproduced lossy UTF-8 canonicalization and a source observation remaining eligible after its permitted window. Added tests cover cross-scope and ancestor denial, corruption, authority revoked during retrieval, immutable ID conflicts, concurrent creates, missing contracts, pagination, cancellation, tombstones, mixed-run reports, missing evidence and expired human decisions. SQLite and real Docker PostgreSQL/MongoDB passed the initial shared persistence race tests. The first broad run hit a SQLite timeout and two PostgreSQL container-start failures; the full SQLite retry and affected PostgreSQL retries passed. After fast-forwarding the checkout to the telemetry and dependency updates, fresh race suites passed for evidence, verification, engine, SQLite, PostgreSQL and MongoDB. The engine took 384 seconds and SQLite 669 seconds under shared machine load. Root build, documentation lint/types/build, formatting and the nine-module pre-sandbox lint pass are recorded in the validation logs. No store test was counted as live sandbox qualification.

CI run `39152040751` passed on `9b4a882`, including docs, formatting, security, root and nested-module gates. This closes the exact-commit CI item for the Stage 1 cancellation fix. The interrupted fresh review and installed live-provider/scanner checks remain open. S2.1 does not enable engine verification or claim a qualified sandbox.


## Sandbox broker and control contracts

Ruling: split S2.4 into a broker packet and a Linux runtime packet. The user chose to build the broker and tests now and select a Linux worker later. This packet ships the host-independent code; it does not mark S2.4 or G2 complete. Production runsc lifecycle wiring, the daemon and live probes remain implementation work.

The root `sandbox` package defines sealed schema-one jobs, runtime capabilities, receipts and execution results. Jobs bind exact scope/principal, image digest, argv, read-only evidence inputs, a locale environment allowlist and finite limits. Unknown privileges, network access, unpinned images and unsupported qualification fail closed. The broker uses a private SQLite journal with an exclusive connection, records receipt identity before dispatch and preserves cancellation intent before contacting the runtime. Admission limits bound active jobs and retained records. Runtime adapters must preserve job ID history across ambiguous starts and restarts.

`integrations/sandbox` adds HTTPS authentication callbacks, a redirect-denying client, Ed25519 receipts and terminal results, current-grant checks, bounded bodies and outputs, and supervisor reconciliation of expired recorded work. Export reads use no-follow descriptors on Linux/macOS and reject traversal, symlinks, hard links, special files and excess bytes. The host still authorizes content and destination. No candidate stdout can supply a trusted verdict.

The OCI builder pins runtime-spec v1.3.0 and encodes non-root execution, empty capabilities, no new privileges, private namespaces, read-only root/inputs, CPU/memory/process limits and bounded tmpfs scratch. This is configuration evidence only. `compatibility.json` remains unqualified with no invented runtime/image pins. Live descendant-kill, cgroup exhaustion, network/metadata denial, runsc restart and protected-grading probes remain open.

TDD failures covered the missing port/broker/export/recovery implementations, a policy callback mutating sealed job arguments, and cancellation after acceptance but before runtime creation. The fixes pass race tests together with concurrent idempotency, conflicting retries, tenant denial, revoked collection, signature tampering, output caps, exclusive journal ownership, restart recovery and admission capacity. Full ten-module lint, final formatting, root/module builds and the root contract plus broker/OCI race suites passed. CI and the release workflow now include the new module; no release was dispatched by this packet.


## Linux qualification probes and CI repair

The `sandbox_qualification` test tag now supplies trusted host-side runsc probes for baseline execution, read-only inputs/rootfs, host-file denial, scratch exhaustion, private fixture TCP/DNS denial, cgroup CPU/memory/PID values, descendant termination and runtime-state reopening. The suite requires Linux, root, cgroup v2 and explicit binary hashes. Missing prerequisites fail the gate rather than skip it. The fixture digest identifies the test executable; it does not qualify a production image.

A failing policy test exposed the missing guest `RLIMIT_NPROC`. The OCI profile now includes that limit as well as the host cgroup PID limit, and disables core dumps. Unit/race tests pass. The host probe suite and static fixture compile for Linux, all ten modules pass lint with the qualification tag included, and the attempted local probe run fails with `qualification blocked: a Linux worker is required`. No live assertion is recorded as passed. Memory/CPU/guest-process exhaustion, broker crash recovery, wall/output supervision, deployed image pins and protected grading remain open.

CI `39512157030` passed tests, security, lint, docs, formatting and every nested module. Its sole failure was root module tidiness after the telemetry merge: `testify v1.11.1` is imported directly by telemetry tests but remained marked indirect. Commit `c6bc79e` fixes that classification without changing the version. A second local tidy is stable, and telemetry race tests plus the root build pass. New exact-commit CI evidence remains pending.


## Worker result validation and hosted Mongo fixture

Parallel orchestration could return success after a worker failed if its aggregator succeeded. A regression reproduced that failure and another reproduced queued dispatch after cancellation. Sequential, router and debate strategies also ignored errors carried inside an agent result. All strategies now share the hierarchical invocation checks, retain failed run identities and reject missing, mismatched, empty or cancelled results. Parallel synthesis requires every worker to pass; errors from multiple workers remain inspectable. Ten race repetitions and the engine orchestration adapter tests pass. These checks establish execution failure propagation, not verified task acceptance.

CI `39513609152` exposed a hosted-runner change: Linux `7.0.0-1017-azure` triggers the MongoDB 8.3.2 image's allocator compatibility refusal. The default database test fixture now pins MongoDB 7.0.41 by digest. Its real-container conformance, migration and rescope race tests pass locally without skips. You can still override the fixture explicitly. MongoDB 8.3.2 remains unqualified on these kernels; no startup guard is bypassed.

Final coordinated formatting, all ten modules of lint, the root build and fresh orchestration/engine adapter race checks pass. Exact-commit CI remains pending. G1, G2 and the deferred live Linux sandbox gate remain open.


## S3.1 domain foundation

The pure `planning` package now seals bounded task DAGs with exact scope, immutable acceptance contracts, finite task budgets, resource revision grants, owner rosters and unique artifact ownership. `ApplyRevision` preserves the plan identity and contract, rejects widened grants or budgets, records the previous digest and derives the changed/dependent task set. Replaying the same proposal produces the same digest. Storage still needs an atomic expected-revision check.

`Ready` takes an explicit observation time and returns an error for malformed or superseded state. Pending tasks become eligible only when every prerequisite has fresh trusted reports for its assigned criteria. The check follows transitive artifact references and current authority/context bindings. Artifact producers include the sealed plan digest and task ID, so another task's result or an older revision's evidence cannot be relabeled as current. The host must authenticate verifier records, rebuild current bindings from authorized state and confirm artifact availability before calling this pure function.

TDD covered missing implementations, graph cycles and unknown dependencies, criterion loss, scope/grant/budget widening, digest mutation, deterministic replay, depth/edge bounds, stale/revoked/missing checks, cross-task artifact reuse, old-evidence relabeling and transitive provenance. Race tests pass for planning, execution and verification. Final coordinated formatting and all ten modules of lint pass; the root build is recorded with the final checks.

S3.1 remains partial. Durable plan revisions and backend conformance, task state persistence, effect/retry requirements, source provenance capture and strategy adapters are still open. This packet does not add a scheduler, claims, shared budget reservations, evidence reuse policy or production task acceptance. G2 remains required before activation.


## S3.1 immutable plan persistence

SQLite, PostgreSQL and MongoDB now expose the optional `planning.Store` capability through the shared assurance repository. Create/read/list operations require exact scope. Each successor is a single immutable insert keyed by plan ID and a fixed-width revision number; the existing scoped unique index resolves competing writers. Reading the highest committed revision avoids a separate mutable head. Identical retries return the original accepted revision without changing newer history.

Conformance tests cover creation conflicts, detached reads, pagination, history through revision 12, ancestor/sibling denial, cancellation, scope inventory and twelve competing proposals with one winner. SQLite and real-container PostgreSQL/MongoDB pass the planning and existing assurance race tests without skips. A SQLite fault adapter loses the acknowledgement after the durable insert; a fresh repository recovers the same revision on retry and rejects changed retry content. This tests the ambiguous acknowledgement window, not a full worker/process crash matrix.

The existing assurance table/collection and indexes hold these additive record kinds, so this packet needs no new database migration. Task claims, task-state transitions, effect/retry policy, source capture and strategy adapters remain open. Stored definitions do not activate scheduling or close G2/G3.

CI `39516036204` at `e51dea8` passed every job, including root race/coverage, docs, security and all nested modules. It covers the orchestration failure fix, Mongo fixture repair and planning domain foundation. The newer persistence packet needs its own exact-commit CI run.

The persistence packet passes final coordinated formatting, all ten modules of lint, the root build and post-format planning/assurance race tests on SQLite and real PostgreSQL/MongoDB containers. No database test skipped.


## Task effect/source manifests and strategy adaptation

Plans now pin host effect declarations with tool revisions, effect class, bounded retry policy and explicit idempotency, reconciliation or compensation references where required. Tasks select from that manifest and cannot introduce a handler or claim write effects against read-only resources. These are declarations only. Automatic retry, external reconciliation and compensation execution belong to S3.4 and are not enabled by this packet. An absent effect manifest allows no effect tools.

Captured sources reuse the verification source contract. Each source must match a resource grant and have a consuming task. Readiness checks the exact tool and source context as well as current reports; regressions reproduced acceptance of missing/extra tools, changed tool revisions and substituted source digests/timestamps before the fix. Ordinary plan revisions cannot replace host source or handler pins. An authorized source-recapture policy remains future work.

`orchestration.AdaptPlan` builds static sequential, parallel and hierarchical graphs from host task templates without dispatching agents. It preserves acceptance, resource and budget declarations, requires every worker before synthesis, rejects ambiguous templates and reuses the strict hierarchical parser. Hierarchical instructions must match a captured proposal digest that every worker consumes. Execution-relevant configuration is hashed into the graph origin, including model/settings and ordered participants. Invalid UTF-8 scope data is rejected rather than hashed after lossy JSON replacement.

Router and debate still use the existing runner strategies; they have no static graph adapter. Task execution registration, blackboard/runtime integration, claims and scheduler recovery remain open. The current APIs create and validate definitions; they do not activate production task acceptance.

CI `39517232117` at `6b7e1f6` passed all jobs, covering immutable planning persistence from `42d5d45` and the earlier domain foundation. This newer manifest/adapter packet needs its own exact-commit CI result.

Final formatting, all ten modules of lint, the root build and execution/planning/verification/orchestration race suites pass for this packet. The shared manifest round-trip and revision tests also pass on SQLite and real PostgreSQL/MongoDB containers without skips.

## Runtime deployment inventory

The sandbox module now validates a complete runsc installation against a trusted host manifest. Its digest binds architecture and the sorted file inventory, including each executable's path, SHA256, size and permissions. Sidecars under `gvisor-bin` and an installed containerd shim must be included. A legacy single-binary manifest is accepted only when no sidecar directory is present. Expected pins must come from an approved release, not from the installation under inspection.

Filesystem verification walks no-follow descriptors, checks owners and permissions, hashes bounded regular files with cancellation, and rejects missing/extra entries, symlinks, hard links and special files. The trusted owner must keep the installation unchanged while it is used. This check neither freezes files against that owner nor establishes isolation. The Linux probe gate now requires this inventory check before invoking runsc and records the complete deployment digest alongside the binary checksum.

TDD covered the missing manifest/verifier implementations and rejects changed sidecars, wrong architecture/owner, unsafe modes, path ambiguity, inventory/byte/depth limits, unexpected directories, links, FIFOs and cancelled verification. Final coordinated `make f`, all ten modules of `make l`, sandbox module race tests/build and Linux-targeted lint pass. The qualification suite compiles for Linux amd64 and arm64. These checks run fixtures and compilation only; live Linux qualification remains deferred by the user's worker-selection decision.

The production runsc adapter, image/input staging, durable supervision of wall/output limits and crash recovery remain open. `compatibility.json` remains unqualified, with the deployment digest unset. This packet does not activate evaluation or close G2.

CI `39519005197` at `908eae6` passed every job, covering the planning manifests/adapters from `1cfe5da` and the optimizer reader slice. The deployment inventory packet needs its own exact-commit CI result.

## Bounded worker lifecycle and runsc control

The internal supervisor now applies the receipt deadline and job wall limit, shares one output budget across stdout/stderr, and runs cleanup after preparation, startup, wait failures and normal exit. Cleanup uses an independent bounded context after cancellation. A cleanup failure returns no collectible terminal result. Concurrent output, late output and late cancellation cannot turn a failed execution into success.

The runsc driver checks the complete deployment pin, opens private worker directories through shared no-follow helpers, and compares the bundle's OCI configuration against the policy rebuilt from the sealed job before execution. It uses fixed control verbs, argv and a minimal environment. Guest output pipes remain open after the create command exits, while a separate bounded wait response must identify the exact job and a valid exit status. Export reads and runtime bundle reads share the same regular-file checks.

Two review regressions failed before their fixes: a substituted OCI command reached the runtime despite the supplied job digest, and an ambiguous create could yield a collectible failure after forced deletion. The first now fails before execution. The second remains unresolved because forced deletion succeeds even when runsc has no state record; an external reaper must establish containment cleanup before collection.

Linux probes now exercise the worker path for normal exit, combined output overflow and a deadline with descendants, then check for an empty or removed job cgroup. These probes are implemented and cross-compiled only. Production image/input staging, durable worker claims, service-manager containment, orphan reconciliation, broker activation and live qualification remain open. The internal supervisor is not wired into broker admission and does not close S2.4 or G2.

CI `39519889695` at `084db20` failed an existing A2A telemetry test because the short private-name marker `w1` appeared inside a generated TypeID. Local repeated runs reproduced the collision. The fixture now uses hyphenated agent names that TypeIDs cannot contain, while retaining the checks for private names and message content. Two hundred repeated race runs and the complete A2A race suite pass.

Final coordinated formatting and full ten-module lint pass. Sandbox module race tests, native/Linux scoped lint, root/module builds and Linux amd64/arm64 qualification compilation pass against the final source. The trusted probe binary also builds for Linux. No live isolation result is claimed; the next exact-commit CI run remains pending.

## Verified evidence input staging

The internal staging service now copies evidence into private directories keyed by exact scope and job ID. It requires host job authorization, evidence authorization and an explicit byte budget, checks exact source length/digest and retention, and publishes its completion record only after synchronized files and renewed access checks. Policy/source callbacks receive detached identities. Input mount paths that overlap a file with a directory are rejected by the root job contract in either ordering.

Retries validate the completion binding, file inventory, owner, permissions, regular-file type and stored bytes. A regression demonstrated that checking only local bytes and grants would allow deleted source evidence to be reused. Retries now reopen and verify the authoritative source as well. Interrupted directories remain untouched for a fenced reaper; failed fresh copies remove only files owned by that attempt. Blocked reads are closed on cancellation.

Tests cover valid staging and retries, changed content, excess stream bytes, per-job aggregate bounds, cross-scope job-ID reuse, missing host principal authorization, revoked and expired evidence, source deletion, symlinked files, source-close failure, incomplete directories and callback mutation. The input stager is implemented independently of production worker activation. Image staging, durable claims, external supervision/reaping and broker integration remain open; live Linux qualification is still deferred.

CI `39522046871` at `7c62986` passed all jobs. It covers the runtime deployment verifier, worker supervisor, telemetry fixture correction and optimizer SQLite foundation. The input staging packet needs its own exact-commit CI evidence.

The input staging packet passes final coordinated formatting, all ten modules of lint, native/Linux sandbox lint, root sandbox/evidence races, the full sandbox module race suite and root/module builds. Linux arm64 staging-test compilation is included in the final checks. These checks do not establish a qualified worker or production image path.


## Verified OCI image staging

The internal image service verifies an approved single-platform OCI manifest, its config, compressed layers and uncompressed DiffIDs before building a private rootfs. It supports Linux amd64/arm64 tar and gzip images under explicit byte, archive-entry/component, layer and time bounds. Host authorization runs before content access, before layer application and before returning the tree. Sources receive digest identifiers only; descriptor URLs and embedded content cannot redirect reads.

Containerd v2.2.10 supplies OCI extraction, link resolution and whiteouts. The preflight rejects unsafe paths, duplicate entries, devices, FIFOs, extended attributes, setuid/setgid permissions and trailing payloads. Tests reproduced an implicit-directory budget bypass and duplicate/case-colliding JSON acceptance; both are now rejected. A link-escape fixture checks that the host canary remains unchanged while guest-root path resolution is preserved. Image process defaults are not applied, and the worker retains file ownership. Evidence and image staging share bounded, cancellation-aware content copying.

Each attempt creates its own private directory. Failure cleanup retains errors and removes only that attempt; retries reauthorize and rebuild. There is no persistent image cache or crash-recovery claim. Broker dispatch still needs durable claims, external worker containment/reaping, disk quotas and qualification of the selected Linux deployment. The live gate remains deferred by the user's environment decision.

The image packet passes final coordinated formatting, all ten modules of lint, native/Linux scoped lint, root sandbox/evidence races, full sandbox module races and root/module builds. The full sandbox races and build also pass with Go 1.26.9. Linux amd64/arm64 test binaries compile, and module tidy is stable. The installed Go 1.26.3 vulnerability scan reports 16 reachable standard-library advisories; a fresh scan with Go 1.26.9 reports no vulnerabilities. The default local toolchain was not changed. Deployed builds still require a patched toolchain.

CI `39523884180` at `ec02ab4` passed every job, covering the preceding evidence-input staging packet and optimizer PostgreSQL persistence. This image packet needs its own exact-commit CI result. No live Linux isolation or image qualification is claimed.


## Durable worker ownership and acceptance handoff

The runtime now receives the broker's signed original receipt, including its acceptance time and runtime binding. A trusted host clock makes expiry deterministic in tests. An expired accepted retry records cancellation before contacting the runtime, so retrying cannot restart the wall budget. Detached receipt tests prevent runtime callbacks from changing persisted arguments, scope or signatures. Runtime-result validation now rejects an unsealed job and invalid exit status.

The internal worker journal persists the exact receipt before granting one worker token. Concurrent contenders have one winner; process exit does not release that claim. Identical completion retries retain the terminal revision, while changed results, forged or replaced receipts and foreign tokens fail. Cancellation or recovery fences late completion and leaves a nonterminal reaping record until a trusted external supervisor proves containment stopped at the expected revision. Uncertain work becomes failed or cancelled, never successful. Terminal identity records are retained and count against capacity.

Broker and worker journals share an exclusive SQLite opener with private owner/mode checks, file-link and special-file rejection, and application binding. The worker journal also pins the broker name/key and schema version. Tests cover failed database writes, corruption, future schema, capacity, expired claims, process exit and competing claims. The owner must preserve filesystem integrity; this is not protection against restoring an old database snapshot or a malicious host owner.

This packet establishes durable ownership independently of runtime activation. Claim/staging wiring, authenticated worker control, launch/cancel races at the external process manager, disk quotas, orphan containment and live Linux qualification remain open. The journal's `Reaped` call records a trusted proof supplied by its caller; the external proof implementation is still pending.

Final coordinated formatting and all ten modules of lint pass. The full sandbox race suite and build pass, as do native/Linux scoped lint and Linux amd64/arm64 worker-journal test compilation. The full sandbox race suite and build also pass with Go 1.26.10. A fresh scan reported GO-2026-6809 against Go 1.26.9; the Go 1.26.10 scan reports no vulnerabilities. The default local toolchain remains unchanged.

CI `39526179993` at `a0ae9ac` passed the sandbox module, root tests, security, docs and formatting, covering the preceding image-staging packet. The overall run failed because optimizer Mongo tests exceeded the default ten-minute package timeout; the optimizer owner is handling that bounded CI timeout separately. This worker-ownership packet needs its own exact-commit CI result.

## Job service containment boundary

The contained runsc path now places its payload beneath the same fixed service as the worker. The worker must occupy the exact `supervisor` subgroup, with the payload in its sibling `payload` subgroup. Membership is checked before every control command, and bundle validation rejects an OCI configuration targeting the older external hierarchy. The low-level driver remains available for independently supervised qualification probes.

The Linux containment helper checks procfs, cgroup-v2, root ownership and control-file types, then uses `cgroup.kill` and bounded population observation. It refuses to kill its own hierarchy. Tests cover changed membership, wrong payload placement, malformed observations, disappearance, an appearing group after an absent kill target, cancellation and cleanup failure. A cancellation regression initially returned an absent observation alongside an error; the fix returns no cleanup observation on that path.

These helpers do not launch a systemd service or prove a future start is fenced. Empty and absent observations remain distinct, and neither calls the worker journal's `Reaped` operation. The service adapter, authenticated daemon control, launch/cancel fencing, external resource bounds, broker activation and live Linux qualification remain open. The selected worker service must explicitly use `system.slice` and delegate controllers with the worker in its own subgroup.

Final shared formatting, all ten modules of lint, the full sandbox race suite and build pass. Linux-specific lint passes, and containment tests compile for Linux amd64 and arm64. These checks do not establish deployed service isolation. CI `39527689658` at `9c0d1e5` passed all jobs, covering the durable worker-ownership packet and the optimizer concurrency changes. This containment packet still needs its own exact-commit CI result.

## Persistent service fences and journal recovery

The internal service reaper now uses the systemd manager API over a fixed local socket whose peer must be root PID 1. It permanently masks a job instance without replacing existing files, reloads the manager, replaces queued starts with a stop job and drains the entire containment hierarchy. It waits for no pending job and an inactive or absent service, drains again for stop-helper descendants, then rechecks the mask and service state. It refuses its own service before issuing manager operations.

Two review regressions failed before correction: stop helpers could leave descendants after the first drain, and a loaded unmasked or transient unit could be mistaken for a fenced service. The second drain and strict loaded-state checks now reject those cases. The deployment must use an installed template and reserve its service names; transient unit overrides are unsupported.

Recovery connects this service proof to the worker journal's revision fence. Failed cleanup leaves durable reaping state; a concurrent cancellation prevents an older reaper from overwriting the new revision. A lost terminal database write is retried after reopening the real SQLite journal. Completed retries do not repeat service operations. Manager acknowledgement, property decoding, errors, cancellation and deadlines are tested through an internal simulated bus; no live systemd or runsc result is claimed.

Template installation and pinning, safe launch admission, daemon control, external resource ceilings, periodic orphan discovery, broker activation and live qualification remain open. S2.4 and G2 remain incomplete.

Final shared `make f`, all ten modules of `make l`, the full sandbox race suite and build pass. Linux-targeted lint and amd64/arm64 compilation pass. The service fixtures and Linux containment refusal tests also run successfully in a restricted local Linux container; its simulated manager does not establish live systemd behavior. Go 1.26.10 vulnerability scans for macOS and Linux report no findings. CI `39529487667` at `0e11b1c` passed all jobs and covers the preceding containment packet. This service-recovery packet needs its own CI result and deployed qualification.

## Pinned worker profiles and effective launch checks

Launch profiles now render a bounded installed service template and verify approved worker/configuration pins, exact template bytes and the private state directory. A shared host-file verifier uses the existing bounded digest-copy implementation and rejects symlinks, hard links, special files, unsafe permissions, cancellation and changed lengths or hashes. Profiles reject directive injection and keep deployment files outside writable worker state.

The preflight loads the job unit without starting it, then checks effective identity, commands, credentials, roots/mounts, controller delegation, resource ceilings, restart/kill behavior and logging. Missing properties and stale configuration fail closed. Review tests first exposed acceptance of alternate roots, bind mounts and credential overrides; the corrected checks reject them. The manager reader is shared with recovery so both paths decode properties consistently.

A D-Bus encode/decode regression exposed a mismatch between simulated arrays and the actual command/credential struct-array types. It failed before the expected types were corrected and now covers real wire encoding independently of a live manager.

This packet does not consume a launch claim or execute the worker. Credential creation and single-use handoff, the worker CLI/configuration contract, controller dispatch/recovery wiring, receipt-deadline enforcement, template installation and live systemd/runsc qualification remain open. The outer template wall-time ceiling does not replace the original signed acceptance deadline. S2.4 and G2 remain incomplete.

CI `39530942630` at `bd5df2b` passed all jobs and covers the preceding service-recovery implementation. Its earlier exact-commit run was superseded by that optimizer push.

Final shared formatting, all ten modules of lint, the full sandbox race suite and build pass. Linux-targeted lint and service-test compilation for amd64/arm64 pass. The service/admission and host-file pin suites also pass in a restricted Linux container, including the real D-Bus wire-format regression. These fixture results do not qualify an installed template, live manager or runtime. This packet still needs its own CI result.

## One-time service dispatch and protected credentials

The internal launcher now joins receipt enrollment, current host authorization, deployment preflight, durable claim acquisition, private credential publication and one non-replacing `StartUnit` request. Concurrent submissions have one claim winner. Existing claims and terminal records cannot trigger another start, and unresolved reaping remains pending. Authorization callbacks receive detached jobs.

Credential files bind the signed receipt, claim revision/token and approved profile digest. New private directories, exclusive file creation, synchronization and refusal to replace an existing identity protect publication. The reader checks private metadata, canonical encoding, signature, job/profile binding and time validity. Tokens never enter a guest mount or logging path through this implementation; retained files still need fenced lifecycle cleanup.

The journal now exposes current claim verification, sharing token matching with completion. Cancellation, changed revisions, completion and expiry reject stale credentials. The launcher uses this check immediately before dispatch. A qualification-expiry regression failed before the dispatch context was shortened to the earlier signed boundary. Lost start acknowledgements and other failures after claiming enter independent bounded recovery instead of a second attempt.

Fixture tests cover twelve concurrent submissions, a lost start response, revoked grants, late cancellation, expiry, changed deployment, failed recovery, callback mutation, credential substitution and unsafe directories. The worker CLI, authenticated daemon control, startup/periodic recovery, credential cleanup, full deadline supervision, broker activation and live systemd/runsc qualification remain open. S2.4 and G2 remain incomplete.

Final shared formatting and all ten modules of lint pass. The full sandbox race suite and build pass, along with post-format Linux-targeted lint and service-test compilation for amd64/arm64. Service and worker-journal fixtures pass inside a restricted Linux container. These checks do not establish installed systemd or runsc behavior. CI `39532436440` at `4fd8b5e` passed all jobs and covers the preceding pinned-profile packet. This launch packet still needs its own CI result.

## Bounded startup and periodic recovery scans

The internal recovery scanner walks verified journal entries in pages of at most 256. Startup recovery cancels unclaimed acceptances and fences every old claim without issuing another execution token. It continues through later pages after an individual failure and retains bounded error reporting. Periodic scans keep live authorized jobs, retry reaping and cancel work after its original deadline, qualification expiry or grant revocation.

Grant checks receive detached jobs and a context bounded by the original acceptance and qualification times. Provider outages remain errors even after successful cancellation and cleanup. A regression test first exposed a provider timeout being mistaken for ordinary expiry; separate expiry identification fixes that distinction. Launch, credential reading and recovery now share the acceptance deadline calculation.

Tests cover pagination after failed cleanup, successful retry, restart state, expiry during authorization, qualification expiry, clock failure, callback mutation, cancellation and journal read/write failures. When a known service record cannot be read or a cancellation cannot be saved, the scanner still attempts a service fence and retains the unresolved error.

Daemon admission gating, continuous scheduling, independent service discovery, worker/control integration, exact execution-deadline supervision and retained-file cleanup remain open. Sequential recovery pages do not promise timely execution cutoff; the worker and outer service still need their own bounds. S2.4 and G2 remain incomplete, and live Linux qualification stays deferred.

Final shared formatting and all ten modules of lint pass. The full sandbox race suite and build, Linux-targeted lint, amd64/arm64 service-test compilation and restricted Linux service fixtures pass. CI `39534354717` at `f0881b3` passed every job, covering the launch handoff and optimizer retention work. This recovery-scanner packet still needs its own CI result; no deployed qualification is claimed.

## Controller admission and recovery lifecycle

The internal controller now owns launch admission, startup recovery, periodic scans and shutdown for one daemon lifetime. It requires the launcher and scanner to share their exact journal and reaper. Before startup recovery succeeds, submissions return pending without enrollment or dispatch. Scan errors close admission and stop the controller; they remain available through its status and return value.

Shutdown cancels active launch contexts and joins their calls before recovering remaining claims under an independent 30-second context. Admission and launch registration share a lock, preventing a new submission from crossing the shutdown wait. A stopped controller cannot run again. Tests cover blocked startup cleanup, failed startup, an active claim on shutdown, cancellation during credential preparation, authorization outages, unresolved cleanup and mismatched ownership. The controller race tests also pass twenty repetitions.

Executable daemon setup, authenticated worker control, the worker CLI, runtime activation, independent service discovery, retained-file cleanup and exact worker deadlines remain open. The controller's scan interval does not bound the time spent recovering earlier pages. Live systemd/runsc qualification remains deferred; S2.4 and G2 are still incomplete.

Final shared formatting and all ten modules of lint pass. The full sandbox race suite and build, Linux-targeted lint, amd64/arm64 service-test compilation and restricted Linux service fixtures pass. CI `39535273937` at `bd94a13` passed every job for the preceding recovery-scanner packet. This controller packet still needs its own CI result; fixture evidence does not qualify a deployed manager or runtime.

## Worker process identity and current authorization

The internal peer verifier captures a Unix-stream connection's original process through `SO_PEERPIDFD`. It requires root socket and current process credentials, matching procfs PID numbering, bounded no-follow procfs reads and the exact worker subgroup. It checks liveness before and after reading identity and serializes checks with descriptor closure. Tests use real Linux sockets and child processes to verify process capture, exit detection, ambient-root refusal and handle cleanup.

Ruling: require the kernel's peer-pidfd socket option and refuse numeric-PID fallback. Opening a new handle from a received PID would reintroduce PID reuse between socket authentication and process inspection. Hosts without that option cannot run this control path; the Linux worker is still selected later.

Running-service admission reuses the pinned-profile checker and additionally binds the live main PID, execution record and active state. Inactive launch admission now also rejects stale main-process metadata. `Controller.AuthorizeWorker` joins the controller lifecycle, verifies profile and durable claim identity, rechecks host grants, checks the running service and returns detached current state. It does not start execution or commit completion.

Two regressions were observed failing before correction. A peer could exit during the last grant read after its liveness check had passed; the final peer check now follows that read. Grant lookup also inherited a caller's context scope; it now receives the verified signed job scope without exposing stored state to callback mutation. Cancellation and changed claim revisions still fail the final journal check.

The new sandbox-only CI step runs the tagged peer-kernel test binary as root. It exercises local process authentication without systemd or runsc, so it does not qualify the complete service path. Private worker transport, worker startup, result finalization, independent service discovery, retained-file cleanup, runtime activation and live Linux qualification remain open. S2.4 and G2 remain incomplete.

Final shared formatting and all ten modules of lint pass. The full sandbox race suite and build, Linux lint with the peer-kernel build tag, amd64/arm64 test compilation and restricted Linux peer/service fixtures pass. Authorization and controller race tests also pass ten repetitions, and actionlint accepts the CI change. CI `39536007636` at `c0e7b7e` passed every job for the preceding controller packet. This worker-authentication packet still needs its own CI result; deployed qualification remains deferred.

## Bounded private worker control

The internal worker control server now connects a private Unix socket to kernel peer authentication and current controller authority. One connection carries one canonical claim, with a 2 KiB frame ceiling, explicit connection limits and a total exchange timeout. The client verifies the private endpoint and root daemon before sending its token. Replies contain only a denial or authorization bound to the exact request; internal errors and capabilities stay out of responses.

Controller shutdown closes the listener, cancels exchanges and joins their handlers before journal recovery. The server takes ownership of a host-created listener but does not unlink or replace its path. The daemon must install the socket in a stable root-owned private directory and perform fenced restart cleanup. Host grant callbacks must honor cancellation.

Tests first failed against the empty transport implementation, then passed with real local sockets. They cover malformed and duplicate fields, oversized or truncated frames, trailing data, stalled clients, bounded concurrent calls, denial sanitization, forged replies, cancellation after a successful check and joined shutdown. Linux root fixtures exercise private-daemon authentication and rejection of a root caller outside the job's worker service. The container fixture uses an init process because the daemon identity check refuses PID 1.

CI `39539113157` at `6e41f43` passed all jobs, including the preceding kernel peer-identity tests. The new transport still needs its own CI result. Worker startup, daemon socket installation/restart cleanup, result finalization after independent service cleanup, runtime activation, independent service discovery and retained-file cleanup remain open. Live systemd/runsc qualification stays deferred. S2.4 and G2 remain incomplete.

Final shared formatting and all ten modules of lint pass. The full sandbox race suite and build, Linux lint with the peer-kernel tag, amd64/arm64 test compilation and restricted Linux control fixtures pass. Control and controller race tests pass twenty repetitions, and actionlint accepts the extended kernel-test CI step. Installed systemd and runsc qualification remain unverified.

## Durable reports and independent result finalization

The worker journal now separates report receipt from finalization. `Propose` binds a bounded report to the current token and claim revision, records its digest and receipt time, and removes execution authority. The `finalizing` state has no collectible result. `Finalized` requires the exact pending revision and rechecks the original acceptance and qualification deadlines. The direct completion method has been removed.

Cancellation advances the revision and marks the proposal discarded. A stale cleanup attempt cannot overwrite it. Terminal records keep the proposal digest and disposition without duplicating output bytes. Identical report retries preserve state; changed reports and corrupted bindings are refused. The journal marker migrates transactionally to version 2 after broker-key verification. Version 1 records remain readable and transition without granting another claim; older binaries reject the upgraded journal.

`Controller.ProposeWorkerResult` authenticates the live worker, current claim, deployment and grants before persistence. Its small acknowledgement confirms report receipt only. Controller lifecycle registration spans authorization and the journal write. Startup and periodic scanners independently fence and stop the entire service, then recheck current authority before committing. Cleanup failures and lost terminal writes retain the proposal for recovery. Revoked grants or expired deadlines discard it and complete cancellation cleanup; provider outages remain visible.

Tests failed before each implementation: report persistence, scanner recovery and controller report submission. Coverage includes restart, a lost final database write, concurrent identical proposals, cancellation during cleanup, grant revocation, expiry, provider failure, malformed results, corrupt proposal bindings, legacy migration, mutation isolation and shutdown. The private wire protocol still carries authorization only. Report transport, worker startup, daemon socket installation, runtime activation, retained-file cleanup and live systemd/runsc qualification remain open. S2.4 and G2 remain incomplete.

CI `39540079487` at `7dd3b19` passed all jobs for the preceding private transport packet. This result-finalization packet still needs its own CI result.

A regression exposed late finalization when the configured clock stopped advancing. The test failed before both report writes were bounded by the absolute signed deadline. Service and journal checks now share the earlier job/qualification deadline calculation. Final shared formatting and all ten modules of lint pass, along with the full sandbox race suite and build, twenty repetitions of proposal/report race tests, tagged Linux lint, amd64/arm64 test compilation and restricted Linux service/journal fixtures. These tests do not qualify a deployed manager or runtime.

## Authenticated report transport and bounded upload capacity

The private control socket now accepts version 2 report requests alongside unchanged version 1 authorization requests. A small header binds the claim, encoded length and report digest. The server verifies the live worker and current authority before accepting the larger payload, retains the process handle during upload and rechecks authority through `ProposeWorkerResult` before persistence. The exchange cannot exceed the original signed deadline.

Report uploads have a separate explicit ceiling of up to four, below the accepted connection count. Zero disables report submission. A full upload queue returns a request-bound pending response before opening another peer handle or accepting payload bytes. The client distinguishes that response from denial, allowing only the same report to be retried within its original deadline. It never retries automatically or reruns execution. A lost final acknowledgement must be reconciled through broker inspection.

Shared framing enforces canonical JSON, exact length, a 2 KiB header limit and a 2 MiB report limit. Client-side input bounds apply before encoding. Readiness and receipt bindings, signatures, duplicate or unknown fields, trailing bytes and partial frames are checked. Errors and claim tokens are not copied into responses. A report acknowledgement confirms durable receipt only, leaving independent cleanup and result finalization to the scanner.

Tests failed before the report exchange was implemented, and a separate failing capacity test drove the distinction between backpressure and denial. Tests cover revocation or peer exit during upload, lost acknowledgement after one durable proposal, maximum output, capacity reservation, signed-deadline cancellation, shutdown, malformed frames, reply substitution and peer-handle closure. Linux fixtures exercise the report client's private endpoint checks and rejection of an unrelated root process by the public server.

CI `39541860307` at `9c6fb19` passed all jobs for the preceding result-finalization packet. This transport packet still needs its own CI result. Worker and daemon startup wiring, runtime activation, disk accounting, service/orphan discovery, retained-file cleanup and live systemd/runsc qualification remain open. S2.4 and G2 remain incomplete.

Final inspection found an artifact digest field that could reach JSON encoding without a length check. A failing regression drove canonical digest validation before encoding. A repeated race run also exposed the maximum-output test's one-second throughput assumption under concurrent load. That capacity test now uses a ten-second exchange allowance; the separate short-deadline tests keep their original bounds.

Final `make f` and `make l` pass across all ten modules. The full sandbox race suite and build, twenty repetitions of control/report race tests, tagged Linux lint, amd64/arm64 test compilation and restricted Linux service fixtures pass. These fixtures do not qualify a deployed daemon or runtime.

## Worker configuration, activation and authenticated bootstrap

`WorkerConfig` binds host-installed service settings, the broker public key, private control socket and complete runtime inventory. Its protected file is canonical JSON with an exact mode and a one-MiB ceiling. The loader computes the configuration pin from those bytes and builds the launch profile without a self-referential hash. A shared owned-file reader also supplies the metadata checks used by file-pin verification.

`BootstrapWorker` requires the exact Linux worker subgroup, verifies installed files and the signed credential, matches the runtime digest and obtains current authority over the private socket. Its 30-second startup bound can only shorten the signed deadline. A validated outstanding service job can produce a distinct internal activation signal after all deployment, peer, grant and claim checks. The wire client exposes that signal as pending. Startup retries it every 50ms for at most ten seconds, bounded by the signed deadline; other failures stop startup.

A regression showed that a provider's pending error could otherwise be mistaken for activation. That test failed before the internal activation signal was separated. Joined peer-cleanup errors cannot preserve retry permission. The worker session rechecks deployment and authority before execution, returns detached snapshots, and prevents further execution authorization after reporting starts. It permits only identical report retries after explicit upload backpressure; lost acknowledgements require broker inspection.

The configuration loader, bootstrap and report-session tests failed against their empty implementations before passing. Coverage includes configuration/credential/runtime substitution, activation races, revocation, peer exit, cancellation, snapshot mutation and report retry binding. The Linux socket fixture keeps its service-membership seam explicit. Executable worker assembly, staging/source integration, disk accounting, runtime/export wiring, daemon setup, orphan discovery, retained-file cleanup and live Linux qualification remain open. S2.4 and G2 are still incomplete.

CI `39545303597` at `968b94b` passed all jobs for the preceding report-transport packet. This bootstrap packet still needs its own CI result.

A test through the full deployment checker exposed an error wrapper that hid the sole activation signal. It failed before the wrapper preserved that signal when no context error existed. Cancellation and peer-cleanup failures still prevent retries. Concurrent report tests confirm that one upload excludes another report and further execution authorization.

Final `make f` and `make l` pass across all ten modules. The full sandbox race suite and build, twenty bootstrap/session race repetitions, tagged Linux lint, amd64/arm64 service-test compilation and restricted Linux service fixtures pass. The Linux fixture includes the protected configuration, credential and private-daemon activation/report exchange, while retaining an explicit membership seam. Live installed-service and runtime qualification remain deferred.

## Bounded worker workspace storage

Worker configuration now pins staging bytes, staging and scratch inode limits,
and a worker memory reserve. Bootstrap refuses jobs whose storage, guest memory
and reserve exceed the outer service memory ceiling. Scratch is rounded down to
native pages, never above its signed limit. The installed template and effective
service checks now require `PrivateMounts`.

The internal Linux workspace verifies exact worker containment, current grants,
job scope and the original deadline before setup. It owns a new job directory
and two bounded, nonswapping tmpfs mounts in a namespace distinct from PID 1.
Descriptor-based mount APIs and unique mount IDs are mandatory. Missing features
fail closed. Staging, scratch, owner/mode, mount flags, quota totals and mount
propagation are rechecked before use; no ordinary-disk fallback exists.

Cleanup uses an independent bounded context, normal unmounts and retained
identity descriptors. Busy cleanup stays retryable. A replacement regression
failed before cleanup was changed to preserve an empty directory substituted
for an owned mountpoint. Replaced filesystems and job directories are refused as
well. Failed setup returns a nonusable handle when cleanup is unresolved, so its
caller can retry or arrange independent service recovery.

The kernel tests run in a child mount namespace created before Go starts. They
exercise real byte and inode exhaustion, namespace isolation, mount replacement,
revocation, cancellation, partial cleanup and concurrent checking/closure. The
local disposable container has only the mount capability added, no network,
read-only root and explicit process/memory ceilings. These fixtures do not
qualify a deployed worker, systemd or runsc.

CI `39547791052` at `d356dd0` passed every job for the preceding bootstrap packet.
This workspace packet still needs its own CI result. Worker execution/export
assembly, source integration, daemon setup, orphan discovery, retained-file
recovery and live Linux qualification remain open. S2.4 and G2 are incomplete.

Final `make f` and `make l` pass across all ten modules. The sandbox race suite
and build, tagged Linux lint, amd64/arm64 workspace-test compilation, twenty
kernel-fixture repetitions and the restricted Linux service suite pass.
`actionlint` accepts the new isolated-mount CI step. Live installed-service and
runtime qualification remain deferred until the Linux worker is selected.

## Runtime binding to retained scratch storage

`WorkspaceSpec` derives the contained OCI profile and replaces only guest `/tmp`
with the worker's bounded scratch mount. The bind remains writable, private,
`nosuid`, `nodev` and `noexec`. Rootfs and input overlap are refused. Runtime
removal can now leave scratch owned by the workspace for later authorized export;
actual runsc behavior still requires live qualification.

`NewWorkspaceRunsc` accepts a live workspace and binds the exact job, bundle,
runtime state, staged image and staged input paths. Callers cannot supply an
independent scratch path through runtime configuration. Each command verifies
workspace identity and worker membership. Execution also checks current grants;
cleanup uses a separate storage-only check so revocation and execution expiry do
not prevent reaping. Changed storage blocks cleanup commands and withholds the
result for independent recovery.

Tests first failed for the new OCI and driver contracts. They cover unchanged
guest restrictions, path/job substitution, missing guards, revocation between
create and start, and replaced storage before cleanup. A constructor regression
also failed before typed nil drivers were prevented from escaping error returns.
Linux fixtures verify storage-only checks without execution authority and the
workspace-bound path validator. Full worker assembly, source adapters, export
authorization and live qualification remain open. S2.4 and G2 remain incomplete.

CI `39550464023` at `460423e` passed every job, including the new kernel mount
fixture. Final shared formatting and all ten modules of lint pass for this
runtime-binding packet. The full sandbox race suite/build, twenty supervisor/OCI
race repetitions, tagged Linux lint, amd64/arm64 supervisor compilation and
twenty Linux workspace/supervisor fixture repetitions pass. Live runsc binding
and export behavior remain unqualified.


## Worker staging, execution and report coordinator

The internal Linux coordinator now connects a bootstrapped session to bounded
workspace creation, image staging, evidence staging, exclusive OCI bundle
creation, contained runtime execution and authenticated reporting. Each session
permits one execution entry, including concurrent calls and failed entry checks.
It retains reauthorization and report access without permitting another attempt.
Staging limits are pinned in worker configuration and validated through the same
value types used by the existing stagers.

Source policies receive detached signed job identity, principal and scope. They
check source grants separately from current broker authority. Tests verify that
policy mutations cannot alter retained job/input state. The worker uses the
original signed deadline throughout and sends generic preparation-failure reasons
without copying source errors into reports.

Runtime cleanup must complete before workspace cleanup or reporting. A failed
runtime reap retains storage for independent service recovery. Workspace cleanup
uses an independent context, and any failure suppresses the report. A regression
failed before source errors were prevented from impersonating unresolved runtime
cleanup. A nil runtime factory regression also failed before invalid results
were replaced by an explicit preparation failure.

Reports follow cleanup and another authority check. Only explicit report
backpressure permits an identical retry, bounded by the original deadline.
Uncertain acknowledgements do not replay execution. The Linux staging fixture
uses the real image and evidence stagers and bundle writer with explicit session,
workspace and executor seams. It verifies the staged bytes and refuses an
existing OCI configuration. It does not prove installed runtime isolation.

CI `39551610960` at `44f920a` passed every job for the preceding runtime binding.
Artifact capture before workspace cleanup, source adapters, executable worker
and daemon installation, broker activation, orphan/retained-file recovery and
live Linux qualification remain open. S2.4 and G2 remain incomplete.

Validation passed after final formatting: lint across all ten modules, Linux
tagged sandbox lint, the sandbox race suite, twenty session race repetitions,
the sandbox build and Linux amd64/arm64 test compilation. The Linux pipeline
suite passed twenty repetitions, including real image/input staging and bundle
creation. The Linux private-control and worker-session suite also passed.
Workflow validation and whitespace checks passed. These fixtures do not replace
the deferred installed systemd/runsc qualification.

## Declared output capture before workspace cleanup

Signed jobs now declare output paths, preallocated evidence identities, media
types and per-file byte ceilings. A separate artifact-byte capability grant
defaults to zero. Capture permits at most sixteen files, one MiB per file and
sixteen MiB total, all beneath the guest scratch output directory. Input identity
reuse, duplicates, path overlap and traversal are refused. Jobs without outputs
retain their earlier JSON representation.

The worker checks capture services and pinned retention before execution. After
runtime cleanup, it reads only bounded regular files without following links,
writes restricted evidence through the host repository, verifies the returned
binding and reopens the stored bytes to check length and digest. Current job,
workspace and evidence policies are checked throughout. Policy callbacks receive
detached signed identities. Archives and HTML remain opaque untrusted bytes.

Capture runs before workspace removal. A missing file, denied write, changed
stored content or uncertain upload produces a failed result with a generic
reason. Verified earlier captures can remain attached. Capture failures cannot
impersonate unresolved runtime cleanup and prevent workspace removal. The broker
requires every declaration for an exited result and rejects substituted,
duplicate, undeclared or running-job artifact references. These checks failed
before implementation; the running-job case exposed a missing boundary and now
passes after enforcement.

There is no automatic upload replay. The signed job retains each allocated ID
for recovery after an uncertain write, cancellation or report loss. Installed
repository adapters and artifact reconciliation/retention workers remain open,
along with source adapters, executable services, broker runtime activation,
independent service discovery, retained-file recovery and Linux qualification.
Capture does not grade output or authorize release. S2.4 and G2 remain incomplete.

CI `39554176533` at `59afe2d` passed every job for the worker coordinator.

After final formatting, all ten modules passed lint, including separate Linux
tagged sandbox lint. Root sandbox/evidence race tests passed twenty repetitions;
the complete sandbox module race suite and root/sandbox builds passed. Linux
amd64 and arm64 pipeline binaries compiled. The Linux capture and complete
pipeline fixtures each passed twenty repetitions, including real file capture,
repository readback and image/input staging. Whitespace checks passed. These
tests use explicit runtime/repository fixtures and do not establish installed
systemd/runsc qualification or a production repository integration.

## Durable capture intents and confirmed readback

The daemon journal now retains each capture reference before repository upload
and records authenticated readback confirmation separately. Capture revisions
never alter the current worker claim. Identical requests preserve one durable
revision; changed bindings, stale claims, revoked claims and expired retention
are refused. Result proposal and finalization require matching confirmed records.
Retention is checked again at both boundaries. A regression exposed acceptance
after artifact expiry and passed after the checks were added.

Capture requires an injected authenticated journal port and refuses missing
services before execution. Intent persistence precedes upload; confirmation
follows bounded readback. Neither a lost intent acknowledgement nor a lost upload
or confirmation acknowledgement triggers replay. Current evidence permission is
checked again after confirmation. A regression showed that a grant revoked during
confirmation could otherwise escape the final check.

Journal version 3 adds a constrained capture table while retaining ownership
record version 2. Upgrades preserve existing claims; old daemons refuse the new
journal. Exact schema checks reject missing constraints. Per-job reads remain
bounded to signed declarations, and capture recovery pages include terminal jobs.
Discard records retain the full binding and require terminal execution state plus
a caller-supplied proof of authorized repository cleanup. The journal does not
manufacture that proof or perform deletion itself.

Fixtures connect real file capture to the real SQLite journal through an explicit
in-process bridge. They cover restart and acknowledgement loss at each boundary,
concurrent duplicate requests, stale revisions, cancellation, corrupt records,
expiry and historical schema upgrades. Production worker-control transport for
capture, repository adapters and the recovery worker remain open. Runtime gates
and live Linux qualification remain deferred. S2.4 and G2 are not complete.

CI `39555744326` at `7a9094e` passed every job for declared artifact capture.

Final formatting and lint across all ten modules passed, along with separate
Linux tagged lint, the sandbox build and complete sandbox race suite. Capture
and journal race suites passed ten repetitions, including abrupt process exit
after intent and confirmation commits. Linux arm64/amd64 pipeline compilation
passed. Linux capture and pipeline fixtures passed twenty repetitions; the full
Linux journal fixture passed ten. Whitespace checks passed. The in-process bridge
and fixture services establish persistence/ordering behavior, not deployed
authentication, repository integration or runtime isolation.


## S2.4 authenticated capture transport

Capture intent and confirmation now share the authenticated upload handshake with
result reports. Reference frames are capped at 32 KiB, use the same capacity
ceiling and retain a slot for current-authority checks. Replies bind the complete
reference, job, claim revision and durable capture state. Unknown operations,
malformed references and mixed receipt types fail closed.

An explicit daemon capture policy authorizes put or read under the signed job's
principal and scope. The controller rechecks current peer, grants, deployment and
claim after the policy callback, then persists before acknowledging. Callbacks
receive detached values. Capture writes remain registered through controller
shutdown and use the original acceptance deadline.

The worker session now provides the pipeline's capture journal. Hosts no longer
inject an arbitrary journal through worker Sources. Each declared output gets one
intent attempt and one confirmation attempt, serialized with authority checks and
reporting. Lost responses, explicit backpressure, changed receipts and revocation
consume the attempt. No upload replay follows uncertainty. Failed capture can still
produce a failed job report after cleanup.

Targeted red tests preceded the controller and worker-session implementations.
Fixtures cover durable acknowledgement ordering, permission changes during policy
and after acknowledgement, exact intent binding, malformed frames, capacity
reservation, uncertain replies and concurrent reporting. The Linux private-daemon
fixture covers startup, intent, confirmation and report exchanges, unsafe socket
permissions and ambient-root rejection. Its service-membership seam is explicit.

CI `39558378630` at `a371cc2` passed every job for durable capture persistence.
Repository adapters, artifact recovery, executable daemon/worker installation,
production runtime activation and live Linux qualification remain open. Choosing
the Linux worker remains deferred at the user's request. S2.4 and G2 are incomplete.

Final checks passed: `make f`, `make l` across all ten modules, separate Linux
lint with qualification/peer/mount tags, root and sandbox race suites, sandbox
build, and Linux arm64/amd64 service and worker fixture compilation. The final
Linux private-control/capture fixtures passed ten repetitions; the complete Linux
worker fixture passed twenty. No required fixture was skipped. These checks do
not qualify an installed systemd/runsc worker or an external evidence repository.
Independent review of the full plan remains outstanding.


## S2.4 artifact recovery and delayed-upload fences

Artifact recovery now follows execution recovery at daemon startup and maintains
its own bounded cursor during periodic scans. Live or finalizing jobs are left
alone. Exact artifacts retained by terminal results survive until their retention
deadline. Abandoned and expired captures require independent deletion/audit policy
and exact durable repository tombstone proof before journal disposal. Recovery
never promotes an uncertain upload into a successful result.

The controller refuses capture configuration without matching recovery services.
Startup also refuses existing captures when those services are unavailable. A red
regression demonstrated admission with unresolved captures before this check.
Each job gets a 30-second cleanup budget. Failed artifacts do not starve later
artifacts or pages; unresolved scans close admission and retain recovery state.

The shared evidence adapter now tombstones absent identities. This closes the
race where an upload arrives after recovery observed no object. SQLite,
PostgreSQL and MongoDB preserve the first full binding, actor, reason and deletion
time and reject delayed saves. No schema migration is needed: existing tombstone
rows already fence writes. Oversized reference metadata cannot reserve an identity.

Tests began with failing absent-upload, concurrent-upload, metadata-bound and
missing-recovery cases. Scanner fixtures cover permission changes, malformed or
missing proof, lost deletion acknowledgements, reported retention, execution
cleanup, cancellation, bounded pagination and continuing past failures. A lost
acknowledgement is reconciled through tombstone readback without repeating delete.

CI `39560612122` at `bee9290` passed every job for authenticated worker capture.
The recovery scanner uses injected repository and audit services. Production
source/repository adapters, executable installation, independent service discovery,
retained-file cleanup, broker runtime wiring and live qualification remain open.
S2.4 and G2 remain incomplete; Linux worker selection remains deferred.

Final checks passed: `make f`, `make l` across all ten modules, separate tagged
Linux lint, the complete sandbox race suite, root evidence tests, root/sandbox
builds and Linux arm64/amd64 service compilation. Evidence and verification
conformance passed with race detection on SQLite and real PostgreSQL/MongoDB
containers. MongoDB used `mongo:7`; no backend test was skipped. The Linux
recovery/controller/private-socket fixtures passed ten repetitions. Independent
full-plan review and installed external-provider qualification remain outstanding.


## S2.4 broker evidence-provider boundary

The daemon-side WorkerEvidence service now binds input reads, output uploads and
output readback to the authenticated worker claim. Inputs require exact signed
references. Outputs require the retained capture binding and use the same evidence
repository as recovery. Upload after confirmation is refused. Source and capture
policies receive detached signed identity and context scope; repository credentials
stay in the daemon.

Reads use the shared bounded digest/length copier and close sources exactly once
on cancellation. Open, read and close failures remain failures. Current grants are
checked after opening and after copying, and capture state is checked again after
policy evaluation. Regressions caught bytes copied after source revocation during
Open and upload started after confirmation during a policy callback; both now fail
before the content operation. Uploads check the returned complete reference and
never retry provider errors. They do not establish readback or grading success.

Fixtures cover foreign and substituted references, missing intents, byte and
metadata limits, stale claims, expired retention, revoked peers/policies, partial
reader failures, blocked-read cancellation, uncertain upload acknowledgements and
provider mutation. A race in the fixture's shutdown setup was fixed by configuring
real recovery before starting the controller, with no runtime field replacement.

The byte-transfer protocol and worker source adapters are still pending. Approved
image delivery, installed provider configuration and live qualification also remain
open. This service is a tested provider boundary, not a deployed end-to-end
integration. S2.4 and G2 remain incomplete.

Final formatting, lint across all ten modules, separate tagged Linux lint, the
complete sandbox race suite and sandbox build passed. Linux arm64/amd64 service
compilation passed, and the Linux evidence-service fixtures passed ten repetitions
without skips. CI `39562227912` at `5f73670` passed all recovery-commit jobs.
Independent full-plan review and installed provider verification remain open.


## S2.4 private evidence transfer

The private worker socket now carries signed input reads, bounded output uploads
and output readback. Its optional WorkerEvidence service must share the controller.
The protocol reuses report/capture authentication, capacity limits, deadlines and
peer lifetime. One connection remains reserved for authorization. A missing
evidence service refuses the request before admitting payload bytes.

Downloads use bounded binary chunks and verify length and SHA256 on both sides.
The worker accepts success only after an end marker, a request-bound receipt for
the complete evidence identity and EOF. Upload success acknowledges storage only;
readback and durable capture confirmation remain separate. There are no automatic
retries. A regression reproduced success after a provider ignored an oversized
write. The writer now retains its first error and refuses later completion.

Socket fixtures cover empty and maximum-size artifacts, malformed requests and
chunks, corrupt and incomplete content, mismatched or mixed receipts, missing or
extra completion frames, cancellation, shared capacity and peer-close failure.
A real controller/journal fixture exercises input read, intent, upload, readback
and durable confirmation, then rejects further upload and a dead worker. Service
membership remains an explicit fixture seam. Linux tests exercise private endpoint
authentication, unsafe socket permissions, service configuration binding and
ambient-root denial. These tests do not qualify systemd or runsc containment.

CI `39563762496` at `3c7564d` passed all jobs for the preceding provider-boundary
change. Worker source adapters, approved image delivery, installed providers and
the live Linux worker remain outstanding. S2.4 and G2 remain incomplete.

Final checks passed: `make f`, `make l` across all ten modules, separate Linux
lint with kernel/mount/qualification tags, the complete sandbox race suite and
sandbox build. Linux arm64/amd64 service test binaries compiled, and the Linux
evidence/socket fixtures passed ten repetitions without skips. Independent
full-plan review and installed provider qualification remain outstanding.


## S2.4 worker evidence adapters and pipeline wiring

WorkerSession now exposes input and artifact capabilities over the private broker
transport. Staging and capture in worker.Run obtain those capabilities from the
bootstrapped session. The public Sources configuration supplies only image access;
workers no longer accept an alternate evidence repository or input provider.

Session adapters enforce signed scope/reference bindings, execution state, current
deployment and job grants, and the original deadline. Artifact upload consumes one
attempt after acknowledged intent, before external authorization or transport.
Uncertain replies, including capacity refusal, cannot trigger replay. Readback
requires acknowledged upload. A regression reproduced capture confirmation before
readback; evidence-enabled sessions now refuse that transition.

Input and readback streams use a bounded pipe. EOF follows the bound broker receipt
and final authority check. Early Close cancels and joins the transfer; lost receipts,
late revocation and cancellation remain failures even after all bytes arrived.
Session serialization prevents a report from overlapping an active transfer.

Tests cover scope/binding substitution, missing capabilities, execution/report
state, upload attempts, provider mutation, early close, post-transfer failures,
and production source binding. Linux socket fixtures connect a bootstrapped
session's capture, upload and readback through the private endpoint. Image/input
staging and output capture fixtures retain explicit runtime/service seams.

Approved image delivery, installed provider configuration, daemon lifecycle and
live systemd/runsc qualification remain open. S2.4 and G2 remain incomplete.

Final checks passed: shared `make f`, `make l` across all ten modules, separate
Linux lint with qualification/kernel/mount tags, the complete sandbox race suite
and build. Worker and service test binaries compiled for Linux arm64 and amd64.
Linux service/socket fixtures and the worker pipeline suite each passed ten
repetitions without skips. Independent full-plan review, installed image/provider
verification and live containment qualification remain outstanding.


## S2.4 approved image-provider boundary

WorkerImages now authenticates a worker claim and serves only the signed image
manifest or a configuration/layer digest declared by it. Registry credentials stay
in the daemon. The current image policy receives detached signed identity and
scope. An unrelated digest cannot reach the content provider except for the
approved manifest read needed to establish membership.

Image staging and broker access share the manifest validator. It checks bounded
metadata, supported descriptors, external/embedded content, total declared bytes
and conflicting sizes/media types for one digest. Regressions reproduced the
conflicting-descriptor acceptance before the shared check. Staging retains its
platform, DiffID and archive validation. No image process defaults are applied.

Reads verify exact bytes/digest and recheck current grants around provider access.
Cancellation closes blocked readers exactly once; nil readers, readers returned
with errors, corrupt content and close failures cannot acknowledge a transfer.
WorkerEvidence and WorkerImages share the controller operation/deadline helper.

Private image transport, worker image-source wiring, installed content services,
daemon installation, cleanup and live qualification remain outstanding. This is
a provider boundary, not a deployed image integration. S2.4 and G2 remain open.

Final verification passed: `make f`, `make l` across all ten modules, separate
Linux lint with qualification/kernel/mount tags, the complete sandbox race suite,
focused image/service race tests and sandbox build. Service test binaries compile
for Linux arm64 and amd64. Linux image-service and staging suites each passed ten
repetitions without skips. CI `39625181051` at `f6cd5ce` passed every job, covering
the preceding worker evidence adapters and optimizer recovery. Independent
full-plan review and installed image-provider/runtime qualification remain open.


## S2.4 private image transfer

The private control endpoint now serves the signed manifest and its direct blobs
through WorkerImages. WorkerContent configures evidence and image access against
one controller; each omitted capability is refused before payload admission.
Image requests carry a pinned digest and byte ceiling in at most two KiB of
canonical metadata. They share bounded transfer capacity with reports and
artifacts, preserving the reserved authorization connection.

Image and evidence clients share bounded download framing and cancellation.
The image client verifies SHA256, the observed byte count, the request digest,
job and claim revision, then requires a final receipt and EOF. Server/provider
errors and peer-close failures never emit a successful stream terminator.
Received bytes remain provisional until the client returns success.

Regression tests cover corrupted and partial content, ignored destination errors,
provider failure after bytes, malformed requests, mixed receipts, unrelated blob
requests, expired claims, cancellation and capacity saturation. Linux fixtures
exercise private endpoint permissions, foreign controller bindings and denial of
an ambient root caller. The signed-manifest socket fixture uses a declared peer
identity seam; it is not live service-membership qualification.

CI 39627035662 at 2961e57 passed for the preceding provider boundary. Worker
image-source assembly, installed content services, daemon/worker lifecycle,
independent cleanup and Linux systemd/runsc qualification remain open. S2.4 and
G2 are incomplete.

Validation: the sandbox module race suite and build passed. The existing `make f`
target ran from the sandbox directory with `-f ../../Makefile`, limiting the
formatter to this module while concurrent optimizer work continued. Root `make l`
passed across all ten modules, and Linux lint passed with qualification, peer and
mount fixture tags. Service binaries compiled for Linux arm64 and amd64. The
Linux image, evidence, report and private-socket tests passed ten repetitions
without skips. Live runtime qualification remains deferred by the user's choice.


## S2.4 worker image-source assembly

WorkerSession.ImageSource now connects image staging to ReadImageAt. It requires
an active execution, the exact signed scope, a valid digest, and the configured
compressed-byte ceiling. Current deployment and claim checks run before transfer
and again before clean EOF, under the original signed deadline.

Image and evidence readers share one bounded pipe lifecycle. The reader owns the
session lock until transfer completes; Close cancels and joins the transfer.
Late receipt failures and revoked grants remain read/close errors even when every
content byte arrived. Artifact readback still records verification only after all
transport and authority checks succeed.

worker.Run now obtains all content capabilities from its bootstrapped session.
The external image-source argument is removed from this internal package. Its
local image guard checks signed job identity, scope and manifest pin before
staging, while the daemon owns source credentials and current provider grants.

Tests first failed for the absent session image capability and pipeline binding.
They cover execution ordering, missing scope, malformed image pins, mixed job
identity, lost final receipts, revoked authority, cancellation and concurrent
session access. The Linux bootstrap fixture now reads image bytes through the
private socket before capture and report submission. It retains the explicit
service-membership seam and does not claim a deployed runtime integration.

Installed source configuration, executable daemon/worker assembly, independent
service and retained-file recovery, broker wiring and live Linux qualification
remain open. S2.4 and G2 are incomplete.

Validation: the first broad race run failed several short-deadline fixtures during
heavy shared-host load. A serial retry passed every package except the bulk
upload success test, whose one-second allowance expired. The image/evidence bulk
success fixtures now allow ten seconds; production limits and deadline-denial
tests are unchanged. The cancellation fixture waits for provider entry before
cancelling. The complete service race suite then passed, covering the shared
reader change. Linux worker-pipeline and authenticated-session suites each passed
ten repetitions without skips. Linux builds and arm64/amd64 service compilation
passed. CI 39628861484 passed at c411c52 for the preceding image transport.
Scoped `make f`, final root `make l` across all ten modules and Linux tagged lint
passed. The earlier full lint runs caught in-progress optimizer changes, which
its owner resolved before the final pass. Independent full-plan review and live
runtime qualification remain outstanding.


## S2.4 worker executable

The sandbox module now builds cmd/cortex-sandbox-worker for Linux arm64 and amd64.
The entry point accepts the exact installed template arguments, validates job
identity and canonical paths, reads systemd's credential-directory setting, and
calls the existing bootstrap and worker coordinator once. It adds no execution
fallback. Signals cancel the same context, and uncertain completion is not retried.

Command tests first failed for the missing implementation. They cover malformed
and duplicate arguments, extra commands, wrong ID prefixes, unsafe paths, missing
assembly, failed bootstrap, nil sessions, cancellation before and during execution,
and an uncertain report. The executable emits no diagnostic errors or content.
A zero exit status means coordinator success, not task success or finalization.

Linux command fixtures passed ten repetitions. The built arm64 executable refused
missing arguments and an ambient-root launch with exit 1 and empty output. The
previous fixture image was no longer available; these checks used the installed
arm64 image sha256:1bcc18d81671b02906296aad470ec9778a6b71016ce953092024b680bad007b0
with its entry point replaced, networking disabled, all capabilities dropped and
a read-only filesystem. No database service was started.

These fixtures do not exercise an installed systemd worker or runsc. Daemon
assembly, installation, broker runtime integration, independent recovery and live
Linux qualification remain open. S2.4 and G2 are incomplete.

Validation: command and service race suites passed. An initial macOS command also
named the Linux-only worker package and failed package selection; the corrected
command covered the host-supported packages, while Linux command fixtures exercised
the executable assembly. Both Linux binary builds, scoped make f, root make l
across all ten modules and Linux lint with qualification/peer/mount tags passed.


## S2.4 daemon-owned control endpoint

WorkerControl.ServeAt now creates the private worker socket under an existing
protected directory. It holds an exclusive persistent lock, refuses existing
endpoints and verifies the socket's owner and mode before serving. The socket
remains created by the daemon, preserving the existing kernel peer identity check.

After the transport joins its handlers, cleanup uses the retained directory
handle and recorded socket identity. It removes only the matching inode. Replaced
files or sockets survive, with a cleanup error. Normal shutdown releases the lock
and permits restart. Lock files remain in place across daemon lifetimes.

Tests first failed for the missing endpoint implementation. They cover competing
daemons, unsafe parent and lock permissions, aliases, cancellation, stale sockets,
replacement paths, repeated Close and descriptor release. Linux fixtures use the
real private client/server path, reject an ambient root worker, join shutdown and
open the endpoint again. These fixtures passed ten repetitions.

A stale socket after a crash still requires independent recovery. The private
owner must prevent directory and lock-file replacement. Daemon assembly, durable
crash recovery and live systemd/runsc qualification remain open; S2.4 is incomplete.

Validation: scoped make f, root make l across all ten modules, Linux tagged lint
and the Linux build passed. The complete service race suite passed in 63.443s
under shared-host load; endpoint race tests passed ten repetitions. The final
Linux endpoint suite also passed ten repetitions without skips, including a
replacement-socket cleanup refusal and two normal daemon lifetimes.


## S2.4 recorded control-socket recovery

ServeAt now persists socket ownership in its protected, exclusively locked file
before serving. The canonical record binds the path, device/inode, owner/group,
mode, link count and change timestamp. Both record and directory are synchronized.
Permission changes use the retained directory descriptor.

On restart, the daemon accepts only an exact metadata match. It probes the Unix
socket without sending a request and reclaims it only after an explicit kernel
connection refusal. Identity is checked again before descriptor-relative removal.
A live listener, uncertain liveness, replacement or malformed record blocks
recovery. Old empty lock files remain usable when the endpoint is absent, but do
not authorize deletion of a stale socket.

The initial tests failed for the absent recovery record. A subprocess fixture now
creates an endpoint and exits without cleanup; its parent then recovers the socket.
Other tests preserve a retained live listener, replacement socket, changed mode,
forged inode and malformed/duplicate/oversized/unknown record fields. Metadata
mutation tests include nanosecond timestamp, device, ownership and link count.

A crash before the ownership record reaches durable storage can leave an unknown
socket requiring operator inspection. Automatic recovery does not infer ownership
from a filename. Installed daemon assembly, provider configuration, broker runtime
integration and live Linux qualification remain open. S2.4 remains incomplete.

Validation: scoped make f, all-module make l, Linux tagged lint and the Linux
amd64 build passed. The full service race suite passed in 22.280s, followed by
focused recovery races after the portable metadata conversion. The final Linux
arm64 recovery and endpoint suites passed ten repetitions without skips,
including the child-process crash and retained-listener refusal cases.

## S2.4 broker runtime connection

The service runtime adapter now connects the broker's durable acceptance journal
to the controller's worker journal. It rechecks a trusted deployment attestation
before enrollment and preserves the original acceptance, claim and deadline.
A shortened qualification window that cannot cover the accepted execution
window rejects the start.

Inspection withholds proposals until independent finalization. Cancellation
persists intent before cleanup, preserves unresolved failures, and fences unknown
service identities before allowing the broker to record cancellation before start.
Journal failures still trigger a containment fence. Runtime operations join the
controller lifetime and remain bounded to 30 seconds.

Tests first failed for the missing adapter. A separate regression exposed and
then verified the shortened-qualification check. Real broker and worker journals
now exercise signed final results, broker restart, ambiguous launch recovery,
foreign-principal denial and cancellation before enrollment. Other tests cover
proposal withholding, forged receipts, detached output, cancellation retries,
journal failure, missing identity masks and controller shutdown.

The attestation callback is a trusted assembly boundary, not a completed live
qualifier. Installed daemon assembly, providers, orphan discovery, retained-file
cleanup and live systemd/runsc qualification remain open. S2.4 is incomplete.

Validation: scoped make f and final make l across all ten modules passed after
fixing four local lint findings. Broker, service and worker-journal race suites
passed; the final service run took 33.125s. Linux tagged lint and the amd64 build
passed. The arm64 Linux runtime and broker fixture suite passed ten repetitions
without skips. This validates the adapter against controlled manager behavior,
not a live systemd/runsc worker.

## S2.4 daemon lifecycle assembly

The daemon now owns a broker lifetime and private worker endpoint around prepared
controller/content services. Construction checks the signing key and worker-journal
broker identity, binds the runtime adapter and rejects reused or mismatched
components. Startup acquires exclusive journal and endpoint ownership, recovers
workers, waits for private transport readiness and reconciles the broker before
calling the public server.

A child exit cancels the shared context immediately. Shutdown joins public serving,
worker exchanges and controller recovery before releasing the socket and broker
journal. Cleanup errors remain visible. The host owns transport authentication,
request shutdown, prepared providers, worker journal and reaper lifetimes.

Tests first failed for the missing daemon and transport readiness signal. Broker
integration exposed a pending-acceptance mismatch: an unclaimed acceptance now
remains pending during reconciliation, while joined runtime failures still fail.
Additional failing tests established rejection of reused components and a broker
identity that differs from the worker journal.

Coverage includes startup failures, cancellation during private startup, old-claim
recovery before public admission, unexpected public/private exits, unresolved
shutdown cleanup, signed broker results and endpoint/journal release. Linux kernel
fixtures use production endpoint and transport code with ambient-root denial.
They do not exercise a live systemd worker or runsc.

Installed configuration/provider assembly, a bounded public TLS server, independent
orphan discovery, retained-file cleanup and live qualification remain open. The
four-stage plan and S2.4 remain incomplete.

Validation: scoped make f, final make l across all ten modules, Linux tagged lint
and the Linux amd64 build passed. Broker, service and worker-journal race suites
passed, including the full service suite in 48.572s. The arm64 Linux daemon suite
passed ten repetitions without skips, including the production private endpoint,
ambient-root denial, joined shutdown and endpoint ownership release. Runtime
adapter commit 49963bc also passed CI.

## S2.4 uncertain absence handling

A failure-path audit found that errors.Is could turn ErrNotFound joined with a
cleanup or observation error into confirmed absence. Four new cases failed before
the fix: inspection, cancellation, collection after cancellation intent, and
supervisor reconciliation. Cancellation could otherwise persist a terminal result
without proof that containment was stopped.

The broker now requires bare ErrNotFound for the special before-start absence
path. Wrapped and joined errors remain failures. Repeated collection under the
same fault cannot return a cached terminal result. Existing confirmed-absence
cancellation remains supported. This does not change the outstanding deployment
and live-qualification gates.

TLS round-trip regression tests then exposed the same ambiguity in HTTP status
mapping. Joined absence failures returned 404 and joined pending failures returned
202. The handler now maps only bare observation sentinels to those statuses;
uncertain wrapped/joined observations return 503 without exposing backend details.

Validation: the broker and service race suites passed after the journal fix
(service 52.407s). After the HTTP correction, focused transport/absence races
passed three repetitions and the complete broker race suite passed in 2.207s.
Scoped make f, final make l across all ten modules and the Linux amd64 build
passed. Confirmed-absence cancellation and ordinary TLS client round trips
remain covered.

## S2.4 bounded public TLS server

The public server now supplies a TLS 1.3, HTTP/1.1 listener around the existing
broker handler. It requires finite connection, header, request and idle limits,
static certificate material and host authentication. Optional client CAs require
verified mTLS. Key/certificate material and trust pools are copied; malformed,
unsupported, mismatched or oversized inputs fail construction. RSA precomputation
uses a detached key, and ECDSA cloning uses the supported raw-key encoding API.

The first tests failed for the missing server. A resource regression then showed
that a timed-out connection could release admission while its handler was still
finishing. Separate connection and handler limits now preserve the work slot until
return. Request expiration closes the associated connection. Cancellation and
listener failure close admission, cancel requests and join active handlers before
releasing server ownership.

Tests cover signed broker round trips, mTLS success and denial, frozen trust/key
material, RSA/ECDSA/Ed25519 handshakes, disabled multiplexing, plaintext and old-TLS
refusal, oversized headers, slow bodies, request deadlines, connection saturation,
unfinished handler admission, malformed keys, listener failure and single-use
ownership. A combined daemon/TLS fixture verifies submission and joined shutdown
through worker recovery and broker-journal release.

The server is an assembly component. Host credential validation, certificate
provisioning, installed provider/configuration assembly, independent orphan
recovery, retained-file cleanup and live systemd/runsc qualification remain open.
S2.4 and the four-stage plan are incomplete.

Two shutdown regressions additionally showed that listener closure alone left raw
connections open and could admit an accept completing during close. The bounded
listener now tracks and closes raw connections before HTTP/TLS cleanup, rejects
late registrations and releases each connection slot once. This avoids serial TLS
close-notification waits during daemon shutdown.

Validation: scoped make f, final make l across all ten modules, Linux tagged lint
and the Linux amd64 build passed. Final broker and service race suites passed in
3.303s and 24.107s. The arm64 Linux TLS/connection suite and daemon TLS/private
endpoint suite each passed ten repetitions without skips. The closed-pipe fixture
sets its read deadline before shutdown, since net.Pipe rejects deadline changes
after closure. These tests establish transport/lifecycle behavior with controlled
worker-manager fixtures, not live sandbox isolation.

### S2.4 installed-deployment attestation binding

Added `service.NewDeploymentAttestation` for installed Linux daemon assembly.
Construction verifies protected worker configuration and retains the broker,
profile digest and runtime inventory digest. Each observation verifies the
installation before and after a current host qualification lookup. A changed
installation requires new assembly; returned evidence must name the expected
deployment and runtime. Qualification is never cached.

The trusted host verifier still owns live probe evidence, host/kernel/control
matching, revocation and freshness. Neither this constructor nor its fixtures
qualifies a Linux worker. Capability shape and expiry checks now live in
`sandbox.Capabilities.Validate`, shared with job admission. Observations preserve
caller cancellation, use a 30-second ceiling and support concurrent verification.

TDD started with missing-API failures. Tests cover configuration, binary,
template, runtime and directory-permission changes before and during verification;
identity mismatch; malformed or expired capabilities; verifier errors;
revocation; cancellation; concurrent calls; and the production Linux constructor.
A fixture name containing `=` initially violated the protected path policy and
was corrected without weakening that policy.

Scoped make f and exact-file root formatting passed, followed by make l across
all ten modules, Linux tagged lint and the Linux amd64 build. Root sandbox race
tests passed in 2.840s, focused attestation race tests in 1.715s, and Linux fixtures
passed ten repetitions without skips. Under host load above 770, the first broad
race run had deadline failures and a ten-minute service-suite timeout. All named
failures passed on a constrained rerun (service 3.044s, broker 1.702s).
The final full service and broker race suites also passed with `GOMAXPROCS=2`
and serial package execution, in 26.774s and 3.865s respectively.

Host provider assembly, the concrete live-evidence verifier, installation,
orphan recovery and live systemd/runsc qualification remain open. S2.4 and the
four-stage plan are incomplete.

### S2.4 installed daemon assembly

Added `service.AssembleDaemon` to wire protected worker configuration, the worker
journal, reaper, source providers, capture recovery, private transport and broker.
It requires root on Linux, retains caller ownership of supplied resources and
does not start services or invoke qualification during construction. The control
socket, launch profile and transfer limits come from the installed configuration.
Launch and attestation share one verified snapshot to avoid binding them to two
installations during a configuration change.

Public-operation policy, connected-caller launch policy and signed-subject
revalidation remain explicit host services. Recovery now loads the original
receipt, requires its exact job digest and checks current grants and qualification
against its accepted controls and deadline. Runtime launch and recovery share the
same capability comparison. Worker access already uses this recovery eligibility
path. Policy callbacks cannot mutate the retained job through their arguments.

Capture recovery remains mandatory when new capture is disabled. Its journal must
match the worker journal, and worker upload and cleanup use the same repository.
Capture authority must agree with the installed retention setting. Private report
capacity cannot be disabled in an assembled worker host.

The first tests failed for the missing assembler. Linux fixtures then exercised
construction, lifecycle, missing providers, identity/key mismatches, capture
configuration, grant revocation, qualification drift, shortened qualification,
reduced resource limits, changed installation and mismatched journal jobs. The
lifecycle fixture initially omitted authenticated identity; it now tests both
anonymous denial and authenticated capability access.

Validation: scoped make f, final make l across all ten modules, Linux tagged lint,
Linux amd64 build and actionlint passed. Full service and broker race suites
passed in 30.837s and 3.679s. Host and attestation Linux fixtures passed ten
repetitions without skips; the expanded CI fixture selection also passed locally.
CI now includes host assembly, attestation and daemon fixtures in the existing
root Linux service race step. Remote CI is pending integration and push.

The attestation commit remains local because remote main advanced through
telemetry PR #59. A conflict-free merge preview was prepared and approval was
requested under the no-merge-without-request rule. Work continued on assembly
while that approval was pending. No histories were rewritten or merged.

Concrete source adapters, certificate/credential provisioning, the live-evidence
verifier, standalone broker installation, orphan recovery and live systemd/runsc
qualification remain open. S2.4 and the four-stage plan are incomplete.

### S2.4 independent worker discovery

Installed daemon assembly now requires bounded service discovery in addition to
journal recovery. The systemd adapter queries only the reserved worker namespace,
includes inactive services and queued jobs, and rejects malformed identities,
aliases, duplicate IDs and oversized snapshots. The snapshot limit applies after
D-Bus decoding because the systemd method does not paginate.

Missing and terminal records trigger persistent fencing and whole-service drain
checks. Journal read errors also trigger cleanup and remain unresolved afterward.
Recovery creates no claims or results. Known nonterminal jobs stay with execution
recovery. The host must reserve the complete worker namespace for one journal.

Startup recovery gates admission. Periodic discovery has a separate page cursor,
and failures close admission before shutdown recovery. A subsequent pass revisits
insertions before the cursor. This is independent discovery inside the daemon;
installed restart supervision and external enforcement remain required.

TDD began with missing-API failures. Tests cover real-shaped D-Bus replies,
canonical identities, ambiguity, bounded pages, insertions between pages, journal
failure, cleanup retries, startup denial, periodic discovery failure and ownership
consistency. Linux fixtures passed ten repetitions without skips. Final make l
passed across all ten modules after scoped make f; Linux tagged lint and the
Linux amd64 build passed. Final race suites passed for service (47.367s),
containment (1.134s) and broker (4.204s).

The user approved integrating remote main, rerunning checks and pushing. The
merge and combined-tree validation follow this packet. Concrete source adapters,
credential provisioning, live evidence verification, standalone installation,
retained-file cleanup and live systemd/runsc qualification remain open. S2.4 and
the four-stage plan are incomplete.


### Approved main integration

The user approved merging remote main, rerunning checks and pushing. Sandbox
commits `1c044ad`, `504d6df` and `f550247` were integrated with telemetry PR #59
and dependency updates through merge `2de2778`. The merge had no conflicts and
root make f produced no changes.
Final make l passed across all ten modules with zero findings.

On the combined tree, the engine race suite passed in 226.504s; audit hooks,
telemetry and root sandbox contracts also passed. The full sandbox module race
suite passed, including service recovery (71.529s) and worker storage (9.510s).
Forge extension and dashboard contract race suites passed, as did Fabriq.
Root and Linux sandbox builds, Linux tagged lint, docs type checking, docs lint
and the docs production build passed. The docs build reported a
missing metadataBase warning for social-image URLs.

These checks validate the merged implementation. Live systemd/runsc isolation
qualification remains deferred to the Linux worker the user will select. The
four-stage plan remains incomplete.


### S2.4 retained workspace recovery

The prior integration at `dc38f47` passed every remote CI job, including sandbox
kernel fixtures and security checks, in run `39884402969`.

Installed assembly now pins the private workspace root and requires retained
workspace recovery. Startup, periodic and shutdown scans discover directories
independently of loaded services. Service orphans and retained workspaces share
one bounded scanner for journal eligibility, cursor handling, error retention
and cleanup counts. Known nonterminal jobs remain with execution recovery.

Cleanup requires a persistent service fence and complete cgroup drain. It removes
only empty stage/scratch mountpoints and their job directory using no-follow
handles. Inode, owner, permissions and unique Linux mount identity checks reject
replacement roots, symlinks, unexpected content and mounted paths, including
same-filesystem bind mounts. It never recursively deletes payload files or
unmounts filesystems. Partial setup and absent directories can be retried.

TDD started with missing workspace and service APIs. Linux fixtures cover failed
fences, retries, partial setup, unexpected entries, size limits, cancelled calls,
root replacement before and during fencing, tmpfs and bind mounts. Service tests
cover live-job preservation, startup cleanup and periodic failure closing
admission. The final Linux filesystem and host suites passed ten repetitions
without skips. Lint identified seven local error assignments and four Linux-only
fields exposed to the macOS build; these were corrected without exemptions.

Scoped make f, final make l across all ten modules and Linux tagged lint passed.
The final service and workspace race suites passed in 33.357s and 1.088s.
The Linux amd64 sandbox module build passed as well.

Launch-credential file cleanup, installed source adapters, credential provisioning,
live evidence verification, standalone installation and live systemd/runsc
qualification remain open. This packet does not close S2.4 or the four-stage plan.


### S2.4 launch-credential recovery

Workspace cleanup commit `a5086b7` passed every remote CI job in run
`39885715666`.

Installed assembly now pins the private launch directory and requires credential
recovery during startup, periodic scans and shutdown. A missing or terminal job
can have at most one retained file, claim.tmp or claim.json. Cleanup fences the
service, checks the no-follow file handle for root ownership, mode 0400, one link,
a one-MiB ceiling and matching unique mount identity, then unlinks and syncs the
directory changes. Interrupted, malformed and expired contents need no execution
authority and are never parsed or logged. Unexpected entries remain unresolved.

Filesystem recovery moved into internal/retention so workspace and credential
layouts share root identity, discovery, bounded scans and directory removal.
The Linux mount identity primitive now lives in hostfs and remains shared with
workspace mount enforcement. Journal eligibility and error retention continue to
use the same cleanup scanner.

Credential publication runs in the daemon, outside the worker cgroup. Cleanup
therefore shares a fixed-size per-job coordination gate with the launcher. It
waits for in-flight publication; a delayed publisher verifies current claim
ownership inside the gate and cannot recreate a credential after cancellation
or terminal cleanup. Gate waits respect cancellation and release after failure.
Controller reconstruction retains the same gate without nested locking, and a
mismatched or omitted gate is rejected.

TDD covered missing APIs, partial writes, no secret-content dependency, links,
permissions, special files, oversize files, conflicting filenames, failed fences,
writer/cleanup ordering, delayed publication and cancelled waiters. An additional
regression exposed a panic from empty cleanup service values. Shared nil-safe
binding checks now reject those configurations without panic.

Validation: scoped make f, final make l across all ten modules, Linux tagged lint
and the Linux amd64 build passed. The full sandbox module race suite passed;
the final service rerun passed in 22.480s and focused credential regressions in
1.947s. Retention, installed host and real mount fixtures each passed ten Linux
repetitions without skips. The first file fixtures tried to rewrite mode-0400
files without DAC override; fixture setup now changes and restores owner mode
explicitly. The production cleanup policy remains mode 0400.

Installed providers, credential provisioning, live evidence verification,
standalone installation and live systemd/runsc qualification remain open. The
four-stage plan remains incomplete.

### S2.4 installed local image source

Launch-credential recovery commit `38a2edc` passed every remote CI job in run
`39887615295`.

The sandbox module now has a protected local image source for preinstalled
`blobs/sha256` content. Its constructor pins root-owned mode-0700 directories.
Each open checks directory identity, rejects aliases and unsafe metadata, and
verifies a mode-0400 single-link blob before returning an in-memory snapshot.
The existing hostfs pin checks and verifiedio hashing are shared with deployment
verification. WorkerImages still enforces current access and signed manifest
membership, and staging still verifies content and layer DiffIDs.

Each blob is limited to 64 MiB. Reader capacity is 1 through 32 with at most
256 MiB of reserved snapshots; readers hold their slots until close. Capacity
waits and snapshot reads honor cancellation. Concurrent closes release a slot
once. The source requires responsive local storage because cancellation cannot
forcibly interrupt a stalled kernel file call. Larger blobs need another source.

Tests started with missing APIs and cover content substitution, unsafe links,
special files, wrong permissions, directory replacement, invalid limits,
cancelled capacity waits, concurrent access and actual OCI layer staging. The
first fixture run exposed a nonprivate temporary directory; the fixture now sets
mode 0700 explicitly. Production permission checks were kept intact.

Validation passed: scoped make f, final make l across all ten modules, Linux
tagged lint, the Linux amd64 sandbox build and the full sandbox race suite.
Final image and hostfs race tests passed in 1.609s and 1.143s. Linux source tests
passed ten repetitions, 50 top-level test runs, without skips. Two lint findings
were fixed by using separate error variables.

Installed provider configuration, credential provisioning, live evidence
verification, standalone broker installation and live systemd/runsc qualification
remain open. S2.4 and the four-stage plan remain incomplete.

### S2.4 pinned daemon credentials

Local image source commit `e623d16` passed every remote CI job in run
`39888987778`.

The installed credential loader now reads protected, hash-pinned receipt and TLS
keys, the server chain and client CA bundle through the shared hostfs reader.
Every file requires root ownership, mode 0600, one link and an explicit byte
ceiling. The receipt key must be Ed25519 and match the verified worker public
key. The loader returns no partial credentials after failure.

PEM parsing rejects unconsumed text, headers, nested malformed blocks and excess
blocks. Private keys require PKCS8 and cannot contain trailing DER values after
the key. Client trust requires distinct CA certificates with signing usage.
Server certificate validity, key matching, algorithm limits and detached TLS
state remain owned by the existing NewServer validator. This installed transport
requires mutual TLS plus the host's current authentication callback.

TDD began with missing loader APIs. Regression tests then reproduced Go's
permissive handling of a malformed PEM prefix followed by a valid key, and of
trailing DER values inside a key block. Both are now rejected. The loopback test
submits a broker job and verifies its signed receipt, rejects a client without a
certificate before authentication and rejects an authenticated transport after
host authentication is revoked. Removing the installed TLS key after loading
also confirms that the server uses its detached credential snapshot.

Validation passed: scoped make f, final make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. After the final
DER check, the complete service race suite passed in 20.201s. The final Linux
credential suite passed ten repetitions, 40 top-level tests, without skips.

Certificate issuance and installation, host identity policy, installed provider
configuration, live evidence verification, standalone broker installation and
live systemd/runsc qualification remain open. S2.4 and the four-stage plan remain
incomplete.

### S2.4 current host policy

Pinned daemon credential commit `43acbe9` passed every remote CI job in run
`39890422072`.

The host policy loader now binds verified mTLS leaf fingerprints to exact
principal/scope identities and applies current image, resource and validity
limits to broker operations, launches and recovery. The policy is a root-owned,
single-link mode-0600 canonical JSON file, bounded to one MiB and 512 subjects.
Every check rereads it. Invalid, missing or expired policy denies access without
retaining an earlier grant snapshot.

Certificate fingerprints are globally unique. Authentication checks the current
validity of a verified chain on every request, including reused connections.
Removing a certificate revokes transport access; removing or expiring the subject
also revokes execution. Each job must retain its sealed digest, exact subject,
allowlisted manifest and every resource ceiling. Image access shares that grant
check. Input evidence, capture cleanup, publication and runtime qualification
remain independently authorized.

TDD covered the missing policy API, revocation, invalid documents, ambiguous
mappings, scope widening, stale TLS state, time boundaries and each resource
limit. A loopback test wires the loaded credentials, policy and broker together,
submits a signed job, reduces its grant, rejects a retry and confirms recovery
cancels and reaps the accepted job. Service-manager behavior remains a controlled
fixture; this is not live systemd/runsc qualification. The credential tests share
the same TLS client fixture.

Validation passed: scoped make f, final make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The final service
race run passed in 22.305s. Linux policy tests passed ten repetitions, 60 top-level
tests, without skips. The local lint assignment was corrected; the optimizer
owner separately corrected an assignment in its concurrent work before the final
all-module lint passed.

Installed policy/provider configuration, live evidence verification, certificate
provisioning, standalone broker installation and live systemd/runsc qualification
remain open. S2.4 and the four-stage plan remain incomplete.

### S2.4 job-bound evidence repository

Current host policy commit `30605fe` passed every remote CI job in run
`39891861852`.

The daemon can now reuse Cortex evidence storage through a repository adapter
that requires a private validated job context. Scope, principal and deletion
actor alone grant no access. The context retains a detached job whose original
digest must validate before copying; an invalid nested context shadows earlier
authority. Worker contexts clear inherited deletion actors. Recovery binds its
configured cleanup actor separately from the original job subject.

Worker access permits exact declared inputs and declared output captures through
current policies. Deletion and tombstone audit require the independent cleanup
context and a declared output. Cleanup cannot read or write content or delete
inputs. The adapter reuses the root repository's immutable storage and content
checks. Audit reads verify the full reference binding and recheck cleanup policy
before returning proof. Worker liveness, claims, capture intent and confirmation
remain enforced by WorkerEvidence and its controller.

TDD started with missing adapter APIs. Real SQLite fixtures now exercise input
read, capture intent, immutable upload, readback and confirmation through the
worker service. Recovery fixtures verify both stored and absent captures become
durable tombstones that reject late uploads. Failure cases cover ambient or
altered authority, invalid job digests, mutated policy arguments, disabled capture,
revocation during storage reads, cancellation and mismatched tombstone metadata.
The sandbox module pins Grove and its SQLite driver at the root module's existing
versions for these integration tests.

Validation passed: scoped make f, final make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The service race
suite passed in 40.304s. Linux evidence
repository tests passed ten repetitions, 70 top-level tests, without skips.
Module tidy reported no diff.

Installed database provisioning, source grants, live evidence verification,
standalone broker installation and live systemd/runsc qualification remain open.
S2.4 and the four-stage plan remain incomplete.

### S2.4 installed evidence permissions

The installed evidence policy now reads a protected canonical JSON file on every
check. Exact input binding digests approve content and its provenance, sensitivity
and retention together. Capture grants bound each declared output's bytes and
retention. Independent cleanup grants bind an actor to the signed job's original
subject and exact scope, so revoking content access does not prevent recovery of
abandoned captures. Cleanup grants must themselves remain current.

Policy files are root-owned, single-link mode-0600 files capped at one MiB.
Subject and cleanup lists each allow at most 512 entries, with finite validity
windows inside the document lifetime. Duplicate or invalid grants reject the
whole document. Missing, unsafe, malformed and expired files deny access without
falling back to a prior snapshot. Host execution and evidence policy share the
protected reader, identity and interval validators. Their tests share one atomic
policy-file writer.

TDD reproduced a missing context at capture intent: that path supplied scope
alone, so the installed policy correctly refused it. Intent and confirmation now
use the same validated job context as evidence transfer. A real SQLite fixture
checks installed input access, intent, upload, readback and confirmation, revokes
the subject grant, then cleans the abandoned capture through the independent
cleanup grant. Delayed uploads remain fenced by the repository tombstone.

Regressions cover wrong operation, actor, subject, scope and job binding; revoked
input/capture/cleanup grants; byte and retention ceilings; future or expired
evidence; cancellation; expiry during authorization; invalid documents; duplicate
grants; oversized files; wrong ownership; unsafe permissions; symlinks and hard
links. The production constructor accepts only root-owned files. Expired evidence
can be cleaned after content authority is revoked, but cleanup revocation denies
that access too.

Validation passed: scoped make f, final make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The service race
suite passed in 37.599s. Evidence and host policy tests passed ten Linux
repetitions, 120 top-level tests, without skips.

Database and policy provisioning, live evidence verification, standalone broker
installation and live systemd/runsc qualification remain open. S2.4 and the full
four-stage plan remain incomplete. Linux worker selection remains deferred.

### S2.4 protected evidence database startup

Installed evidence policy commit `f7f8a89` passed CI run `39894793132`.

The broker now has an internal evidence database factory that reuses the root
SQLite store and its migrations. Production opening requires root and a canonical
path in a private directory. Database, ownership and SQLite recovery files must
have exact ownership, mode and link metadata. A new database receives a dedicated
application ID and format version before Grove can enable WAL. Foreign databases
and unsupported format headers are rejected without changing their contents.

The factory holds a separate ownership-file lock across initialization and use.
SQLite also uses exclusive locking, one connection and full synchronous writes.
Close waits for active storage operations before closing SQLite and releasing
ownership. Storage and audit methods retain the root store's exact scope, size,
immutability and tombstone rules. Authorization remains in the service repository
and installed policy.

TDD reproduced two ownership gaps. Locking the database file itself conflicted
with SQLite on macOS, so lifetime ownership uses a separate protected file. That
lock alone still allowed another SQLite process to read the database; the new
cross-process regression passes with SQLite exclusive locking. Metadata checks
use descriptor-relative stat rather than opening and closing another database
file descriptor. Private state creation and validation are shared with the
existing journal opener.

Tests verify clean reopen, content and tombstone recovery after process exit with
committed WAL, delayed-upload rejection after recovery, competing ownership,
close during active operations, exact scope, byte bounds and URI-safe filenames.
Unsafe directories, file modes, ownership, symlinks, hard links, special files,
foreign roles, future formats and orphan sidecars are rejected. Rejected database
contents remain unchanged.

Final validation passed: scoped make f, make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The evidence
store race suite passed in 27.660s; the service suite passed in 40.752s. The Linux
database suite passed ten repetitions, including crash subprocesses and the
independent SQLite client, without skips.

Standalone configuration, directory/policy provisioning, input-provider assembly,
live evidence verification and live systemd/runsc qualification remain open.
The factory does not install or start the broker. S2.4 and the four-stage plan
remain incomplete.

### S2.4 installed broker configuration and lifetime

Protected evidence database commit `c43fee7` passed CI run `39896489072`.

The installed broker configuration binds explicit transport, controller, image,
evidence and capacity limits to protected credential references, current policy
files and the installed worker. Reading it verifies canonical JSON and the worker
installation without opening storage or services. Database roles, sidecars and
ownership files must remain distinct; image storage cannot overlap runtime or
worker state. A regression caught database names colliding with the reserved
workspaces and launch directories. Both now fail configuration validation.

`RunInstalledDaemon` loads those settings, policies and credentials, opens local
providers and databases, connects to systemd and assembles the existing daemon.
It checks broker configuration bytes and worker profile identity again before
recovery. Public mTLS serving starts only after recovery and a fresh qualification
check. Missing verifier, rejected qualification, configuration changes and
service/listener failures return errors and release owned resources. Partial
service and listener results are closed too. The daemon joins its workers before
the runner closes storage and systemd.

Linux fixtures exercise the installed path with real protected files, SQLite,
sockets and mTLS. They verify revocation on a reused connection, startup failure
cleanup and database ownership release. Their controlled service manager and
fixture verifier provide no live systemd/runsc qualification. CI includes the
new installed broker tests in its root Linux race fixture step.

Final validation passed: scoped make f, make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The service race
suite passed in 87.463s. Final configuration and installed startup tests passed
ten Linux repetitions, 50 top-level tests, without skips.

The service package remains internal. A standalone executable, concrete live
qualification verifier, installation/provisioning flow and input ingestion are
still open. Linux worker selection remains deferred. S2.4 and the four-stage
plan remain incomplete.

### S2.4 protected installed input delivery

Installed broker startup commit `acb042c` passed CI run `39898806605`.

Ruling: installed inputs use a separate protected file source, while captured
outputs keep the exclusive evidence database. This lets a trusted host provision
new input bindings during a daemon lifetime without opening the capture database
or adding upload authority to worker requests. Host provisioning remains
responsible for input-file retention and deletion.

`LocalInputs` requires a private root-owned directory and mode-0400 single-link
files named by the full evidence reference binding digest. It verifies the raw
bytes against the job's exact size and SHA256. The caller must carry private
validated job authority, declare that input and pass current source policy.
Authorization runs before waiting for bounded reader capacity, after the wait
and after reading. Retention is checked before and after each policy call.
Missing files, unsafe metadata, aliases and directory replacement return no
reader. No URL fetching, upload or deletion API is exposed.

The configured source permits at most one MiB per input and 16 open snapshots.
Images and inputs share the extracted cancellation-aware snapshot reader, which
releases its capacity exactly once on Close. The shared protected pin reader now
authenticates empty files too; executables, runtime inventories and credentials
retain their separate nonempty constraints. TDD caught empty-file rejection and
expiry during the final authorization callback before both regressions passed.

Installed broker configuration requires `input_root` and `input_readers` and
rejects overlap with image, runtime, state or protected configuration paths.
Startup loads the source before opening durable storage. Worker input transfer
uses it through the existing claim, grant and digest checks; capture storage and
independent recovery keep their existing database and tombstone rules.

Final validation passed: scoped make f, make l across all ten modules, Linux
tagged lint, Linux amd64 build and the full sandbox race suite. The service race
suite passed in 64.726s. Input-source, configuration and installed startup tests
passed ten Linux repetitions, 110 top-level tests, without skips. The tests cover
empty content, unsafe file metadata, changed bindings, cancellation, revocation,
expiry during authorization, reader capacity and real worker transfer.

The provisioning command, standalone broker executable, concrete live verifier
and installation flow remain open. Linux worker selection remains deferred.
S2.4 and the four-stage plan remain incomplete.

### S2.4 input provisioning command and atomic publication

Protected input delivery commit `4341d13` passed CI run `39899988310`.

The Linux `cortex-sandbox-input` command now installs one declared input from
protected broker/job configuration and a mode-0600 source file. It checks the
full reference, exact bytes and current evidence grant, publishes the mode-0400
binding file, reads it through the installed source and emits a bounded metadata
receipt. Configuration, job and current policy are checked again before commit
and before success. Provisioning opens no broker database, listener or service
connection and grants no job execution or runtime qualification.

The shared host-file publisher writes a unique private staging file, applies the
final mode, syncs it and uses the platform's exclusive rename operation. It
never overwrites an existing destination. Identical retries independently verify
metadata and bytes; conflicts remain untouched. Publication syncs the parent
directory and checks the installed pin. TDD caught a retry accepting a file
changed during its policy callback; verification now runs again afterward.

Process-exit tests cover interruption before rename and after a committed write.
An interrupted preparation can leave a private staging file, which input readers
cannot address. Retry installs or verifies the exact destination without adopting
that orphan. Host cleanup must confirm the original publisher has stopped before
removing staging files. This command does not claim automatic crash cleanup.

Fixtures also cover concurrent publishers, cancellation, changed prepared config
and jobs, revoked grants, replaced directories, unsafe source/destination metadata
and mismatched bytes. The root Linux suite executes the built command, checks an
identical receipt on retry and verifies that failed requests preserve installed
content and do not print source bytes. CI builds and runs that command fixture.

Validation passed: scoped make f, sandbox lint, Linux tagged lint, Linux amd64
build and the full sandbox race suite. The service race suite passed in 53.685s.
The Linux command/configuration fixtures passed 90 top-level tests and atomic
publication fixtures passed 50, each across ten repetitions without skips.
Repository-wide make l passed eight modules on the final recheck, including
optimization. The extension module needs dependency tidying after the concurrent
Sentinel changes; the new Sentinel adapter has two gocritic findings. These were
sent to their owner and remain outside this sandbox commit.

The standalone broker executable, concrete live qualification verifier and full
host installation/provisioning flow remain open. Linux worker selection remains
deferred; S2.4 and the four-stage plan are incomplete.

### S2.2 run revision fences

Input provisioning commit `90e45f0` passed CI run `39902290045`.

Ruling: add a persisted run revision before atomic result publication. The
current unconditional update could overwrite changes made after verification.
The revision is a prerequisite for the result commit's expected-version check;
it does not enable contextual verification or satisfy G1.

New runs start at revision one. UpdateRun compares the caller's revision and
advances it in the same database write. A stale writer receives
`cortex.ErrRunConflict`; failed writes preserve its revision and timestamp.
Missing runs and writes outside the exact scope remain `ErrRunNotFound`.
Historical SQL rows and Mongo documents start at zero and advance on their first
successful write. Negative and exhausted revisions cannot wrap around.
REST maps conflicts to HTTP 409. The dashboard returns a conflict without an
automatic retry hint; both use a fixed message without wrapped internal details.

Resume and expiry claims advance the same revision. Each claim returns the run
snapshot acquired by its database operation, so resume, cancellation, checkpoint
rejection and expiry use the acquired version without another fallible read.
SQL reads the snapshot within the claim transaction; Mongo returns the updated
document. The snapshot is transient and never enters suspension JSON or storage.
Older custom stores keep their existing behavior. All writers must be upgraded
before you rely on revision conflicts; old binaries do not advance the field.

TDD reproduced a stale writer overwriting a current result and both claim paths
leaving revisions unchanged. The fixes passed the focused backend race tests.
Shared tests cover eight competing writers with one winner, scope denial,
cancellation, invalid revisions, stored/listed versions and stale pre-claim
writes. SQL migration replay preserves historical completion; Mongo tests cover
updates and claims on documents without a revision field.
The API and dashboard mapping tests also failed before their conflict handling
was added.

Validation passed: scoped make f, make l across all ten modules, root build,
API and dashboard contract race suites, and full engine/run/suspension race
suites. The final engine run passed in 211.121s. Full PostgreSQL and MongoDB
race suites passed in 54.250s and 91.775s without skips. The five-minute engine
and ten-minute SQLite package limits were reached during fresh fixture setup,
with active subtests only one or two seconds old. Both longer reruns passed;
the standalone SQLite race suite completed in 305.045s without skips. The final
claim-content regression also passed on all three backends after formatting.

Atomic report consumption, result/outbox publication and runtime verification
activation remain open. S2.2 and the full plan are incomplete.


### S2.2 atomic result storage

Run revision commit `6ca32fe` passed CI run `39905046212`.

Built-in stores now expose optional `verification.CommitStore`. You supply a
trusted current binding, expected run revision, saved contract ID and saved
report IDs. The store consumes those records, validates retained evidence bytes,
compares the candidate with saved run output and requires every required check
to pass. The saved run must be completed with a final answer, pending verification
and the same prepared digest and acceptance contract.

The receipt, metadata-only outbox and run update share one transaction. The run
revision advances once. Same-request retries return the original historical
receipt and stable event ID, including after evidence is later revoked. Reusing
the result ID with changed input fails. The outbox contains typed identifiers and
a binding digest, with no candidate text, principal or source URI. It has no public
stream sequence; public event replay and dispatch remain S2.6 work.

SQLite acquires its write lock before reading. PostgreSQL locks the exact run and
holds assurance row locks until commit. MongoDB uses snapshot transactions with
majority writes and an internal evidence write fence, so revocation after the
initial snapshot forces a retry and recheck. Standalone MongoDB returns
`ErrResultCommitUnavailable`; there is no partial-write fallback. Request and
receipt encodings are bounded to one MiB, with at most 256 report IDs and eight
MiB of combined report JSON and evidence bytes processed per attempt.

This is a trusted storage boundary. You must hold the host's current authorization
and revision fence while calling it. The database cannot establish freshness in
an external service, and `ObservedAt` records the validation observation rather
than a promise of freshness at physical commit or later delivery. Runtime
integration must supply that host guard before enabling verified publication.
Existing engines and transports do not call this operation yet.

TDD first failed on the missing capability, then on the absent transaction.
Shared tests cover exact receipt identity, conflicting retries, scope and
cancellation denial, stale authority/policy/source bindings, unknown or duplicate
reports, failed and expired reports, changed output and run revisions, revoked
evidence and concurrent commits with one winner. Real transaction tests inject
failure after both writes and assert complete rollback. PostgreSQL and MongoDB
also reject revocation between initial reads and evidence validation. A real
standalone MongoDB fixture confirms the explicit unsupported error. SQLite
reopen coverage checks receipt recovery and retry after a lost acknowledgement.

Validation passed: scoped make f (store and verification), exact root-file
formatting, root build, all ten modules in make l, verification race tests and
full backend race suites. SQLite completed in 268.591s, PostgreSQL in 27.473s
and MongoDB in 93.374s. Focused verbose runs exercised the real PostgreSQL,
MongoDB replica-set and standalone MongoDB containers. SQLite reopen also passed
in a separate focused run. These are storage checks, not runtime or live sandbox
qualification.

Host authorization fencing, verification job recovery, engine gates/hooks and
public event dispatch remain open. S2.2 and the four-stage plan are incomplete.


### S2.2 guarded host commit boundary

Atomic storage commit `d9f29e4` passed CI run `39907241822`.

Ruling: require an explicit host fence in addition to `BindingAuthorizer`.
A standalone permission check cannot prevent a concurrent revocation before
storage commits. The host owns grant and resource revision writers, so a generic
process mutex in Cortex would give a false guarantee across host replicas.
The cost is an explicit host adapter before runtime activation; there is no
unfenced fallback.

`verification.NewCommitter` now combines `CommitStore`, `CommitFence` and the
existing `BindingAuthorizer`. It freezes caller inputs, checks exact scope and
current binding, authorizes inside the fence and keeps that fence around the
atomic storage call. It validates and detaches the receipt before returning it.
Historical retries are reauthorized. Storage read access remains a separate
host audit decision. A final fence error can follow a committed write, so callers
must reconcile the same request and result ID after an ambiguous response.

The host callback must run once and synchronously. Missing, repeated, retained
and escaped callbacks fail; escaped work receives cancellation. Suppressed
callback errors cannot become success. This detects adapter protocol failures
but cannot repair a host guard that silently releases locks or excludes another
writer. Production adapters must prove their own cross-process coverage and
external observation or conditional-operation policy.

TDD began with the missing committer API. Race tests cover guard lifetime,
current-grant denial, changed bindings, exact scope, input ownership, cancellation,
nil ports, invalid receipts, ambiguous fence errors and callback protocol misuse.
Shared backend fixtures hold a host policy guard across a real commit, queue a
revocation against that guard, and deny an idempotent retry once revocation takes
effect while preserving the historical receipt for separately authorized audit.
The fixture is a host policy model, not a production authorization provider.

Validation passed: scoped make f, make l across all ten modules, root build,
full verification race tests and the complete verified-result test group on
SQLite, PostgreSQL and MongoDB. The final backend runs completed in 10.772s,
6.845s and 6.926s respectively, with real containers and no skipped tests.
Published documentation passed `pnpm types:check`, `pnpm lint` and `pnpm build`.

Host deployment adapters, durable verification jobs, engine gates/hooks and
public event dispatch remain open. G1 is still required before runtime activation.


### S2.2 durable verification jobs

Guarded commit change `235a6ec` passed CI run `39908257458`.

Ruling: persist one job per captured candidate/contract attempt and bind it to the
pending run revision. Keep reports as they finish, and require the current job
lease at result commit. A worker replacement can recover accepted checks without
regenerating the candidate; a stale worker cannot add reports or commit success.
The cost is a host-approved new job when accepted checks need to be repeated.
Ownership attempts are bounded separately from evaluator budgets, which the
runtime still needs to enforce.

`verification.JobStore` now provides exact-scope enqueue, lookup, bounded listing,
claim, renewal, report acceptance, expiry and cancellation. Enqueue checks the
saved completed final answer, prepared digest, acceptance contract, candidate
bytes and retained evidence. It saves the job, run-revision binding and pending
run update together. An identical enqueue is idempotent. Deadline and ownership
attempt limits are explicit; lease durations are capped at one hour and the job
deadline.

Claims use unique IDs and increasing generations. Same-claim retries recover a
live lease without spending another attempt. An initial regression showed that
replaying an older claim after a replacement lease expired could consume another
generation. Persisted consumed claim IDs now reject that replay. Renewal cannot
revive an expired lease or exceed the deadline. Storage checks its saved expiry,
owner and generation rather than trusting worker token fields.

Report acceptance checks the live lease, current run revision, immutable binding,
contract and freshness. The report and job progress share a transaction. Accepted
criteria cannot be overwritten by a favorable report. Required failure or
inconclusive verdicts end the job and update run verification; completed execution
keeps its original meaning. All required passes make the job ready. Result commit
then requires that job's exact accepted reports and live lease, and saves the
committed job with the result, outbox and run update. Manual commits remain
available only when that run revision has no job binding.

Result and job operations share one run transaction adapter. SQLite acquires its
write lock first; PostgreSQL holds the run and assurance row locks. MongoDB writes
an internal run fence so a snapshot-based job claim conflicts with concurrent run
updates, in addition to its evidence fence. Multi-document transactions remain
mandatory. Failed transactions return no snapshot or receipt.

Tests cover enqueue and claim retries, scope and cancellation denial, partial
report recovery, ownership takeover, renewal, stale report/result rejection,
missing commit leases, ready/committed transitions, deadlines, exhausted attempts,
failed and inconclusive checks, cancellation and four competing claimants. Real
transaction fixtures inject failure after enqueue, report acceptance and final
commit, verify complete rollback, then retry successfully. SQLite closes and
reopens its database after a partial report and resumes the same claim without
spending another attempt. Pure value tests check request bounds, detached data,
snapshot integrity, claim history and impossible ready states.

Review regressions reproduced two jobs waiting until deadline unnecessarily:
a verifier reported a pass without contract-required proof, and an accepted
report expired while another check was running. Report acceptance now applies
the aggregate contract verdict to all accepted required criteria. Missing proof,
human decisions or required source observations end verification as inconclusive;
expired reports record stale verification. Criteria still awaiting their first
report keep the job running.

Full verification, SQLite, PostgreSQL and MongoDB race suites passed. Database
suites took 221, 61 and 23 seconds respectively. Docs types, lint and production
build passed, along with root build and the ten-module lint gate. After the policy
fix, affected result/job race checks passed on SQLite, PostgreSQL and MongoDB
without skips, including missing proof, missing human decisions, missing sources
and expired accepted reports. Final scoped formatting, root build, ten-module
lint and diff checks passed.

Upgrade every writer before activation. Older binaries do not enforce job
ownership. These are trusted storage APIs, not public queue routes; production
host authorization, clock configuration, evaluator dispatch, budget accounting,
atomic engine finalization and public event delivery remain open. The scheduler
must recover the window between captured output and job enqueue without rerunning
generation or tools. G1 still gates runtime activation, and S2.2 remains active.


## Evaluator dispatch and conservative accounting

Baseline `5d14304` is pushed; CI run `39911840103` passed. This packet extends
verification storage without activating engine verification gates.

S2.2 ruling: one verifier name/revision group may be dispatched once within a
budgeted job. Its durable call ID, assigned criteria and reserved allowance are
saved before the provider call. A replacement worker may reconcile a retained
outcome under its current lease, but changing the call ID cannot authorize a
repeat. This follows the job's immutable accepted-criterion policy. The cost is
that a lost acknowledgement before any provider invocation can consume an
allowance until the host establishes what happened; safety takes precedence over
a speculative retry. A new job still requires host approval and remaining budget.

`JobRequest.EvaluationBudget` opts into bounded call, token and money accounting.
Money accounting requires a pinned pricing revision; `TokenOnly` explicitly opts
out of a monetary ceiling. A zero token limit permits zero-token checks only.
`CallStore.BeginVerificationCall` acquires the same run transaction as job
ownership, checks the current lease and remaining allowance, derives the assigned
criteria from the saved contract and records dispatch. A committed begin grants
one invocation. Repeated begins return `ErrCallUncertain` and never grant another.

`CompleteVerificationCall` detaches and validates the result, requires every
assigned criterion exactly once, and saves usage, report and job progress in one
transaction. Budgeted jobs reject direct report acceptance. Report validation and
terminal-state decisions share the existing helper. Already dispatched optional
checks remain pending until they settle, even after the required checks pass.
An explicit evaluator failure records inconclusive verification. Unknown usage
retains its full reservation; known usage requires a host-attributed receipt.
Actual overruns remain visible and block later dispatch. Arithmetic saturates
instead of wrapping. These are admission limits, not a provider billing guarantee.

Tests first failed for the absent API, then for dispatch against the unimplemented
store. Behavioral regressions also reproduced required-report persistence failing
while an optional call was active, and a snapshot accepting a failed evaluator
without terminalizing its job. Those cases now pass. Shared backend tests cover
scope denial, changed receipts, wrong reports, unknown verifiers, overruns,
concurrent calls competing for the same allowance, duplicate dispatch, lease
takeover, transaction rollback and SQLite database reopen. A lost acknowledgement
survives restart and still refuses another begin. Pure tests cover unknown versus
known-zero usage, budget bounds, overflow, detached snapshots and invalid history.

Final focused verification/job/result race checks passed on all three backends,
including real PostgreSQL, MongoDB transactions and standalone MongoDB refusal.
Scoped formatting, the ten-module lint gate, root build and diff checks passed.

The full verification and all three backend race suites also passed: SQLite in
227 seconds, PostgreSQL in 23 seconds and MongoDB in 22 seconds. Docs type
generation, type checking, lint and the production build passed. The final
focused run includes the later scope-denial and competing-budget cases.

Remaining integration: the evaluator executor, trusted allowance/receipt adapters,
provider reconciliation, allocation from the remaining run budget, captured-output
recovery and public delivery. Terminal jobs retain unresolved reserved charges;
a late-receipt accounting path remains open and those charges must not disappear
when the host allocates a later job. Live provider/scanner qualification and G1
still gate activation. This packet does not close S2.2 or the four-stage plan.


## Evaluator execution and recovery

Baseline `2ed08db` is pushed; CI run `39913532675` passed. This packet connects
budgeted verification jobs to an explicit host evaluator adapter. Engine gates
remain disabled.

S2.2 ruling: the executor advances one contract-defined verifier name/revision
group per invocation under an existing lease. Scheduling, claiming and renewal
stay outside it so synchronous workers and later parallel schedulers can use the
same implementation without copying ReAct. The cost is that a host must provide
the scheduling policy before this becomes an automatic verification service.

`verification.NewExecutor` requires durable storage, an evaluator adapter, current
binding authorization and a trusted clock. It validates and detaches the saved
job and contract, obtains a conservative estimate, records dispatch, then invokes
the adapter. Pricing revisions must match the job. Missing dispatch records,
changed call identity, wrong evidence bindings and mismatched reports fail closed.
The executor validates the saved settlement before returning it. Original run
output, generation and tool execution are not invoked through this interface.

Recovery uses a separate `EvaluationAdapter.Reconcile` method with the original
call ID, criteria, allowance and job deadline. Completed calls return their saved
state after current authorization checks. Provider errors discard partial output
and retain the conservative charge. Definitive evaluator failures use the typed
failure result; a missing outcome remains uncertain. Reconciliation cannot do
billable work or fall back to another evaluation. Charged recovery needs its own
host reservation, which remains a separate integration requirement.

Authority is checked around adapter access and before returning persisted data.
A shared `Job.CheckLease` supplies the store and executor ownership rules. Contexts
are bounded by caller, saved lease and job deadlines, and trusted time is checked
after host callbacks. The executor does not claim that cancellation physically
stops a remote effect or uncooperative callback. Qualified adapters must honor
limits, retain authenticated receipts and enforce actual resource access.

TDD evidence: the initial store-backed executor test failed against the absent
behavior, then passed through dispatch and settlement. New regressions reproduced
reservation after expiry during estimation, evaluator invocation after expiry
during authorization, and a completed-call read returning after grant revocation.
All three now pass. Tests also cover missing/typed-nil services, cancellation,
revocation, wrong prices, wrong call/binding, plausible partial provider errors,
callback input mutation, missing dispatch receipts and lost acknowledgements at
begin, provider response and settlement. SQLite reopen recovers the retained
provider outcome without another estimate or evaluator invocation. Shared tests
run against real PostgreSQL and MongoDB as well as SQLite.

Final focused executor race suites passed on all three backends, including the
latest revocation regression. Scoped formatting, ten-module lint, root build and
diff checks passed. Full verification and backend race suites also passed:
SQLite in 240 seconds, PostgreSQL in 21 seconds and MongoDB in 20 seconds.
Final docs type generation, type checking, lint and production build passed.

Remaining work: qualified provider adapters and their receipt provenance,
late-receipt accounting, cross-job/run budgets, worker scheduling and renewal,
captured-output/enqueue recovery, engine sync/stream/prepared/resume integration,
completion hooks, public delivery and installed qualification. The full S2.2
packet and four-stage goal remain open.

## Explicit engine verification worker

S2.2 ruling: add `WithVerificationWorker` and `Engine.VerifyRun` as an explicit
host operation before installing the automatic publication gate. This connects
the existing durable job, evaluator and guarded committer without changing normal
run completion. The full S2.2 runtime and hook checkboxes remain open.

The host supplies saved candidate evidence, its exact contract/context binding,
a reserved evaluation budget, claim identity and stable result ID. Construction
requires all trusted host ports and optional durable store capabilities, in either
option order. There is no default evaluator, fence or in-memory job store.
Malformed claims and result identities fail before enqueue. Returned ownership
must match the requested claim ID and owner before evaluation starts.

Each unique verifier name/revision group runs in contract order. Required-check
readiness stops further dispatch, so later optional groups are skipped. Already
dispatched optional checks still have to settle. The worker returns explicit
failed, inconclusive or cancelled job states without a success receipt. Budget
exhaustion retains partial reports and conservative charges for recovery.

Uncertain evaluator calls reconcile the original operation. Commit recovery
checks the stored receipt against the job and reuses its original request through
the host fence. Current authorization applies before work, at commit and before
returning saved outcomes, including historical retries. Errors may follow durable
writes; the same job and result identities must be reused. Lease renewal, expiry
sweeping and cross-scope discovery are not installed by this API.

TDD evidence: tests first failed against the missing engine API. A later
regression reproduced acceptance of a different returned claim ID, then passed
after checking requested ownership. Real SQLite tests execute a prepared agent
and tool, verify its captured output and reopen the database after lost evaluator
or commit replies. Generation and tool counters remain unchanged during retries.
Other cases cover required-check failure, terminal outcomes, wrong scopes,
incomplete or changed candidates, expired jobs, missing/typed-nil ports, revoked
authority around commit, deleted evidence, changed historical receipts, duplicate
verifier groups, bounded dispatch and detached authorization inputs.

Final verification passed: scoped `make f`, all ten modules through `make l`,
root build, diff checks, and race suites for engine, verification, execution, run
and suspension. The engine suite completed in 126 seconds. Docs type generation,
type checking, lint and production build passed after the final content edit.
These fixtures do not qualify a production evaluator, host fence, provider,
scanner or Linux sandbox.
Automatic candidate capture/enqueue recovery, shared run budgets, scheduling,
sync/stream/prepared/resume publication gating, completion hooks and public event
delivery remain open. The contextual-verification capability remains false.

## Atomic saved-candidate capture

S2.2 now exposes optional `verification.CaptureStore` on all built-in stores.
`CaptureVerificationJob` reads the completed run's output inside the same
transaction that creates candidate evidence, the job/revision binding and the
pending run update. It consumes the existing job request and shares enqueue's
validation and idempotency path. No candidate bytes come from the caller.

Capture requires matching run revision, prepared digest, contract and output
digest/length. The candidate is bounded to one MiB. Existing identical evidence
is reused; conflicting metadata and durable deletion tombstones reject new
capture. Historical retries return the saved job without reconstructing removed
bytes. The raw store capability requires the host to authorize the run read,
evidence write, retention, sensitivity and current verification binding.

TDD evidence: the first conformance test failed on the absent capability, then
passed through capture and idempotent enqueue. Shared backend cases cover scope,
cancellation, changed output/revision, incomplete runs, missing contract or
supporting evidence, expired retention/deadline, oversized candidates, tombstones,
conflicting/preexisting artifacts, repeated capture and competing jobs. Failure
injection after each evidence, job, revision-index and run write proves atomic
rollback; retries reuse the same identities. SQLite reopen preserves the capture
and candidate bytes. Drivers without a real run transaction fail closed.

This operation closes the separate evidence-upload/enqueue write gap for an
already saved candidate. It does not persist intent before generation or connect
automatic finalization, publication, shared budgets or scheduling. Those S2.2
requirements remain open. No schema migration or composite store widening is
needed.

Final checks passed: scoped `make f`, all ten modules through `make l`, root
build and diff checks. Full race suites passed for verification and all three
backends: SQLite in 246 seconds, PostgreSQL in 27 seconds and MongoDB in 34
seconds. Verbose focused capture race tests passed against real PostgreSQL and
MongoDB with no skipped cases. Docs type generation, type checking, lint and
production build passed after the final content edit.

## Durable capture intent

S2.2 now has immutable `verification.CaptureIntent` and optional
`CaptureIntentStore`. The intent fixes a budgeted job template, result identity
and maximum candidate size before output exists. The candidate starts with a
zero-byte/empty-digest placeholder; its remaining metadata and context stay
pinned. The candidate creation time records the host's identity reservation.
`JobFor` changes only the later run revision, hash and byte count and detaches
its returned request.

Registration requires the exact pristine running run, its preparation and
acceptance contract, retained supporting evidence, a live deadline bounded by
the saved run deadline, and an unused candidate identity. Existing evidence and
tombstones reject registration. The intent and unique exact-scope run/job/result
indexes share one transaction. Same-request registration remains idempotent after
execution; it does not grant permission to execute again.

Recovery looks up the intent by run ID. `CaptureFromIntent` reads the completed
run and derives its candidate/job inside the same transaction as capture and
enqueue. The enqueue implementation is shared with direct capture. Normal enqueue
and commit validate pinned intent policy and result identity. Historical capture
retries validate the saved job and never recreate deleted evidence. No schema
migration is needed, but every writer must upgrade before enabling this protocol.

TDD evidence: domain tests failed against the absent intent types, and store
conformance failed on the missing capability. Later regressions reproduced
registration beyond the saved run deadline, registration of existing or
tombstoned candidate IDs, and insufficient run-revision headroom; those cases
now reject. Tests cover policy detachment,
changed templates, competing run/job/result identities, immutable result binding,
rollback after each registration/index write and during capture, and SQLite
reopen before final output capture.

The host must register before external dispatch. Stored zero counters do not
prove that ordering, and storage does not authorize access or reserve shared
budgets. Engine preparation, finalization, streaming/resume withholding and
public completion hooks still need integration. The automatic contextual
verification capability remains false.

Final checks passed: scoped `make f`, ten-module `make l`, root build and diff
checks. Full race suites passed for verification and all backends, with SQLite
in 282 seconds, PostgreSQL in 29 seconds and MongoDB in 39 seconds. Final focused
intent and exact-scope recovery race suites passed on all three backends with
no skips. The existing engine-worker recovery race suite passed in 45 seconds.
Docs type generation, type checking, lint and production build passed.

## Verification admission checks

Ruling: close two admission gaps before connecting required execution gates.
The intent fixes a result identity and deadline before generation, but capture
must still respect a subsequently shortened run deadline, and a worker should
reject the wrong reserved result identity before it spends evaluator budget.
Commit checks remain necessary for concurrent changes and custom stores.

TDD reproduced both gaps. Direct enqueue, atomic capture and intent recovery
accepted deadlines beyond the current saved run limit. The worker estimated and
evaluated absent, queued and running jobs with a wrong reserved result ID before
commit rejected them. The shared enqueue transaction now checks the deadline
before evidence or job writes. The explicit worker reads a supported store's
intent after authorization and rejects invalid scope, run or result identity
before enqueue or dispatch. Intent read errors stop processing.

Regression coverage includes equal, later and unlimited run deadlines, unchanged
run revision and absent evidence/job writes after denial, valid result recovery,
historical committed retries, corrupted intent reads, authorization before reads
and custom stores without the optional intent reader. Automatic required gates,
private candidate storage and safe public delivery remain open.

The real MongoDB run exposed a related precision bug: BSON dates rounded the run
deadline to milliseconds while the sealed intent retained nanoseconds. Equal
deadlines then failed admission. A prepared-budget round-trip regression also
failed. Run and suspension models now preserve an exact deadline alongside the
native date and validate agreement when reading. Legacy documents retain their
existing precision, malformed or inconsistent companions reject, and update can
narrow or clear both values. This requires every writer to understand the new
companion field before relying on exact policy restoration.

Final checks passed: scoped `make f`, ten-module `make l`, root build and diff
checks. Full engine and verification race suites passed; the final worker suite
passed again in 57 seconds. Selected capture, job, call, deadline and prepared
conformance races passed on SQLite in 80 seconds and PostgreSQL in 17 seconds.
The full MongoDB race suite passed in 33 seconds. Verbose database logs contained
no skips. Docs type generation, type checking, lint and production build passed.


## Private candidate capture and commit

Ruling: a required runtime gate needs private persistence before automatic
finalization can use the verification worker. The previous capture path starts
from an already public run output. Keep that explicit legacy path and add an
atomic private transition under the same capture intent, job, lease and commit
protocol, with no second evaluator or publication queue.

`run.CandidateCapture` binds the final output, saved running revision, finish
reason and cumulative usage to a retry digest. Built-in stores expose optional
`run.CandidateStore`. Capture validates the registered intent, current preparation,
accounting and limits; saves bounded candidate evidence and immutable metadata;
then completes execution and enqueues verification in one transaction. The run's
public output stays empty. Existing job validation checks the private candidate
reference and execution metadata instead of expecting its bytes in the run row.

Passing reports alone do not release content. The guarded result commit copies
the exact retained candidate bytes to the run in the transaction that records the
receipt, outbox, result reference and passed status. Failed, inconclusive,
cancelled, stale or revoked work cannot take that transition. Capture retries
validate the original request digest and return historical job state without
recreating tombstoned evidence. No schema migration or composite store widening
is needed; all verification workers and writers must upgrade before use.

TDD first failed on the missing capture request and store capability. The next
regression showed that a passed commit still left the private output empty, then
passed after atomic publication was connected. Conformance covers hidden output
in run reads/lists, accounting and deadline denial, scope, cancellation, changed
identity/content, failed checks, tombstones, concurrent captures, rollback after
each capture write and each commit write, and lost replies after both commits.
SQLite reopen and explicit engine-worker evaluator recovery use the same saved
candidate without new generation or tool calls.

This is the persistence transition required by S2.2, not the automatic runtime
gate. Prepared policy versioning, pre-dispatch registration, stream/step/history
withholding, completion hooks, shared budget allocation and scheduling remain
open. Contextual verification capability stays false.

Final checks passed: scoped `make f`, ten-module `make l`, root build and diff
checks. Full race suites passed for run, verification and engine, with engine in
145 seconds. All backend race suites passed: SQLite in 320 seconds, PostgreSQL
in 44 seconds and MongoDB in 53 seconds. Final focused private-capture races
passed again on all three backends after lint fixes, including real PostgreSQL
and MongoDB, with no skipped cases. Docs type generation, type checking, lint
and production build passed.


## Required verification runtime, initial tool-free path

Implemented `WithRequiredVerification` across normal/prepared synchronous and
streaming execution. Schema 3 freezes contract identity, policy revision and
evaluator allowance. Schema 2 ordinary snapshots remain compatible. Generation
receives the total token budget minus the frozen evaluator reservation; host
capture intent is registered before any model call.

Private capture and the guarded worker now run in shared finalization. Candidate
text is withheld from token events, generation steps, conversation history and
completion hooks until commit. Required history remains unpublished even after
commit. Failed/inconclusive checks return `ErrRunUnverified`, uncertain failures
return static errors, and A2A cannot expose an unverified required output artifact.
Capture revocation fails the run while CAS preserves an uncertain durable capture.

Tools, resume and checked-segment publication fail closed in this first path.
They remain implementation work, alongside durable result hooks/history delivery,
scheduling, lease renewal, repair and recovery before candidate capture. The
option is experimental and off by default. G1 and production contextual-verification
qualification remain open; capability advertising stays false.

Regression tests cover registration ordering, token reservation, private surfaces,
sync/stream rejection, policy downgrade, missing services/budgets, unsupported
paths, capture revocation, completion-hook ordering and SQLite restart after a
lost evaluator reply without repeated generation or evaluation. The A2A regression
failed before its projection fix. Constructor/API tests first failed for the absent
runtime option, and capture-revocation testing exposed the stranded running state.


Validation for this packet:

- Full engine/run/verification race suites passed; engine took 185.377s. After
  the final concurrent-capture correction, all required-verification race tests
  passed again in 42.615s, and terminal-outcome regressions were rerun.
- SQLite, PostgreSQL and Mongo private-capture, rollback and prepared-metadata
  race tests passed in 42.798s, 13.619s and 16.042s. Both container backends ran;
  the log contains no skips. Required policy markers survive backend round trips.
- A2A and gRPC binding race suites passed in 6.136s and 1.305s. Docs type checking,
  lint and production build passed. Formatting, all ten module lint checks, root
  build and diff checks passed on the final code.
- A concurrent owner could previously capture a candidate while generation was
  still running and make finalization return success with pending verification.
  The new regression failed first; required completion now rejects that state.

These checks qualify the local implementation and fixtures only. Live provider,
scanner, sandbox and installed-host qualification remain open.
