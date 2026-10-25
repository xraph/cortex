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
| S2.2 | active | this chat | Atomic result/outbox storage, guarded commit API, leased jobs, budgeted dispatch and evaluator executor implemented; experimental runtime gate binds inputs and private transcripts across tools and resume; scoped recovery and callback renewal implemented; qualified host adapters, durable effect recovery, multi-scope worker deployment and durable public delivery remain open |
| S2.4 broker/runtime | broker, worker, transport, protected providers, input provisioning, configured assembly and recovery implemented; deployment pending | this chat | Race and Linux fixtures cover staging, capture, attestation, input delivery and independent service discovery; standalone broker command, complete installation and live qualification remain open |
| S3.1 | active | this chat | Bounded graphs, immutable backend revisions, task contract projections, dependency evidence, effect/source manifests and pure strategy adapters implemented; runtime integration remains open |
| S2.3 and Stage 4 | active | optimization owner | Trial execution and recorded Sentinel optimization adapters are committed in `7f96edf` and `c6b5483`; remaining integration and qualification work is tracked in the optimization progress ledger |
| S2.6 | active | this chat | Atomic lifecycle replay, REST/dashboard pages, bounded SSE, A2A pages and remote subscriptions, ordinary terminal recording, verified-result artifact announcements, fresh snapshots, guarded host content reads and HTTP/dashboard/A2A content adapters with protected historical projections implemented; arbitrary artifact producers and installed qualification remain open |
| S2.7 | active | this chat | Guarded React result views and durable metadata polling implemented and fixture-tested; installed host qualification remains open |
| S2.5 | active | this chat | Registered manifests, separate candidate/grader dispatch, broker authentication, contextual state observations and approved local export implemented; concrete supervisor, durable evaluation recovery and live qualification remain open |
| S3.4 | active | this chat | Durable effect reservations, provider receipts, engine handlers and bounded exact-scope discovery/reconciliation implemented; continuation, retries, compensation, conflict handling and worker fencing remain open |
| S3.3 | active | this chat | Sealed handoffs, transport bindings, durable submissions/reviews, atomic verified task acceptance and its guarded host entry point implemented; review adjudication, concrete host grant fences, scheduler integration and installed qualification remain open |
| S3.2 | active | this chat | Durable ownership, guarded readiness/capacity admission, fair bounded scheduling, prepared engine worker dispatch, claim renewal storage, expiry and verified completion implemented; automatic renewal, concrete host services, lifecycle/event delivery and recovery qualification remain open |
| S3.5 | active | this chat | Transactional parent/child reservations, conservative settlement and pinned monetary accounting implemented; bounded proposal policy and shared-account validation implemented; runtime accounting and automatic replan execution remain open |
| S3.6 | queued | shared owners | Production verification rollout remains disabled pending G1 |

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


## Required tool execution and private handoffs

Schema 4 now carries required verification through tools, external suspension,
approval checkpoints and sync/stream resume. The prepared payload remains the
single source of verification policy. Before a claim, the engine checks frozen
inputs/accounting, capture intent, current handoff/evidence grants and matching
host configuration. It checks again after claim and keeps live tool authorization.

`PendingVerificationTools` gives authorized hosts a private handoff read. Ordinary
streams and dashboard detail omit pending IDs/arguments. Public tool rows/hooks
omit arguments, results and private errors; unknown provider-supplied tool names
use fixed metadata. The authorized handler and model retain the full inputs.
Malformed resume errors cannot disclose private missing-call IDs.

Focused tests cover local sync/stream tools, external sync/stream resume, SQLite
reopen, handoff revocation, approval exactly once under repeated resolution,
changed policy, missing authority, budget alteration, schema downgrade and public
hook/row/dashboard privacy. The first test run failed for the absent handoff port
and reader. Existing required tool-free behavior stays covered.

This does not promise crash-atomic tool effects. Durable effect receipts, qualified
host adapters, result delivery/history, scheduler, lease renewal, repairs and G1
production qualification remain open. Checked-segment publication stays rejected.


Validation for the tool/resume packet:

- The full engine/run/verification race suites passed (engine: 200.603s).
  Required-verification race tests passed again after the final error-marker
  change in 59.298s. A new behavioral regression first failed because redaction
  left failed/denied tool rows without an error marker; they now retain a fixed
  failure or denial message without private content.
- Extension and dashboard-contract race suites passed (1.921s and 17.471s).
  SQLite/PostgreSQL/Mongo private-capture and prepared-state race checks passed
  (41.694s, 13.920s and 16.990s); container tests ran without skips.
- Final formatting, all ten module lint checks, root build, diff checks and docs
  type/lint/build checks passed. CI for the preceding commit `4322520` is green.

Next runtime audit: bind per-run input/session/history evidence and exact
model/tool revision records into verification context, and check host transport
error semantics. The current host builder owns context provenance; passing
fixture checks does not qualify those host integrations. These items remain open
alongside crash-atomic tool effects and durable result publication.


## Required execution-input binding

Required schema-5 runs capture the scanned task and initial history, agent,
session, prepared digest and host model/tool pins in one immutable evidence
artifact before generation. The intent builder receives detached inputs and
chooses the artifact's retention and sensitivity. Cortex checks its exact bytes,
authorizes creation and saves it. Missing revision pins or unavailable history
stop before generation; ordinary schema-2 execution keeps its existing behavior.

The binding references the artifact through its existing support-evidence list.
Storage registration, private capture, evaluator claims and result commit compare
saved run identity against those bytes. Resume checks the initial conversation
and current pins before claiming, then repeats the check after claim. Deleted
evidence and altered pending tool names reject resume without consuming the pause.

The first domain tests failed for the absent input schema. Behavioral storage
tests then showed that changed task/session/agent/model/tool values and deleted
input evidence were accepted. Those regressions pass with shared validation.
Engine fixtures cover builder mutation, missing/changed/denied evidence, scanned
input/history, missing revision pins, unavailable history and continuation drift.
Existing SQLite reopen and required sync/stream/tool tests consume the new input
artifact through the same runtime.

Transport error semantics still need their own pass. Tool-result evidence,
crash-atomic effects, durable publication/history, scheduler/lease renewal and
live host/provider/sandbox qualification remain open. Contextual-verification
capability stays disabled.


Validation for input binding:

- Full engine/run/verification race suites passed (223.681s, 1.147s and 1.482s).
- SQLite, PostgreSQL and Mongo input-binding/private-capture/intent conformance
  race suites passed (89.844s, 19.560s and 25.983s), with no container skips.
  PostgreSQL first caught a fixture referencing an absent agent; the fixture now
  creates that agent before testing input-binding rejection.
- Initial and final all-module lint, final formatting, root build and diff checks
  passed. Docs type checking, lint and production build passed. Three local
  shadowing warnings were fixed before the final lint run.
- CI for preceding commit `50c4005` passed (run `39933521329`).


## Verification failure transport semantics

The runtime, REST API and dashboard share a safe verification failure projection.
It preserves stable failure code and available run identity/state without output
or wrapped diagnostics. REST maps rejected verification to 409 and unavailable
verification to 503. Dashboard commands use conflict/unavailable and preserve run
identity through the generic audited command wrapper. Neither advertises retry;
SSE errors include the same recovery fields.

Tests first reproduced missing stream failure codes, premature SSE headers on
startup failure and a provider left running after an SSE writer disconnected.
The handler now sets headers after startup and cancels only its owned context.
A lost evaluator reply remains pending with its durable job identity discoverable
from the streamed run ID. That regression checks that generation and evaluation
are not retried automatically. Generic dashboard command dispatch is covered too.

This completes the current public-error mapping pass. It does not implement
crash-atomic effects, tool-result evidence, a recovery scheduler or durable event
replay. Those remain required by S2.2 and the later coordination/delivery packets.


Validation for transport semantics:

- Full engine/run race suites passed (218.471s and 1.311s). Run tests passed
  again after the final JSON schema annotations (1.179s).
- API race tests passed on the final response schema (1.511s), including failed
  startup headers and provider cancellation after a disconnected writer.
  Dashboard contract race tests passed (18.624s), including audited command
  dispatch with a failed run and its recovery identity.
- Final formatting, all ten module lint checks, root build, diff checks and docs
  type/lint/build checks passed. No dependency changes were needed.
- CI for `914de4d` has passed docs, format, submodules, lint, security and verify;
  its root test job was still running at this packet's final check.


### S2.2 private execution transcript evidence

Required schema 6 captures a canonical `verification.ExecutionTranscript` with
checked model/tool messages, cumulative usage and host-recorded call outcomes.
The trace links to the initial input digest and distinguishes local dispatch from
an authorized external result. It is bounded to one MiB, 4,096 messages and 4,096
calls; each requested call needs one result and one outcome. Unknown tool names
are permitted only as locally denied calls without a tool charge.

Schema-2 capture intents reserve the transcript identity and retention before
generation. Capture derives only its content hash and byte count, adds the exact
reference to the verification binding, and saves it with candidate bytes, private
completion metadata and the queued job in one transaction. Retry identity includes
the transcript bytes. Claims and commit validate the input, usage and final
candidate again; deletion tombstones prevent publication.

Both ReAct loops record outcomes through the same helper. Suspensions retain
completed outcomes, so a reopened run can combine local and external results
without losing earlier work. Required resume rejects a scanner that changes
retained messages before further execution. Public rows, hooks and streams keep
the existing private-content boundary. Ordinary schema 2 and explicit schema-1
capture intents remain compatible; required schema-3/4/5 pauses must be drained
or cancelled on compatible workers before preparing fresh snapshots.

The first runtime regressions failed because no transcript was retained. A
second regression demonstrated silent task rewriting during resumed scanning.
Backend tests found that directly inserting over a tombstone aborted PostgreSQL
and MongoDB transactions before returning the deletion error. Candidate and
transcript capture now share the existing identity precheck.

External results still assert what the authorized host reports. This evidence
does not establish exactly-once effects, recover a crash before capture or supply
durable result delivery. Those remain open with worker scheduling, lease renewal,
repair and G1/G2 qualification. The four-stage plan is incomplete.

Validation for transcript capture:

- Full engine, verification, run and suspension race suites passed (235.080s,
  1.664s, 1.146s and 1.118s). Runtime fixtures cover local and external results,
  failures, denials, unknown tools, approval, streaming, reservation permissions,
  transcript limits and mixed local/external results across a SQLite reopen.
- The final required-execution and verification-worker race checks passed
  after sharing the capture helper (144.241s).
- Final shared storage race suites passed on SQLite (96.685s), PostgreSQL
  (19.624s) and MongoDB (26.136s). Docker-backed checks ran without skips and
  include transcript tombstones, changed retries, atomic rollback and the
  surrounding intent, candidate, job and result contracts.
- Final `make f`, all ten module lint checks, root build and diff checks passed.
  Documentation type checks, lint and production build passed.
- The previous commit `e5d9962` has successful CI (`39937788414`).


### S2.2 scoped verification recovery loop

Ruling: schedule saved jobs through the existing `VerifyRun` worker in an explicit
host scope. Do not infer tenant enumeration or a replacement result ID. The host
supplies a supervised service context; intent-less jobs remain explicit work.
This preserves the authorization and stable-identity boundary while adding
restart recovery after capture.

`RecoverVerificationPage` validates and detaches a bounded page before any paid
work. Each job passes current binding authorization. The cursor advances past
terminal, denied and busy jobs, so older terminal rows cannot trap the scan on
its first page. Cancellation during a partial page preserves the processed cursor.
`RunVerificationRecovery` carries that cursor between sequential scans, wraps at
the end and exits on cancellation or a discovery failure. Its optional observer
receives safe metadata, without candidate bodies or wrapped diagnostics.

Live leases remain busy even for a matching owner name. Recovery waits for expiry
before claiming a new generation, then reuses saved evaluator call IDs and the
intent's result ID. Tests reopen SQLite after a lost provider response and prove
that reconciliation does not repeat generation, tool execution or evaluation.
Lost claim acknowledgements wait for the existing lease; lost commit
acknowledgements are observed as terminal without another commit. Expired and
exhausted work becomes inconclusive without refunding unknown provider charges.

The API-absence test failed first. Behavioral regressions then caught cancellation
being lost during discovery, an incorrect partial-page cursor, and mutation of a
store-owned page by a callback. Each regression passed after its fix. Tests also
cover current authority denial, cross-scope reads, malformed and oversized pages,
missing capabilities, unmanaged jobs and cancellation while polling.

The loop is opt-in and is not launched by `Engine.Start`. It supplies single-scope
scheduling after capture. Host deployment, multi-scope supervision, lease renewal,
pre-capture recovery, durable effects, completion hooks/public delivery, repair and
production qualification remain open. The full four-stage goal remains active.


Validation for scoped recovery:

- Focused recovery tests passed (11.794s). The final focused race run passed
  (34.624s), and the full engine race suite passed (255.087s).
- Final `make f`, all ten module lint checks, root build and diff checks passed.
  Documentation type checks, lint and production build passed.
- CI for `99c9364` has passed docs, format, all submodules, lint, security and
  verify. Its root test job was still active at this packet's last CI inspection.
- No dependency or database schema changes were needed. Production evaluator,
  fence, worker deployment and Linux sandbox qualification remain unverified.


### S2.2 evaluator lease renewal

Ruling: renew only while an evaluator callback runs, under an explicit host
policy. Reuse the existing atomic store renewal and ownership generation. A
separate watchdog enforces the last acknowledged expiry, so a blocked renewal
cannot silently extend the worker's authority. A lost acknowledgement may stop
work despite a committed renewal; recovery retains the original paid call ID.

`LeaseRenewalPolicy` is optional on both the executor and engine worker. It bounds
lease duration to one hour and interval to half that duration. The executor copies
the policy, renews during estimation, evaluation and reconciliation, and joins the
renewal goroutine before storage transitions. Nil preserves fixed-lease behavior.
Job and caller deadlines still bound the operation. Current authority, request
binding and exact claim ownership are checked around renewal. Custom stores must
implement `LeaseRenewer`; the built-in stores already do.

The initial API tests failed before implementation. Long-callback tests now cover
estimation, evaluation and reconciliation across their starting lease expiry,
competing claims, stable generations and one-time settlement. Failure fixtures
cover revoked authority, blocked renewal, malformed ownership, regressed expiry,
lost and late acknowledgements, deadline expiry, caller cancellation and a provider
that returns a report after cancellation. Recorded calls remain unresolved in
these cases. No background renewal survives callback return.

Host callbacks must honor cancellation, and host ports must support concurrent
access. This packet does not supply idle-job renewal, pre-capture recovery,
durable effect recovery, public delivery, repair or production qualification.
The full four-stage goal remains active.


Validation for evaluator renewal:

- Full engine and verification race suites passed (286.887s and 1.247s).
  The final focused race run, including late renewal acknowledgements, passed
  after formatting (24.731s and 1.178s).
- The shared executor race suites passed on SQLite (34.921s), PostgreSQL
  (12.006s) and MongoDB (14.070s). Docker-backed tests ran without skips.
  The renewal case crosses lease expiry in both estimation and evaluation.
- Final `make f`, all ten module lint checks, root build and diff checks passed.
  Documentation type checks, lint and production build passed.
- CI for the preceding recovery commit `5a090dd` completed successfully
  (`39942247964`). No dependency or database schema changes were required.


### S2.6 durable public metadata replay

Ruling: reuse the existing run transaction for sequence assignment and committed
result events. Keep public envelopes limited to closed lifecycle types and stable
IDs. Current subscriber authorization belongs to the reader, independently of
historical result authority. This prevents a crash after result commit from losing
its metadata event while preserving the existing publication boundary.

`event.Store` supports atomic batches with expected run revision and sequence,
consistent scoped pages, watermarks and explicit pruning. The bounded window
retains 256 events per run. Permanent ID/digest/sequence-range tombstones keep
pruned identities from being reused or recreated by a delayed retry. Concurrent
appenders serialize on the existing run fence; only one expected-sequence write
wins. Invalid batches roll back envelopes and identity records together.

New result commits append their outbox ID in the same transaction as the receipt,
run and job. Existing receipt retries preserve historical behavior and do not
invent a missing event. Mixed old/new result writers therefore cannot guarantee
complete replay; upgrade all writers together. No schema migration is required.

The optional engine reader reauthorizes every page before and after storage,
validates cursor/schema/run identity, sequence completeness and bounded retention,
and detaches slices before invoking host code again. Failure returns no page.
Cursor gaps carry only a watermark and retained boundary after fresh authority.
The host must fetch an authorized current snapshot before resuming.

The API-absence tests failed first, followed by a behavioral failure showing the
result commit had no replay event. Both passed after implementation. Tests cover
SQLite reopen, lost-acknowledgement retry, backend transaction rollback, competing
appenders, changed batches, scope denial, automatic and explicit retention,
malformed pages, revocation during read, cancellation and callback alias mutation.

Automatic result events are implemented. Other lifecycle events still require
explicit host append calls. Approved answer-content replay, artifact events,
SSE/dashboard/A2A subscriptions, completion hooks and installed qualification
remain open. S2.6 and the full four-stage goal remain active.


Validation for metadata replay:

- Full engine, event and verification race suites passed (306.619s, 1.143s and
  1.260s). The final focused engine/event run passed after formatting (128.439s
  and 1.188s), including worker, recovery and lease-renewal regressions.
- Expanded storage race suites passed on SQLite (133.704s), PostgreSQL (26.533s)
  and MongoDB (33.709s). Docker-backed tests ran without skips. These include
  public events, result commits, verification jobs/calls, private candidates and
  transaction rollback. SQLite reopen preserves the event ID and sequence.
- Final `make f`, all ten module lint checks, root build and diff checks passed.
  Documentation type checks, lint and production build passed.
- CI for lease renewal commit `5b23804` completed successfully (`39944141714`).
  Replay transport and deployed subscriber policy remain unverified.


### S2.6 verification lifecycle producers

Ruling: record metadata where the run verdict changes, inside the existing
transaction. Private capture writes execution-finished at its completed revision
and pending at the enqueue revision. Reports, provider failures, cancellation and
expiry write terminal verdict events only when they update the current run.
A superseded job cannot publish a verdict over its replacement.

The shared append helper now serves lifecycle transitions and result commit.
Commit writes verification-passed immediately before result-committed, with the
same committed run revision and result identity. Becoming ready emits no pass:
current authority and freshness still need the guarded commit. Existing IDs and
sequences survive retries; a rolled-back transaction exposes no event or identity
tombstone. Unknown provider charges remain reserved after failure.

Tests first showed the missing private-capture and pending/terminal events.
They now cover ordered revisions, idempotent retry, verdict failure, cancellation,
expiry, stale accepted reports, private diagnostic redaction and superseded jobs.
Fault injection after event identity and stream writes checks private capture and
final commit rollback. Additional failures after report settlement, provider
failure and cancellation preserve the pending log and original job until retry.

The new passed/stale event types require readers and writers to upgrade together.
Historical events and receipts keep their original shape; there is no backfill or
database migration. Ordinary run producers, artifact events, approved content,
transport subscriptions and installed qualification remain open. The full goal
and S2.6 stay active.


Validation for verification lifecycle producers:

- Full engine, event and verification race suites passed (300.727s, 1.122s and
  1.239s). Focused engine/event regressions passed (74.501s and 1.180s), and final
  replay/type tests passed after formatting (7.482s and 1.099s).
- Expanded storage race suites passed on SQLite (138.267s), PostgreSQL (18.177s)
  and MongoDB (22.220s), with no Docker-backed skips. These cover lifecycle
  ordering, terminal verdicts, jobs/calls, private capture, result commits,
  rollback and SQLite restart.
- Final `make f`, all ten module lint checks, root build and diff checks passed.
  Documentation type checks, lint and production build passed.
- The metadata replay foundation `e351e09` has successful CI (`39946067588`).
  Transport and deployed subscriber-policy qualification remain open.


### S2.6 authorized paged replay transports

Ruling: durable cursors get their own protocol. `GET /v1/runs/:id/events` and
`runs.replay` v1 share one bounded replay adapter, canonical run-bound cursor
codec and static failure classifier. Existing `runs.events` offsets retain their
process-local meaning. No evaluator or execution path runs during replay.

The response includes the validated metadata page, an opaque next cursor and a
gap flag. REST uses HTTP 410 for a gap; the dashboard returns the typed envelope
without an error so its dispatcher preserves the recovery boundary. Both require
a fresh authorized snapshot before resuming. JSON sequence numbers can exceed
JavaScript precision, so transports provide a string cursor for continuation.

The reader labels authorization denial explicitly while preserving wrapped causes
for trusted Go callers. Public errors contain only static diagnostics. Corrupt
storage pages classify as unavailable, not a client input error. Authorization
still runs before and after each storage read, and failed checks discard all
metadata, including retention watermarks. REST disables response caching.

Tests first failed for the missing cursor API, REST route and dashboard intent.
They cover cursor canonicalization and overflow, cross-run rejection, resume,
retention gaps, malformed pages, revoked grants, scope isolation and private error
redaction. HTTP tests exercise the assembled Forge router; dashboard tests use
persisted SQLite events through the registered dispatcher. A private run output
is absent from replay. Forge optional binding tags are verified by requests that
omit the cursor and page size.

Paged replay is implemented. Live subscriptions, approved content, artifact events,
ordinary run producers, A2A/gRPC replay and installed reconnect/revocation checks
remain open. S2.6 and the full goal stay active.

Validation for paged replay transports:

- Final event race suite passed (1.169s), with focused engine replay regressions
  (7.625s). Full API race tests passed (1.542s); full extension and contract race
  tests passed (1.803s and 21.014s). Root, API and extension builds passed.
- Final `make f`, all ten module lint checks and diff checks passed.
  Documentation type checks, lint and production build passed.
- Lifecycle producer commit `fd0c385` has successful CI (`39947375269`).
  The new routes have local router/dispatcher evidence; deployed host grants and
  installed client behavior remain unqualified.


### S2.6 bounded SSE metadata subscriptions

Ruling: reuse the authorized page boundary and retain one page per subscriber.
`event.FollowReplay` now drains retained pages synchronously, then polls idle runs.
Every read passes through current subscriber authorization. Read deadlines and a
capped lifetime bound cooperative host callbacks; consumer failure, cancellation
or a retention gap stops the loop. No channel, detached producer or evaluator is
started by a subscription.

`GET /v1/runs/:id/events/stream` accepts the existing cursor and page size.
`Last-Event-ID` takes precedence over the initial query cursor when browsers
reconnect. Normal and heartbeat frames use the validated next cursor as their SSE
ID. Gap frames expose the authorized recovery boundary but omit an ID and stop,
so browsers cannot silently skip missing history. Error frames contain only
static failure metadata and stop. Consumers deduplicate IDs and process a whole
page before advancing their cursor.

API instances admit at most 64 subscriptions. Lifetimes are five minutes, idle
polls one second, read deadlines five seconds and write deadlines ten seconds or
the remaining lifetime. Unsupported deadline control fails before SSE headers.
The writer preserves wrapper flush behavior, then probes an error-aware inner
writer because Forge's void `Flush` wrappers can hide socket errors. A full-router
failure test exposed that issue before the fix. Host callbacks must cooperate
with cancellation; middleware must preserve deadline support.

The capacity test also exposed Forge's default 5xx sanitizer dropping the safe
replay code and retryability fields. Paged and SSE preflight errors now share an
explicit static response helper; private provider messages remain excluded.

Tests cover cursor precedence, foreign cursors, failed preflight, idle revocation,
gaps without browser cursor advancement, write and flush failures, read/lifetime
timeouts, slow-consumer backpressure and immediate backlog draining. A real HTTP
socket disconnect releases the slot. Concurrent admission/cancellation exercises
all 64 slots and rejects the next subscriber before storage access.

This packet does not qualify installed browsers/proxies or add answer-content,
artifact or ordinary-run producers. A2A/gRPC integration and remaining S2.6 work
stay open. The full four-stage goal remains active.

Validation for bounded SSE replay:

- Final event race suite passed (1.223s), as did focused engine replay tests
  (8.049s), the full API race suite (1.573s), and full extension/contract race
  suites (2.005s and 24.076s). Root, API and extension builds passed.
- Final `make f`, lint across all ten modules, diff checks, documentation type
  checks, lint and production build passed.
- CI for paged replay `fd9b938` failed in the existing long-evaluation renewal
  test (`39949024564`): concurrent SQLite writes returned `SQLITE_BUSY` and
  safely cancelled the evaluator. The worker fixture lacks the WAL/busy-timeout
  configuration already used by storage concurrency fixtures. This is being
  handled separately; the CI run is not green.


### SQLite worker fixture concurrency settings

CI `39949024564` failed the long-evaluation lease-renewal test when its competing
claim overlapped renewal. The worker fixture opened SQLite with no WAL or busy
timeout, so a writer received `SQLITE_BUSY` immediately. Verification correctly
cancelled the callback and kept the dispatched call uncertain. The run passed ten
local repetitions, confirming that local timing alone did not reproduce the
reported interleaving.

The worker fixture now retains a WAL/busy-timeout DSN for initial opens and restart
opens, matching the existing storage concurrency fixtures. Production transaction
and lease handling are unchanged. The SQLite guide now covers verification and
replay alongside messaging, including the fact that assurance reads acquire a
write lock for a consistent snapshot. Renewal still fails closed if storage cannot
acknowledge within the live lease.

Validation for the SQLite worker fixture correction:

- The long-evaluation renewal race test passed 20 repetitions (52.705s).
- Full engine, verification and event race suites passed (280.206s, 1.371s and
  1.191s), including worker restart, recovery, renewal and fail-closed cases.
- Formatting, lint across all ten modules, root build, diff checks and
  documentation type checks, lint and production build passed.
- The original CI failure remains recorded. A fresh CI run must verify the
  corrected fixture on Linux before this is treated as CI-qualified.


### A2A terminal delivery ordering

The shared remote stream emitted its final status before output artifacts. A
consumer that stopped on the terminal status could therefore miss the output.
The service now emits artifacts first and the terminal status last. A failed
artifact write stops without publishing final. HTTP and gRPC schemas are unchanged.

Regression tests first reproduced the missing output in a real SSE consumer that
closes on final, in the native gRPC binding, and in the shared service. A separate
failure test verifies that artifact-write failure never sends final. All bindings
use the same ordering implementation. This fixes current task-stream delivery;
durable A2A event-log replay remains open.

Validation for A2A terminal ordering:

- Full remote-module race tests passed (4.591s), as did the separate gRPC module
  (1.277s). Both modules built successfully. The gRPC module was checked
  explicitly because root CI does not run it.
- Final formatting, lint across all ten modules, diff checks and documentation
  type checks, lint and production build passed.
- Installed peer/proxy behavior and durable remote replay remain unqualified.


### A2A authorized metadata pages

The opt-in public-events extension serves bounded replay through `GetTask` on
REST, JSON-RPC and gRPC. Hosts must configure engine replay authorization and
explicitly enable the service capability. Policy receives a detached authenticated
peer and its scope. The service checks route ownership, the run/watermark revision,
and the peer's current identity after the authorized read. Changed credentials,
scope, identity or revision discard the page.

Replay responses contain task status and closed metadata only. Candidate output,
artifacts, history and provider failures are excluded. Sequence and revision
fields are decimal strings so protobuf Struct cannot round them. The shared
cursor remains run-bound, retention gaps remain explicit, and clients receive
static retry diagnostics. Replay headers on mutations, lists and subscriptions
are refused before the operation acts. Existing task projections remain unchanged
when the extension is inactive.

The request deadline is five seconds; host ports must honor cancellation. This
packet adds paged remote replay only. Durable remote subscriptions, approved
content delivery, ordinary-run event producers and installed peer/proxy
qualification remain open. S2.6 and the four-stage plan remain active.

Validation for A2A metadata pages:

- Regression tests reproduced missing extension acknowledgements in both HTTP
  bindings and native gRPC before the binding changes. All now pass.
- Full remote-module race tests passed (9.154s), including SQLite-backed engine
  authorization, cross-scope denial, revocation, retention gaps, invalid headers,
  deadlines and operation isolation. The gRPC race suite passed (1.293s),
  including exact resume beyond 2^53 and static error details.
- Focused engine replay race tests passed (8.607s). Root and both remote modules
  built. Final formatting, lint across all ten modules, diff checks and docs
  type checks, lint and production build passed.
- Prior commit CI run 39952169782 failed when the SQLite race conformance suite
  reached its ten-minute package timeout during fixture migration. This packet's
  local checks do not qualify that full Linux suite or installed remote peers.


### Root CI coverage timeout

CI run 39952169782 passed the ordinary SQLite suite in 107.815s, then exceeded
Go's default ten-minute package timeout during race coverage. The trace was
inside a fresh fixture's scope-column migration, near the end of conformance.
The shared workflow used its fallback coverage command because the repository
had no `test-coverage` target.

`make test-coverage` now supplies the same race detector, atomic coverage mode,
profile path and full root package set with an explicit thirty-minute timeout.
The shared workflow discovers this target without a workflow fork. A timeout
still fails the run. No storage behavior or conformance cases were changed.

Validation for the coverage timeout target:

- The shared workflow's exact target probe now succeeds and resolves the full
  race/atomic coverage command with a thirty-minute bound.
- Full local SQLite race coverage passed in 339.712s with 74.9% statement
  coverage. Formatting, all-module lint, root build and diff checks passed.
- Linux qualification remains pending. CI run 39954455702 failed earlier, in
  `TestFollowReplayDeadlineAndCancelledRead`, after an extra send at the exact
  lifetime boundary. That failure needs a separate replay deadline correction.

### Replay deadline notification race

CI run 39954455702 found a fourth idle replay send at a three-second lifetime
boundary. Repeating the existing test locally reproduced it. The poll timer and
context deadline callback can become runnable together, leaving `ctx.Err()` nil
when the poll callback wins.

The follow loop now checks the deadline timestamp as well as context cancellation
before reads and sends. The per-read boundary also discards a page if its deadline
has elapsed. Deterministic tests model delayed cancellation notifications both
before the first read and while a page is being read. The existing lifetime test
keeps its original send bound.

Validation for the deadline correction:

- The original lifetime test failed during 1,000 local repetitions before the
  fix. A deterministic delayed-notification test also failed before the change.
- All event tests passed 1,000 race-enabled repetitions after final formatting
  (3.472s). Full API and remote race suites passed (1.700s and 8.097s).
- Root/API builds, final lint across all ten modules and diff checks passed.
  Fresh Linux CI must still confirm the correction and the coverage timeout.


### Ordinary terminal lifecycle recording

`WithPublicEventRecording` enables atomic terminal state and metadata persistence
for ordinary runs. It requires the optional `event.TerminalStore` capability.
The shared repository commits a complete terminal snapshot, request-bound receipt
and `execution_finished` observation in the existing run transaction. SQL and
Mongo adapters reuse their ordinary update implementation inside that transaction.
No migration or new database table is needed.

Exact retries preserve the receipt and event ID after a lost acknowledgement or
retention pruning. Changed requests, stale revisions, competing terminal writers,
wrong scopes and verification bypasses fail closed. Runtime completion, failure,
cancellation and external terminal updates share one helper; persistence failure
cannot silently fall back or publish completion. Required-verification capture
and result commit retain their existing event sequence.

Recording remains opt-in, independent of subscriber authorization. Direct store
writes, imported terminal rows and historical backfill are outside this mode.
Artifact events, approved-content replay and durable observer delivery remain
open. The full four-stage plan remains active.

Validation for ordinary terminal recording:

- New request/runtime tests failed before the capability existed. A separate
  metadata integer regression exposed rounding during snapshot freezing and now
  passes with exact JSON number decoding.
- SQLite runtime tests cover synchronous and streaming completion, provider
  failure, cancellation, external updates, mock runs, disabled-mode compatibility,
  unavailable storage and suppression of completion hooks after write failure.
  Required verification retains its four-event sequence without duplication.
- Conformance tests passed on all three backends for terminal states, competing
  writers, scope/revision denial, changed retries, rollback, lost acknowledgements
  and retry after event pruning. PostgreSQL and MongoDB fixtures ran in Docker.
- Final full race suites passed: engine 296.394s, event 1.196s, run 1.194s,
  SQLite 352.977s, PostgreSQL 44.137s and MongoDB 51.617s. API and remote race
  suites passed in 1.898s and 10.675s. Root/API builds passed.
- Final formatting, lint across all ten modules, diff checks and docs type checks,
  lint and production build passed. Installed host rollout remains unqualified.
- Previous baseline `aad1305` passed Linux CI run 39955834822, confirming the
  replay deadline and race-coverage timeout corrections. This packet still needs
  its own post-push CI result.


### S2.6 verified artifact announcements

Guarded result commit now appends `verification_passed`, `artifact_created` and
`result_committed` in one transaction. The artifact event names the logical result
with its result ID. It carries no private evidence identity, digest or content,
and it grants no authority to fetch the result. Candidate and transcript capture
remain private. Ordinary terminal recording still emits only execution-finished.

The event validator accepts the new closed type only with a result ID and a public
event ID. Storage checks the committed receipt against the exact run, revision
and passed state. Shared backend tests cover pre-commit denial, sibling-run
rejection, stale revisions, transaction rollback, concurrent commits, restart and
retry after retention. Engine restart coverage verifies replay through subscriber
authorization. HTTP and A2A/gRPC tests preserve artifact metadata without returning
candidate content.

The new type uses the existing schema-1 envelope. Older readers reject unknown
types, so upgrade readers before enabling these writers. Historical receipts are
not backfilled. Arbitrary artifact producers, approved content replay, remote
durable subscriptions and installed qualification remain open. S2.6 and the full
four-stage plan remain active.


Validation for verified artifact announcements:

- Red phase: the new event validation test rejected `artifact_created`, and the
  SQLite lifecycle tests showed the missing event before implementation.
- `GOWORK=off make f`, then `GOWORK=off make l`: all ten modules passed lint.
- Focused race tests passed for event, engine, SQLite, PostgreSQL and MongoDB,
  covering commit lifecycle, rollback, replay, concurrent writes and binding
  rejection. A verbose repeat confirmed both container-backed artifact-binding
  cases ran without skips. The full event race suite also passed.
- Full API, A2A and gRPC race suites passed, along with the dashboard contract
  race suite and the root build.
- Docs type check, lint and production build passed. `git diff --check` passed.

These checks qualify the metadata path locally. They do not qualify installed
subscribers or a Linux sandbox worker.


### S2.6 remote durable metadata subscriptions

A2A SubscribeToTask now accepts the public-events extension when replay and
streaming are enabled. The opening Task and later status updates carry authorized
metadata pages from the shared log. Backlogs drain before terminal status; an
already terminal task refuses subscription and recovers through GetTask. This
preserves the pinned protocol and leaves the vendored protobuf unchanged.

Every read binds to the initial run, peer node and scope, rechecks route ownership
and invokes the host-authorized reader. Changed credentials or grants discard the
page. Gaps close with recovery metadata and no final status. The service admits
64 replay subscriptions, polls idle readers once a second, and bounds reads to
five seconds and lifetime to five minutes. Custom emitters must bound their own
writes. No detached producer is started.

REST POST and the existing GET subscription route, JSON-RPC SSE and gRPC share the
service implementation. HTTP waits for an authorized snapshot before sending
stream headers, writes bounded frames, propagates flush errors and carries cursor
IDs except on gaps. Last-Event-ID takes precedence on reconnect. gRPC requires a
client deadline within five minutes to bound blocked Send calls and acknowledges
activation only after authorization. The API and remote adapters share the HTTP
flush helper, including error-aware inner writers behind wrappers.

The new tests first failed because replay subscriptions were unsupported. Added
coverage exercises ordered completion, pinned identity, current-grant revocation,
consumer failure, capacity, lifetime, retention gaps, HTTP framing and preflight,
stalled writes, live HTTP disconnects, gRPC deadlines and negotiation. A real
SQLite engine test commits an ordinary terminal event during subscription and
recovers the missed final frame through GetTask. Installed proxy/client behavior,
approved content replay and broader plan qualification remain open.

Validation for remote durable subscriptions:

- Red phase confirmed that replay subscriptions were refused before implementation.
  A separate HTTP preflight test exposed metadata being written by a writer with
  deadline support but no flush capability; the shared capability check fixes it.
- `GOWORK=off make f`, followed by `GOWORK=off make l`: all ten modules passed.
- Full race suites passed for A2A (7.881s), gRPC (1.276s) and API (1.527s).
  The event race suite and root build passed too.
- The 64-stream cancellation test fills admission slots, rejects overflow and
  verifies all slots are released. HTTP and gRPC stalled-send tests establish
  their configured bounds; live HTTP disconnect and in-process gRPC tests passed.
- Docs type check, lint and production build passed. `git diff --check` passed.
- Prior artifact commit `5e0fc22` passed Linux CI run `39959920033`.

These are local and CI results. Installed client/proxy qualification, arbitrary
artifact producers, approved content replay and the remaining four-stage scope
are still open.


### S2.6 fresh result snapshot storage

Built-in stores expose `verification.ResultSnapshotStore` as an optional trusted
capability. `ReadResultSnapshot` binds the request to an exact run, result and
receipt digest, then reads the current passed run, contract, reports and retained
evidence under the existing backend run transaction. Commit and read share bounded
material loading and snapshot validation. No schema migration or widening of the
legacy composite store is required.

The read checks captured execution inputs and transcripts, current run revision,
receipt integrity, candidate bytes and report freshness. It returns no snapshot
after a transaction error, context cancellation or expiry during acknowledgement.
It preserves lifecycle state and replay events. Historical receipt retries keep
their existing semantics after evidence expiry or deletion, while fresh reads
refuse that evidence.

This is a prerequisite for approved-content replay. The snapshot contains private
material, even though output is excluded from JSON. Host code still has to fence
current content grants and source revisions, apply current publication policy,
and reject revocation or deletion during those checks before serving bytes.
No public content route or content-bearing replay is enabled by this packet.

The initial SQLite regression failed because the capability was absent. Added
tests cover request identity, material tampering, expiry, exact scope, cancellation,
changed runs, deleted candidate/input/transcript evidence, detached reads,
metadata retention, restart recovery and failed or delayed transaction acknowledgements.

Validation for fresh result snapshots:

- Full verification, engine and SQLite race suites passed. Engine took 323.308s
  and SQLite took 396.563s. After lint corrections, focused snapshot and restart
  race tests passed again on the final code.
- PostgreSQL and MongoDB verification/commit conformance ran against Docker with
  race detection and passed in 28.454s and 35.079s. Final snapshot boundary checks
  passed on both backends too, with no skipped cases.
- `GOWORK=off make f`, final `GOWORK=off make l` across all ten modules, root build
  and `git diff --check` passed. Lint corrections narrowed two local error
  variables and asserted the initially successful snapshot in the boundary test.
- Docs type check, lint and production build passed.
- Remote subscription commit `14a0f83` passed Linux CI run `39962487804`.

Public content authorization, current-resource fencing and publication screening
are the next integration work. Linux sandbox qualification remains deferred by
the user; this packet does not close the four-stage plan.


### S2.6 guarded content reads

`verification.ResultReader` checks explicit current content permission before
resolving a private receipt, then reads and screens the result under the host's
current binding fence. A second snapshot after screening catches evidence deletion,
expiry or run changes. A final permission check occurs inside the fence, and
freshness is checked again after it returns. Each call has a five-second deadline;
trusted host ports must honor cancellation.

`engine.WithResultContent` and `ReadResultContent` wire the reader to the existing
publication policy, gate, hooks and metrics. Reads require an enabled gate and
use the current agent safety profile. The host fence must guard that profile as
well as source and grant revisions. Database locks are held only for snapshot
reads, never for the publication callback. Metadata replay permission and worker
verification authority do not grant content access.

The public projection contains screened content and minimal result metadata.
Changed text must carry a redacted decision and sets `ContentChanged`; its
verification fields still describe the original candidate. Unscanned, unknown,
oversized or silently rewritten decisions return no content. Private diagnostics,
receipts, reports and bindings are excluded. Reads preserve the stored run and
never regenerate or reevaluate the answer.

Commits and reads share one callback-lifetime guard, retaining the existing
missing, repeated, retained and escaped-callback checks. Typed-nil publication
checkers and scanners are rejected by engine configuration using the same host
port test as verification constructors.

The TDD checks began with missing reader and engine APIs. A separate behavioral
regression exposed the typed-nil publication gate before its configuration fix.
Root tests cover permission ordering, denial before storage, stale bindings,
redaction semantics, cancellation, five-second screening deadlines, expired
fence returns, changed material and callback protocol errors. SQLite engine tests
cover reopened committed results, unchanged stored runs and refusal after deletion,
revocation or run updates during screening.

HTTP, dashboard and remote content transports remain open. Their existing run and
store APIs retain host access controls and are not implicitly replaced by this
reader. Metadata SSE carries no answer content. Installed screening and resource
fences still need qualification; the Linux worker remains deferred by the user.

Validation for guarded content reads:

- Full verification, safety and engine race suites passed; the engine suite took
  342.397s. Final verification/safety race suites and focused engine publication
  and content-read regressions passed after the last changes.
- Snapshot and verified-commit conformance passed with race detection on SQLite
  (31.773s), PostgreSQL (14.892s) and MongoDB (17.641s). The latter two ran against
  Docker without skipped cases.
- A new red test showed that invalid UTF-8 could reach the result projection.
  The reader now rejects it before JSON encoding can change screened bytes.
- `GOWORK=off make f`, final `GOWORK=off make l` across all ten modules, root build
  and `git diff --check` passed. Scope checks reuse the existing engine helper.
- Docs type check, lint and production build passed.
- Snapshot commit `21f7cfb` passed Linux CI run `39965570894`.

This completes the host reader, not the transport or installed qualification
work. The four-stage goal remains active.


### S2.6 HTTP and dashboard content access

`GET /v1/runs/:id/results/:result_id` and dashboard `runs.result` v1 call the
configured guarded reader. Both parse the exact run/result pair with a shared
helper and map failures through one static vocabulary. Dashboard reads retain
principal and exact scope from the dispatcher, require `cortex.read` and still
require the reader's separate content grant. Neither adapter resolves raw output
from a run detail record.

The HTTP route sets no-store caching, limits concurrent reads to 64 and requires
write-deadline support before accessing private material. Writes have a ten-second
limit, narrowed by an earlier caller deadline. A bounded JSON response carries
content length, and failed writes never append a second body. Admission slots are
released on cancellation and delivery failure. Dashboard hosts own their network
delivery bounds and private-response cache controls.

`ResultContentResponse` keeps the public result fields and encodes the run revision
as a decimal string. The engine's Go result remains unchanged. The explicit
transport field also gives generated OpenAPI the correct string type; a JSON tag
alone was insufficient for the current Forge schema generator.

Transport tests share an actual committed SQLite result fixture. Initial tests
failed on the absent HTTP route and dashboard intent. Coverage includes screened
success, current revocation, evidence deletion during screening, foreign scope,
invalid identities, missing services, publication failure, no-store responses,
64 concurrent cancellations and stalled writes. A live HTTP client reads a result,
then receives denial after grant revocation. JSON preserves revisions above 2^53,
and a generated OpenAPI test checks the same wire type.

Metadata SSE still carries no answer text. Remote content, installed browser and
proxy behavior, dashboard-host delivery and broader qualification remain open.
The test fixture uses sequential host guards and does not qualify a deployed
cross-process fence. Existing administrative run/store access remains separately
host-authorized.

Validation for HTTP and dashboard content access:

- Full API race tests passed in 35.485s; dashboard contract race tests passed in
  34.573s. Root verification and focused engine content-read race tests passed.
- The 64-reader cancellation test fills admission, rejects overflow and checks
  that every slot is released. Stalled writes stop at ten seconds, and a real
  HTTP connection verifies content delivery followed by current-grant denial.
- JSON and generated OpenAPI both preserve the decimal revision representation.
  The new envelope leaves the engine's `PublishedResult` type unchanged.
- Coordinated `GOWORK=off make f`, final all-ten-module `GOWORK=off make l`, root,
  API and extension builds, and `git diff --check` passed. API and extension
  `go mod tidy -diff` checks passed; new module entries support SQLite transport
  tests and do not change existing dependency pins.
- Docs type check, lint and production build passed.
- Guarded reader commit `bffa2c5` passed Linux CI run `39968186862`.

Remote content, installed delivery qualification and the remaining four-stage
work stay open. The concurrent Sentinel metering work owns separate commits.


### S2.6 guarded A2A content access

A2A now exposes the guarded reader through an opt-in `result-content/v1`
extension on `GetTask`. REST, JSON-RPC and gRPC share the same service logic.
Requests name the committed result in `Cortex-Result-ID`; the service resolves
the task's canonical run, checks the routed agent, reads through the current
content authorizer/publication gate and reauthenticates the peer before returning.
The card follows the service capability. Missing and typed-nil gateways fail
construction when guarded content is enabled.

The task carries `ResultContentResponse` under `cortex.result`, with a decimal
revision and explicit redaction status. No raw run output, error, history or
private receipt enters this projection. Mixed replay/content activation,
ambiguous headers and content negotiation on work/stream operations are refused.
The first regression test showed an unknown content request returning the raw
stored answer; the implemented path now fails without that fallback.

Service reads have a five-second bound and 64 slots. HTTP holds a separate
64-slot admission through delivery, requires writer deadline support before
reading content, sends no-store and bounds writing to ten seconds within a
fifteen-second request lifetime. gRPC requires a caller deadline within fifteen
seconds. Host callbacks must honor cancellation; gRPC server connection/stream
limits and installed transport behavior remain host qualification work.

Tests cover successful redaction, current grant denial, resolved-scope isolation,
peer/node revocation, route mismatch, mismatched result/revision, unsafe or
oversized publication, deadlines, cancellation and slot release. An actual
committed SQLite result exercises the engine gateway and preserves the stored
candidate. Live HTTP and gRPC tests cover activation, content and precision.
A later red regression caught an error body on a JSON-RPC notification when
writer setup failed; notifications now retain their no-response semantics.

Legacy requests without the extension retain existing projections and need
separate host access controls. This packet does not establish current content
policy for every old task/list/send/stream path. Arbitrary artifact producers,
installed qualification and the rest of the four-stage plan remain open.

Validation for guarded A2A content access:

- Final full remote race suite passed in 10.664s; full gRPC binding race suite
  passed in 1.320s. Both module builds passed, and their `go mod tidy -diff`
  checks passed. Added indirect dependencies support the shared SQLite fixture.
- The HTTP admission test filled 64 slots, rejected both HTTP and direct-reader
  overflow, cancelled every request and verified that all slots were released.
  Stalled writes stopped at ten seconds without appending another body.
- Coordinated `GOWORK=off make f`, final all-ten-module `GOWORK=off make l`, and
  `git diff --check` passed. Formatting touched only this packet's files.
- Docs type check, lint and production build passed.
- Prior transport CI run `39971269395` was cancelled. Successor run `39971728918`
  had passed docs, formatting, security, lint and all submodules, with the root
  Linux test job still running at this checkpoint. Neither run is recorded as
  fully passed here.


### S2.6 protected historical projections

The historical transport audit found raw stored answers in legacy A2A tasks,
HTTP run reads, dashboard details/lists and `agents.stats.last_run`. A new A2A
regression test reproduced the raw answer without content-extension negotiation.
These paths now withhold protected content and direct clients to the guarded
reader. Ordinary legacy runs keep their existing behavior.

`run.ContentProtected` protects saved verification requirements, any saved result
reference and hosts with a configured reader. `run.PublicView` copies an explicit
metadata allowlist into a detached record. It preserves identity, state, counts,
assurance and valid result identity while dropping input/output, diagnostics,
arbitrary metadata and private provenance. A missing current reader cannot
remove protection recorded on the run. Direct engine/store APIs remain trusted
host capabilities and are unchanged.

A2A's pure mapping protects required/committed runs; service polling and listing
also protect configured-reader runs. Subscriptions inherit that projection and
send terminal metadata with the result reference, without answer artifacts.
The old mapping test now uses a valid result ID and expects this guarded-read
handoff. HTTP run reads set no-store. Dashboard protected detail omits private
step/tool bodies and pending calls, while cached `runs.events` refuses protected
reads with a typed conflict pointing to `runs.replay` and `runs.result`.

Tests cover preserved ordinary behavior, detached scope/time/metadata, invalid
result references, unchanged stored answers, HTTP lists/details, dashboard
lists/details/statistics, cached replay refusal and terminal A2A subscriptions.
The metadata endpoints retain their existing host authorization; this packet does
not turn those endpoints into the separate replay-authorizer API. Installed
client adaptation and the broader live qualification matrix remain open.

Validation for protected historical projections:

- Final race suites passed for root `run` (1.183s), full API (36.047s), dashboard
  contract (36.417s), full remote module (17.990s) and full gRPC binding (1.285s).
  Root and all four affected nested-module builds passed.
- The first full API run found its old response test double did not implement
  `SetHeader`. The test double now records headers and asserts no-store; the
  final full suite passed without weakening the handler's cache policy.
- Final `GOWORK=off make f`, all-ten-module `GOWORK=off make l`, docs type check,
  lint and production build, and `git diff --check` passed. No dependency files
  changed in this packet.
- The new shared projection leaves trusted stored data intact. The dashboard
  cache refusal and A2A terminal subscription are tested separately from the
  guarded content reads. Installed client migration is still unverified.


### S2.7 guarded dashboard result views

The React client in `forge-dashboard/packages/plugin-cortex` now uses durable
metadata replay for protected live runs and the guarded `runs.result` query for
committed text. Dashboard commit `2ea7eff` contains this packet. The user approved publishing
the seven earlier Tether commits with it, and all eight are now on remote main.
The checkout's unrelated untracked files were preserved.

The result view bypasses the shared query cache, validates run/result identity,
version, publication state and the decimal revision string, and renders literal
text. Refresh, authority-context changes and hidden tabs clear displayed bytes.
Each request has a ten-second client deadline; successful views renew after
fifteen seconds. Late replies are discarded. Denial, stale data or malformed
responses leave no previous-answer fallback. Redaction explicitly describes a
projection of the verified candidate, not a separately verified answer.

Protected live monitoring refreshes the run snapshot before accepting each
opaque cursor, including retention gaps. It bounds retained metadata, stops
after five minutes, preserves cancellation, and waits for required verification
when execution has finished. The typed legacy cache refusal switches clients to
this path. Saved run details withhold private input and use the same result view.

Validation:

- All 60 Cortex plugin tests passed, covering gap recovery, large cursors,
  malformed identities, access denial, cancellation, timeout/late replies,
  authority changes, hidden tabs, display retention and literal HTML text.
- The plugin's documented `format`, `lint`, `typecheck` and `test` pnpm scripts
  passed. The sibling has no make lint/format targets. The full shell production
  build (`tsc -b` and Vite) passed. Formatting changed only owned plugin files.
- A labelled local UI fixture was checked at 1280x900 and 390x844. Result text,
  redaction labels, wrapped controls and access revocation were inspected in the
  browser. Temporary preview sources and the preview server were removed.
- This is client implementation and fixture validation. Installed host cache
  controls, delivery deadlines, real authorization wiring and live provider or
  sandbox qualification remain open. The four-stage plan is not complete.

The prior Cortex CI run `39975802817` failed in the sandbox expired-capture test.
Its assertion raced deferred peer cleanup after the socket closed. Commit
`fc59e50` joins the server before inspecting cleanup; 100 focused race runs and
the full sandbox race suite passed, followed by all-module lint and formatting.
The fix is pushed. Linux CI must confirm it independently.


### S2.5 captured grading manifests and host registration

`verification.EvaluationManifest` seals the captured binding, exact test criterion,
protected test bundle, fixture dataset, result schema, supervisor implementation
digest, required check names and permitted state probes. Protected references must
be distinct supporting artifacts in the same binding; candidate content cannot
be substituted for authoritative tests. Permitted probes must exactly match
captured source references. This is metadata validation, not runtime isolation.

`ValidateRequest` rejects a changed candidate, run, requirement, tenant, dataset,
environment, authority or criterion. It uses a host-supplied observation time,
requires a live deadline strictly inside every artifact's retention interval and
rejects future or stale source observations. Current grants and external source
state still require independent host checks.

The immutable host registry selects by exact execution binding, criterion and
manifest revision. It refuses ambiguous entries, caps registration at 256
manifests and sixteen MiB, checks context scope, and returns detached values.
Sealing JSON does not register it. Criterion validation is shared with acceptance
contracts, preserving their existing digest and validation rules.

Ruling: the manifest binds a captured check through the existing `Binding` and
`Criterion` contracts rather than introducing another objective or tenant model.
This keeps candidate and acceptance identities identical across grading and
commit. It also means hosts register the captured manifest before dispatch;
template expansion and durable registration remain orchestration work.

These APIs do not submit a job, parse candidate stdout, grant export or publish a
verdict. Separate candidate/grader jobs, authenticated protected-supervisor
observations, contextual adapters and Linux adversarial qualification remain
required before S2.5 can pass its gate.


Validation for the grading manifest foundation:

- Manifest and registry behavior first failed against placeholder implementations;
  the implemented checks then passed the full verification package.
- The final verification race suite passed in 4.450s, including concurrent
  detached registry reads and the combined registration byte limit. Root build
  passed. Existing contract digest fixtures still pass after sharing criterion
  validation.
- Initial all-module lint found formatting and one redundant error assignment.
  After those fixes, `GOWORK=off make f`, final all-ten-module `make l` and
  `git diff --check` passed. Formatting touched only owned verification files.
- Docs type check, lint and production build passed. The progress table now
  records S2.5 as active, not accepted.
- Cortex CI `39978966777` passed on prior commit `8ef97e1`, independently
  confirming the sandbox cleanup fix on Linux. Dashboard CI `39979911379` is
  running on the approved and pushed `2ea7eff`; no success is claimed yet.


### S2.5 authenticated candidate and grader orchestration

`SandboxTestVerifier` now dispatches host-registered plans through the root
`sandbox.Runner` port. The candidate receives only its captured input; the grader
receives protected tests, fixtures, the result schema and a digest-checked
candidate export. Job IDs, idempotency keys and output IDs are distinct and must
be reserved durably before registration. No environment variables pass through.

Receipts and terminal results must match the expected jobs and pass an explicit
authenticity port. The sandbox module's `grading.NewAuthenticity` pins the broker
name and key and reuses the existing Ed25519 verification helpers. Evidence reads
check exact byte counts, SHA-256, scope and retention. Current authority is checked
around external calls, including after the supervisor returns.

The external supervisor receives detached job and artifact bindings without
stdout or stderr. Its implementation revision and named observations must match
the registered manifest and both exact job digests. Missing checks are
inconclusive; forged stdout cannot override a failed assessment. The supervisor
must retain authenticated evidence and independently corroborate tests that run
candidate code. This packet defines and checks that port, not its concrete
implementation or deployment.

Admission is bounded to the configured one through 64 concurrent evaluations.
Reads accept at most 32 MiB of original evidence and 16 MiB per reference; output
artifacts also retain the sandbox's tighter declared limits. Each evaluation has
a maximum five-minute deadline. Unfinished jobs receive a two-second cancellation
attempt using only the expected reserved ID. An uncertain submission or failed
cleanup remains uncertain and never creates a replacement job.

Ruling: reuse the typed test evaluator registration and existing broker signature
helpers. Keep the root module independent of the heavy sandbox adapter. Hosts
still own total evaluation accounting, durable phase recovery and supervisor
retry reconciliation. New supervisor artifacts are not yet attached to the
captured verification report binding. These gaps, contextual adapters, approved
export and Linux adversarial qualification keep S2.5 open.

Validation:

- Separate-job execution, forged verdicts and invalid broker identity failed
  against the initial implementations, then passed after the checks were wired.
  Admission overflow also failed before the bound was added.
- Root verification and the full sandbox module pass with the race detector;
  both modules build. Regression cases also cover malformed supervisor outcomes
  and cancellation failure, which preserves `ErrCallUncertain`.
- Initial all-module lint found redundant error assignments and formatting.
  After correction, formatting and all ten module lint checks passed.
- Cortex CI `39980617720` passed on `ccf1724`. Dashboard CI `39979911379` passed
  on the approved eight-commit push through `2ea7eff`. These supersede the pending
  CI notes above and do not establish live sandbox qualification.
- After the cleanup review, the verification race suite passed in 3.652s;
  grading authentication passed in 1.206s. Root and sandbox builds passed.
  Docs type checking, lint and production build also passed.


### S2.5 registered state checks and observation windows

`StateManifest` pins a state criterion and its exact execution binding to a
nonempty subset of captured sources. `StateVerifier` freezes those registrations,
selects only the exact scoped check, and asks a host-supplied authenticated reader
for current revisions and digests. Shared request validation keeps binding,
criterion, retention and source-age rules identical to sandbox test evaluation.

Each read has the manifest's context deadline, capped at 30 seconds. The full
check also has a 30-second bound and configured admission of one through 64
concurrent evaluations. Current authorization runs before and after reads.
Changed source revisions produce failed checks; wrong tenant/task/resource
identity, replayed or malformed observations, revoked grants and source outages
cannot produce passed results.

Observation windows now travel from `CheckResult` into `Outcome` and the sealed
report. Each binds the manifest, captured binding, source, read interval and
host-derived expiry. Report validation rejects duplicate or foreign resources,
changed source data in a passing outcome and malformed timing. Contract
validation bounds expiry by the state criterion. Freshness checks reject future
or expired observations. The optional JSON field preserves existing report
encodings; historical reports gain no new guarantee.

Ruling: retain external observations inside the existing immutable report instead
of creating another evidence store or claiming a remote lock. The host observer
must authenticate its source and scope. Publication still requires the existing
current-binding fence. Concrete source adapters, objective coverage, schema/units,
approved export, supervisor recovery and Linux qualification remain open.

Validation so far:

- Initial regressions failed because the placeholder skipped reads and returned
  success for changed context. The report integration test then failed because
  the host dropped observation windows. Both paths now pass.
- The per-probe deadline test exposed the inherited 30-second overall deadline;
  the checker now gives the observer the manifest's shorter deadline.
- Full verification race tests passed. SQLite, PostgreSQL and MongoDB passed the
  shared observation storage and expiry case under the race detector, including
  rejection of a saved report that exceeds its criterion's allowed age.
  PostgreSQL and MongoDB ran in Docker; Mongo used the pinned 7.0.41 fixture.
- Initial lint found formatting and one unnecessary struct conversion. These
  were corrected before the final format, lint and test cycle.
- Final formatting, all-ten-module lint, verification race tests (4.291s), root
  build, docs type checks, docs lint and docs production build passed.
- CI `39983797018` passed on sandbox orchestration commit `ace6674`.
- The broader engine race suite passed in 371.019s, including its SQLite-backed
  verification recovery tests. Its build also passed.


### S2.5 separate approved artifact export command

`ArtifactExporter` accepts a host-reserved operation ID, committed result,
exact artifact reference, actor, approval and logical destination revision.
It requires current export authorization, an explicit export fence, evidence
access, content screening and an idempotent sink. Fresh result snapshots share
the guarded content reader's validation helper. Candidate and supporting refs
must match the captured binding completely.

The command checks stored result freshness, reads and hashes bounded artifact
bytes, screens a detached copy, and rechecks stored material and current approval
before delivery. Screening must approve the original digest. No redaction or
rewritten artifact is silently exported. Callback inputs are detached and the
existing fence lifetime guard prevents repeated or escaped callbacks from
starting a second delivery. Errors after delivery starts preserve an uncertain
outcome and never authorize a replacement operation ID.

The sandbox module's `grading.NewFileExporter` composes this command with a
local file adapter. A global private journal reserves the full request before
artifact publication. Logical destination mappings hold verified directory
identities; requests supply no paths. The adapter reuses `hostfs.PublishPin` for
bounded, synced, exclusive publication and current approval checks. Identical
retries verify existing files, while changed requests conflict across all
registered destinations. Links, special files and replaced directories fail.

Ruling: local file export is an explicit host command, separate from sandbox
execution and result viewing. Reuse the current snapshot validator, fence lifetime
guard and protected filesystem publication primitive. Exporting supporting
evidence does not relabel it as a verified candidate. Concrete API/dashboard
commands, production approval and screening, external sinks, audit reconciliation
for expired approvals and installed qualification remain open.

Validation so far:

- Root export tests first failed because approval was bypassed and no delivery
  occurred. They now cover revoked approval, scanner outage, changed bytes,
  changed snapshots and uncertainty after delivery starts.
- Local adapter tests first failed on missing files and accepted unsafe paths.
  They now pass under the race detector, including composed command checks,
  restart, concurrent identical writes, interrupted delivery after reservation,
  changed destinations and link/directory rejection.
- An authorization-cancellation regression exposed an incorrect access-denied
  classification. The command now preserves cancellation while keeping provider
  diagnostics out of its public error path.
- CI `39986200222` passed on contextual-state commit `566c319`.
- Admission tests also exposed a cancelled caller receiving an unavailable error
  when all slots were occupied. Entry now checks cancellation before admission;
  normal overflow remains bounded and completed attempts release their slots.
- Initial all-module lint reported formatting only. The final format pass touched
  owned files. Full sandbox race tests and builds passed, including grading,
  protected file publication and the service lifecycle suite. Docs type checking,
  lint and production build passed.
- Final all-ten-module lint, root verification race tests (4.017s), grading race
  tests (1.542s), root/module builds and diff checks passed after the cancellation
  and substitution review.

### S3.4 durable effect reservations and explicit recovery

Added an optional `execution.EffectStore` capability to all three built-in
backends through the shared assurance repository. The request binds the exact
scope, run, stable operation key, original attempt ID, argument digest,
principal, authority reference, deadline and trusted effect requirement. The
first confirmed insert grants dispatch permission. Identical requests never
receive that permission again, and changed requests conflict. An immutable
terminal receipt records provider identity, version and retained evidence.
Argument bytes are not written to this journal.

`execution.NewEffectExecutor` connects that store to host-registered adapters
and current authorization. It reserves before dispatch, reads completed receipts
on replay and probes pending outcomes through read-only reconciliation. Unknown
results stay reserved. A lost reservation or completion acknowledgement returns
an uncertain outcome. Recovery remains available after the original execution
deadline, with fresh authorization and a bounded context. Per-instance admission
limits concurrent calls; they do not replace shared budgets or worker fencing.

Ruling: implement the independent S3.4 effect foundation while G2 live deployment
remains deferred. Keep this executor explicit and conservative: it never retries
a mutation automatically, including a handler declared idempotent. This avoids
inferring retry permission before provider reconciliation and scheduler fences
are installed, at the cost of leaving retry policy execution for the runtime
integration packet. Host adapters must enforce current grants at the external
write boundary. This packet does not enable production task acceptance.

Red tests first showed the missing executor and storage capability. A behavioral
red test then showed that a lost reservation acknowledgement was reported as an
ordinary database error; it now includes `ErrEffectUncertain`. Tests cover changed
arguments and attempts, current authority, expired dispatch versus authorized
recovery, competing reservations, immutable receipt conflicts, and missing or
wrong scope. The process fixture performs and fsyncs an external file write,
then gets killed before returning a receipt. A reopened SQLite store reconciles
that write and a repeated execution leaves the external write count at one.
The race detector caught a test stderr-buffer read before process shutdown; that
fixture race was fixed and the killed-process suite passed again.

Validation: root execution/planning/orchestration race tests passed. Shared effect
conformance passed on SQLite, PostgreSQL 16 and the pinned MongoDB 7.0.41
container without skips. The final backend run completed in 8.149s, 4.803s and
6.606s respectively; the separate SQLite crash suite passed in 5.744s. Root build,
formatting, final all-module lint and documentation typecheck/lint/build passed.

Engine tool wiring, durable operation discovery, atomic task/event updates,
automatic retries, explicit compensation, artifact conflict resolution and
stale-worker fencing remain open. The fixture does not qualify an external
provider or the deferred Linux sandbox. S3.4 and the four-stage goal remain active.

### Sandbox deadline fixture correction

CI run 39989422674 for export commit `33dc45e` failed only the root race test
`TestSandboxEvaluationCancelsOwnedJobOnDeadline`. Its 30 ms wall-clock timeout
expired before submission under CI load, leaving zero owned jobs when the test
expected one. The other CI jobs passed.

The cleanup test now uses `testing/synctest`, so elapsed fake time reaches the
polling deadline after dispatch regardless of host scheduling. A separate test
expires the deadline before dispatch and requires zero jobs, cancellations and
supervisor calls. Production deadline and cleanup behavior is unchanged.

Both deadline cases passed 100 repetitions under the race detector (5.333s).
The complete verification race suite passed (5.666s), as did formatting,
all-module lint and diff checks. Effect recovery is committed as `c88f71c`.

### S3.4 explicit engine effect handlers and uncertain stops

CI run 39991896222 passed for `bea9d39`, including effect persistence and the
sandbox deadline fixture correction.

`engine.NewEffectToolHandler` connects a host-registered `EffectExecutor` to
`WithTool`. Its resolver returns the retained operation for the invocation; it
must preserve the original request across retries and worker replacement. The
helper checks exact scope, run, tool, argument digest and call identity before
reservation, and gives the resolver a detached scope. It returns provider outcome
and external identity/version while retaining private evidence and authority
references in the journal. Resolution and execution share `MaxEffectDuration`.
No ordinary handler acquires an inferred idempotency or reconciliation contract.

A typed `ErrEffectUncertain` now stops synchronous, streamed and approved calls
before remaining sibling tools or another model request. The engine records an incomplete
run with `tool_error` and a fixed uncertainty error, while preserving existing
cancellation/deadline outcomes. It does not save an unfinished tool exchange to
conversation history or evaluate it as a required-verification candidate.
Streaming never reports successful completion for an uncertain action, including
when a conflicting terminal row is already present. A failed terminal write
emits the fixed uncertainty error without announcing a durable incomplete state.
Tool telemetry records the bounded `uncertain` outcome.

Approval is recorded separately from provider outcome. An approved action that
becomes uncertain retains the human's decision after the engine confirms its
durable stop. Repeating that checkpoint decision cannot dispatch again. A failed
terminal write leaves the decision pending for host audit, and the consumed
suspension prevents repeated execution. This retains the existing non-atomic
checkpoint/run boundary; it does not claim a transaction across provider effects.

Behavioral red tests reproduced continued model/sibling execution after an
uncertain effect, an empty direct-dispatch result, a pending approval after the
run stopped, unbounded operation resolution, a lost run deadline during approval
resume, a misleading verification-outage stream error and a success event from
a conflicting terminal row. Those tests pass after the corresponding fixes.
SQLite-backed handler tests cover acknowledged writes, lost replies and journal
reopen with one provider dispatch. Adversarial cases reject mismatched scope,
run, tool, arguments, expired requests, resolver mutation and missing identities.
Required-verification fixtures cover synchronous, streamed and approved calls,
with no evaluator call, accepted result or private tool content on uncertainty.

The host still owns durable operation allocation, current authority and the
qualified provider adapter. Reconciliation records provider knowledge; it does
not automatically resume an incomplete run or verify the task. Operation
discovery, governed continuation, shared budgets, retry policy, compensation,
artifact conflicts and scheduler fencing remain open. The four-stage goal is
active, and Linux qualification remains deferred.

Final validation for the engine integration: `GOWORK=off go test -race ./engine
./execution -count=1` passed on the finished code (engine 344.253s, execution
1.218s). The earlier full run also passed before the final deadline/streaming
corrections; those corrections have behavioral red/green tests and were included
in the final full run. Formatting, all-module lint, root build, documentation
typecheck/lint/build and diff checks passed. The provider and authorization ports
in these tests are fixtures, not installed-service qualification.


### S3.4 durable discovery and bounded effect reconciliation

`EffectDiscoveryStore.ListEffects` pages the same immutable operation journal
used by dispatch. Its optional run filter and exact scope apply in SQLite,
PostgreSQL and MongoDB through the shared assurance repository. Terminal records
count toward the page limit, so discovery never scans an unbounded set while
looking for unresolved work. Reading one operation and listing a page share the
same request/receipt validation path.

`EffectExecutor.RecoverPage` uses its own journal, validates and detaches the
whole page, then reconciles each original request through current authorization.
It never allocates attempts or dispatches mutations. A denied or unknown operation
advances the cursor, and the next full scan revisits it. The operation-key digest
cursor wraps after a short page; concurrent inserts before a cursor require that
wrap. The host owns exact-scope discovery authorization and supervised scheduling.

A page has a 30-second bound including discovery, local admission control and
partial progress on cancellation. Returned metadata excludes authority, provider
evidence and private errors. A terminal observation records provider knowledge,
not acceptance, run continuation or public delivery. Concurrent workers can issue
read-only probes; immutable receipts reject conflicting terminal knowledge.

Red tests established the missing storage capability and recovery behavior before
implementation. Tests cover bounded pagination across terminal rows, exact-scope
and run isolation, corrupted stored bindings, whole-page rejection before probes,
denied and unknown operations, deadline/admission limits, partial cancellation,
detached records and recovery on a later scan. The process-crash fixture now
reopens SQLite and discovers the retained operation after killing the dispatching
process; the replacement worker records the external outcome without another write.

Governed continuation, shared budgets, retry execution, compensation, artifact
conflicts, atomic task/events and scheduler fencing remain open. Installed host
and provider qualification remain separate, and live Linux selection is deferred.
The four-stage goal remains active.

Validation: the complete execution package and engine effect tests passed with
`-race`. Effect conformance and recovery tests passed with `-race` on SQLite
(31.629s), PostgreSQL (11.843s) and MongoDB (14.962s); both server backends ran in
real containers, with no skipped conformance cases. The first combined backend
run exposed shared fixture scope across cases, corrected by giving discovery its
own scope before rerunning. `make f`, `make l` across all ten modules, root
`GOWORK=off go build ./...`, docs typecheck/lint/build and diff checks passed.


### S3.3 structured assignment proposals and host admission

CI 39997280007 passed for effect discovery commit `281f768`. The superseded
engine commit's CI run was cancelled when the newer push replaced it.

`coordination.Handoff` seals exact plan/task/parent-run and assignment/fence
bindings, endpoint/principal claims, criteria, artifact refs, deadline and a
bounded authority declaration. `ValidateHandoff` checks those claims against
current local plan and host-resolved ownership/grants. `planning.Authority`
reuses budget, resource and effect-manifest checks. A child cannot retain mutation
tools after removing every writable resource. Review proposals allow no effect
tools or writable resources and require a host-resolved producer principal that
differs from the receiver's principal.

`HandoffReceiver` validates and detaches before invoking bounded host resolution
and acceptance. The acceptance port must atomically check current ownership,
plan revision and grants, and durably deduplicate assignment/fence/digest. This
packet supplies the admission boundary, not that scheduler transaction.

The bus retains its leaf-package dependency boundary through `HandoffHandler`.
The typed adapter lives in `coordination`; `WithA2AHandoffHandler` connects it to
an explicitly enabled engine bus. Unsupported versions, malformed or conflicting
transport identity/deadline/run fields and missing handlers fail before message
storage. Delivery rechecks scope, identity, deadline and conversation state.
A cancelled conversation cannot pass a queued assignment to host admission.
Structured proposals never start an ordinary agent run or use text-reply tokens.

Remote mapping preserves fractional-second deadlines and parent run IDs. Service
acknowledgements say `queued` without creating a task-success claim. REST and SSE
loopback tests use actual HTTP and SQLite plan/message storage; repeated proposals
reach fixture host deduplication, while denied credentials and changed sender
names fail without a model call. Legacy messaging and the existing transports
remain covered by their suites.

Behavioral red tests reproduced missing handoff validation, dropped parent IDs,
ordinary task acknowledgement, delivery after conversation closure and a mutation
tool retained under read-only resources. The A2A import test also caught a direct
coordination dependency; moving typed validation behind the handler port preserved
the existing architectural boundary rather than widening its allowlist.

Durable assignment transitions, submission/review evidence, contradiction handling,
verified task acceptance, shared reservations, scheduler fencing and installed
multi-agent qualification remain open. The four-stage goal remains active.

Validation: the full engine race suite passed (340.643s). Final coordination,
planning and A2A race suites passed after the authority correction, including the
new cancellation and mutation-grant tests; coordination passed again after final
import formatting (14.146s). The complete remote suite passed with `-race`
(16.750s), including actual HTTP/SSE handoffs, and the gRPC suite passed (1.287s).
`make f`, `make l` across all ten modules, root build, documentation
typecheck/lint/build and diff checks passed. Tests keep the A2A import allowlist
intact. The host acceptance callback and grant resolver remain fixtures, so these
results do not establish durable scheduler or installed-worker qualification.


### S3.3 durable assignment progress and plan serialization

CI 40001111614 passed for handoff admission commit `375ba9e`.

`coordination.AssignmentStore` retains sealed proposals and immutable lifecycle
observations in SQLite, PostgreSQL and MongoDB. Changes bind the expected revision,
handoff digest, fence, actor and reason. The journal supports proposed, accepted,
working, blocked, rejected and cancelled states. It rejects submitted, reviewed
and succeeded claims until their separate evidence admission is implemented.

Plan revisions and assignment writes serialize on the same initial plan row or
MongoDB document. MongoDB uses a real transactional write to fence concurrent
snapshots. The shared repository validates current plan bindings before new
progress, while permitting terminal observations after expiry or replan. Identical
historical retries return the original observation and cannot authorize dispatch.
A changed retry conflicts. Each revision and its audit fields occupy one immutable
row; paginated history validates predecessor links.

The first assignment lifecycle test run failed for the absent API. Unit tests
cover skipped states, stale revisions, changed digests/fences, actor restrictions,
expiry and unsupported success/submission/review claims. Backend conformance adds
exact scope, detached reads, historical replay, concurrent proposals/transitions,
replanning, cancellation and bounded history. Transaction fault tests exercise
rollback after insert, lost commit acknowledgments, expiry and a concurrent plan
writer blocked by assignment persistence. The PostgreSQL and MongoDB tests use
real containers.

This is a progress journal, not a scheduler claim or authorization boundary.
Hosts must authenticate actors and fence live grants and ownership separately.
The journal alone does not fulfill the receiver acceptance port. Evidence-backed
submission/review, verified task acceptance, contradiction handling, shared
reservations, scheduler integration and installed qualification remain open.
The full four-stage goal remains active.

Validation: full coordination (38.326s), planning (2.328s), A2A (1.213s),
PostgreSQL (102.418s) and MongoDB (122.170s) race suites passed. The full SQLite
race run completed in 550.714s with one failure: its previous lost-plan-response
fixture injected after a row insert, which the new transaction no longer used.
The fixture now injects after the transaction commits and asserts that the failed
response exposes no accepted revision. The final SQLite assignment, plan,
transaction and lost-acknowledgment race checks passed (14.997s). All other SQLite
tests passed in the broad run. The complete remote transport race suite passed
(19.662s). Final `make f`, all-module `make l`, root build, documentation
typecheck/lint/build and diff checks passed. No installed-worker qualification is
claimed.


### S3.3 retained submission evidence and execution lineage

CI 40004979616 passed for journal commit `274f75d`.

Working assignments can transition to submitted only with a stored verification
job, exact request/binding digests and a pinned child run revision. Prepared runs
retain the trusted host's handoff digest and parent run through persistence and
resume. Submission checks that lineage against the complete sealed proposal,
then checks the task's candidate shape, source pins, tool revisions and principal.
The task-binding helper is shared with readiness without revalidating the entire
plan for each dependency.

Storage acquires locks in plan, run, evidence order. It validates current job/run
state and captured output, or the matching committed result receipt, and loads
all candidate/supporting bytes under deletion fences. Reference count, per-object
size, total bytes, expiry and the post-read deadline check bound admission.
Failed/inconclusive verifier verdicts stay unchanged, and submitted never means
verified task success. Cancellation and rejection retain the submission pins.
Historical retries acknowledge the original transition after later deletion.

Red tests first failed for missing submission and assignment-provenance APIs.
Backend fixtures reject wrong lineage, principal, producer, media, sources/tools,
revision, binding, job, scope, stale plans, changed runs and deleted evidence.
Transaction tests cover rollback, lost acknowledgments, expiry during reads and
concurrent run mutation/deletion. The initial PostgreSQL concurrency assertion
incorrectly assumed a client timeout meant the write could never happen. The
corrected test validates both rows while the transaction still holds its locks;
a timed-out statement may execute after commit. This preserves the existing
uncertain-outcome rule instead of claiming cancellation guarantees.

Independent review admission, contradictions, verified task acceptance,
scheduler/authority integration, shared reservations and installed qualification
remain open. The full four-stage goal remains active.

Final validation passed: complete coordination, planning, A2A and run race tests;
focused prepared-run, resume, provenance and assignment runtime race tests; and
assignment, submission, planning and transaction checks on SQLite (33.571s),
PostgreSQL (19.331s) and MongoDB (24.018s). Both container backends ran without
skips. After the final Go edits, `make f`, all-module `make l` and the root build
passed. Documentation typecheck, lint and build passed too. Live worker and
installed host qualification remain deferred.

### S3.3 independent review observations

Ruling: retain review findings as immutable observations separate from assignment
success. A review may support, contradict or lack evidence for a criterion, but
only the verification contract can establish acceptance. This preserves competing
findings without a majority-vote path around a failed check. Assignment closure
and human adjudication remain scheduler/policy work.

Review records bind the exact submitted revision, complete verification binding,
a separate read-only handoff and a distinct prepared reviewer run. Principal and
agent endpoint must differ from the producer. Every criterion has a finding with
a reason; support and contradiction require known input references. Review JSON
must match the completed run output, including when the output is retained in
protected candidate storage. No candidate is published by recording a review.

The shared repository implements exact-scope immutable create/get/list on every
backend. It fences the plan and both runs before checking jobs and retained
bytes, preserves historical idempotent acknowledgments, and rejects changed
retries. Reference, byte and deadline limits apply to evidence reads. Review
storage leaves the producer assignment and verifier status unchanged.

Ruling: verification can advance the producer run revision while its submission
is under review. Admission rechecks that current revision against the same
immutable job and binding. Tests first rejected legitimate failed, inconclusive
and committed verification outcomes; they now preserve all three without
altering the original submission pins.

Red tests established missing review domain and storage APIs. Domain tests cover
self-review, copied context, changed subject/binding/plan, unknown or missing
findings, effect grants, expiry and malformed output. Backend tests cover ordinary
and protected output, changed runs, deleted evidence, cancellation, scope,
rollback, lost acknowledgments and expiry during transactional reads.

Current-grant enforcement, scheduler completion, human adjudication, verified
task acceptance and installed reviewer qualification remain open. A different
prepared digest does not prove model independence or the quality of a review.

Final checks passed after formatting: all-module `make l`, root build, complete
coordination/planning/run race suites and review/submission transaction race
checks on SQLite (33.133s), PostgreSQL (29.671s) and MongoDB (33.980s). Both container
backends ran without skips. Documentation typecheck, lint and build passed.
CI 40007588094 for the preceding submission commit still has coverage running;
all its other jobs passed at this checkpoint. No live reviewer qualification
or full-plan completion is claimed.

### S3.2 durable ownership and dispatch uncertainty

Ruling: implement ownership storage before the readiness-aware scheduler entry
point. `ClaimStore` provides acquire, renew, dispatch-intent and expiry primitives;
the planned `ClaimReadyTask` must compose current verified dependencies, grants
and budget reservations with them before runtime dispatch. A reservation alone
never grants execution permission. This leaves the scheduler packet open.

Claims pin exact scope, plan revision, task, worker, grant, deadline and expected
revision. Leases are capped at 30 minutes and the task/plan deadline. Immutable
history and operation acknowledgments share a plan transaction; attempt IDs and
child run bindings cannot be reused elsewhere in the same scope. Renewal keeps
the fence and advances the revision; replacement increments the fence. Replanning
blocks new renewal and dispatch while retaining old claims for discovery.

Ruling: an expired reservation can be released only if dispatch never began.
Dispatch intent is durable before an external call. Once it exists, expiry enters
recovery and cannot be reclaimed through this API. Identical dispatch retries
return `Fresh=false`; after an uncertain commit, a historical acknowledgment
cannot authorize another runtime call. Freshness is checked again after storage
writes, and any failed transaction returns no ownership or dispatch result.

TDD first failed on missing claim APIs, followed by pure lease/fence/state tests
and backend persistence tests. The conformance cases exercise eight competing
claimants, stale revisions, historical retries, immutable history, exact scope,
lease expiry across repository recreation, recovery after dispatch, rollback,
lost dispatch acknowledgments, replanning, removed-task discovery, attempt/run
uniqueness and expiry before commit. No agent or external effect is dispatched by
these tests.

Readiness-aware atomic admission, scheduler fairness/concurrency, parent
cancellation, current-grant rechecks, shared budgets, recovery settlement, task
transitions/events and installed worker crash qualification remain open. The
four-stage goal remains active.

Final review added a fault after the dispatch acknowledgment write. The test
confirms rollback removes the run binding and ownership revision together, so a
retry can still be a fresh first dispatch. A separate red test exposed plan
regression in the pure acquisition function: it accepted a released claim against
an older plan, although storage already prevented that path. Acquisition now
rejects the older plan directly.

After final formatting, all-module `make l`, the root build and complete
coordination/planning race suites passed. Final claim/transaction race checks
passed on SQLite (12.034s), PostgreSQL (15.192s) and MongoDB (17.491s), including
the last-write rollback case. The broader submission/review/planning backend
checks also passed earlier in this packet, with no container skips. Documentation
typecheck, lint and build passed. CI 40009024187 for the preceding review commit
was still running at this checkpoint. Worker dispatch and installed recovery are
not qualified by these storage tests.

### S3.5 shared reservation accounting

Ruling: keep the existing per-run `execution.Budget` behavior while adding the
shared `ReservationPolicy` and `BudgetStore`. Shared zero allowances mean zero
capacity, and the immutable account adds explicit fan-out, replan and monetary
bounds. Integrating these dimensions into prepared execution remains open; no
new per-run field claims enforcement before that integration exists.

`Reserve` and `Settle` serialize through the existing plan transaction. Requests
pin plan/task identity, the current plan digest, a host attempt reference,
allocation and deadline. A plan has one immutable account policy. Replanning
narrows effective limits without clearing earlier charges. Root and parent
capacity are checked together, including concurrent children.

Branches allocate capacity to children without charging it twice. An open branch
retains its full allocation, while child overruns propagate immediately to the
root. Closing requires final child accounting and derives the branch charge from
stored receipts. Known usage can refund unused capacity; unknown dimensions keep
their allocation. Every reservation consumes at least one fan-out attempt. A
1,024-record and 16-level ceiling bounds reads and accounting work.

Money uses integer USD micros and a pinned pricing revision, matching evaluator
accounting. Token-only policies explicitly disable a money ceiling. Estimates and
usage receipts remain trusted host inputs, not model assertions. Aggregate
overflow blocks further admission while preserving original receipt amounts.

TDD first exposed missing shared-account APIs. Tests cover concurrent overbooking,
settlement idempotency, partial/unknown usage, parent accounting, overrun
propagation, immutable policies, scope, price/deadline denials, rollback, lost
acknowledgments, expiry, replanning and overflow. Final review added red tests for
mismatched pricing in aggregate accounting and creation-time tampering in stored
reservations. The shared validators now reject both; the stored reservation digest
covers the complete observation as well as retaining its separate request digest.

Claim admission, runtime/provider usage, review/verification dispatch, replan
policy and installed multi-worker qualification remain open. The four-stage goal
remains active.

Final `make f`, all-module `make l`, root build and execution/coordination/planning/
assurance race suites passed. Backend budget, claim and planning checks passed
on SQLite (27.939s), PostgreSQL (18.902s) and MongoDB (21.570s), with both container
backends running and no skips. Documentation typecheck, lint and build passed.
The preceding ownership commit's CI, 40011074055, was still running at this
checkpoint; the review-only run 40009024187 was cancelled. These local results do
not qualify live provider estimates or installed worker accounting.


### S3.2/S3.5 atomic claim and capacity admission

Ruling: expose `BudgetedClaimStore` as an optional accounting capability while
`ClaimReadyTask` remains pending. The combined request names one claim attempt,
plan/task and leaf reservation. It authenticates no principal and accepts no
caller assertion that dependencies passed.

`AcquireBudgetedTaskClaim` reuses the existing claim and capacity validators
inside one plan transaction. Claim identity, allocation and the immutable
association commit together. Separately created records cannot be adopted, and a
claim cannot acquire another allocation through a changed retry. Confirmed first
acquisition returns `Fresh=true`; retries and recovery lookups return retained
snapshots with `Fresh=false`, including after a lost acknowledgment.

Renewal and dispatch through the ordinary claim API check the associated
allocation and current aggregate account. Settled allocations, expired capacity
and overdrafts block new intent. Lease expiry remains available without a budget
refund. Runtime accounting and authoritative readiness/grant admission still
need integration; no database lock spans an agent or tool call.

Tests first failed at the missing combined acquisition. A later red regression
showed that time could move backward between claim and budget writes without
rolling back. The combined operation now rejects that ordering as well as expiry
after the association write. Tests also cover competing workers and tasks,
rollback at each write group, lost responses, changed retries, standalone-record
adoption, recovery lookup, scope, settlement, overrun, renewal and replanning.

The four-stage goal remains active. Scheduler dispatch, task acceptance,
cancellation, recovery settlement, provider usage and installed multi-worker
qualification are still open.

Further regressions exposed a nested-account gap: an ancestor could exceed its
allocation while the root still had capacity. New reservation admission and
budgeted claim renewal/dispatch now check every ancestor through the same bounded
accounting logic. Both nested-admission and branch-overrun dispatch tests failed
before that change and pass with it.

Final `make f`, all-module `make l`, root build and complete execution,
coordination, planning and assurance race suites passed. Budget/claim/planning
backend checks passed on SQLite (27.339s), PostgreSQL (18.431s) and MongoDB
(24.430s), with real containers and no skips. Documentation typecheck, lint and
build passed. CI 40013300772 for the preceding shared-budget commit is green.
These tests establish the storage behavior, not installed runtime accounting.


### S3.1/S3.3 task-specific verification contracts

Acceptance integration exposed a contract mismatch: verification jobs required
all required checks from the plan contract, even when the task owned only a
subset. The readiness validator already checked assigned criteria, but a job for
one output could not complete without checks belonging to other outputs.

Ruling: derive an immutable task contract from the sealed plan before dispatch.
`TaskContract` takes a new host-selected contract ID, preserves plan constraint
text and every assigned verifier parameter, and makes each assigned criterion
required. It retains plan-contract order and binds plan digest/task ID in the
revision. Replanning requires a new contract. The original plan contract and
handoff remain unchanged.

Readiness accepts a retained projection through `TaskState.Contract`; omitted
contracts preserve existing plan-contract behavior. Submission and review storage
resolve the actual execution contract and use the shared validator. Root-plan
contracts remain supported. A reviewer using the plan contract can resolve it
from the immutable plan even when the producer saved only a task projection.

TDD reproduced missing derivation, rejected projected readiness, rejected task
submissions before/after result commit, and the root-contract reviewer lookup
gap. Tests also reject modified verifier revisions, manifests, constraints,
freshness limits, evidence requirements, another task's contract and a superseded
plan. An assigned optional criterion remains required for the task, and checks
keep their original plan order. The multi-task store fixture completes one
verification job, submits its result and derives dependency readiness while the
whole plan remains unverified.

This fixes contract composition before durable task acceptance. It does not
persist task success or provide current-grant fences and scheduler dispatch. The
four-stage goal remains active.

The existing review suites now run against both plan and projected task contracts,
including failed/inconclusive/committed producer verification and protected
reviewer output with deletion denials. Final `make f`, all-module `make l`, the
root build and complete planning/coordination/assurance race suites passed.
Task-contract, submission, review, transaction and planning backend checks passed
on SQLite (68.046s), PostgreSQL (83.373s) and MongoDB (107.827s), with real containers
and no skips. Documentation typecheck, lint and build passed. CI 40015814552 for
the preceding atomic claim/budget commit was still running at this checkpoint.


### S3.2/S3.3 durable verified task acceptance

`TaskAcceptanceStore` now commits a completed claim and an immutable acceptance
receipt with metadata outbox intent under the plan transaction. Admission pins
the submitted assignment, exact dispatch claim, verification job/result receipt
and current binding. It reloads retained bytes and reports for the task and the
complete accepted prerequisite chain, validates task-specific contracts and
requires direct prerequisite candidates in the child's supporting evidence.

The claim revision, receipt and per-task uniqueness index roll back together.
Identical retries return historical receipts with `Fresh: false`; changed retries
conflict. Completed ownership cannot be renewed or reassigned within the same
plan revision. A newer plan can create a fresh attempt with a larger fence.
Unknown budget usage stays reserved.

TDD exposed a compatibility error when an optional value ID added an empty field
to old claim encodings. The acceptance ID now uses an omitted pointer, preserving
old digests. Further regressions caught renewal changing the dispatch timestamp,
a handoff deadline being extended by a longer claim lease, and completed claims
being unavailable after a new plan revision. The implementation now uses the
original durable dispatch record and checks the handoff deadline independently.
A final pure-validator regression also caught acceptance after the task deadline;
the shared validator now rejects that boundary even without the store claim check.

Tests cover conflicting and identical concurrent acceptance, rollback at all
three writes, lost acknowledgment, stale claims, wrong actors, changed context,
cancelled assignments/runs, replanning, deleted evidence, expired reports and
expiry during reads or after the final write. Dependency tests include a
three-task chain, missing transitive acceptance, missing supporting artifacts,
changed dependency pins and revoked parent evidence.

Host authorization and external revision fences remain required. The fixtures
exercise real storage, but create generation runs before dispatch and do not
qualify an installed scheduler. This packet does not implement runtime dispatch,
review adjudication, budget settlement or public event delivery. G2, live Linux
sandbox qualification and the four-stage implementation goal remain open.

Final local formatting, all-module lint, root build, documentation typecheck/lint/
build and the complete planning/coordination/assurance race suites passed. The
final SQLite task-acceptance race suite passed (34.782s), followed by the added
replan, parent-deletion and pagination checks (7.438s). The broader regression
run passed on SQLite (113.545s) and PostgreSQL (194.920s). Result-commit and review
regressions passed on all three backends, including MongoDB (85.847s).

The broader MongoDB run failed one `transitive_valid` case with an immutable
verification conflict after 122.23s, beyond the fixture's two-minute report
lifetime. That timing suggests expiry, but the rerun must confirm it. Follow-up
PostgreSQL/MongoDB runs then stalled during Docker container creation; an
independent container listing also exceeded an eight-second timeout. Final
backend confirmation and push are pending. No Docker services were restarted.
CI 40017715698 for the preceding task-contract commit passed.


### S3.3 host authorization for task acceptance

Ruling: use an explicit host fence for the whole acceptance request, including
all prerequisite bindings, then authenticate and authorize inside that fence
before entering storage. Separate per-artifact reads cannot establish one guarded
observation of the complete dependency chain. Concrete host integration still
has to supply the current grant/revision guards.

`NewTaskAcceptor` requires storage, `TaskAcceptanceFence` and
`TaskAcceptanceAuthorizer`. It detaches the request, checks exact scope and current
request digest, authorizes every call including historical retries, then validates
and detaches the returned receipt. It preserves the store's confirmed `Fresh`
value. Lost acknowledgment remains ambiguous and requires the same request ID.

The callback lifetime guard now lives in `internal/hostport` and is shared with
verification commits, result reads and exports. Existing verification behavior
and errors remain intact. Tests cover missing, duplicate, escaped and unfinished
callbacks, cancellation, stale dependency bindings, changed actors, authorizer
mutation, typed-nil ports, changed receipts and authorization on historical
retries. This adds a guarded host entry point, not a concrete grant provider,
scheduler or production qualification.

Docker restart was approved and completed after the stalled database runs.
The complete affected backend matrix then passed serially on SQLite (138.387s),
PostgreSQL (273.828s) and MongoDB (144.688s), with real containers and no skips.
The unchanged MongoDB `transitive_valid` case passed in 5.36s. The earlier failure
and container startup timeouts remain recorded above; this rerun clears the
backend verification blocker.

Final `make f`, all-module `make l`, root build and documentation typecheck, lint
and build passed. Complete coordination and verification race suites passed
(29.424s and 5.050s), covering both users of the shared callback guard. Durable
acceptance is committed as `3ae6eec`; both packets are ready for publication.
Atomic readiness and budgeted claiming, scheduler integration, concrete host
grants and live qualification remain open. The four-stage goal is still active.


### S3.2 atomic readiness and capacity admission

Ruling: compose readiness with the existing budgeted claim transaction and the
same retained prerequisite loader used by task acceptance. `ClaimReadyTask`
selects the explicitly requested task; deterministic ordering and fairness remain
scheduler responsibilities. Reusing the shared validators keeps admission and
acceptance aligned. If the host lets external revisions change outside its
fence, storage cannot establish current authorization for those sources.

`ReadyTaskClaimRequest` pins one budgeted attempt and the complete accepted
prerequisite chain. The store reloads assignments, completed claims, verification
jobs, committed results and evidence, applies `planning.Ready`, then writes the
claim, capacity reservation and metadata event intent together. It checks report
freshness again after the final write. Reads remain bounded to 32 MiB total and
8 MiB per result. No transaction invokes an agent or external service.

The original admission survives lost acknowledgment and later evidence deletion.
Only a confirmed first commit returns outer `Fresh: true`; its nested acquisition
is historical. Changed retries conflict, and independently acquired budgeted
claims cannot be upgraded. `GetReadyTaskClaim` provides exact-scope recovery by
claim ID. Host authorization, dispatch-time revalidation, parent cancellation,
scheduler lifecycle and event delivery remain required.

The new request and store tests first failed for the missing APIs. SQLite then
passed admission, conflicting and identical concurrent requests, write rollback,
lost acknowledgment, insufficient capacity, historical retry and dependency
checks. The latter include a three-task chain, missing transitive acceptance,
changed context/digests, cancellation, replanning, deleted evidence and expiry
during reads or after the final write. Shared budget acquisition and acceptance
material loading were extracted without adding another accounting implementation.

Final `make f`, all-module `make l`, root build and documentation typecheck,
lint and build passed. Initial lint found formatting and a byte-comparison issue;
both were corrected before the final checks. Complete coordination, planning and
assurance race suites passed (44.400s, 3.399s and 1.181s).

The affected backend race matrix passed on SQLite (91.045s), PostgreSQL (68.860s)
and MongoDB (111.602s), with real containers and no skips. Each backend passed
101 tests, including 28 new admission cases and existing claim, budget, planning
and task-acceptance regressions. CI 40023614073 for the preceding host-acceptance
commit was still running its coverage job at this checkpoint. Guarded host
admission and scheduler dispatch are next. The four-stage goal remains active.


### S3.2 guarded host admission

Ruling: use the same request freezing, exact-scope check, guarded callback and
current-authorization sequence for task admission and acceptance. The shared
helper owns that sequence; each operation keeps its typed host ports and receipt
checks. This avoids separate authorization paths drifting as the scheduler grows.
Concrete host guards still have to cover all writers and external revisions.

`NewTaskClaimAdmitter` requires storage, `TaskClaimFence` and
`TaskClaimAuthorizer`. It authenticates the caller/worker and authorizes the
requested allocation and complete prerequisite chain inside the host guard.
Every retry repeats authorization. Changed bindings, callback misuse and
unrelated or invalid receipts return zero results. A lost fence acknowledgment
may follow a durable commit, so recovery preserves the original request ID.

`BudgetedClaim.Validate` and `ReadyTaskClaim.Validate` now check request identity,
claim/lease bounds, allocation and event associations. Storage reads reuse those
validators and still compare admissions with the retained budget record. The
admitter detaches returned data and preserves only the trusted outer `Fresh`
flag. It does not invoke the runtime, and historical receipts do not grant
permission to dispatch.

Tests first failed for the missing admitter and receipt methods. The new suites
exercise revocation, changed worker/grant/budget/prerequisite context, cancellation
inside authorization, missing and duplicate callbacks, escaped and unfinished
callbacks, typed-nil ports, lost acknowledgment, value mutation and forged
request digests on unrelated valid acquisitions. A database fixture composes the
host ports with actual storage and checks retry authorization after revocation.
These are port fixtures, not installed identity qualification.

Final `make f`, all-module `make l`, root build and documentation typecheck,
lint and build passed. Initial lint caught a test channel shadowing the newly
imported errors package; renaming that channel cleared the check. Full
coordination and assurance race suites passed (48.470s and 1.210s).

The affected backend race matrix passed on SQLite (67.169s), PostgreSQL (51.916s)
and MongoDB (72.911s), with real containers and no skips. Each backend exercised
host admission, historical retry and revoked authorization alongside the claim,
budget, readiness and acceptance regressions. CI 40025353483 for `1b7e881` was
running at this checkpoint; the older 40023614073 run was cancelled.

Prepared execution still generates its own run ID. Scheduler integration must
bind the child run reserved by dispatch intent to that existing runtime and
preserve claim/authority checks through execution and recovery. Dispatch and
lifecycle integration remain open; the full four-stage goal is active.


### S3.2 readiness-aware dispatch intent

Ruling: retain dispatch intent and the accepted-to-working assignment transition
inside the same plan transaction. Reuse the claim mutation and prerequisite
validators. Lock the handoff's parent run through commit and require its expected
revision in a running or paused state. A paused coordinator may await children;
a terminal coordinator cannot begin new dispatch intent. Current host grants and
external revisions still need host guards.

`TaskDispatchRequest` binds the ready admission, current claim token and digest,
accepted assignment, parent revision, child run ID, complete prerequisite chain,
actor and reason. Dispatch rechecks the current plan, assignment, parent, shared
reservation and retained prerequisite material before writing ownership, the run
association, working assignment and metadata event intent together. It checks
freshness again after the last write. Read bounds remain 32 MiB total and 8 MiB
per result.

Historical receipts validate their retained claim and assignment journal links.
Identical retries recover the original receipt with `Fresh=false`; changed
requests conflict. Independently retained raw dispatches and already-working
assignments cannot be adopted. An intent does not create or adopt a child run.
The prepared runtime still needs to consume the reserved ID through guarded,
unique creation and recovery. No external call occurs in this transaction.

The new store tests first failed for the missing dispatch APIs. Initial SQLite
and domain checks then passed. Coverage includes cancellation, stale revisions,
changed plans and prerequisites, write rollback, lost acknowledgment, concurrent
identical and conflicting requests, expiry after writes and historical recovery.
Final `make f`, all-module `make l`, root build and documentation typecheck,
lint and build passed. Initial lint reported formatting in the four new Go
files; formatting cleared those errors. Complete coordination and assurance race
suites passed (65.321s and 1.230s).

The final dispatch race suite passed on SQLite (30.304s), PostgreSQL (43.531s)
and MongoDB (24.777s), with 37 passing test entries per backend, including the
suite wrappers, and no skips. These tests used real database containers.
CI 40026796716 for the preceding `52de39c` commit completed successfully.
The broader backend race matrix also passed, with 119 passing test entries per
backend and no skips: SQLite 102.372s, PostgreSQL 110.611s and MongoDB 60.868s.
It covers claim, budget, assignment, submission, acceptance, readiness and
dispatch regressions. The final dispatch run additionally covers the strengthened
historical journal checks and dispatch after renewal with a paused parent.

Host dispatch guards, prepared-runtime binding, lifecycle integration and live
qualification remain open. The four-stage goal remains active.


### S3.2 guarded dispatch admission

`NewTaskDispatchAdmitter` now wraps dispatch storage with explicit host fencing
and authorization. It uses the same request freezing, exact-scope checks and
synchronous callback guard as claim admission and task acceptance. The host
resolves the retained admission, assignment and parent, authenticates the worker
and actor, and authorizes the allocation and complete prerequisite chain.
Every retry repeats that check.

The wrapper validates the full dispatch receipt, detaches its data and preserves
only the trusted storage Fresh flag. Changed requests, cancellation, callback
misuse and invalid associations return zero results. A lost acknowledgment may
follow commit; recovery preserves the original request. This operation still
admits intent without invoking a worker.

Tests first failed for the missing API, then passed the focused domain and
SQLite fixture checks. They cover fence ordering, request and receipt mutation,
revocation on historical retry, changed prerequisite revisions, parent and run
bindings, malformed receipts, missing ports and lost acknowledgments. The shared
callback guard retains its existing escaped and unfinished callback coverage.
The database fixture checks fresh admission, historical retry and revocation.
It is not installed identity qualification.

Final `make f`, all-module `make l`, root build and documentation typecheck,
lint and build passed. Initial lint found formatting in the new wrapper, its
tests and the database fixture; formatting cleared those errors. Full coordination
and assurance race suites passed (35.690s and 1.176s). The dispatch backend race
suite passed on SQLite (18.928s), PostgreSQL (18.553s) and MongoDB (16.424s), with
real containers and no skips. CI 40028631395 for `cc7e36b` was still running at
this checkpoint.

Prepared execution needs atomic reserved-child creation and verification-intent
registration under the live claim, assignment and parent guards. Adding only a
run-ID option would leave a check/create race and cannot establish safe dispatch.
Runtime integration and the full four-stage goal remain open.


### S3.2 atomic reserved child creation

Ruling: create a child and its required verification policy under the plan
transaction before invoking the runtime. Reuse capture-intent registration,
active-parent validation and each backend's run insertion mapping. Mongo's
transactional insert uses Grove's document builder and the session context,
without invoking mutation hooks inside a replayable transaction. Ordinary
CreateRun retains its existing hook path.

`TaskRunRequest` binds current dispatched ownership, dispatch intent, working
assignment, parent revision, complete prerequisites, pristine run configuration
and schema-2 capture intent. Local agent identity must match the claimed worker.
The store reloads current retained materials, checks the handoff and reservation
bounds, validates task contract and input provenance, then writes the child,
capture intent, identity indexes and creation receipt atomically. Lease, deadline
and prerequisite freshness checks repeat after the final write. No provider,
tool or agent lifecycle callback runs in this transaction.

The creation receipt retains metadata and the initial run digest, without
copying private input into another journal. Original requests can reconstruct
the initial snapshot using its creation time. Creation timestamps use milliseconds
for consistent storage round-trips; fences and full clock observations determine
admission. Identical retries acknowledge history with `Fresh=false`; changed
requests conflict, and existing child rows cannot be adopted. Historical recovery
does not restore deleted input evidence or change the child's current state.

The store tests first failed for the missing APIs. Initial SQLite and domain
checks passed, including stale ownership, cancellation, changed plans and input,
budget/output bounds, rollback at each write, concurrent requests, lost response,
current prerequisites and expiry after the final write. Review removed a draft
receipt shape that duplicated private input. The receipt privacy regression now
checks the retained database row. The first backend matrix exposed a PostgreSQL
comparison failure: the driver returned identical timestamps in the connection's
local zone. The regression now checks the instants and normalizes only those
three timestamp fields before comparing every remaining run field.

After final formatting, all-module `make l`, the root build and the complete
coordination, engine, run and assurance race suites passed. The engine suite took
384.094s. The corrected child-creation race suite passed on SQLite (12.425s),
PostgreSQL (10.055s) and MongoDB (11.864s), with no skips. The broader matrix also
covered dispatch, capture intents, execution inputs, transcripts, run revisions,
budgets and outcomes; its only failure was the corrected timestamp assertion.
Run scope checks passed on all three backends, and docs typecheck, lint and build
passed. Docker-backed PostgreSQL and MongoDB checks ran against real containers.

Prepared-runtime startup, per-call authority and lease enforcement, cancellation,
recovery settlement and installed worker qualification remain open. The full
four-stage goal remains active.

### S3.2 guarded child creation

Ruling: reuse the shared task authority guard before atomic child creation.
Require explicit host fencing and authorization, and validate the entire returned
receipt against the frozen original request. Creation and execution remain
separate operations.

`NewTaskRunAdmitter` requires `TaskRunStore`, `TaskRunFence` and
`TaskRunAuthorizer`. The authorizer authenticates actor and worker, resolves the
retained dispatch, working assignment and parent, and checks current grants,
allocation, prerequisites, prepared configuration and capture policy. The fence
holds the relevant external revision and grant guards through storage. Every
relevant writer must participate. External services need conditional operations
or a declared bounded observation window.

The shared guard checks exact scope, detaches input, rejects changed bindings,
and restricts callback lifetime. Each attempt authorizes again, including a
retry that returns historical creation metadata. The admitter validates the
initial run digest, intent identity and outbox associations, then detaches the
bounded receipt. A lost response may follow a committed child, so recovery must
preserve the original request and inspect current run state.

Tests began with the missing API. They cover callback ordering, detached nested
input, changed input/parent/assignment/budget/policy, revoked authority, changed
prerequisites, cancellation, missing or duplicate callbacks, ambiguous responses,
invalid receipts and required ports. The backend fixture tests fresh creation,
historical replay and revoked retry against each actual store. These fixture
ports are not an installed host identity integration.

After `make f`, all-module `make l`, the root build, docs typecheck/lint/build
and the complete coordination (22.166s) and assurance (1.120s) race suites passed.
The child-creation race suite passed all 40 test entries on each backend:
SQLite (16.367s), PostgreSQL (8.903s) and MongoDB (11.433s), with no skips.
Prepared execution, per-call guards and the full scheduler remain open.

### S3.2 prepared task request construction

Ruling: assign the reserved child ID before scanning or building verification
inputs, and reuse the ordinary prepared-run construction path. Keep preparation
separate from creation and external execution so callers can retain one exact
request across ambiguous acknowledgments.

`Engine.PrepareTaskRun` accepts current claim and dispatch references, the sealed
working assignment, parent revision, reserved child ID, session, prerequisites
and reason. It checks structural associations, local agent identity, required
verification and the prepared budget ceiling. The shared preparation helper
validates frozen policy and revisions, scans input and private history, builds
the capture policy and persists authorized execution-input bytes. It returns a
pristine detached `TaskRunRequest`, without a child row, capture-intent
registration, lifecycle events or model/evaluator calls. Session resolution and
input-evidence retention remain real storage operations.

Task contract projections remain supported. Preparation must not compare their
digest with the full plan contract. Atomic creation validates the exact assigned
checks and artifact binding under current storage guards. A host builder still
supplies producer and context provenance; preparation is not authorization.

Tests began with the missing preparation API. They cover reserved identity,
pristine output, private input evidence, scanned history, detached options,
invalid ownership/assignment/budget/scope, absent required verification, changed
builder input, denied evidence access and cancellation. A projection regression
covers differing task and plan contract digests. The SQLite integration fixture
creates a plan, reserves capacity and ownership, records dispatch, prepares the
child and admits its creation with the actual engine and store. It checks atomic
capture-policy registration and idempotent replay without invoking a provider.
The complete engine (371.504s), coordination (18.947s), run (1.117s) and assurance
(1.134s) race suites passed. The final task-preparation race tests, including the
projection constructed and checked through the planning API, passed in 21.917s.
After final formatting, all-module `make l` passed. The root build and docs
typecheck, lint and build also passed. Runtime invocation, per-call guards,
recovery and the complete scheduler remain open.

### S3.2 task invocation leases

Ruling: hold a host-issued authority lease through the complete external call,
including streamed response consumption. Carry the task claim marker in run
provenance so resume does not silently revert to ordinary dispatch authorization.

`WithTaskCallGuard` installs an explicit lease provider. `TaskCallRequest` binds
scope, agent/run/parent/claim identities, handoff and prepared digests, operation,
deadline and exact private payload. Requests are bounded and detached before the
host sees them. The host must authenticate the context principal, resolve current
ownership and parent state, verify grants/prerequisites/capacity and fence every
relevant writer. Cortex checks the returned digest, expiry and revocation channel.
It narrows call context lifetime and releases the lease once. Invalid grants are
released when possible. Host errors do not expose private policy details.

Sync and streaming generation use the guard, as does shared tool dispatch after
ordinary authorization, including approved resume dispatch. Stream ownership
lasts through consumption and close. Cancellation overrides late provider success
or EOF. Release failures are uncertain outcomes and never successful EOF; usage
remains available. Panics during provider opening, stream consumption or scanner
setup still release the lease. Synchronous return-boundary checks observe the
revocation channel before accepting success, including late EOF. The
existing uncertain tool path stops subsequent effect dispatch.

`PrepareTaskRun` sets `TaskClaimID`; creation rejects a conflicting marker and
retains the field through backend run mapping. Empty legacy markers retain the
existing execution path. This is cooperative cancellation, not hard isolation.
The host must implement conditional external fencing or declare an observation
window, and must not hold database transactions across calls. This guard does not
settle shared accounting or replace durable provider dispatch/recovery records.

Tests began with missing guard APIs. They cover missing/revoked/malformed grants,
expired or widened deadlines, detached payloads, cancellation during generation
and stream consumption, duplicate close, uncertain release, provider panic and
shared tool dispatch. Real sync and streaming loops reject task-marked calls
without a guard. Claim-marker conflicts and backend round-trips are covered.
Two further regressions reproduced a provider-consumption panic leaking its
lease and synchronous revocation returning success before the watcher ran.
Legacy continuation checks also reject task markers that would be discarded.

The final SQLite, PostgreSQL and MongoDB task-run transaction matrices passed
under the race detector with no skipped tests. The full engine suite reached its
default ten-minute timeout in a verification fixture. The thirty-minute retry
passed in 450.384s; the final task-call race regressions passed in 1.358s.
Resume, required-verification and preparation tests passed in 179.314s, and
coordination, run and assurance race suites passed. Final `make f`, all-module
`make l`, root build, docs types/lint/build and diff checks passed.

The admitted-task runtime entry point, installed host qualification, durable
recovery and the full scheduler remain open.

### S3.2 admitted prepared task execution

Ruling: admit the saved request inside the runtime entry point and execute only
on a confirmed fresh receipt. Reconstructing a request or using a historical
creation receipt could change inputs or duplicate a provider invocation after a
lost acknowledgment. Ambiguous starts require reconciliation.

`WithTaskRunAdmitter`, `RunPreparedTask` and `StreamPreparedTask` connect atomic
reserved-child creation to the existing ReAct runtime. Both the admitter and
per-call guard are mandatory. Preparation revisions, exact scope, provenance,
persona, budgets and capture policy are checked before creation. Captured input
reads are authorized and validated against their complete artifact binding and
run identity. Current conversation history is never reloaded.

The runtime verifies the fresh receipt, stored initial child and capture intent
before emitting lifecycle hooks. Historical receipts, admission errors and
post-commit ambiguity return `ErrTaskRunReconciliationRequired`. The child then
uses ordinary prepared execution, streaming, suspension and required verification.
Task-call leases apply through the persisted marker. Failed verification keeps
candidate output private.

Prepared provenance construction and stream launch are shared with ordinary
prepared runs. The authorized input reader is shared with required-verification
resume, including artifact byte and reference checks.

The first test failed on the missing public entry point. SQLite fixtures now
cover a verified fresh execution, immutable captured history, a refused second
execution, changed bindings, missing ports, current-policy/input denial, lost
acknowledgments, post-commit cancellation, malformed receipts, changed children,
request detachment, per-call denial and verified/failed streaming. An additional
regression reproduced mutation of an input reference through a storage adapter's
shared scope slice. The reader now detaches the expected reference and hashes it
before loading; task entry and required resume reject the changed binding.
These fixture ports do not qualify host identity or cross-process fencing.

One added test incorrectly expected a non-nil run on generation denial. The
runtime returns an error and records failure in storage, so the assertion now
checks that durable state. The in-flight broad run containing the old assertion
was stopped and restarted after correction.

Final checks passed: `make f`, all-module `make l`, root build, docs types/lint/build
and diff checks. The complete engine race suite passed with a thirty-minute
limit in 708.264s. The corrected task-runtime matrix passed in 47.446s and the
aliased-input resume regression in 2.882s. SQLite fixtures exercised these runtime
paths; the preceding task-creation packet separately passed SQLite, PostgreSQL
and MongoDB transaction matrices without skips.

Child completion still needs task acceptance and shared settlement. Scheduler fairness/lifecycle, ambiguous-start recovery,
provider journaling and installed host qualification remain open.

### S3.2 durable call journal storage

Ruling: retain each logical invocation slot before dispatch, and record a normal
local return separately. A missing completion is unresolved. Changing the attempt
ID, waiting for expiry or finding a completed record cannot grant a fresh call.
Provider effects and shared usage require their own evidence and settlement.

`execution.RunCallStore` now has bounded metadata contracts, exact request and
result digests, immutable return observations and paged discovery for one run.
SQLite, PostgreSQL and MongoDB expose the capability through the shared assurance
repository. No migration is needed: records use distinct kinds in the existing
assurance table. Context helpers detach correlation metadata for provider adapters.
Prompts, arguments and output bytes are excluded from journal rows.

The initial domain test failed to compile on the missing API; SQLite conformance
then failed because the journal capability was absent. Both passed after
implementation. An expanded scope test initially used four levels, which current
Cortex rejects. It now checks valid scopes that differ at the third level.

The race conformance tests passed against SQLite (8.487s), PostgreSQL (13.286s)
and MongoDB (19.289s), with real containers and no skips. They cover competing
starts, replay, changed attempts, conflicting returns, concurrent identical
completions, stable observation times, exact scope, cancellation and bounded
pages. Domain and shared-repository race tests passed, including detached context
metadata and lost insert/return acknowledgments. A committed insert with a lost
acknowledgment never grants a fresh retry.

Runtime invocation wiring, qualified provider recovery and scheduler integration
remain open. The storage capability alone does not protect provider calls.

Final `make f`, all-module `make l`, root build, docs types/lint/build and
diff checks passed. The storage packet is complete; runtime wiring remains next.

### S3.2 runtime call journal integration

Ruling: reserve a stable logical slot under the current authority lease before
invoking a callback. Generation uses the ReAct step index, shared by sync and
stream modes. Tools use their originating generation index and tool-call ID.
Approval resume retains that index. Changing an attempt ID or payload cannot
create a fresh slot for the same logical call.

Task-marked generation and local tool dispatch require `execution.RunCallStore`.
Only a confirmed fresh start permits invocation. Journal errors, historical
records and withdrawal during the write prevent the callback. The journal binds
the exact private request digest accepted by the lease, including a narrower
caller deadline, and the provider receives detached correlation metadata.

Normal returns are recorded while the lease is held. Streams hash the ordered
JSON chunk sequence and record a completion only after a clean EOF and successful
close. Early close, cancellation, callback failure and panic leave an unresolved
reservation. A joined EOF error cannot hide another failure. Usage observations
remain separate from provider billing and shared settlement. Uncertain tool starts
or returns stop further effects. Lease-release failure remains uncertain even
when a local return was recorded.

The first runtime test failed because calls bypassed the journal. The corrected
race matrix passed in 57.797s, covering missing storage, historical starts, write
errors, correlation metadata, stable slots, stream completion boundaries,
cancellation, revocation, panic cleanup and tool uncertainty. Real SQLite tests
cover sync and stream completion, a committed start whose acknowledgment was
lost, return-write failure, and direct versus approved tool execution. Approval
resume produced the same logical tool slot as direct dispatch; both retained
three completed records for two generations and one tool.

Additional race regressions passed in 16.155s for the narrowed lease request
digest, storage-input detachment and a return committed before its acknowledgment
was lost. The last case retains a local completion while keeping output private
and withholding verification.

The whole-engine race run reached its thirty-minute package limit after 633
passing cases (334 top-level tests), with no individual assertion or race failure
reported before the timeout. The host load average was observed near 300 during
that run. The lower-parallelism follow-up passed all 26 remaining top-level tests under
the race detector in 134.753 seconds, including the later-added binding regression.
The union covers all 360 current top-level engine tests. The additional lost-return
acknowledgement regression also passed separately. The original log is retained;
these passing batches do not turn the original timeout into a passing suite.
Runtime recovery, provider evidence,
shared settlement, scheduler lifecycle and installed host qualification remain
open. No journal observation grants automatic retry or task acceptance.


### S3.5 bounded replanning proposals

`c5742c5` adds `planning.ProposeReplan` with versioned progress snapshots,
normalized policy bounds and deterministic action/reason output. Defaults allow
two proposal attempts, trigger on two equivalent failures or three attempts
without progress, and observe five-minute idle/wait bounds with thirty seconds
of deadline reserve. Hosts can change the defaults within finite hard bounds.

The policy reuses `execution.BudgetSnapshot` for conservative charges, including
outstanding replan holds. It rejects an account allowance wider than the policy
ceiling. There is no second attempt counter. Unknown effects require reconciliation
even after a deadline. Other decisions honor shared capacity and absolute time;
waiting cannot reset a deadline. Every attempt and conflict binds the current
plan digest, so a revised graph rejects its predecessor's observations.

This helper selects a proposal and performs no I/O. Automatic execution still
needs current host authorization, durable fresh admission, one reserved replan
unit and proposal resources before a model call. Failed or rejected proposals
retain their charges. Apply resulting graph changes through `ApplyRevision` and
the expected-revision store commit; acceptance and authority checks are shared.
Scheduler integration and provider-qualified settlement remain open.

The first tests failed on the missing API. Race tests now cover repetition,
progress, waits, deadline pressure, contradictions, malformed observations,
policy limits, graph binding, deterministic decisions, unchanged inputs and
rejection of acceptance or budget weakening. Eight concurrent reservations admit
only two replan units; unknown usage and a subsequent plan revision preserve the
charges. The final matrix passed against SQLite (13.405s), PostgreSQL (13.778s)
and MongoDB (24.072s), with real containers and no skips. Focused planning race
tests passed in 1.618s; the complete planning race suite passed during development.
Final formatting, all-module lint, root build, docs types/lint/build and diff
checks passed.


### S3.2 bounded scheduler admission

`coordination.TaskScheduler` implements `Scheduler.Tick` around the existing
readiness and guarded claim contracts. Each tick reloads state after local
capacity becomes available, selects the first ready task in graph order, resolves
current worker identity and allocation, and invokes a host worker only after a
validated fresh claim. Ambiguous or historical admission returns a reconciliation
error without dispatch. Worker completion does not imply task acceptance.

The local queue is FIFO within each exact scope and round-robin across queued
scopes. Global concurrency, per-scope concurrency, queue depth and total tick time
are finite. Caller cancellation and shutdown cancel queued and owned work; the
worker context also ends at its original claim, task or plan deadline. Shutdown
retains uncertain durable ownership and reservations. A callback that ignores
cancellation keeps its local slot until it returns.

The red test run failed on the missing scheduler API. The first targeted race
matrix passed; a later context-aliasing test exposed an incorrect fixture
assumption: `ScopeFromContext` already returns detached scope. The fixture now
checks that mutating the returned scope cannot affect the worker context, reusing
the existing scope isolation contract.

Real SQLite, PostgreSQL and MongoDB checks exercise eight competing ticks across
two scheduler instances, both identical and distinct request identities. Exactly
one worker is invoked. A restarted scheduler cannot execute the retained admission
again, and the allocation remains charged after the local worker returns. Revoked
worker authority prevents both invocation and reservation. The fixture worker does
not invoke a provider, and its test fence does not qualify a distributed grant
service.

Concrete engine worker wiring, automatic claim renewal, persisted parent-cancel
delivery, sweeper integration and real worker crash/restart qualification remain
open. This packet is not completion of S3.2 or the four-stage goal.


Validation for this scheduler packet:

- Full coordination race suite: 23.127 seconds.
- Scheduler database race checks: SQLite 6.386 seconds, PostgreSQL 5.251 seconds,
  MongoDB 7.298 seconds. PostgreSQL and MongoDB ran real containers without skips.
- `make f`, all-module `make l`, root `GOWORK=off go build ./...`, docs
  `pnpm types:check`, `pnpm lint` and `pnpm build` passed.
- Shutdown during a successful admission is observed before the cancellation
  callback runs, preventing a newly committed receipt from starting a worker.

The previous runtime journal commit is `55b9503`. Its local engine evidence covers
all 360 current top-level tests across the original run and the passing remainder;
the original whole-suite timeout remains recorded. The earlier replanning commit
`c5742c5` had its CI run superseded by the runtime journal push, so it has no separate
successful CI result. The runtime journal CI includes that change.


### S3.2 engine scheduler worker adapter

`Engine.NewTaskWorker` connects the scheduler's fresh claim to guarded dispatch,
`PrepareTaskRun` and `RunPreparedTask`. The host resolver supplies an accepted
assignment, current parent revision, stable child identity and immutable prepared
agent. It receives detached admission values and must not invoke a provider or
tool while resolving definitions.

The adapter validates the exact admitted claim before dispatch and requires a
fresh dispatch receipt. It reuses existing private input capture, child admission,
call journals, publication controls and verification. Reusing an old claim's
`Fresh` flag cannot replay dispatch. A lost child-creation acknowledgement leaves
the child retained without provider invocation. Neither verified run output nor a
worker return completes task acceptance or settles shared capacity.

Tests use real SQLite persistence through scheduler admission, dispatch and engine
execution. Provider and verifier fixtures establish local control flow only.
Parent cancellation, revoked dispatch, missing call authority, historical receipts,
changed bindings and acknowledgement failures stop execution. The shared fixture
now supports both already-claimed runtime tests and fresh scheduler admission.

The initial red run failed on the absent adapter API. The first adapter race
matrix passed in 20.514 seconds. Full engine validation is tracked separately.
Host grant services and resolvers, automatic lease renewal, recovery settlement,
installed lifecycle wiring and provider qualification remain open.


The bounded scheduler packet is committed as `0973c28`. Its CI superseded the
runtime journal run before the latter finished the root test job. Local evidence
remains as recorded; a cancelled CI run is not a passing qualification.


The adapter and prepared-runtime regression matrix passed under the race detector
in 165.572 seconds. All-module lint, formatting, root build and docs type/lint/build
checks passed. Final post-format adapter and scheduler integration race checks
passed in 24.559 seconds, followed by another successful root build. The full
engine race suite then passed in 558.179 seconds: 731 cases across all 365
top-level tests, without an assertion failure, race report or package timeout.
This full run also covers the earlier runtime journal implementation. The older
whole-suite timeout remains historical evidence, not a passing run.


### S3.2 atomic execution renewal

`TaskRenewalRequest` binds dispatched ownership to the expected parent, child and
assignment revisions, complete accepted prerequisite chain, worker actor and
reason. `RenewTaskExecution` uses the shared plan transaction and existing claim
renewal logic. It checks the retained child-creation chain and capture provenance,
locks the parent and child, rechecks prerequisites and shared allocation, and
persists the renewed claim with its metadata event intent. There is no provider
or tool invocation inside the transaction.

The claim cannot extend beyond the parent, child, assignment or capture-policy
deadline. Cancelled, failed and incomplete children are rejected. Paused children
and completed runs awaiting acceptance may retain ownership under host authority;
this does not resume them or refund their allocation. The task claim marker is
required, so an unmarked legacy child cannot acquire this renewal behavior.

`NewTaskClaimRenewer` reuses the shared synchronous host fence and authorization
implementation. Every retry repeats authorization. Storage returns historical
receipts with `Fresh: false`; a lost acknowledgement never proves that a local
worker may extend its execution window. The existing scheduler still stops at its
original claim expiry. Its guarded renewal loop remains the next integration.

Red tests failed on the absent public APIs. A SQLite expiry fixture initially
advanced one minute while requesting a two-minute renewal, so the lease was
still valid. The corrected fixture advances to the renewed expiry and verifies
transaction rollback. An initial broad test expression also ran unrelated SQLite
checks; the recorded failing run is not treated as passing evidence. Subsequent
runs use explicit conformance and transaction test names.

The first complete renewal matrix passed under the race detector against real
SQLite, PostgreSQL and MongoDB stores in 16.410, 9.776 and 13.302 seconds. It covers
parent/child cancellation, changed revisions and provenance, cancelled assignment,
expired ownership, settled capacity, deadline limits, paused/completed retention,
rollback after the event write, lost acknowledgement and concurrent identical or
distinct requests. Final full task-run transaction validation is tracked below.

Final coordination race tests passed in 60.064 seconds. The complete task-run
transaction suites passed against SQLite (29.409 seconds), PostgreSQL (17.571
seconds) and MongoDB (19.766 seconds), with real containers and no skipped cases.
Formatting, all-module lint and the root build passed. Docs type checking and
lint passed before the successful production build. The build retained its
existing metadataBase warning.


### S3.2 scheduler ownership watchdog

The atomic renewal packet is committed and pushed as `ca93d9c`. Optional
`SchedulerConfig.Renewal` now watches the original claim expiry from worker entry
and begins renewal only after fresh child creation. The engine adapter supplies
that activation through the existing prepared runtime. Dispatch and child creation
therefore consume their original claim revision before any renewal changes it.

The loop reloads current execution bindings, calls the guarded renewer and accepts
only a fresh, exact successor acknowledged before the previous expiry. Historical
receipts, uncertain writes, revoked authority, malformed bindings, clock rollback
and late responses cancel local work. A stalled renewal cannot prevent the expiry
watchdog from cancelling the worker. Cleanup cancels in-flight renewal before
waiting for it, and the local slot stays occupied until those callbacks return.

The scheduler does not extend a provider or tool call's separate authorization
lease. That remains a host-enforced limit. Tick, plan and task ceilings also remain
fixed. Suspended work needs its existing authorized resume path, and completed
runs still need task acceptance and shared usage settlement.

Red scheduler tests failed on the absent renewal API. Engine activation tests
then failed because the adapter did not implement the renewal worker interface.
The first watchdog race matrix passed in 1.371 seconds; the lifecycle matrix
passed in 1.906 seconds. Engine activation tests passed in 10.208 seconds.
An integration fixture initially used Assignment.ID instead of the actual
Handoff.AssignmentID; correcting those test fields resolved compilation. The
scheduler/worker integration matrix then passed under the race detector in
18.587 seconds, including real SQLite renewal beyond the original claim expiry.
Fixture host ports establish local behavior, not installed host qualification.
Final formatting, lint and regression evidence is recorded below when available.

The prepared task, call guard, worker and scheduler regression matrix passed under
the race detector in 115.899 seconds. Coordination passed in 32.726 seconds.
Docs types, lint and production build passed. Lint identified a local error
reassignment in the activation callback; the corrected version passed all-module
lint and the root build.

Review found one earlier clock boundary: a clock rollback immediately after claim
admission could enter the worker before activation rejected it. The new
TestSchedulerRenewalRejectsClockRollbackBeforeWorker failed because Tick returned
nil. The watchdog now rejects an observation before the admitted claim timestamp
before invoking the worker. Final checks include this fix.

Final post-format checks passed with the startup clock fix: full coordination
race tests (25.990 seconds), scheduler/engine renewal integration and activation
race tests (14.423 seconds), all-module lint and the root build. No formatter
changes reached unrelated files. Linux CI for the preceding atomic renewal
commit passed every completed job, with the root test job still running at the
last observation; that run is not yet a passing CI qualification.


### S3.2 scheduling lifecycle integration

The guarded watchdog packet is committed and pushed as `ae707bc`. Its complete
Linux CI run, `40048809271`, passed, including root tests, nested modules, security,
formatting and docs. The earlier atomic renewal CI run was superseded and cancelled;
the passing watchdog run includes that code.

`TaskService` owns bounded discovery and scheduler ticks. The host supplies scoped
plan identities through an explicitly authorized discovery port. Pages are bounded,
detached and validated before dispatch; current tick admission still controls
execution. Per-instance active-plan deduplication and finite capacity prevent
duplicate local ticks and unbounded goroutines.

`WithTaskScheduling` supplies the engine's prepared worker after validating all
options. Engine startup preserves host identity values but owns cancellation.
Stop cancels discovery and ticks before joining them. A timeout retains stopping
state and rejects restart until callbacks drain. Forge retains its started state
when engine shutdown reports an error. Shutdown hooks run after successful
drainage. Durable claims, journals and reservations remain retained.

Initial service and engine tests failed on absent APIs. The first service race
matrix passed in 1.326 seconds. Engine testing exposed mutation of a reused option
closure; each application now copies the configuration. A Forge regression failed
because a timed-out Stop marked the extension stopped. Its initial failure also
exposed missing fixture cleanup for deliberately blocked discovery; a deferred
release made that failing test terminate cleanly before fixing production behavior.
The expanded service race matrix passed in 1.349 seconds, engine lifecycle and
restart integration in 4.451 seconds, and Forge timeout coverage in 1.743 seconds.

The SQLite engine test runs admission, dispatch, child creation, call journaling
and verification. Restart cannot invoke the provider again or refund shared
capacity. Fixture authorizers are not installed host qualification. Expired-claim
transitions, provider/effect reconciliation and real kill/restart tests remain open.

The broader coordination suite passed in 24.025 seconds. Extension race suites
passed in 1.794 seconds for the package and 27.352 seconds for contracts. Docs type
checking, lint and production build passed, as did all-module lint and root build.
The complete engine race suite passed in 489.164 seconds: all 739 cases and 369
top-level tests, without failed or skipped cases. That full run preceded the two
discovery review fixes below, which receive separate post-format regression checks.

Review found that JSON could repair invalid UTF-8 scope bytes before validation.
The regression observed a scoped state read for an invalid discovery identity.
The service now checks scope bytes and aggregate size before serialization; the
malformed-page race matrix passed in 1.268 seconds. Core scope validation outside
this boundary still needs a separate UTF-8 audit. The failing test's cleanup was
also corrected so a fatal assertion cannot leave its polling service running.

A discovery fairness test initially passed because two first-page workers could
race for admission. The revised fixture starts one scope's worker before offering
another plan in that scope, then asks for a different scope. It failed because
the queued same-scope tick consumed the remaining discovery slot. The service now
applies the scheduler's existing MaxPerScope limit to in-flight discovery too.
Skipped plans remain eligible on later passes. The full service race matrix,
including both fixes, passed in 1.457 seconds.

Final diff inspection caught a documentation script copying the orchestration
page over this ledger. The accidental content matched that page byte for byte.
The committed ledger history was restored and this packet's notes reconstructed
before staging; no historical entries or unrelated files were removed.

Final post-format checks passed with both discovery fixes: full coordination
race tests (43.345 seconds), lifecycle/task-worker/scheduler runtime race tests
(191.943 seconds), and extension package/contract race tests (2.211 and 49.457
seconds). All-module lint, root build, docs types, docs lint and production docs
build passed. The complete engine suite and the subsequent focused runtime run
are distinct evidence; neither is claimed as deployed host qualification.


### F0 scope identity encoding validation

Lifecycle scheduling is committed and pushed as `85a52a5`. Its discovery review
identified a broader shared-validator gap: malformed UTF-8 keys and values passed
ValidateScope. New root regressions failed for invalid, overlong, truncated and
surrogate byte sequences. The pre-fix SQLite conformance test also failed because
invalid contexts could create and list agents.

ValidateScope now rejects malformed UTF-8 before identity access or reconstruction.
Context lookup, canonical parsing and stored-scope decoding consume that shared
rule. Discovery keeps its pre-serialization byte budget but delegates scope UTF-8
validation to Cortex instead of maintaining a second rule. Valid Unicode is not
normalized; composed/decomposed names and valid replacement characters retain their
original identity. Root and discovery focused race tests passed in 1.138 and 1.615
seconds. Backend and final checks are recorded below when complete.

The backend Unicode fixture initially used literal escape text for two intended
characters. We corrected it to contain the actual replacement character and
combining accent, then reran the final matrix. SQLite (19.853 seconds), PostgreSQL
(15.852 seconds) and MongoDB (26.693 seconds) passed the scope-boundary race tests;
the database containers ran and no cases skipped.

Final formatting and all-module lint passed. Root and coordination race suites
passed in 1.128 and 70.676 seconds; focused engine lifecycle and task-worker tests
passed in 21.491 seconds. Root build, docs types, docs lint and production docs
build passed. These checks cover stored identity isolation, not live worker or
provider qualification.


### S3.2 bounded claim expiry sweep

The scope identity fix is committed and pushed as `737307c`. The next recovery
slice adds a scoped `ClaimSweeper` over the existing list and atomic expiry
operations. Its initial tests failed because the API was absent. The first race
matrix passed in 1.619 seconds after implementation.

The sweep validates the complete bounded page before writing, uses exact observed
revisions, and keeps dispatch uncertainty in the recovering state. It returns no
cursor after an error or cancellation because earlier writes may have committed.
Conflicts are observations for the next complete pass. It neither executes work
nor settles capacity. Automatic lifecycle polling and provider reconciliation
remain open.

Backend fixtures cover tasks removed by replanning, retained state after a new
sweeper is constructed, and a lost acknowledgment after an expiry transaction.
This is storage restart evidence, not a killed worker or live provider test.
Final checks follow below.

The first backend sweep matrix passed on SQLite (7.537 seconds), PostgreSQL
(24.563 seconds) and MongoDB (62.767 seconds), with real containers and no skips.
The full coordination run caught duplicate output names in the cancellation test's
plan fixture before the sweep could execute. That fixture now gives each task its
own output. Lint found two error-variable reassignments; those were corrected.
The failed lint pass was stopped after its root diagnostics so source fixes could
be checked in a fresh pass. Pagination validation now lives on ClaimListRequest
and is shared by storage and the sweeper.

After the fixture and validation cleanup, focused expiry race tests passed in
1.505 seconds. Final formatting passed, followed by the full coordination race
suite (143.374 seconds) and root build. The complete claim transaction matrix
passed on SQLite (18.927 seconds), PostgreSQL (37.500 seconds) and MongoDB
(62.375 seconds), with no skipped cases. Docs types, lint and production build
passed. All-module lint is still running at this entry.

All-module lint finished with zero issues. CI run `40051658320` also passed for
scope commit `737307c` in 14 minutes 36 seconds, including the preceding lifecycle
work. The claim sweep remains a caller-owned bounded operation; lifecycle
recovery polling, provider reconciliation and killed-worker qualification are
still required.


### S3.2 claim recovery lifecycle

The bounded expiry sweep is committed and pushed as `8591e04`. Recovery now has
an optional `ClaimRecoveryService` and `engine.WithClaimRecovery` bridge. The
service reuses task scheduling's discovery validation, scope limits, cancellation
and drainage through a private worker factory. It pages retained claims with a
per-page timeout and a delay between pages. Failed or restarted passes reread
storage from the beginning; no cursor or historical acknowledgment grants work.

Engine shutdown signals execution and recovery before joining either. Stop timeout
retains owned work and suppresses shutdown hooks. The Forge lifecycle test now
covers both services. The initial service and engine tests failed because their
APIs were absent. Focused service tests passed in 1.365 seconds; engine tests using
real SQLite claim storage passed in 6.040 seconds. Shared reservations stayed
charged and no provider was invoked by expiry.

Review found that a stable discovery page could starve later plans in the same
scope when its first plan finished quickly. A two-page regression failed. The
shared loop now rotates page starts across complete discovery passes without
retaining a queue of skipped identities. Service/fairness race tests passed in
1.337 seconds and the Forge stop-timeout matrix passed in 1.696 seconds.

This implements lifecycle expiry, not provider reconciliation or live worker
qualification. Each active plan pass holds a bounded local slot until completion
or error. Its page count is not capped; the host must provide finite discovery
passes and cancellation-aware storage. Final checks follow below.

The first broad checks passed coordination race tests (30.537 seconds), extension
and contract race tests (2.035 and 33.736 seconds), and docs types/lint/build. Root
lint flagged the rotation's integer conversion; its offset now uses a bounded
byte covering every position in the maximum 256-entry page, without suppression.

A second regression showed that Start could return success while the preceding
Stop was still notifying shutdown subscribers. Engine lifecycle calls now share
a context-aware serialization gate through notification. Queued callers can
cancel their own waits; an uncontended Stop still signals services with an
already-cancelled context. Final checks cover this additional change below.

Final post-format checks passed with the lifecycle gate and bounded rotation:
full coordination race tests (31.801 seconds), engine startup/shutdown/sweeper/
A2A/telemetry/recovery race tests (62.084 seconds), extension and contract race
tests (2.089 and 34.383 seconds), all-module lint, root build, docs types, docs
lint and the production docs build. The earlier full engine run is separate
pre-gate evidence and is still finishing. CI run `40052823799` passed for the
preceding sweep commit `8591e04`, including Linux root tests and security checks.

The full engine race suite finished in 553.847 seconds: 741 passing cases,
including 371 top-level tests, with no failures or skips. This run began before
the final lifecycle serialization change; the post-format runtime matrix above
checks that change. Both results are retained without treating fixture execution
as installed host or provider qualification.


### S3.2 killed task-worker evidence

Recovery lifecycle support is committed and pushed as `1a90403`. A new subprocess
matrix reuses the prepared task-worker fixture and real SQLite file storage. The
parent starts a test-owned worker, waits for its atomic boundary manifest, kills
that exact process, joins it and reopens the database through a fresh connection.
All child temporary files live under the parent's test directory and are removed
by parent cleanup.

The four boundaries are held ownership, committed dispatch before acknowledgment,
entry to a journaled provider call before return, and completed local execution
with verification. Lifecycle expiry preserves dispatched run identity and fences,
claim history, pending/completed call records and charged shared reservations.
Historical admission cannot reach work resolution, a repeated call slot is never
fresh, and changing its attempt ID conflicts. Repeated engine starts leave the
same expired claim revision intact.

The first race run passed all four cases in 11.535 seconds. This is qualification
of existing behavior, so no production change or invented failing regression was
needed. It proves process-crash persistence with a fixture provider and injected
lease time. Live providers, PostgreSQL/MongoDB process failures, remote workers,
late external receipts and governed continuation remain open. Final checks follow.

The combined crash/lifecycle race matrix passed in 31.524 seconds, and docs
checks passed. Lint required the child command to carry a context. It now uses
the parent test context, in addition to the explicit kill/join cleanup and the
child's own test timeout. Final formatting, lint and affected checks are rerun
for that change.

Final formatting, all-module lint and root build passed. The crash/lifecycle race
matrix passed in 24.655 seconds with no skips; the four killed-worker cases took
11.34 seconds of that run. Docs types, lint and production build passed. CI run
`40054330800` also passed for lifecycle commit `1a90403` in 11 minutes 10 seconds,
including Linux root tests, submodules and security checks.


### S3.2 authenticated provider-call receipt recovery

A host-configured provider reconciler now retains immutable terminal proof for
original complete and stream call records. It requires current authorization,
proof authentication and a synchronous host authority fence. Its adapter can only
query provider state. Reconciliation has bounded local concurrency and timeouts;
callbacks must honor cancellation. Saved proof is reauthenticated without another
provider query, including after lost storage acknowledgment.

Receipt persistence reuses the shared assurance repository for SQLite, PostgreSQL
and MongoDB. The full original journal request binds each receipt. Known token
and money usage stays distinct from unknown usage. Local returns remain separate,
so a late contradiction preserves both records and fails recovery. No claim, run,
reservation or task acceptance is changed by a receipt.

The initial receipt and reconciler tests failed on missing APIs. The SQLite
conformance case failed because receipt persistence was absent. A corruption
regression then showed that changed raw JSON could pass canonical digest checks;
reads now also verify the retained body digest. Initial race tests passed the
three real backends and two killed-worker SQLite scenarios. The latter recover
fixture proof after a pending provider call or completed local execution, reopen
storage and check that execution and budget state remain unchanged.

This is host-port and persistence coverage, not live provider qualification.
Original routing evidence, installed provider adapters, lifecycle reconciliation
discovery, guarded claim adoption and shared usage settlement remain open. Final
checks follow below.

Final formatting and all-module lint passed with zero issues. After the lint
fixes, execution and shared assurance race suites passed in 1.535 and 1.261
seconds. The real SQLite, PostgreSQL and MongoDB call/receipt conformance matrices
passed in 11.809, 24.460 and 14.226 seconds, with no skips. They include identical
concurrent receipts and competing proofs, where exactly one proof wins. The
runtime journal and six killed-worker scenarios passed in 33.686 seconds. Root
build, docs type checks, docs lint and the production docs build passed. CI run
`40055208267` also passed for the preceding worker-crash commit `b028774`.


### S3.2 stable call membership before recovery settlement

Ruling: S3.2 recovery and S3.5 settlement need a closed set of invocations before
claiming a complete scan. The existing journal allowed entries to arrive behind
a discovery cursor. Add an irreversible, authorized call-admission barrier before
aggregate recovery or settlement; closing membership does not prove that admitted
calls finished. The protocol requires every deployed writer to participate.

The shared assurance repository now retains run-call seals and serializes them
with every journal admission. Backend plan transaction helpers also serve the
new call anchor, keeping exact-scope SQL and Mongo transaction behavior shared.
A separate anchor supports empty and legacy journals without rewriting retained
call rows. Historical admissions after closure fail closed. Late return and
provider-proof writes remain available.

`execution.NewRunCallSealer` requires a synchronous host authority fence and
authorizer, checks exact request and receipt bindings, and bounds an operation to
thirty seconds. `Engine.NewRunCallSealer` uses the engine's existing store. The
prepared worker test closes a journal before its first call, verifies zero
provider invocations and retains shared charges across a SQLite reopen. The
public engine error is redacted by the existing publication policy.

The first model/storage and host-wrapper tests failed on missing APIs. Initial
race/conformance matrices passed on SQLite, PostgreSQL and MongoDB, including
admission racing with closure and existing planning/claim behavior. Fault tests
cover rollback, lost acknowledgment and legacy rows. A new cancellation-after-
commit regression failed because a successful transaction could still return
fresh admission after cancellation; the shared wrapper now checks cancellation
after commit and returns no admission or seal acknowledgment on that path. The
committed record remains available for recovery. Final checks follow below.

Final formatting, all-module lint, root build and docs type, lint and production
build checks passed. Execution and assurance race suites passed in 1.963 and
1.166 seconds. The focused call/receipt matrices passed on real SQLite,
PostgreSQL and MongoDB in 20.698, 30.158 and 33.643 seconds, without skips. A
standalone MongoDB test also passed, confirming that unavailable transactions
deny admission and closure without retaining a call or seal.

The broad worker, journal and killed-process recovery matrix passed in 329.190
seconds. The broader storage transaction regressions passed SQLite in 446.222
seconds and PostgreSQL in 331.050 seconds. MongoDB initially failed during
container startup when Docker could not resolve its mapped port; the isolated
MongoDB retry passed in 114.630 seconds. Final focused runtime tests passed in
7.006 seconds. CI run `40057246779` passed for the preceding provider-receipt
commit `d829e4b`.

This closes journal membership only. Recovery must still validate late returns,
provider proof, tool effects and verifier usage before guarded adoption or shared
settlement. Those integrations and live qualification remain open.


### S3.2 bounded provider recovery scans

`ProviderReconciler.ReconcilePage` now consumes the sealed journal through bounded
pages. It requires current exact-run discovery authorization, checks the retained
seal and validates every record before its first provider query. The existing
single-call reconciler supplies proof authentication, current commit fencing and
durable receipt replay. Separate page slots prevent callers waiting for provider
slots from exhausting the same queue. The configured timeout bounds the full page.

Terminal, unresolved and tool-call counts remain distinct. A terminal provider
failure is not task success, and tools still need effect reconciliation. Errors
return no cursor even when earlier proof writes committed. Repeating a page
reauthenticates saved receipts and revisits uncertainty without repeating model
execution. A scan does not authorize adoption, retries or budget settlement.

The initial tests failed on the missing page API and discovery authorizer.
The first race run passed page validation, malformed ordering, wrong scope/run,
missing or changed seals, revoked authorization, partial failure and replay tests.
Additional cases cover queue cancellation, whole-page timeout, detached host
inputs and empty journals. The killed-worker SQLite tests now seal and scan before
reopening storage and repeating recovery. Final checks follow below.

Final formatting and all-module lint passed. Lint required three scoped error
variables and preallocation in the shared test fixture; no behavior changed.
The post-fix provider recovery race suite passed in 1.434 seconds and the two
killed-worker recovery cases passed in a 7.709-second engine run. Real SQLite,
PostgreSQL and MongoDB scan tests passed in 3.885, 10.653 and 8.018 seconds, with
no skips. They cover multiple pages, delayed proof, terminal provider failure,
retained proof after service reconstruction and contradictory late returns.

The preceding broader call/receipt/seal matrices passed all three stores, and
full execution and assurance race suites passed. Root build, docs type checks,
docs lint and the 132-page production build passed. Automatic discovery of
recovering runs, guarded adoption, shared usage settlement and installed provider
qualification remain open.


### S3.2 lifecycle provider recovery discovery

`coordination.NewProviderRecoveryService` now traverses authorized plan discovery,
bounded claim pages and sealed call pages through the existing `TaskService`.
`engine.WithProviderRecovery` joins the engine's shared startup/drain sequence and
exposes local status. The Forge lifecycle tests include provider recovery. No new
polling pool or shutdown implementation was added; claim and provider page waits
also share one cancellation helper.

The service scans recovering claims only. Unsealed runs and active claims remain
untouched. Discovery must authorize plan/claim reads, including outstanding work
in cancelled plans, and the scanner still requires current exact-run authorization
and authenticated provider proof. This service does not close journals, adopt
results, retry execution or settle usage.

The first service and engine integration tests failed on missing APIs. A later
fairness regression failed because a run-specific error stopped the pass before
later claims. The pass now retains its first error, continues other claims and
reports failure at the end. Invalid claim pages and cancellation still stop it.
Tests cover scope and service identity, restart, nested call/claim pages, an
arriving seal, bounded reads and timed-out shutdown. Killed-worker SQLite tests
now expire ownership with an injected clock and recover proof through a running
engine before repeating the process with reopened storage. Final checks follow.

Final formatting and all-module lint passed with zero issues. The race detector
caught an unsynchronized seal update in the arrival fixture; the fixture now
guards that update with a mutex. Full coordination and execution race suites
passed in 41.569 and 1.578 seconds. The lifecycle and six killed-worker cases
passed in a 24.042-second engine run. Full extension and contract race suites
passed in 2.147 and 42.037 seconds.

Lint also required a tagged switch and context-first test helper. After those
test-only changes, affected engine and Forge lifecycle race tests passed again,
as did the root build. The final lint pass used an isolated temporary directory
and cache after an earlier pass waited on another repository's lint lock. Only
the waiting Cortex processes were stopped. Docs type checks, lint and the
132-page production build passed. CI run `40060302961` passed for scanner commit
`c308cc0`.

Provider discovery and lifecycle wiring are implemented and fixture-tested.
Installed host discovery and provider proof adapters still need qualification.
Guarded claim adoption, effect recovery integration and shared usage settlement
remain open.


### S3.2 guarded tool effect identity

The first binding tests failed because `EffectRequest` had no original task-call
identity and stores had no bound-admission capability. Requests now retain the
full optional `RunCallRequest`, which participates in the canonical request hash
and the adapter receipt. The executor attaches runtime metadata before admission,
requires `BoundEffectStore`, and keeps reconciliation tied to the original record.
Nested scopes are detached before host or adapter callbacks.

Built-in stores serialize every effect admission with the run seal. Bound fresh
operations require the exact retained tool call, a live deadline and no local
completion. Call completion uses the same guard to prevent a return/admission
race. Late call returns and effect receipts remain writable after sealing. Legacy
unbound requests remain readable but cannot be retroactively assigned to a call.
All built-in effect admission now requires usable database transactions.

Tests cover missing, changed and expired calls, missing store capability,
legacy conflicts, late proof, exact scope, admission/seal and admission/return
races, engine handler propagation, rollback and ambiguous commit acknowledgment.
An older SQLite fault fixture injected outside the new transaction path; it now
injects lost acknowledgment after commit. Final verification follows.

This is a prerequisite for recovering claim adoption. It does not authenticate
external proof by itself, establish task success or settle provider usage.

Final formatting and all-module lint passed. Lint required checked capability
assertions in the shared conformance helpers. After that fix, the final SQLite,
PostgreSQL and MongoDB race matrices passed in 14.376, 9.509 and 14.526 seconds,
with no skips. The MongoDB run also proved that standalone effect admission
returns unavailable without retaining an operation.

The earlier broader effect, call, seal and provider recovery database matrices
passed on all three backends. Full execution and assurance race suites passed,
as did engine effect/recovery tests and the prepared-task/call-journal matrix.
Root build, docs type checks, docs lint and the 132-page production build passed.
Docker was healthy on recheck, so these runs needed no further daemon restart.
CI run `40061756934` passed for preceding lifecycle commit `a66a486`.

Guarded adoption, supervised effect recovery, shared usage settlement and installed
host qualification remain open. Live Linux sandbox qualification remains deferred
until you choose a worker.


### S3.2 sealed effect recovery pages

The initial tests failed on missing `AuthorizeScan` and `RecoverSealedPage` APIs.
The executor now checks current exact-run discovery authority and the retained
seal before and after a bounded effect page. It reuses `RecoverPage` for complete
page validation, per-operation authorization and read-only reconciliation.
Provider and effect scanning share the same detached seal-check helper.

A separate bounded scan queue covers authorization and seal reads without holding
an operation slot. The existing thirty-second effect bound covers the whole
page, including queue wait. Errors return no cursor; earlier durable receipts
remain available to a later authorized pass. Unknown outcomes permit traversal,
while denied or unavailable operations fail the page. Legacy unbound effects are
included without inventing a call relationship or task-acceptance proof.

Tests cover exact scope, missing capabilities and authorizer, absent or changed
seals, malformed whole pages, authority withdrawal after receipt persistence,
unknown and denied operations, detached host metadata, queue and callback timeout,
and repeated multi-page recovery. Real-store conformance includes bound and
legacy operations, delayed terminal evidence and executor reconstruction. Final
checks follow. Lifecycle effect supervision and claim adoption remain open.

Final formatting and all-module lint passed after preallocating the shared test
fixture's request slice. Full execution and coordination race suites passed in
1.412 and 31.714 seconds. Provider and effect scan conformance passed together on
SQLite, PostgreSQL and MongoDB. The final effect-only race rerun passed in 2.897,
6.002 and 7.504 seconds, with no skips. Root build, docs type checks, docs lint and
the 132-page production build passed. Lifecycle supervision and installed host
qualification remain separate work.


### S3.2 lifecycle effect recovery

`coordination.NewEffectRecoveryService` discovers sealed recovering runs through
bounded plan and claim pages. `engine.WithEffectRecovery` adds it to the shared
engine lifecycle and exposes status. The Forge stop-timeout fixture now includes
effect recovery. Provider and effect services share claim validation, seal reads,
page traversal, fairness and drainage through `runRecoveryScheduler`; no second
polling implementation was added.

Initial service and engine tests failed on missing APIs. Tests now cover scoped
service identity, multi-page recovery, restart using retained receipts, detached
registration slices, unsealed and active claims, invalid configuration and owned
callbacks after a stop timeout. The combined shutdown test verifies cancellation
of all four services before waiting for a blocked one.

A subprocess fixture fsyncs one external effect, records the original bound tool
call and is killed before returning its receipt. A new engine reopens SQLite,
scans recovering ownership and retains terminal fixture proof. Another engine
reopens storage and reads that receipt without another probe or external write.
The original pending call, recovering claim, task run and charged budget remain
unchanged. Final checks follow. Guarded claim adoption, shared settlement and live
host qualification remain open.

Final formatting and all-module lint passed. A strengthened active-claim test
caught a fixture that changed state without updating its digest; the fixture now
uses `BeginDispatch` and verifies its validity before testing the skip. The
recovery implementation correctly rejected the malformed claim. Lint also
removed an extra blank line in the extracted traversal helper.

Full coordination and execution race suites passed in 22.388 and 1.324 seconds.
The engine lifecycle and seven killed-worker recovery cases passed in 28.842
seconds. Full Forge extension and contract race suites passed in 2.282 and 48.925
seconds. Root build, docs type checks, docs lint and the 132-page production build
passed. The preceding sealed-scan CI run `40063740341` had passed every job except
its still-running Linux test job at this checkpoint.

Effect recovery lifecycle wiring is implemented and fixture-tested. Guarded
adoption, aggregate provider/verifier usage settlement, installed authority ports
and the remaining Stage 3 and Stage 4 gates remain open. Live Linux sandbox
qualification is still deferred until a worker is chosen.


### S3.2 atomic recovery evidence snapshots

Added `RunRecoverySnapshot` and the optional `RunRecoverySnapshotStore`
capability. Initial tests failed on the missing APIs. Built-in stores now read a
sealed run's full call and effect journals under one transaction guard, with a
thirty-second deadline, a combined 4,096-entry limit and a 16 MiB encoding limit.
Oversized reads and transaction errors return no snapshot.

Provider and effect receipt writes now share that guard. They read existing
receipts before inserting, preserving idempotency without aborting PostgreSQL
transactions on an expected duplicate. Late evidence remains writable after
sealing. The snapshot preserves missing or contradictory observations and legacy
unbound effects; its validation proves shape and membership, not adoption safety.

Shared tests cover stable digests, delayed receipts, caller mutation, exact scope,
changed and absent seals, empty journals, multi-page reads, rollback, lost commit
acknowledgments, cancellation and serialization with all three terminal writers.
The first database run caught a fixture that reused a tool's operation slot for
a model call. The store correctly rejected it; the fixture now uses distinct
slots. Docker was responsive and real PostgreSQL and MongoDB containers started
without a restart. Final checks follow.

Guarded adoption, provider/verifier usage settlement, installed host integration
and live Linux sandbox qualification remain open.

Final review added a failing boundary test for the digest's encoded length. The
16 MiB limit now includes the returned digest, and a snapshot exactly at that
limit passes. Formatting and all-module lint passed after the fix. Full execution
and assurance race suites passed in 19.092 and 1.173 seconds. The final snapshot
conformance rerun passed on SQLite, PostgreSQL and MongoDB with no skips; the
preceding all-three-backend run also passed terminal-writer serialization and
transaction fault cases. Targeted engine lifecycle and killed-worker recovery
cases passed in 18.470 seconds. Root build, docs type checks, docs lint and the
132-page production build passed.

Broader SQLite and engine test runs were interrupted and are not counted as full
suite passes. The engine stack was in SQLite fixture migration; the SQLite run
was within existing private-candidate conformance. CI for the preceding commit
8777671 completed successfully (run 40064472930). New-commit CI remains separate.


### S3.2 recovery evidence inside plan transactions

Initial tests failed on the missing transaction-local snapshot API. The internal
repository now offers `LockRunRecoverySnapshot`: it joins the enclosing database
transaction, verifies the retained journal guard and seal, reads all evidence,
and compares the expected snapshot digest. It cannot start an independent
transaction or succeed without a live transaction. The snapshot remains
provisional until the enclosing commit is acknowledged.

Each database driver reuses its anchor-lock implementation for transaction
startup and journal locking. MongoDB checks the active
session and performs a write to the guard so snapshot isolation cannot permit
concurrent receipt writes to commit unnoticed. SQL uses the existing transaction
object. Standalone snapshots and locked reads share their full evidence reader.

Real-store tests cover missing capabilities, use after transaction completion,
wrong scope, changed seal/digest, cancellation, rollback of a plan-side write,
and all three late receipt writers waiting until the outer transaction commits.
These fixtures exercise storage transactions and do not authenticate external
proof or complete recovering claims. Final checks follow.


Final formatting and all-module lint passed. Full assurance and execution race
suites passed in 1.213 and 17.008 seconds. The broader transaction regression run
passed on SQLite, PostgreSQL and MongoDB in 46.569, 36.996 and 41.731 seconds. The
post-format recovery and run-journal transaction rerun passed in 8.964, 8.123 and
9.487 seconds, with no database skips. Root build and docs type/lint checks passed.
The first docs command used the repository root and found no package manifest;
the rerun used `docs/` and succeeded. Claim adoption, authenticated quiescence
proof and usage settlement remain unimplemented integration work.


### S3.2 guarded recovering-claim adoption

Added explicit recovery proof, fence and authorizer contracts plus a guarded
`TaskRecoveryAdopter`. Ordinary acceptance cannot authorize this transition.
The store reuses the acceptance transaction, current verifier checks and complete
prerequisite validation, locks the sealed journal through commit, and binds every
call to the original worker, claim, prepared execution and assignment. Unresolved
provider calls, unbound/pending effects and contradictory evidence reject adoption.

Successful adoption retains its proof digest in the completed claim and preserves
the expired lease, fence, run and shared reservation. Identical retries recover
historical receipts after uncertain commits. Proof expiry bounds the adoption
window; authenticated worker/tool shutdown must be permanent. Dependent tasks
recheck the recovered journal and current verifier evidence, even after that window.

Initial tests failed on the missing claim transition, store adoption method and
host facade. The first three-backend run found a PostgreSQL fixture foreign-key
violation: the generated worker ID had no agent record. Creating that record fixed
the fixture. Docker was responsive, so no restart was needed. Tests also cover
rollback, lost acknowledgment, expiry during writes and late-receipt serialization.
Final validation follows.

Host authentication and fencing implementations, installed engine adoption,
process-crash qualification and shared provider/verifier settlement remain open.
Live Linux sandbox qualification remains deferred to the user's worker selection.

Final formatting and all-module lint passed after removing one trailing blank
line. The full coordination, execution and assurance race suites passed in
25.191, 19.984 and 1.217 seconds. The broader adoption/acceptance/claim regression
run passed on SQLite, PostgreSQL and MongoDB in 36.540, 45.893 and 54.071 seconds.
After final formatting, focused adoption tests passed on all three in 12.156,
17.488 and 19.842 seconds, with no skips, and coordination transition/facade tests
passed in 1.798 seconds. Root build and docs type/lint checks passed. CI for the
preceding commit 119f708 remains in progress; this packet's CI is separate.


### S3.2 engine-managed recovery adoption

The new service and engine option started with failing API tests. Managed adoption
now reuses the scoped recovery scheduler for claim traversal and lifecycle ownership.
It authorizes full snapshot reads before resolution, gives host callbacks detached
metadata, and checks the returned request against the original claim, seal and
snapshot before entering the guarded adopter. A nil resolution preserves the claim
for a later pass. Each attempt includes all callbacks within a maximum thirty-second
context; cancellation is checked before moving between phases.

Engine lifecycle integration cancels adoption with the other four managed services
before draining any of them. A late resolver cannot submit after cancellation, and
a callback that has not returned keeps shutdown and restart fenced. Tests exercise
these cases, changed evidence, authority denial, timeout and restart. Real SQLite,
PostgreSQL and MongoDB engine tests complete a stored recovering claim, preserve
its lease and child run, leave the local call return absent, and skip completed
claims after restart. Their authority ports accept fixture proof only.

The first service test rerun found two test assumptions: scan counters also advance
on restart, and a one-entry page delays pass-level failure reporting until the next
claim-page read. The assertions now cover restart ordering and let the denial pass
finish before checking its failure count. Production behavior was unchanged.
Final validation follows. Concrete host adapters, process-crash adoption
qualification and aggregate provider/verifier settlement remain open.


CI run 40067486660 for the preceding guarded-adoption commit failed before its
MongoDB conformance subtests: the fixture began migrations after replica-set
initialization but before the member became writable primary. Shared test setup
now polls `hello.isWritablePrimary` with a thirty-second bound before migration.
The same check covers migration and rescope fixtures. Deterministic tests exercise
an election delay, transient probe failure and cancellation; the real standalone
fixture still passes its unsupported-transaction checks.

After the environment resumed, prior local process handles and temporary logs
were unavailable. Fresh post-format runs passed the full coordination race suite
in 23.031 seconds and targeted engine scheduling, recovery, task-worker and crash
regressions in 42.992 seconds. Database adoption and fixture regression runs passed
on SQLite, PostgreSQL and MongoDB in 14.607, 26.120 and 33.489 seconds without skips.
Root build and docs type/lint checks passed. Aggregate lint initially encountered
an unformatted test in concurrent Fabriq work; that work is outside this packet.

The aggregate lint rerun passed after the concurrent Fabriq formatting and
integration-test module graph were corrected by their owner. Docs production build
also passed. The MongoDB readiness fix is committed separately as 4f26f11.
Managed adoption remains qualified by local lifecycle and real-store fixtures;
concrete proof adapters, deployment and process-crash adoption qualification,
shared usage settlement and the remaining four-stage gates are still open.

### S3.2 adoption coordinator process-crash qualification

The new SQLite suite kills a test-owned coordinator at two real transaction
boundaries: after the adoption callback writes its records but before commit,
and after commit before the store acknowledges it. The parent owns the database
directory and waits for the killed child before opening a fresh connection.

Before-commit recovery retains the recovering claim and exposes no acceptance to
outbox discovery. After-commit recovery retains the completed claim and the same
acceptance. A new engine performs an observed recovery scan; guarded retry returns
the historical receipt without duplicating its outbox identity. Both paths preserve
the original lease expiry, fence and run, including the missing local provider
return. Fixture authority still does not authenticate remote quiescence.

The SQLite entry point initially failed to compile until the shared harness was
added. The first harness build exposed a constructor signature mismatch, corrected
to the existing guarded adopter API. The crash cases passed with the race detector
in 4.098 seconds. The expanded crash and adoption suite passed in 12.347 seconds.
Final format, aggregate lint and regression checks follow. Process-fault coverage
for PostgreSQL and MongoDB, installed host adapters and usage settlement remain open.

The final race run passed SQLite in 13.015 seconds, PostgreSQL in 8.569 seconds
and MongoDB in 10.349 seconds without skips. An earlier PostgreSQL attempt failed
during Docker port inspection before tests began; its isolated retry passed.
Root build, docs type/lint checks and docs production build passed. `make f` now
accepts `FORMAT_PATHS` and still defaults to the whole repository. This packet used
`make f FORMAT_PATHS='store/storetest/task_adoption_crash.go store/sqlite/task_adoption_crash_test.go'`
to preserve the optimizer changes being edited in another chat.

Full `make l` passed after the other chat formatted two concurrent optimizer tests
that the initial lint pass had caught mid-edit. No optimizer files are included in
this packet. Live host authority and Linux sandbox qualification remain separate
gates; these crash tests make no deployment-readiness claim.

### S3.5 complete provider/verifier usage observations

New tests first failed on the missing task-usage snapshot and store APIs. The
implementation now reads current ownership, the sealed provider/tool journal and
all verifier jobs in one bounded transaction. It holds the existing plan, run and
journal guards through commit, reuses the existing decoders and rejects truncated
history. Built-in stores expose the optional API through their shared repository.

The summary includes failed provider calls and prior verifier attempts. It retains
observed lower bounds when another receipt is missing, distinguishes known zero,
rejects contradictory provider returns and reports overflow without treating a
saturated value as exact usage. It does not refund capacity or close verification.
Final billing authentication, verifier-admission closure and guarded settlement
against the retained evidence remain open.

Fixture corrections during the first run fixed a local completion field name and
the required SQLite scope-extra JSON encoding for inserted historical test data.
Focused race tests then passed coordination and SQLite in 1.459 and 2.940 seconds.
The same storage checks passed PostgreSQL in 7.211 seconds and MongoDB in 5.346
seconds without skips. They cover stale claims/journals, scope denial, corrupt or
oversized history, lost acknowledgments and a verifier writer blocked through the
snapshot transaction. Final formatting, lint and broader regression checks follow.

After final formatting, the full coordination race suite passed in 32.188 seconds.
Adoption, usage, submission and SQLite process-crash regressions passed SQLite in
34.303 seconds, PostgreSQL in 23.097 seconds and MongoDB in 30.259 seconds, with no
skips. Root build and docs type/lint/build checks passed. Aggregate lint passed
the changed root code but caught formatting and lint issues in the other chat's
in-progress Fabriq, inttest and optimizer fixtures. Those files remain outside
this packet; the final aggregate rerun follows their owner's corrections.

The final aggregate `make l` rerun passed after those corrections. The scoped
formatter used `make f FORMAT_PATHS='errors.go coordination/task_usage.go coordination/task_usage_test.go store/internal/assurance/task_usage.go store/storetest/task_usage.go store/storetest/task_recovery.go store/storetest/submission_transaction.go'`.
The packet includes no Fabriq, inttest or optimizer edits. Shared settlement and
the other four-stage gates remain incomplete.

### S3.5 durable verifier admission closure

Domain and storage tests first failed on the missing closure APIs. The new guarded
closer authenticates current host authority, detaches port inputs and checks the
returned receipt. Storage reuses the combined usage reader inside the existing
plan/run/journal transaction, requires task acceptance and terminal verifier jobs,
compares the complete snapshot, and writes a permanent closure record.

All verification enqueue paths share the closure check. Job claims, renewals and
metered dispatch check it under the run guard too. Historical acknowledgments and
evidence remain readable; closure does not refund funds or prove external billing
finality. Unfinished jobs, changed evidence and unauthorized requests cannot close
admission. Identical retries recover the original receipt after lost acknowledgments.

Initial race tests passed coordination and SQLite in 1.399 and 11.310 seconds.
Expanded tests passed in 2.204 and 13.492 seconds. They cover one fresh receipt
under concurrent closure, enqueue blocked until closure commits, unfinished-work
denial, scope isolation, rollback and cancellation after commit. Final cross-store
regressions, formatting, lint and docs checks follow.

Post-format coordination and verification race suites passed in 130.062 and
18.894 seconds. The broader adoption, usage, verifier-job/call, private-candidate
and capture regressions passed SQLite in 364.145 seconds, PostgreSQL in 54.809
seconds and MongoDB in 33.554 seconds without skips. The SQLite run also exercised
the coordinator process-crash tests. Root build and docs type/lint/build checks
passed. Initial full lint caught concurrently edited Fabriq readiness and inttest
formatting; the final `make l` rerun passed after their owner's corrections.

Formatting used `make f FORMAT_PATHS='errors.go coordination/task_usage_closure.go coordination/task_usage_closure_test.go store/internal/assurance/task_usage.go store/internal/assurance/task_usage_closure.go store/internal/assurance/jobs.go store/internal/assurance/job_calls.go store/storetest/task_usage_closure.go store/storetest/task_recovery.go'`.
This packet contains no optimizer or Fabriq edits. Guarded budget settlement,
external billing qualification and the remaining four-stage gates are still open.

### S3.5 partial usage and overflow settlement accounting

Settlement now carries attributed lower bounds separately from exact usage.
Unknown dimensions retain the larger of the allocation and observed minimum;
known dimensions cannot fall below that minimum. Per-dimension overflow remains
visible in the account even when only one leaf saturated, blocking new admission.
The optional fields preserve existing receipt digests when absent.

New tests first failed on the missing fields. Domain tests cover partial overrun,
unknown refunds, exact usage below its lower bound, invalid overflow, branch and
token-only misuse, pointer detachment and legacy digest stability. Shared store
tests cover persistence, identical retries, conflicting retries, account totals
and denied admission after partial overrun or overflow.

Docker was healthy when the pending restart approval arrived. The recovery and
closure regressions ran against real PostgreSQL and MongoDB containers without
restarting other workloads: PostgreSQL passed in 19.295 seconds and MongoDB in
92.813 seconds, with no skips.

Broad budget race regressions passed SQLite in 17.673 seconds, PostgreSQL in
30.720 seconds and MongoDB in 40.942 seconds. The post-format partial-usage cases
also passed all three stores, with no skips. Final execution and coordination
race tests passed in 25.029 and 26.986 seconds. Root build and docs type/lint/build
checks passed. Initial aggregate lint runs caught only concurrent optimizer
edits. The final `make l` passed every module after the owner's corrections.

Formatting used `make f FORMAT_PATHS='execution/reservation.go execution/reservation_account.go execution/budget_store.go execution/settlement_minimum_test.go store/storetest/budget.go store/storetest/budget_partial_usage.go'`.
Guarded task settlement and live billing proof remain open.

### S3.5 guarded task usage settlement

Domain and store tests first failed on the missing settlement APIs. The guarded
settler now requires explicit current host authority and billing proof. The store
revalidates accepted ownership, the original budgeted claim, closed admission and
the complete usage snapshot under one plan/run/journal transaction. Ordinary and
guarded accounting share the same settlement helper. Both accounting and the
guarded receipt commit together; an independently settled allocation cannot be
adopted. Unknown usage retains its charge and observed overruns.

Initial domain and SQLite race tests pass. Coverage includes actual metered
verifier records, provider and verifier totals, partial usage, expiry, stale
evidence, denied authority, rollback after accounting, lost acknowledgments,
cancellation after commit, concurrent settlement and journal writes blocked
through settlement. Cross-store regressions and final lint/build checks follow.
Fixture billing references establish no live provider authority. Installed host
billing adapters, external finality qualification and the remaining four-stage
gates stay open.

Final coordination and execution race suites passed in 44.390 and 36.931 seconds.
The broader settlement, recovery, budget, acceptance and submission regressions
passed SQLite in 134.578 seconds, PostgreSQL in 78.976 seconds and MongoDB in
277.063 seconds without skips. SQLite also ran both adoption process-crash cases.
Final late-observation tests passed all three stores: stale evidence is rejected,
while a refreshed snapshot and billing proof can include receipts after closure.
Missing closure/budget bindings, independent reservations and a missing paired
accounting record are refused.

Root build, docs type/lint/build checks and final all-module `make l` passed.
Formatting used `make f FORMAT_PATHS='errors.go coordination/task_usage_settlement.go coordination/task_usage_settlement_test.go store/internal/assurance/budget.go store/internal/assurance/task_usage_settlement.go store/storetest/task_usage_settlement.go store/storetest/task_recovery.go store/storetest/task_acceptance.go store/storetest/submission.go store/storetest/submission_transaction.go'`.
The packet contains no optimizer edits. Runtime settlement scheduling and concrete
host billing qualification remain open.

### S3.5 managed task settlement lifecycle

`TaskSettlementService` now traverses completed, already sealed claims through the
shared recovery scheduler. Explicit scan authority precedes accounting reads.
The service validates the original budgeted claim, snapshots combined usage,
closes verifier admission, resolves host billing proof and calls the guarded
settler. Pending proof and open verifier jobs retain the reservation. A retained
receipt uses its original request under current authorization without another
proof resolution.

`engine.WithTaskSettlement` joins the shared engine and Forge extension lifecycle.
Tests cover detached callback inputs, denied scans/closure/billing, changed proof
bindings, pending proof, timeout and a resolver that ignores cancellation. Stop
keeps ownership until that resolver returns and refuses premature restart.

The database lifecycle test closes admission, settles real retained provider and
verifier usage, injects a lost acknowledgment after commit, then starts a new
engine after the proof window expires. Accounting stays unchanged and the billing
resolver runs once. Recovery/adoption regressions passed under the race detector
on SQLite (24.482s), PostgreSQL (17.463s) and MongoDB (21.630s), with no skips.
Docker was healthy when the approved restart was reconsidered, so these checks
ran without interrupting other containers.

Coordination race tests passed (16.945s), as did the focused engine lifecycle
suite (5.612s) and extension shutdown regression (1.688s). Root build, docs
type/lint/build checks and final all-module `make l` passed. The optimizer owner
fixed both findings from the first lint pass. Formatting used `make f` with
`FORMAT_PATHS` restricted to this packet's twelve Go files, followed by a clean
diff check.

The broad engine race suite reached its six-minute timeout while setting up
`TestVerificationWorkerRejectsUnreadableIntent/wrong_scope`; that subtest had
been running for one second. There was no earlier assertion failure. The test
passed independently in 5.993s. The full engine suite remains unverified by that
run; the focused lifecycle and database checks above passed.

This packet does not change optimizer files. Automatic journal sealing for ordinary completed runs,
historical-debt discovery across superseded claims, concrete host billing and live
qualification remain open.

### S3.5 seal ordinary accepted task journals

The first tests failed on the missing `TaskCallSealRequest` and `SealTaskCalls`
APIs. The guarded implementation now binds the full completed claim digest to
the exact run seal request. Storage holds plan, run and journal guards while
checking the retained acceptance and current committed run. It shares the journal
seal write with the existing run sealer. Closure does not infer external
quiescence or release funds.

Settlement can seal ordinary accepted runs when you supply both `CallSealFence`
and `CallSealAuthorizer`. Missing ports preserve the existing wait-for-seal
behavior. Its actor is `ClosureActor`, with separate authorization for sealing.
Each seal attempt uses the configured attempt timeout, independently of the
following settlement attempt.

The database lifecycle fixture now covers ordinary completion as well as recovery
adoption. Both paths close verifier admission, settle retained provider/verifier
usage, recover a lost acknowledgment and reopen after proof expiry without
requesting another bill. Authority tests cover changed requests and receipts,
denial, cancellation, callback misuse and detached metadata. Store tests reject
unaccepted or changed claims, changed run state, foreign runs and wrong scopes.
Rollback, lost acknowledgment, cancellation after commit and a delayed call
blocked through seal commit passed on SQLite, PostgreSQL and MongoDB.

The broader recovery/adoption race checks passed on SQLite (29.353s), PostgreSQL
(24.123s) and MongoDB (31.017s). Journal sealing/recovery regressions passed too
(13.054s, 6.519s and 8.511s respectively), with no skipped database tests.
Coordination, execution and assurance race suites, root build, docs type/lint/build,
scoped formatting and all-module lint passed. The full engine race suite passed
in 406.827s with no skips, resolving the previous six-minute timeout.

CI for `901abd8` exposed two G304 findings in the SQLite adoption crash-test
harness. Its log and boundary reads now use `os.Root` to confine access to the
test directory. Both process-kill boundaries passed again (4.189s); this fix was
pushed separately as `d015e7f`. Gosec v2.28.0 finished with zero findings and no
load errors using CI's exclusions plus the ignored local `.superpowers` scratch
directory, which is absent from the CI checkout. Live billing, historical-debt discovery
after superseded claims and the remaining four-stage gates are still open.

### S3.5 accepted accounting after ownership replacement

The first lifecycle test acquired a replacement claim under a revised plan and
failed because settlement no longer discovered the old accepted run.
`ListAcceptedTaskClaims` now pages immutable acceptances by acceptance ID. The
shared recovery traversal uses that cursor for settlement and keeps task-ID
pagination for recovering claims. Multiple accepted revisions of the same task
can occupy separate pages.

Historical accounting verifies the exact claim revision, its predecessor,
acceptance and original dispatch. It then snapshots, closes and settles the old
run under the existing guards and current host authority. Ordinary call sealing
uses the same accepted-history check. Recovering ownership still has to be
current, and this history API grants no execution, retry or adoption authority.

Lifecycle tests cover both ordinary completion and recovery adoption after
replacement ownership. They recover lost settlement acknowledgments and restart
after proof expiry while preserving the replacement claim and its entire reserved
capacity. Missing history, acceptance or dispatch records block settlement.
Pagination and exact-scope tests cover the new read API.

The broader claim, acceptance and recovery race suites passed on SQLite (69.925s),
PostgreSQL (42.991s) and MongoDB (51.285s), with no skipped database tests.
Coordination and assurance race suites passed, as did engine lifecycle tests
(6.037s), the Forge lifecycle regression (1.719s), root build and docs
type/lint/build checks. Scoped `make f` and the final settlement unit rerun passed.
All-module `make l` passed the root and every module except the concurrent
optimizer packet, which still reported six gocritic and six formatting issues.
Those files remain under the optimizer chat's ownership. This packet contains no
optimizer edits; the full shared lint check remains open before publication.

Host plan discovery must still include completed/cancelled plans with debt.
Unaccepted terminal-task accounting, concrete billing authentication and live
qualification remain open. CI for the preceding `1ce462c` passed its security job,
confirming the crash-harness file-access fix.

The database retry passed on PostgreSQL (41.530s) and MongoDB (54.937s), with no
skips. Docker was responding again, so no restart was needed. CI for `1ce462c`
finished successfully across every job, including Linux tests and security.

### S4.5 prepared source compatibility observation

`Engine.CheckPreparedSources` compares an artifact's retained source digests with
current records in one read-only store snapshot. Its receipt binds the exact
prepared digest and source count to that capture. Derived edits keep their source
read set, and absent ancestor overlays are checked too. Changes, missing records,
malformed manifests and unsupported snapshot providers return no result.

The capture contract is shared with atomic preparation. Both reject ignored
callback errors, duplicate or missing callbacks, invalid receipts, transaction
failure and cancellation. The source check does not invoke scanners, models or
tools and creates no run. It covers stored sources only; implementation pins,
external knowledge, host policy and grants remain separate compatibility checks.
No lock survives into a later activation.

Tests first failed because the new read API was absent. The preparation regression
suite passed in 49.224s, followed by the final source-check and atomic-preparation
race rerun in 19.764s. SQLite, PostgreSQL and MongoDB passed the capture conformance
fixture with the new engine check, including a changed ancestor overlay. The
SQLite concurrent-write test confirms one consistent observation and rejection of
the committed edit on the next check. Malformed capture and manifest tests passed.

Root build, docs type/lint/build and scoped formatting passed. The final shared
lint run passed the root and every module except optimization, which reported two
formatting issues in its concurrent packet. Publication remains pending that
shared check. Optimizer adapter wiring and the remaining four-stage gates are open.

The shared lint gate subsequently passed across every module. Accepted-history
accounting (`db467f3`) and prepared-source checks (`abbf3ce`) were published.

### S3.5 unsuccessful-run accounting

The initial SQLite tests reproduced the accounting gap: closure rejected failed,
cancelled and incomplete runs even when their call journal was sealed and their
verifier jobs had ended. `TaskUsageClosureRequest.Termination` now binds the saved
run revision, state and completion timestamp. Closure checks original dispatch,
worker, claim and exact current run under the plan/run/journal guards. Fresh
settlement rechecks this binding and reuses the existing billing authority and
reservation transaction. Accepted closure continues to omit the optional binding.

Accounting leaves ownership recovering, with no acceptance receipt. It cannot
authorize another attempt. Running, paused and completed runs, published results,
passed verification and stale bindings are rejected. Known charges settle normally;
unknown usage retains its allocation and observed lower bound. A historical retry
recovers the original receipt even after its proof window closes.

The store tests cover all three unsuccessful states, binding rejection, changed
run state after closure, rollback, lost acknowledgment and cancellation after
commit. The first cross-backend run caught two fixture assumptions: PostgreSQL
requires a persisted worker identity, and MongoDB persists completion timestamps
at millisecond precision. Fixtures now read the saved run before binding it.

The combined settlement/closure regression passed on SQLite (24.965s), PostgreSQL
(23.606s) and MongoDB (28.864s). The final terminal-accounting matrix, including
unknown usage, passed again (12.273s, 19.747s and 27.844s), with no skipped database
tests. Coordination and assurance race suites passed, as did scoped formatting,
all-module lint, root build and docs type/lint/build checks. The full root race
run passed, including engine (442.614s), SQLite (548.754s), PostgreSQL (133.093s)
and MongoDB (173.228s). The documented local scopespy exception was excluded.

Managed unsuccessful-run discovery, completed-but-unaccepted disposition and
governed ownership retirement/retry remain open, alongside live billing and host
qualification. The four-stage plan is not complete.

### S3.5 managed unsuccessful-run settlement

`TaskSettlementServiceConfig.UnsuccessfulRuns` enables current-run reads and
recovering-claim discovery alongside accepted history. The first engine fixture
timed out waiting for settlement because the service only visited acceptances.
The service now interleaves one page from each traversal using separate cursors,
while retaining the shared plan concurrency, scope fairness and shutdown lifetime.
One traversal's page failure does not starve the other.

The service requires scan authorization before reading current run metadata. It
waits on created, running, paused and completed runs, and binds unsuccessful
closure to the saved revision and completion time. Recovering runs need an existing
call seal; accepted-task sealing ports cannot close their journal. Nil preserves
the original accepted-only mode. The same resolver, billing authority, closure
and settlement operations handle both paths.

SQLite, PostgreSQL and MongoDB lifecycle races passed (5.215s, 5.195s and 6.907s),
including unsuccessful work, ordinary/recovered acceptance and superseded claims.
The unsuccessful fixture recovers a lost settlement acknowledgment and restarts
after proof expiry without another resolver call, preserving the run and claim.
Unit tests cover interleaving, independent cursors, page errors, typed-nil ports,
scope/identity checks, active runs, denied authority and drainage of a reader that
ignores cancellation. Coordination/engine lifecycle races passed (3.947s and
43.652s), and the Forge stop-timeout regression passed (1.681s).

The full root test run passed with the documented local scopespy exclusion.
All-module lint, scoped formatting, root build and docs checks passed. Completed
but unaccepted outcomes still need an explicit disposition protocol, and accounting
does not release recovering ownership or authorize retries. Live host and billing
qualification remain open.

### Runtime binding checks for prepared adoption

`Engine.CheckPreparedBindings` requires matching host-declared model and selected
tool revision pins, a configured model provider, unambiguous tool definitions and
present implementations. It compares parameter schemas without rounding large
integers, permits derived descriptions and tool subsets, and checks exact scope.
External declarations alone do not establish implementation availability. It
reuses prepared revision checking and the shared typed-nil host-port check.

The check makes no store, model, scanner or tool calls and grants no authority.
Pins remain host declarations; current policy, grants, external sources and live
provider identity need separate checks. Normal prepared execution retains its
existing behavior for unpinned bindings.

A failing implementation admitted changed, missing and malformed bindings; the
new regression suite rejects those cases, including builtin dispatch precedence,
typed-nil services, duplicate registrations and altered schema integers. The
prepared/atomic-preparation race suite passed (64.647s). Root build and docs type,
lint and build checks passed. Final focused races passed (26.231s), and all ten
modules passed lint after scoped formatting. Concurrent optimizer files remain
with their owner. Runtime pins do not establish live provider qualification.

### Explicit rejection of completed task work

`TaskUsageClosureRequest.Rejection` retains a host-authorized decision to decline
acceptance of a completed run. It binds the run revision, persisted completion
time, result reference and verification status, using the existing closure actor,
reason, fence and authorizer. Termination and rejection are mutually exclusive.
The shared plan/run/journal transaction verifies original dispatch and current
ownership, requires terminal verifier jobs and closes future admission. Later
acceptance checks the retained rejection under the same guards. Only one can win.

Accounting preserves the run, artifacts and recovering claim. Fresh settlement
rechecks the rejected revision and uses existing billing proofs and conservative
unknown-usage charges. With recovering discovery enabled, managed settlement
consumes an explicit rejected closure and recovers lost acknowledgments after
restart. It never infers rejection from completion or releases ownership for retry.

The regression first failed at closure because completed unaccepted work had no
supported disposition. Initial SQLite, PostgreSQL and MongoDB race tests passed,
including exact binding, rollback, lost acknowledgment, concurrent acceptance and
managed lifecycle. The full acceptance/recovery race matrix passed on SQLite
(79.630s), PostgreSQL (53.163s) and MongoDB (79.746s), with no database skips.
Coordination/assurance races, the full root suite with the documented local
scopespy exclusion, all-module lint, scoped formatting, root build and docs
checks passed. Governed retirement, retries, compensation and live qualification
remain open.

### Immutable orchestration preparation

`PrepareOrchestration` captures config, named participant resolution and every
participant's stored preparation sources in one database snapshot. The optional
`OrchestrationPreparationSources` view uses the existing scope and callback-lifetime
guard. SQLite and PostgreSQL route the new snapshot reads through their transaction
reader; MongoDB retains its snapshot session. Shared conformance tests prove config
and named-agent reads cannot observe edits committed during capture or outlive it.

`PreparedOrchestration` retains strategy/config, all executable participants and
the common receipt. `ExecutionRoster` reuses strategy resolution and hierarchical
validation, including auxiliary router/judge/aggregator agents. Inspection is
detached, JSON is bounded and digest checked, and deserialization validates roster,
scope, capture identity and model settings. Artifact inspection preserves exact JSON
numbers; it cannot repair numeric precision already lost in store-returned metadata.

The initial regression rejected preparation because the snapshot view lacked team
reads. The artifact regression then failed without pinning support. Separate
regressions caught numeric rounding in artifact decoding and inconsistent config
size validation at import. Prepared/atomic races passed (70.744s), all orchestration
strategy races passed, and the SQLite/PostgreSQL/MongoDB capture-and-preparation
matrix passed (2.289s/2.857s/3.548s) with no skips. Root tests, build and docs checks
passed with the documented local scopespy exclusion. Final boundary races passed
(11.427s), scoped formatting is complete and all ten modules passed lint.

This completes preparation only. `RunPreparedOrchestration`, shared child-budget
enforcement, isolated child sessions, experiment-limited A2A routing and persisted
partial execution evidence remain required for the prepared execution path. The
optimizer owns its preset approval and trace adapters. No optimizer types enter root.
