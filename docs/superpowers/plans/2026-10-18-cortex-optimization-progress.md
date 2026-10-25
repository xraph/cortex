# Cortex optimization implementation progress

Plan: [Optimization implementation](2026-10-18-cortex-optimization.md). Scope: all seven phases and 21 tasks, using DRY and TDD. No phase is complete yet.

## Shared contracts and ownership

The user authorized direct coordination with **Improve Cortex agent execution**. That chat owns the root runtime, outcomes, prepared execution, continuation and root backend integration. This chat owns Sentinel's recorded evaluation foundation, optimization, adapters and product integration. We consume the shared runtime commits rather than maintaining two execution loops.

The outcome foundation is on Cortex main in `8bebd82`: incomplete termination, stop and finish reasons, verification status and result references. The runtime owner verified engine/run/SQLite race tests, PostgreSQL/Mongo conformance and historical fixtures, all-module lint, root build and A2A compatibility.

Prepared execution was published as Cortex `v1.8.0` from `e798d45`. Root CI `39142591307` and release `39143145756` passed, and the real Go proxy resolves that commit. The API provides `PrepareAgent`, `RunPrepared`, `StreamPrepared`, detached `Inspect()` and immutable `Derive(PreparedEdits)`. Derivation preserves identity, scope, model, budgets, schemas and implementation pins. Optimization applies its narrower edit policy.

Reproducibility still needs a stronger source snapshot guarantee. The runtime owner confirmed that v1.8.0 collects source values without a consistency check. Its follow-up detects changes to the collected read set, but does not promise an atomic multi-source snapshot. Reproducibility-required work stays unavailable until a qualified host provider or consistent store snapshot supplies that guarantee. Source changes are never resolved silently at trial time.

Runtime commit `281d727` adds publication safety and prepared schema 2. Follow-up `77ee8b9` adds read-set conflict detection with explicit atomic-snapshot-unavailable diagnostics. Schema 1 requires new preparation and evaluation before adoption. Optimization tests run against both the published v1.8.0 dependency and the current checkout; the temporary published-dependency modfile is not committed.

## Current state

This table summarizes the implementation. The sections below retain the checks
and limitations recorded for each commit. Implemented does not mean a phase has
passed its full qualification gate.

| Task | Implementation | Remaining gate |
| --- | --- | --- |
| 1: prepared execution | Published prepared execution and current runtime snapshot contracts | Host/source consistency qualification with the runtime owner |
| 2: optimization contracts | Sealed versions, specifications, policies and validated service configuration | Final public API and release audit |
| 3: atomic memory storage | Transactional leases, usage, trials, decisions, activation and lesson records | Final conformance audit against all plan requirements |
| 4: durable persistence | SQLite, PostgreSQL and Mongo; indexed reads, retention and deletion recovery | Throughput and full-system qualification |
| 5: durable worker recovery | Call reservations, controller execution, scoped workers and late receipts | Full-system crash/reconnect qualification |
| 6: Sentinel foundation | Published v1.9.2 recorded evaluation and comparison | Deployment qualification with persistent queues and workers |
| 7: Sentinel bridge | Captured evaluation, metered judges and cost provenance | Live-provider and product-flow qualification |
| 8: isolated trials | Trial execution, environment receipts, recovery and recorded scoring | Production environment qualification |
| 9: sequential refinement | Controller proposal/composition/reflection and phase handlers | Full persistent workflow qualification |
| 10: validation and decisions | Frozen holdout allocations, deterministic comparison and decision gates | End-to-end acceptance audit |
| 11: activation | Review commands, append-only deactivation and bound production execution pass module verification | Built-in current-source compatibility adapter and product capability wiring |
| 12: Fabriq lessons | Governed publication/recall, operator retry, readiness and frozen lesson creation pushed | Final task audit and dashboard application flows |
| 13: parallel and task-local | Bounded parallel trial batches, partial-result commits and persistent recovery | Task-local service, artifact adoption and product capability wiring |
| 14: controller improvement | Paired allocation storage exists | Implement controller comparison and parent/child execution |
| 15: orchestration presets | Runtime integration contract planned | Implement approved preset execution and trace conversion |
| 16-18: API and dashboard | Designed | Authenticated contracts, review UI and persistent browser flows |
| 19: operations and documentation | Artifact retention/deletion foundation implemented | Operations, audit, metrics and user documentation |
| 20-21: qualification and release | Targeted module and consumer checks recorded below | Full-system qualification, CI and release workflow |

## Sentinel evidence

All work is in the primary Sentinel checkout. Existing untracked `_project_files/` remains untouched.

| Commit    | Change                                                                                                          |
| --------- | --------------------------------------------------------------------------------------------------------------- |
| `1540974` | Immutable plan storage, frozen cases/context and pinned scorer revisions                                        |
| `bf62257` | Shared captured scoring with required criterion gates and scorer error handling                                 |
| `0b6a382` | Exactly scoped recorded jobs, idempotent ingestion, fenced leases, bounded attempts and append-only checkpoints |
| `c76234b` | Captured scoring, explicit workers, SQLite restart recovery, readiness and separate execution/judge accounting  |
| `bf7c4f5` | Paired comparison with exact evidence identities and deterministic 2,000-resample bootstrap                     |
| `c3aed04` | Durable source cursors that advance only after enqueue acknowledgment and tolerate overlapping scans            |
| `cd67ab0` | Duplicate-case checks include frozen context so distinct environment variants remain valid                      |

The full race suite and build pass locally. All four store conformance suites pass, with real PostgreSQL and MongoDB containers and no skips. Tagged store vet passes. Focused regressions cover zero target calls, frozen answers, completed retry reuse, unknown historical cost, separate scorer/execution failures, partial cancellation, concurrent ingestion/claims, stale revisions, immutable checkpoints and durable worker/cursor reopening.

Comparisons pair exact case/repetition identities, average repetitions within each case, and bootstrap case means with a recorded seed. Required failures remain separate from score improvements. Unknown cost disables cost comparison. Duplicate input/context rows, missing trials and wrong returned job references are refused.

Ordinary and recorded evaluation share scoring and result aggregation. Recorded jobs retain authoritative evidence independently of legacy suite-linked SQL rows, which cascade on suite deletion. Their report objects reuse the existing evaluation domain types and hooks. Readiness requires both persistent storage and a local worker for the exact app/tenant scope.

## Verification and remaining integration work

A task-specific linter cache avoids stale entries from a deleted checkout. `make l` has passed on coherent slices; several later invocations encountered the machine-wide lock. The equivalent full lint command with integration tags and `--allow-parallel-runners` passes using the dedicated cache. Only this chat's waiting lint process was stopped; other checks continued.

CI passed the recorded worker commit `c76234b`. The first comparison commit exposed standalone `gosec` G404: its scanner does not honor the golangci-lint annotation for the seeded statistical RNG. The scanner-specific justification passes CI's pinned gosec v2.28.0 with zero findings, and comparison race tests and tagged lint pass. CI passed on final commit `af89bf5` in run `39141310907`. The release workflow succeeded in run `39141614567`; Sentinel `v1.9.0` is published and resolves through proxy.golang.org at that commit. Task 6 completion reran the full race suite successfully. The tagged PostgreSQL and Mongo conformance evidence above remains separate from the ordinary suite, which does not compile those integration tests.

Task 7 must bind frozen task context and environment references explicitly, preserve original usage quality and validate every returned identity. Tasks 5 and 7 must also enforce judge reservations at the actual model-call boundary. Persisting aggregate judge usage alone does not enforce an optimization budget. Automatic evaluation stays unavailable without an explicit binding, persistent queue and running worker.

## Fabriq prerequisite qualification

Task 12's generic upstream checks can run before the optimizer adapter exists. Fabriq main was fast-forwarded to `79aa12d`, preserving the existing untracked server binary. New tests reproduced ignored recall filters for typed and dynamic rows. Hydration now combines the requested filter with discovered IDs through the existing relational query port. Filtered recall does not add digest entities automatically because their columns can differ.

Focused regressions pass, including stale revoked/deleted hits, foreign tenant rows, OR filters and undiscovered rows. A real PostgreSQL test under the non-superuser app role also passes. Explicit-ID create with expected version zero already rejects duplicates and preserves the original row. The full root/core race suite and both module lint checks pass. Documentation type checks and production build pass; lint has one existing non-blocking image warning. Code commit `d503a78` and docs commit `c071e83` are pushed. CI `39142380696` passed every code, security and integration job. Its only failure was module tag skew left by the earlier partial v1.8.1 release.

The repository's documented recovery procedure calls for the next patch. Rehearsal `39144695298` and corrective dispatch `39144754887` both passed. All five v1.8.2 module tags exist; core points to `c071e83`, while root and the other modules point to the workflow's require-bump commit `7d625d4`. The real Go proxy resolves core v1.8.2. The unchanged module-version checker now passes. Binary/image release `39144824724` passed all three jobs, including the Helm chart and Docker Hub mirror. This is a prerequisite slice, not completion of task 12.

## Optimization storage progress

The initial memory backend stores drafts and their frozen versions in one transaction. Shared conformance checks cover exact three-level scope isolation, ancestor and foreign-ID refusal, rollback of both experiment and version rows, detached reads and writes, and expired transaction handles. A regression caught reused versions overwriting the original creator; existing version metadata now stays intact. An injected clock owns creation timestamps.

These checks pass with the current runtime and with published Cortex v1.8.0. Scoped lint and builds pass after coordinated formatting. The required full lint run reported eight findings in the runtime owner's ongoing scope-hardening files; those findings were sent to that owner. All other modules, including optimization, passed that run.

Task 3 remains incomplete. Phase evidence requirements, budget accounting, holdout allocations, activation, publication and remaining readers still need implementation. The memory backend provides no durable recovery guarantee. Constructor wiring follows the storage ports so the optimizer does not import a package that already imports its domain types.

The next store slice adds atomic draft and state-change events, payload-bound start idempotency and revision checks. Duplicate starts produce one transition and one audit event; changed payloads and stale revisions fail. A cancelled experiment cannot be requeued through this path. Cursor tests check exact result identities across bounded pages. Worker-controlled phase changes require the current, unexpired lease generation.

CI passed every job for both experiment contracts `cd89669` (run `39145664574`) and initial draft storage `c80e580` (run `39146320084`). The pending event/transition slice passes scoped lint, build and current/published-root race suites after final coordinated formatting.

Lease conformance uses a manual clock across backend factories. A live claim cannot be stolen; an expired claim can be replaced with a new generation. The previous worker cannot commit using its old generation, even if it knows the latest experiment revision. Renewals preserve phase revisions, expire within the experiment deadline and cannot revive an expired worker. Draft creation refuses injected lease or phase state. These tests pass against current and published Cortex.

The lease slice passed post-format module lint, race tests and builds. Its required whole-tree lint run encountered the runtime owner's intentionally failing delegation tests for S1.5 (`parseDelegations`, `InvalidDelegationPlanError` and `ErrInvalidDelegationPlan` were not yet implemented at that check). Every other module passed. The previous S1.4 scope findings were resolved and its final coordinated all-module lint passed.

The runtime owner subsequently completed S1.5 with all-module lint passing, clearing the transient delegation failure above.

Accounting now reserves token and money bounds atomically. Concurrent requests cannot spend the same remaining budget; receipts settle once, and their history survives reconciliation. Expiry, a timeout or an estimate keeps the unresolved debit. Known token usage can settle while money remains unknown, and an observed overrun stays in the ledger and blocks further reservations. Each accounting event links its attempt, call key and debit amounts. Scope, lease and attempt checks reject foreign settlements; transaction rollback removes both the settlement and its event.

Current-checkout and published-v1.8.0 race suites and the module build pass after coordinated `make f`. Final `make l` passed all nine modules with zero issues. This slice still requires a live experiment lease for settlement. Reconciliation after cancellation or the experiment deadline needs its own fenced path during worker recovery work; these tests do not establish that behavior or durable backend readiness.

Holdout allocations now bind cases from the stored specification to a frozen baseline, finalist and controller roster. Concurrent claims have one winner. Renaming a case or harness, or changing surrounding input whitespace, cannot make exposed inputs fresh again. You can release an allocation before exposure; an exposed allocation stays consumed. Reads are detached, and a failed transaction rolls back exposure and its audit event together.

A controller comparison can reserve one parent allocation for two named child experiments. Both children must use the same frozen validation cases, evaluator, objective and repetition count. Unrelated children, changed finalists and replacement rosters are refused. Worker integration must still mark exposure before every external validation dispatch and withhold paired results until both arms finish; the store tests alone do not establish those flows.

Allocation checks pass in both current-runtime and published-v1.8.0 race suites. The module builds, and the runtime owner's coordinated final `make f` and all-nine-module `make l` passed with these changes present.

CI for `f02dec9` (`39148570147`) passed the optimization job but failed two existing Fabriq tool-dispatch fixtures because they omitted scope. The adapter fixture fix uses an explicit scope, checks that an unscoped call cannot reach the handler, and passes the full adapter race suite and build. Runtime scope enforcement stays intact.

Holdout allocation is committed as `6222838`; the Fabriq fixture repair is `97532a6`.

Candidate phase writes now persist each proposal, its frozen version and the transition event atomically. The store derives the proposed edits again from the frozen baseline and compares the resulting digest, so a supplied version cannot substitute instructions or policy. Versions use the same identity checks as draft creation. Duplicate candidate content, foreign experiment IDs, unknown parents and unused version rows are refused. Rejected proposals retain their reason without an executable version. Every proposal, including a rejection or composition, counts toward the candidate cap.

Candidate conformance covers rollback, duplicate acknowledgment, detached reads, exact scope, changed policy, version substitution and composed-parent provenance. Trial-backed proposal evidence remains unavailable until the trial store can verify its ownership and split. Full phase evidence requirements, durable backends, decisions and activation are still pending; these candidate records do not establish an optimizer result.

A review regression reproduced a version identity gap: content digests exclude grouping metadata, so a request could present an existing version ID with another harness association. Shared version validation now refuses a changed harness or parent and preserves the stored record when an ID is reused. The regression passes alongside the existing creator-preservation test.

After that fix, coordinated `make f`, all-nine-module `make l`, both current-runtime and published-v1.8.0 optimizer race suites, and the module build passed. The atomic source foundation's red runtime tests do not prevent lint or these scoped optimizer checks from passing.

CI at `6222838` passed optimization and the repaired Fabriq adapter. It then failed the root stream-cancellation race and docs generation; the runtime owner is correcting both. Atomic source capture is also in progress there, with intentionally failing foundation tests until its store and engine implementations land. Those files remain outside this chat's changes.

The next CI run, `39149889570` at candidate commit `47ec851`, passed every job. The runtime owner still reproduced the intermittent cancellation defect and is shipping its fix alongside the docs-generation correction.

Trial persistence now fixes each case, version, split and repetition, with unique original run IDs across attempts. Trial creation and updates commit with experiment revisions and audit events. A validation attempt requires a matching exposed allocation. Unknown external effects retain their concurrency slots. An approved replacement requires confirmed termination or quarantine and a resettable or idempotent environment; the original attempt and captured execution stay in history.

Captured evidence binds `Outcome.Output`, run and trace identities, usage and the environment receipt into a digest. The receipt must exist in an earlier committed transaction before the store accepts a result. Known failures cannot become unknown after a late timeout, and incomplete execution cannot be stored as a completed trial. Current workers can reconcile the original result after takeover, while stale lease generations fail.

Trial conformance passed under race detection against both the current runtime and published Cortex v1.8.0. The module builds. Coordinated final `make f` and all-nine-module `make l` passed with zero issues (`/tmp/cortex-combined-final-lint.log`). These are memory-store checks. Host approval authorization, trusted environment providers, artifact existence, terminal reconciliation and durable recovery still need their planned service and backend work.


## Captured evaluation persistence

Trial persistence was committed and pushed as `98ffaa7`. CI `39152040751` passed at `9b4a882`, which includes that commit and the runtime owner's atomic capture, cancellation and docs fixes. Atomic capture is now implemented on main; optimization still pins the published v1.8.0 root and cannot claim that release has the newer snapshot guarantee.

Experiments now retain the frozen public evaluation plan: exact case digests, scorer revisions and the criterion roster. Draft creation validates those fields against the specification. A reused plan reference cannot change its metadata. The experiment digest covers both the specification and this plan metadata; private rubrics remain in evaluator storage.

Evaluation writes bind the external job and report to a separately committed execution record, case, repetition and plan. The score, trial link and audit event commit together under the current worker lease. Returned records are detached and exactly scoped. Tests cover stale workers, rollback, duplicate jobs, changed identities and scorer revisions, invalid scores, missing criteria, valid zero scores and scorer failures retained without a score. Evaluating a failed execution preserves the failure. Once evaluated, that trial cannot retry and detach its score from the captured result.

Candidate proposals can now reference stored discovery trials from their own experiment. Referenced trials must have captured execution and evaluation records; duplicates and foreign experiment references fail. Incomplete evaluations remain diagnostics for discovery.

The optimizer's full race suite and build pass after coordinated formatting against both the current checkout and published Cortex v1.8.0. Initial whole-tree lint passed, and final optimizer lint reports zero issues. The shared final lint run found issues in the runtime chat's concurrent evidence/verification changes; that chat owns the fixes and final root recheck. These are storage checks. The Sentinel adapter, model-call budget enforcement, durable database backends, decision gates, activation and product flows remain unfinished.


## Phase evidence gates

Captured evaluation persistence was committed and pushed as `7a3d115`. CI `39153324504` passed.

Phase transitions now check the stored case and repetition identities. Baseline waits for every discovery repetition. Probe waits for every accepted candidate in the current round. Proposal and composition transitions must retain their candidate records, including rejected proposals. Validation waits for both frozen baseline and finalist arms. A final score can commit atomically with the phase transition that consumes it.

For paired controller comparisons, both child experiments and their parent wait for the entire allocation roster. One completed arm cannot advance early. Allocations also reject a finalist that belongs to another experiment, even when the version exists under the same scope and agent.

A completed execution needs its evaluation record before advancing. Explicit terminal failures can advance with their error evidence, so unavailable work can eventually produce an inconclusive decision. Unknown or running attempts remain unresolved. This gate establishes that evidence has been collected; selection policy still determines whether it supports a recommendation.

Conformance fixtures now persist populated execution and evaluation records through the public store API. They use fixture outputs and scores, not live providers. The full optimizer suite and scoped lint pass. Coordinated formatting, post-format race tests and builds against the current checkout and published Cortex v1.8.0, and final all-nine-module lint passed. Decision persistence, activation, outbox work and the remaining phases are still open.


## Comparison contract follow-up

Decision integration exposed a mismatch in the published foundation. Cortex's `EvaluationRequest` carries one trial and the optimizer stores one external job ID per trial. Sentinel's `CompareRecorded` accepts one baseline job and one candidate job, with multiple cases inside each job. Passing one pair of trial jobs cannot produce the full validation comparison.

Task 6 therefore needs a follow-up before Task 7 can use its comparison API. Add a generic comparison operation over explicit baseline and candidate job-ID sets. It must read the authoritative jobs, validate every plan/case/repetition identity and reject overlapping or substituted evidence. Reuse Sentinel's existing paired-case aggregation and bootstrap. The single-job API remains supported. The adapter must not create synthetic completed jobs or duplicate the statistical implementation in Cortex.

The optimizer also needs to retain the evaluator's aggregate quality score separately from per-criterion scores. Sentinel compares `evalrun.Result.Score`; inferring that value from criterion scores could change its frozen weighting. Resolve this record shape before implementing final decision qualification.


## Job-set comparison and aggregate scores

Sentinel now compares explicit baseline and candidate job sets through
`CompareRecordedSets`. It reads the authoritative jobs, rejects repeated or
overlapping evidence and mixed plan identities, and retains each job ID and
request digest. The single-job API uses the same calculation. Tests show that
splitting 60 captures across separate jobs preserves the paired scores, mean and
bootstrap interval, including when the two arms use different batch sizes.
Pending jobs remain inconclusive, and required failures survive a high average.

Sentinel's full race suite and build passed against the updated Forge and Grove
dependencies. Final formatting and the comparison race suite passed. `make l`
was blocked by the shared linter lock; the uncapped equivalent with an isolated
cache and `--allow-parallel-runners` passed with zero issues. The comparison
change is committed as `059a4be`. CI `39510864056` and release workflow
`39511561339` passed. Sentinel v1.9.2 resolves through proxy.golang.org at that
commit, including the job-set comparator.

Cortex now retains the evaluator aggregate as `Evaluation.Score`. A completed
evaluation needs a finite value from zero through one; incomplete evidence keeps
no aggregate. Storage preserves this value independently of criterion scores.
Conformance tests reproduced the missing validation before the fix and now pass,
including zero scores, invalid values and detached reads. Coordinated `make f`,
optimizer lint, full race suites and builds against both the current checkout
and published Cortex v1.8.0 passed. The shared all-module lint run passed
optimization but found issues in the runtime chat's concurrent sandbox changes.
Those fixes are now committed, and the final all-ten-module lint pass is clean
(`/tmp/cortex-s24-final-lint-2.log`). The score change is committed as `3425b6a`.
Its optimization CI job passed; a newer runtime push canceled the remaining
root test job. Decision persistence and the remaining implementation phases
are still open.


## Decision persistence and evidence binding

Decisions now bind the experiment specification digest, exposed validation
allocation, frozen baseline and finalist, policy digest and exact evaluation-job
sets. The decision, experiment link, reflection phase and audit event commit
atomically. Reflection refuses a missing decision. Drafts cannot inject a
decision link, stale leases cannot finalize one, and retries preserve the
original record. Reads are detached and exactly scoped.

Stored trial and evaluation records supply the case/repetition roster and paired
score deltas. The validator checks reported case means, aggregate improvement,
required failures, protected regressions and any reported cost or latency ratio.
It checks confidence settings against the frozen objective and leaves bootstrap
calculation in Sentinel. The service must still obtain that report from the
trusted evaluator and verify current permissions, artifact availability and
host qualification before recommending or activating a version.

Missing scores remain inconclusive. An unchanged baseline can be retained
without inventing a candidate comparison. Recommendations require the configured
sample and improvement thresholds, a positive lower confidence bound, passing
hard criteria and usable constrained metrics. Reproducibility-required decisions
also need receipts declaring atomic source capture and frozen retrieval. These
receipt flags do not establish live host qualification by themselves.

The tests use stored fixture executions and comparisons. The positive storage
fixture explicitly uses a smaller sample threshold and disables reproducibility;
it does not demonstrate qualification under the default policy. Separate checks
refuse insufficient default samples and missing reproducibility declarations.
The full race suites passed against the current checkout and published Cortex
v1.8.0. After the final failure-key correction and coordinated `make f`, the
decision and key race tests, builds and scoped lint passed against both
dependency configurations. Shared all-module lint also passed before the final
formatter. Failure keys preserve case and criterion identities even when their
names contain separators.
Activation, publication outbox, durable stores and service integration remain open.


## Activation storage

Activation now has an atomic compare-and-swap command, an exact scope/agent/channel
pointer and immutable generation readers. The command ID is its retry key. You
can retry an old successful command without moving the current pointer; changing
that command under the same ID returns a conflict. An empty channel resolves to
`default`, and channel names accept up to 64 ASCII letters, digits, dots,
underscores or hyphens.

Each activation binds the stored recommendation, version digest, agent and policy.
The store refuses inconclusive, exploratory, contrastive, task-local and controller
evidence. Rollback appends a generation referencing an activation already stored
for the same agent and channel. It preserves the qualifying decision and checks
the expected current generation. The pointer, immutable record and audit event
commit together, with timestamps supplied by the store.

Conformance tests cover concurrent reviewers, conflicting retries, detached
reads, exact scope, transaction rollback, separate channels and audit resources.
The shared evidence validator also rejects version/experiment scope mismatches.
The new API tests first failed to compile; the scope checks then failed before
the validator fix. Focused tests now pass. Coordinated `make f` and shared `make l` passed all ten modules.
Full race suites and builds passed against the current checkout and published
Cortex v1.8.0 (memory conformance: 55.698s and 51.306s).

This is storage work. Current grants, source drift, artifact retention, lesson
eligibility and the production `RunActive` binding remain service-level work in
task 11. The fixture recommendation uses a reduced sample policy and does not
qualify a live production version.


## Lesson publication and local revocation

Accepted reflections now commit a local lesson and an immutable publication
payload in one transaction. The record fixes the remote aggregate ID before a
publisher can make its first call. It also records the source experiment, local
revision and payload digest. Identical retries preserve the original record;
changed content, duplicate remote IDs and unsupported evidence classes fail.
A decision and its publication can commit together, and rollback removes both.

Experiments freeze lesson IDs, revisions and publication digests in their
specification digest. The store rejects unavailable, expired, revoked or
post-cutoff lessons. `ResolveLesson` checks the frozen reference and source
eligibility through the dependency chain before returning content. Raw lesson
reads remain available for review and audit.

Revocation advances local eligibility to revision two and queues a separate
revocation payload with the same remote aggregate ID. The original publish
payload stays intact and is marked superseded. Direct consumers become paused
with `lesson_revoked`, their leases expire, and new starts, claims, renewals and
phase writes cannot use the withdrawn lesson. Activation lookup refuses the
stale version while historical activation records remain readable. Dependent
lesson resolution and late publication retries also reject invalid source
lessons.

Conformance tests reproduced missing expiry guards, usable activations after
revocation and a delayed publication retry that ignored source invalidation.
Those checks and the full optimizer race suites pass against both the current
checkout and published Cortex v1.8.0 (memory: 87.270s and 78.615s). Both builds
also pass. The coordinated `make f` and all-ten-module `make l` passed after the
final edits. Earlier decision and activation commits passed full CI in
`39516036204` at `e51dea8`. The lesson slice then passed full CI
`39517232117` at `6b7e1f6`.

This is the memory-store contract. Durable backends, publisher claims and
acknowledgments, bounded remote retry, Fabriq schema provisioning and actual
remote revocation are still open. The worker must stop external work and
reconcile unresolved attempts after local invalidation; storage tests do not
prove that operational behavior. Current actor grants and retained artifacts
remain service checks.


## Scoped resource readers

The memory backend now pages versions, candidates, trials, evaluation jobs,
decisions, reservations, validation allocations, lessons, publications and
activation history. Experiment lists use the same pagination code. Results sort
by their stored creation or recording time, then identity; event lists retain
their monotonic sequence cursor.

Cursors bind exact scope, collection, parent and filters. The scope and filter
bindings use digests so a valid long scope cannot create an unusable cursor.
The 8 KiB cursor bound also allows JSON escaping of a valid 512-byte external ID.
Parent resources must exist in the current scope. Page size defaults to 100 and
is capped at 500. Lessons can be filtered by current eligibility, and publication
lists can exclude acknowledged, superseded or terminally failed records. Listing
a pending record does not claim delivery authority.

The store now supplies creation timestamps for new versions, allocations and
publication records. Captured evaluation job/report keys share the accounting
key validator: at most 512 bytes, valid UTF-8, no surrounding whitespace or
control characters. This keeps exact external identities usable in bounded
cursors without silently truncating them.

Tests reproduced the old experiment ID ordering, long-scope cursor failure,
unknown-state empty results and unbounded evaluation IDs before the fixes.
Populated reader checks compare exact identity sets across pages, preserve
original trial attempts, and refuse cursors reused across scopes, parents,
collections or filters. The full race suites and builds passed against the current checkout and
published Cortex v1.8.0 (memory: 75.130s and 72.278s). Shared lint passed all ten
modules after coordinated formatting. Final review then reproduced and fixed
the escaped-ID cursor bound. After the final coordinated formatter, focused
reader race tests and builds passed in both dependency configurations, and a
new shared `make l` passed all ten modules. The verbose reader runs include
all seven pagination and identity cases (11.554s and 11.261s). Durable backends
and recovery remain open.

## Shared storage rules and SQLite

The storage rules now live in `optimization/store/internal/state`. The memory
backend keeps its public constructor and clock option, and SQLite uses the same
transaction checks. This includes the boundary between evidence committed in a
previous transaction and evidence supplied in the current callback.

SQLite stores individual versioned records for each entity, audit event and
idempotency key. Nested reservation and receipt maps become separate rows.
Compound identities keep their original components alongside bounded hash keys.
Reads require the exact canonical scope and all scope-level columns. Writes
acquire a database write lock before loading current state, then commit only
changed records with expected previous digests. No-op retries leave record
history untouched.

Stored records are checked for scope, identity, content digest, duplicate keys,
schema version and unknown fields. Tests reproduced acceptance of missing and
null record values before the decoder was corrected. Event reconstruction sorts
numeric sequences and rejects gaps. Unknown database schema versions refuse
migration, and callback errors or database write failures roll back the whole
transaction.

The first populated SQLite race suite passed in 281.635s. Separate focused race
tests passed for subprocess lease takeover, stale-owner rejection, competing
claims through independent connections, failure on the second database write,
corrupt records, unknown schema versions and idempotent transactions with writes
explicitly forbidden by database triggers.

Final full-module race tests and builds passed against the current checkout and
published Cortex v1.8.0. Memory took 80.699s and 76.683s; SQLite took 425.803s and
414.798s in the concurrent compatibility runs. The shared formatter left the
optimizer source unchanged, and the final `make l` passed all ten modules. An
earlier shared lint run caught two formatting errors in concurrent sandbox work;
that work was formatted before the successful rerun. Reader commit `908eae6`
also passed full CI in `39519005197`.

The adapter currently loads a complete exact scope before applying the shared
rules. Database pagination, selective loading and throughput qualification
remain open. PostgreSQL, transactional Mongo, the artifact adapter and artifact
retention checks are also unfinished, so Task 4 is not complete. No service,
sandbox or production activation readiness follows from these storage tests.

## PostgreSQL and shared SQL persistence

SQLite and PostgreSQL now use one SQL record adapter under
`optimization/store/internal/sqlstore`. Domain checks remain in the shared state
package. The SQL adapter handles parameter syntax and binary column types while
preserving scoped reads, changed-row writes and expected-digest preconditions.

PostgreSQL explicitly uses `READ COMMITTED` and locks the scope row before loading
state. Tests configure the host to default to repeatable read and use a database
trigger to reject writes at the wrong isolation level. Independent connections
still produce one claim winner, with each transaction callback called once.
Workers on other scopes can proceed while a scope is locked, and a cancelled
waiter never enters its callback.

Concurrent migration tests reproduced PostgreSQL catalog collisions despite
`CREATE TABLE IF NOT EXISTS`. Migrations now take an advisory transaction lock
before creating schema objects. Database failure tests abort the second record
insert and verify that both records and the scope row roll back. Unsupported
schema versions refuse migration.

The subprocess lease test moved into `store/storetest` so both SQL backends use
the same contract. A fresh process takes over an expired lease, and the parent's
still-open connection cannot renew the old generation. The focused race runs
passed for PostgreSQL and SQLite. The full normal PostgreSQL suite passed in
115.656s with real containers.

After coordinated formatting, full-module race tests and builds passed against
the current checkout and published Cortex v1.8.0. PostgreSQL took 340.564s and
332.253s, SQLite took 347.200s and 339.530s, and memory took 107.749s and 104.399s
in the concurrent compatibility runs. Shared `make l` passed all ten modules.
The optimizer source remained unchanged throughout those final checks. SQLite
commit `7c62986` passed CI in `39522046871`.

This remains Task 4 work. The shared store instance still serializes its own
operations and each operation loads a complete exact scope. Bounded database
queries, finer concurrency, Mongo transactions, artifact retention and throughput
qualification remain open.

## MongoDB transactions and chunked records

The Mongo adapter uses the same storage rules as memory and SQL. Every operation
reads metadata and chunks in one snapshot transaction. Updates first write an
exact-scope fence, invoke the domain callback at most once, and commit changed
records together. A conflicting fence returns before entering the callback.
Commit acknowledgment failures return unavailable so a caller can reconcile
persisted identities without assuming that the write failed.

Migration performs an actual transactional query and refuses standalone MongoDB.
It also creates collections before accepting work. A regression test exposed
the first-transaction catalog race: implicitly creating the scope collection let
a contender enter its callback before the first transaction committed. Explicit
collection creation fixed that failure. Another test reproduced inherited
secondary read preferences breaking transactions; the adapter now explicitly
selects primary reads, snapshot isolation and majority writes.

Bodies use chunks of at most 4 MiB, committed with their metadata. A real 17 MiB
record round trip passes. Replacing a record checks its previous digest, writes
new chunks and removes superseded chunks within the transaction. Corruption
tests cover altered bytes, chunk identity, scope, order, size, missing content
and unknown schemas. Invalid content blocks both reads and mutation callbacks.
Database validation failures prove that new records and failed replacements
roll back their chunks, metadata and scope fences together. No-op retries pass
with record and chunk writes forbidden.

The normal populated Mongo conformance suite passed against a real replica set
in 92.797s. Separate corruption and replacement tests passed in 12.250s.
Independent connection fencing, concurrent migration, standalone refusal and
lease takeover from a fresh process also passed. Final full-module races and build
passed against the current checkout: Mongo took 361.910s, PostgreSQL 267.300s,
SQLite 274.969s and memory 78.344s. The published Cortex v1.8.0 run passed Mongo
in 353.923s, SQLite in 265.980s and memory in 74.565s. Its PostgreSQL database
failure test encountered a Docker startup timeout before reaching the test body;
the isolated race rerun passed in 3.024s, followed by a successful published
dependency build. All other tests in that compatibility run passed.

Final formatting left optimizer source hashes unchanged, and `make l` passed
all ten modules. PostgreSQL commit `ec02ab4` passed CI in `39523884180`.

The shared exact-scope index helper now serves all three durable adapters.
Complete-scope loading and per-instance serialization remain, along with artifact
storage and retention checks. Task 4 and every implementation phase remain open.

## Concurrent durable store operations

Durable store operations now share a lifecycle read lock. Each operation already
uses its own detached state snapshot and database transaction fence, so a held
callback no longer blocks an independent scope through the same store instance.
Memory retains its exclusive mutex. Migration and close still acquire the
exclusive lifecycle lock and wait for active operations to finish.

The admission tests reproduced blocked reads and writes before the change.
Real PostgreSQL and Mongo tests also timed out with a held transaction in another
scope. Both now pass under the race detector, in 3.360s and 3.876s respectively.
A lifecycle test verifies that close drains an active operation, closes the
backend once, and refuses later reads or callbacks.

The full race run passed memory, PostgreSQL and SQLite in 66.988s, 252.443s and
260.049s. Mongo exposed an outdated reservation test assumption: its database
fence can reject the loser before the callback reaches the budget check. The test
now accepts that conflict while requiring exactly one persisted 60-token debit
and refusing a new 41-token reservation against the 100-token budget. Every other
test in that full run passed. No production behavior changed after that run.

After the final formatter, focused races passed against current and published
Cortex v1.8.0. They cover operation admission, close, independent scopes, database
conflicts, cancellation, subprocess recovery, the strengthened reservation race,
validation allocation and scope isolation. Both builds passed, and the final
`make l` reported zero issues across all ten modules.

CI `39526179993` for Mongo commit `a0ae9ac` reached Go's default ten-minute timeout
while decoding the large-record fixture after conformance had consumed most of
that time. Its PostgreSQL and SQLite tests passed, as did every other CI job.
The optimization CI job now uses the same bounded thirty-minute timeout as local
full race runs; other sub-modules keep ten minutes. A new CI run must verify that
change. Artifact retention, bounded reads and throughput qualification remain open.

## Indexed resource reads

Eleven single-resource getters now use an exact compound record identity instead
of loading the entire scope. The shared identity hash is unchanged, so existing
records need no migration. SQL uses the scope and record primary key with a row
limit. Mongo selects one metadata record and only its expected chunks plus one,
within a snapshot transaction. The additional chunk detects stale or unexpected
content that a strict expected-count limit could hide.

The common decoder still checks identity, scope, schema and content digest.
Current activation eligibility, transitive lesson resolution, lists and mutation
callbacks retain their broader reads. A raw resource lookup does not claim that
its related evidence is currently eligible.

Tests reproduced whole-scope reads for all eleven getters before implementation.
A real SQLite driver counter now proves that one experiment lookup consumes one
database row. Corrupting a different experiment in the same scope leaves that
lookup usable, while reading the corrupt experiment or entering a mutation
callback fails. Mongo tests establish the same separation for chunks and reject
extra target chunks. The 17 MiB round trip and corruption suite also pass.

Full module race tests and build passed: memory took 68.329s, Mongo 236.103s,
PostgreSQL 166.715s and SQLite 166.274s. After shared formatting, source hashes
were unchanged and focused point-read/corruption races passed again. Published
Cortex v1.8.0 compatibility checks passed for point reads, corruption, the large
record, populated resource readers and exact-scope isolation, followed by a
successful build. The final `make l` passed all ten modules.

CI `39527689658` passed all jobs for shared commit `9c0d1e5`, which includes
Mongo storage and the concurrency/timeout changes from `e452baf`. The earlier
`e452baf` run was cancelled when that newer shared commit was pushed.

## Artifact reference and content verification

Artifact references now have one validator and a binding digest covering experiment,
scope, content digest, size, media type, kind and access policy. Execution sealing uses
that validator too. Equal bytes under different policies remain distinct protected
references. Artifact content validation checks the byte digest, length, experiment
identity and a finite retention interval.

Experiment ownership moved from the artifact body into its reference. Without
that identity, two experiments with identical bytes could collide at `Get(ref)`
despite having separate retention records. This changes the unpublished artifact
DTO; old references without an experiment are refused.

The new verifier reads through an injected authorized provider. It checks exact
context scope and the configured size limit before calling that provider, then
validates returned content, reference binding, experiment and current retention.
Missing or denied content retains its original error while also reporting incomplete
evidence. Cancelled reads cannot return content. Input scopes and returned bytes
are copied so callbacks and consumers cannot change each other's values.

Tests cover substituted policies and experiments, corrupted content, expired and
future retention intervals, byte limits, cancellation and shared-memory mutation.
Root-package races passed against both the current checkout and published Cortex
v1.8.0, followed by successful builds against both dependencies. After coordinated
formatting, those checks passed again. Focused memory conformance races covering
captured execution, decisions and activation passed in 32.075s. The final `make l`
passed all ten modules.

This is a read-time verifier. It does not yet fence decision or activation commits
against deletion. The artifact metadata transaction, database content bridge,
retention worker and dependent lesson revocation remain unfinished. Task 4 and
all phases stay open. CI `39529487667` passed every job for the preceding indexed
read commit `0e11b1c`.

## Transactional artifact retention

Retention metadata now shares the experiment transaction in all four stores.
Registration requires the current worker lease, a valid experiment-bound reference
and a finite retention interval. Its identity and dates cannot be rewritten.
Retries preserve one record and one audit event. Raw bytes stay outside the scope
snapshot.

Deletion first commits an ineligible state and queues dependent lesson revocations.
Only a later confirmation records physical deletion. A failed content provider can
leave this step pending without making the evidence eligible again. Deletion is
permanent for that reference, including retries that try to register it again.

Decision commits check retained metadata before accepting a conclusive result.
Missing evidence can still support an explicitly inconclusive decision. Activation,
rollback and lesson recall recheck retention, while historical activation reads
remain available for audit. The service must still verify current provider content
and grants; metadata alone cannot detect a blob removed outside this protocol.

The regression first reproduced a recommendation accepted without its required
trace metadata. Tests now cover atomic deletion rollback, expiry, pending deletion,
activation history, remote revocation outbox entries, paused lesson consumers and
an unrelated deletion that leaves eligibility intact. A separate regression caught
dependencies disappearing when a retry cleared the latest execution. Retention and
revocation now inspect captured evidence from earlier attempts too.

Full module races and build passed: memory took 65.183s, Mongo 244.133s,
PostgreSQL 163.751s and SQLite 163.958s. The final dependency-traversal change
passed focused races, followed by the strengthened artifact conformance cases on
all four stores. The first PostgreSQL/Mongo follow-up failed during Testcontainers
reaper startup before either test body; sequential retries passed in 12.045s and
10.387s. Published Cortex v1.8.0 focused races passed on every backend, and both
current and published-dependency builds passed.

Shared formatting left optimizer source hashes unchanged. Final focused state
races passed, and `make l` reported zero issues across all ten modules.

The database content bridge, provider reconciliation, retention worker, bounded
list reads and throughput qualification remain open. Task 4 and all phases remain
unfinished. CI `39530942630` passed for artifact verifier commit `bd5df2b`.

## Artifact deletion recovery and retained reads

The new `optimization/artifacts` package coordinates the metadata transaction with
an injected content provider. Deletion checks host authority and resolves the
original actor before invalidating evidence. It then makes one provider call and
requires a matching durable tombstone receipt before recording completion. The
provider contract fences delayed uploads, including when the object is absent.

Unknown outcomes and failed completion writes stay pending. Recovery uses the
saved request and current grants for its original actor. Tests cover a new service
instance and a reopened SQLite database, mismatched receipts, revoked grants,
cancellation and provider success followed by a metadata failure. A regression
caught an identity resolver dropping the caller's deadline; resolution now
preserves both deadline and cancellation.

`RetainedReader` checks metadata before and after the shared content verifier.
A deletion committed during the fetch prevents bytes from being returned. Provider
content cannot extend the registered retention dates. Missing, expired, denied or
invalidated evidence remains unavailable. The final read is an observation point;
decision and activation transactions still enforce their own eligibility checks.

The concrete database content adapter and durable scan/scheduling worker remain
open. Provider receipt fixtures verify the coordination contract, not a live
object-store integration. Root evidence APIs remain unreleased, so published
Cortex v1.8.0 compatibility is preserved through the injected provider.

Current and published-version root-package and artifact-package races passed,
followed by builds of the full optimization module. Ten artifact race repetitions
passed in 11.829s, including real SQLite close/reopen recovery. Shared formatting
left the tested source unchanged.

Final shared lint passed all ten modules. Post-format root-package and artifact
races and the optimization build passed again.

## Artifact work discovery and page recovery

Every backend can list pending deletions and artifacts expired at a fixed cutoff.
The cursor binds that cutoff and the exact scope. Ordering uses immutable
registration time and reference binding, so completing a deletion between pages
does not skip later work. Completed deletions and artifacts with later expiry
are omitted. Returned metadata is detached from storage.

`artifacts.Recovery.RunPage` processes one page with a separate timeout for each
item. It rereads metadata before acting, preserves a concurrent manual deletion
request and uses the configured host retention identity only for new expiry
requests. Failed items remain pending and appear in the returned outcomes. A
later sweep retries them. Cancellation returns an error without advancing the
cursor, including when discovery returned an empty page.

The discovery regression failed before the store method existed. Recovery tests
cover expiry, saved authority, provider failure followed by a new runner,
per-item timeout, cancellation and a deletion created after listing. The empty
page cancellation regression first reproduced a false success, then passed after
the fix.

This is a callable recovery runner. Hosts still own scheduling, authorized scope
enumeration, continuation persistence and reporting. Provider calls per page are
bounded; database hydration still loads the whole scope. The concrete content
adapter, scheduled retention worker, bounded database queries and throughput
qualification remain open. Root evidence APIs are still unreleased, so the
optimizer retains injected provider ports and published Cortex v1.8.0 compatibility.
Task 4 and all phases remain unfinished.

Verification for this packet: the full optimization race suite passed, including
memory (84.427s), Mongo (275.710s), PostgreSQL (198.899s) and SQLite (195.510s).
The new recovery and discovery tests passed against published Cortex v1.8.0,
including real PostgreSQL and Mongo containers. Shared `make f` and `make l`
passed across all ten modules. Post-format focused races passed on the current
checkout, and full optimization builds passed against both current and published
root dependencies. The tested optimizer source remained unchanged through the
final review.

## Durable call boundary and authorized meter

`BeginUsageCall` reserves usage and records one possible provider dispatch in the
same transaction. It binds the request digest, trusted start time and current
worker lease. An identical retry cannot authorize another dispatch. A different
payload under the same key conflicts. Existing reservations also recheck the
experiment's total debit before dispatch, so a reported overrun stops them.

`LedgerMeter` applies host authorization before those accounting transactions.
It checks both the caller and the experiment's original requester using current
grants. `Reserve` holds capacity, `BeginCall` grants one committed dispatch and
`Settle` records attributed usage. Unknown usage retains its upper bound until an
authoritative receipt reconciles it. Cancellation after commit returns an error
without removing the dispatch marker or debit.

Artifact deletion now delegates identity resolution to the shared
`ResolveIdentity` helper. It preserves exact scope, deadline and cancellation;
the existing artifact recovery regressions exercise the refactor.

The new tests first failed on the missing dispatch boundary and meter API. A
separate regression reproduced dispatch permission returned after cancellation
at commit, then passed with the post-commit cancellation check. Shared store
tests cover concurrent dispatch, conflicting content, rollback, stale authority,
settled reservations and an overrun reported after reservation.

A review regression also caught an expired dispatch reported as an invalid new
reservation. Dispatch recovery now checks the saved call before expiry validation,
while still enforcing current scope and lease authority. The shared reservation
helpers keep those authority and immutable-content checks consistent.

The model/judge/tool wrappers, estimator and pricing integration, optimizer
commands and worker lifecycle remain unfinished. Worker recovery must also cover
late receipts after cancellation, when the normal running lease is no longer
valid. This packet does not mark Task 5 or any phase complete.

The full optimization race run passed every package except PostgreSQL and Mongo,
whose conformance setup failed while starting the Testcontainers reaper, before
the test bodies ran. Full sequential package retries passed: PostgreSQL took
207.561s and Mongo 271.545s. Memory and SQLite passed their initial full suites
in 69.042s and 194.942s.

After the expiry fix, focused accounting races passed on all four backends,
including the existing reservation and settlement cases. Root-package, artifact
and internal-store races passed again. Published Cortex v1.8.0 compatibility
checks passed, as did current and published full-module builds. Final `make f`
and `make l` passed across all ten modules. The final optimizer source hashes
remained unchanged during review.

## Metered model client

`MeteredClient` now wraps the existing `llm.Client` interface. It checks exact
scope and model binding, clones the request before estimation, and commits one
dispatch marker bound to the full request before calling the provider. Changing
messages, tools, output limits, model revision or streaming mode cannot reuse
that dispatch permission.

`ModelUsageReporter` separates authoritative accounting from raw provider token
counts. Missing or invalid reports retain the reserved debit, while observed
overruns remain recorded. Hosts supply the conservative estimator, receipt
verification and frozen pricing. No concrete provider accounting adapter is
claimed by this packet.

Stream regressions first reproduced hidden accounting errors at EOF, discarded
oversized-chunk errors, unclosed canceled reads, and usage missed during close.
Further regressions caught a mismatched dispatch lease reaching the provider and
an accounting EOF being interpreted as successful iteration. The fixes preserve
non-EOF failure causes and close and settle each stream once. Concurrent close
cancels a blocked read. A test through Cortex's streaming engine confirms that
an accounting failure emits an error without a successful completion event.

Task 5 remains open. Controller, worker and judge integration, concrete estimator
and pricing adapters, tool accounting and late receipt recovery remain unfinished.
This packet does not mark a phase complete.

Verification: the full optimization root-package race suite passed against the
current checkout (1.999s) and published Cortex v1.8.0 (2.380s). Full optimization
module builds passed with both dependencies. The scoped `make f` target and
all-module `make l` passed, with zero lint issues across ten modules. Persistence
implementations did not change in this packet; their database suites were not
rerun.

## Frozen token pricing

`TokenPricing` implements the pricing port with host-approved input, output and
fixed request rates. Quotes require the exact model, model revision and pricing
revision. Each token category rounds upward to whole USD micro-units before the
fixed charge is added. Intermediate arithmetic is exact, and an overflowing
result returns no quote. Zero rates are explicit estimates rather than receipts.

Tests first failed on the missing adapter. A separate fractional-rate regression
then caught combined rounding that could under-reserve when input and output
are rounded separately. Tests also cover invalid rates, mismatched revisions,
negative counts, cancellation, large intermediate values and final overflow.

The host still supplies rates that cover its provider's billable categories and
a token estimator for the actual request. Controller integration and the remaining
Task 5 work are unchanged. No phase is complete.

Verification: scoped `make f` and all-module `make l` passed. The full optimizer
root-package race suite passed against current Cortex (2.261s) and published
v1.8.0 (1.940s), and both full-module builds passed. The model-wrapper commit
`016e4ac` also passed CI. Database implementations were unchanged by pricing.


## Request estimation, controller execution and late receipts

`RequestEstimator` now computes reservations from detached model requests. It
combines a host-qualified input bound with the actual output cap and frozen
pricing, rejects unknown model revisions and checks integer overflow. The host
supplies the provider-specific tokenizer/framing counter. No heuristic estimate
is treated as a guaranteed bound.

`ControllerFactory` resolves frozen instructions and model bindings, then meters
proposal, composition and reflection calls. Inputs must match stored discovery
trials, evaluations and candidates, the frozen baseline, policy, round and lesson
set. Artifact retention covers all prior attempts. The controller refuses held-out
evidence, unsupported tool execution and replay after a durable dispatch marker.
`ControllerPhaseHandler` validates candidate edits and returns atomic transition
resources. Reflection publications commit with review or task-local completion.

`Optimizer` now supports create, get, list, start, pause, resume and cancel, plus
host-started `Work` and `WorkOnce`. Workers renew leases and current authority,
preserve deadlines across resume, stop on persisted cancellation and commit budget
exhaustion. Each instance runs one phase at a time. The host supplies the remaining
trial/evaluation phase handler and owns supervision and environment cleanup.

`UsageRecovery` retrieves verified existing receipts without dispatching work. It
rechecks caller and original requester grants, then uses a reservation revision
and exact request digest to reconcile accounting after lease expiry or experiment
cancellation. Duplicate receipts cannot refund twice or restore execution
permission. Takeover and resume reconcile unresolved calls before another phase.
An unavailable receipt stops that worker. Recovering accounting does not recover
a lost model output or authorize repeating the call.

Regression tests reproduced altered discovery evidence reaching the provider and
budget failures leaving experiments running. Shared conformance tests cover late
receipts after expiry and cancellation, paused/resumed generation fencing, metered
candidate commits and reflection publication rollback. Root worker tests exercise
shutdown, current-grant revocation and verified versus unknown takeover receipts.
Two final regressions caught requester resolution masking the calling worker
identity during heartbeat and receipt recovery. Phase execution now preserves
the caller context so each operation can check both principals.

Remaining work includes concrete trial/evaluator phase adapters, judge and tool
accounting, lost model-output reconciliation, Fabriq publication, activation,
dashboard integration and full-loop qualification. Task 5 and the overall plan
remain open; these tests do not establish a complete deployed optimizer.

Verification: scoped `make f`, all-module `make l` and the optimization module
build passed. Root and artifact race suites passed against both the current
checkout and published Cortex v1.8.0. Full memory, SQLite, PostgreSQL and MongoDB
store race suites passed. The final controller, reflection, lifecycle and receipt
checks were rerun across all four backends after the worker authority fix, with
real database containers and no skips. No hosted model or complete optimizer
application flow was exercised.

## Trial execution and Sentinel adapter packet

`TrialPhaseHandler` now schedules sequential baseline, probe and validation work,
selects a finalist from discovery, pins the validation allocation and produces a
decision from the evaluator's paired statistics. It composes with the existing
metered controller handler through reflection. The original edit policy and
stored evidence checks still control every accepted candidate and decision.

Execution persists the attempt and environment receipt before a durable dispatch
marker. The host enforces the whole-trial allowance. Reconnecting workers inspect
that attempt, retain unresolved budget and require confirmed containment before
committing its capture. Scoring follows capture persistence. Artifacts from every
prior attempt are checked before scoring and selection, and cleanup has a bounded
context independent of caller cancellation.

The new `sentinel/optimization` package binds exact Cortex scopes and public tasks
to private frozen Sentinel plans. It scores recorded output without a target
callback, preserves usage quality and original duration, and reuses durable jobs
on retry. Paired comparison uses Sentinel v1.9.2's authoritative job-set API.
Unsupported confidence settings and unmetered scorers fail before scoring.

The SQLite integration test drives the worker, controller handler, trial adapter
and real Sentinel scoring/comparison through a recommendation and reflection. It
reopens both databases after lost provisioning, execution and scoring
acknowledgements. The single candidate case uses twelve trial executions and two
controller calls. A second
case exercises composition, with eighteen executions and three controller calls.
Shared backend tests cover worker replacement, ambiguous containment and baseline
retention after a rejected proposal. Models and environments remain fixtures.

Sequential scheduling and local scorers are the supported integration in this
packet. Metered judges, cost-ratio provenance, parallel/task-local scheduling,
lost controller-output reconciliation, live sandbox qualification, Fabriq
publication, activation and dashboard application flows remain open. This packet
does not mark the overall optimization plan or deployed system complete.

The extended race run exposed a worker lease defect: short steps reused the
original expiry without reaching a heartbeat tick. A regression reproduced it.
`WorkOnce` now renews the valid current generation before each dispatch. A second
regression caught unknown cost passing Sentinel's cost scorer as zero; the adapter
now refuses that scorer until captured cost is known.

Sentinel requires the published optimization commit `7f96edf` and Sentinel
v1.9.2. Its consumer check passes with both local Cortex replacements removed.
The release workflow now verifies, pins, tags and probes the optimization module
alongside the existing modules. A temporary-copy check covered all six module
pins and preserved their local replacements. No release was dispatched.

Verification: scoped `make f`, all-ten-module `make l`, and optimization, Sentinel
and extension builds passed. Full Sentinel and extension race suites passed.
Final optimization core/artifact race suites and trial/controller recovery tests
passed across memory, SQLite, PostgreSQL and MongoDB. The broad store run passed
memory, SQLite and PostgreSQL; MongoDB's sole failure was the lease regression
from the binary built before the fix. Its corrected recovery cases passed both
the targeted backend run and a verbose MongoDB run. These database fixtures start
real containers and fail when Docker is unavailable.

## Metered Sentinel judges and cost provenance

You can register a `MeteredJudge` before freezing a Sentinel suite and bind it to
an optimizer repository, current authority services and a pinned model route.
Each actual call reserves input plus capped output through the existing ledger.
The evaluation request carries the worker lease, so a stale worker cannot borrow
a replacement's authority. Scorer revisions include the model revision and
limits, while the frozen plan retains the private rubric.

Judges accept strict JSON scores. Missing, malformed or truncated responses stay
incomplete. Recorded-job retries reuse committed scores, and a dispatch marker
prevents another call when the answer was lost before persistence. Late provider
receipts can reconcile accounting after pause or lease loss; they cannot recover
the missing answer. Judge usage remains separate from trial execution costs.

With `Config.Repository`, comparisons join exact Sentinel jobs to stored Cortex
evaluations and sealed captures. Cost ratios require measured execution charges
at the experiment's frozen pricing revision. Unknown or estimated costs,
token-only budgets and zero baseline spend leave the ratio unavailable.
Conflicting report, plan or capture identities fail the comparison.

Verification covers four SQLite loop variants with composition on or off and
metered judges on or off. Each reopens both stores after lost provisioning,
execution and scoring acknowledgements. The priced variants verify a 0.5 trial
cost ratio and one settled judge reservation per execution. Failure tests cover
budget denial, stale leases, revoked authority, strict score parsing, retained
unknown usage and idempotent late receipts without replay. The full Sentinel
race suite, optimization core race suite and memory/SQLite trial recovery race
checks pass. All-module lint and both module builds pass.

Sentinel pins the published optimization lease API at `32be891`. Live provider
qualification, lost model-answer recovery, parallel scheduling, Fabriq
publication, activation and dashboard application flows remain open.

## Ordinary memory isolation for optimization runs

The Fabriq memory plugin excludes evaluation, trial and controller runs before
mapping a tenant or writing memory. A small suppression marker carries the
start-time decision through terminal callbacks that lose their original context;
the marker retains no private input and is removed on completion or failure.
Normal runs keep their existing memory behavior.

The regression uses real prepared engine execution and SQLite persistence. All
six non-normal success/failure cases failed before the fix, then passed, including
terminal callbacks with missing provenance. Full adapter race tests and builds
pass against both the checkout and published Cortex commit `7d7f566`. The real
Postgres/Fabriq integration test and all-module lint also pass.

The adapter requires the proxy-resolved root version
`v1.8.1-0.20261024054512-7d7f56675086`. Its existing shared telemetry import is absent
from Cortex v1.8.0, which the consumer check caught after local replacements were
removed. The integration test module follows the same root pin. This closes the
ordinary-memory feedback path; governed lesson delivery and the full learning
phase remain in progress.

## Fenced lesson publication delivery

You can run `Publisher.Work` with an exact scoped context and a host-supplied
`PublicationSink`. The worker claims existing outbox records, rechecks caller and
requester authority after delivery, and acknowledges the destination's aggregate
ID, payload digest and version under the current lease. Publishing does not
reopen the experiment or change its decision.

Claims, retries and acknowledgements share the existing store transaction and
audit trail. Stale workers cannot acknowledge a replacement's claim, and local
revocation supersedes an unfinished publication. Delivery failures retain a
bounded error code and retry time. Eight dispatched attempts are allowed, including
attempts that lose their lease before they can record a failure. Exhausted or
invalid deliveries remain visible as terminal records.

Race tests cover all four backends, competing claims, rollback, retry delay,
receipt mismatch, cancellation, revoked authority and lost acknowledgements.
SQLite, Postgres and Mongo also close and reopen their databases between claim,
takeover and acknowledgement checks. These tests use an idempotent destination
double; they establish local outbox recovery, not Fabriq delivery semantics.

The Fabriq sink, authoritative remote reconciliation, filtered recall and an
operator retry command remain open in Task 12. The broader implementation plan
is still in progress.

## Fabriq lesson adapter and persistent integration

`integrations/fabriq/optimization` now supplies `LessonSpec`,
`LessonWritePolicy` and an adapter implementing both `LessonStore` and
`PublicationSink`. You provide the tenant mapper, local repository and host
authorization services. Provision the dynamic table before starting a worker.
The schema indexes eligibility and provenance fields and embeds only the lesson
statement.

Publication uses the outbox's stable aggregate ID and create with expected
version zero. An authoritative lookup verifies the full payload digest and
indexed metadata after either success or a lost acknowledgement. Withdrawal
uses a version-checked update. If withdrawal arrives before publication, it
creates a tombstone at the same ID, so an older create cannot revive the lesson.
An unchanged source row after a failed withdrawal remains retryable.

Recall applies exact scope, domain, schema, active, expiry and creation-cutoff
filters in Fabriq, then checks local evidence and current authorization. Missing
channels and token-truncated results return incomplete evidence. Semantic recall
returns a bounded result set; a nonempty cursor is rejected because the search
ranking does not provide a stable continuation.

The integration test uses real Fabriq/Postgres with a non-superuser tenant role
and a persistent SQLite optimizer store. It verifies lost write replies, local
store reopen, vector indexing and recall, local revocation against stale remote
content, durable retry during a destination outage and eventual withdrawal.
The module race suite and build pass with local replacements removed, using
published optimization commit `37b94d6`. All-module lint and the full integration
race suite pass. Fabriq v1.8.2 already contains the generic duplicate-create,
expected-version and filtered-hydration regression tests; no upstream production
change was needed for this adapter.

Task 12 still needs an authorized operator retry command, capability readiness
checks and the host-facing recall-to-frozen-lesson creation flow. Dashboard
application flows and the remaining implementation phases are also open.

## Learning creation, operator retry and readiness

You can now configure `OptimizerConfig.Lessons` and call `CreateWithLessons`.
The query uses the specification's fixed cutoff, exact scope and a bounded
result set. Creation rejects partial results, duplicate IDs, changed content,
revoked evidence and lost authority. The store checks the recalled revisions
and digests again in the creation transaction before freezing them into the
experiment. No draft is persisted when recall fails.

`Publisher.Retry` requires the dedicated `optimization.lesson.retry` permission
and current delivery authority for both the caller and original requester.
It opens another bounded delivery cycle while retaining total attempts and
lease generations. Repeating the same request is idempotent. Each retry event
retains the operator, reason and expected generation; active, completed and
superseded deliveries cannot be reset.

Use `NewLessonToolkit` to bind the guarded write policy and strict retrieval,
then call `Adapter.CheckReady` to verify provisioned columns and semantic
retrieval. The probe performs no writes. Actual delivery still exercises the
destination's write grants. An arbitrary toolkit without the bound policy,
failed embedding or a registered entity without its physical table returns
unavailable.

Creation and retry race tests pass on memory, SQLite, Postgres and Mongo.
The real non-superuser Fabriq integration now creates an experiment from recalled
evidence and checks that local revocation pauses it. Provisioned readiness passes;
an unprovisioned table fails. The optimizer and Fabriq module builds and race
checks pass. The broader plan remains open, including activation service wiring,
task-local refinement and authenticated API/dashboard flows.

Final scoped `make f` and all-module `make l` pass. Retry audit and dedicated
permission checks pass with the race detector on all four stores. The Fabriq
module also passes race tests and build with local replacements removed.

## Activation commands and explicit production execution

You can now call `Activate`, `Rollback` and `Deactivate` through the optimizer.
Activation and rollback require current caller and requester authority, the exact
qualifying decision, a matching sealed version, freshly read artifacts and a
host-supplied `Compatibility.Check`. The compatibility report must match both
the version digest and current policy digest. Missing compatibility or artifact
verification returns unavailable.

`Deactivate` appends a disabled generation with the authenticated actor and
reason. It can disable a route whose evidence has expired. Repeating a command
returns its original record without moving a newer head; rollback and
reactivation append generations rather than rewinding history. Disabled records
cannot be rollback targets.

Bind the production engine with `NewOptimizer(config,
WithActiveEngine(productionEngine))` before calling `RunActive`. The execution
uses that engine's `RunPrepared` and normal production purpose. The selected
harness version stays pinned when another command changes the route. The ordinary
`RunAgent` path remains independent, and an unbound optimizer cannot execute an
active route. Model/tool revision checks still run inside the production engine.

A regression test exposed evidence withdrawal during an external content read.
`ResolveActivation` now rechecks the selected generation's retained artifacts and
lesson eligibility before dispatch, without resolving a different current head.
Historical reads remain available for audit. Artifact traversal is shared with
store retention checks and includes every prior execution attempt.

The source and policy compatibility implementation is an explicit host dependency.
A built-in engine compatibility adapter and dashboard capability wiring remain
to be finished before Task 11 can be marked complete. A dedicated suspension
test now checks that resume retains the selected snapshot after deactivation. The broader implementation goal is still open.

Final verification passes: scoped `make f`, all-module `make l` with a private
cache, the full optimization race suite, module build and consumer race/build
against the published root dependency. Full store suites passed memory, SQLite,
PostgreSQL and Mongo without backend skips. Separate restart and suspension tests
passed on all durable backends. The initial default-cache lint findings in
unchanged nil-guarded tests did not reproduce with a clean private cache.


## Bounded parallel trials

`TrialPhaseHandler` now accepts `ModeParallel`. Each step admits at most
`Limits.Concurrency` operations, with existing attempts ahead of queued trials.
Unknown effects keep their slot. Admission and result assembly use stable trial
identity order; external calls still require their own durable dispatch marker
and share the experiment budget.

A mixed batch commits successful sibling captures or evaluations under the
worker's current lease and returns the batch error. You can resume the unresolved
work without replaying completed calls. A budget refusal cannot close a batch
that contains an unknown attempt. Cancellation or lost authority prevents the
batch commit and retains dispatch markers for recovery.

MongoDB integration exposed contention at the scope fence before simultaneous
reservations could dispatch. The adapter now retries fence acquisition with
bounded backoff, before reading state or invoking the caller. It never retries
a callback or an ambiguous commit. Existing cross-connection fence tests still
pass.

The new conformance tests cover opposite completion orders, charged concurrency,
partial results, mixed budget and unknown outcomes, and cancellation. Restart
tests reopen SQLite, PostgreSQL and MongoDB and take over the lease without
repeating either remote dispatch. Sentinel's SQLite loop now runs parallel
proposal, composition and validation with both deterministic and metered judges.
It recovers lost open, execution and evaluation acknowledgments, keeps judge
charges separate, and preserves the expected measured cost ratio.

Scoped formatting, all-module lint, targeted races on all four stores, the full
optimization and Sentinel race suites, module builds and a consumer build/race
check against published Cortex v1.8.0 pass. The full store suites took 132.758s
for memory, 332.893s for SQLite, 318.979s for PostgreSQL and 445.600s for MongoDB.
Task 13 remains open for task-local selection, explicit artifact adoption and
product capability wiring. No phase completion claim is made.

## Task-local selection and explicit artifact adoption

`OptimizeTask` now uses the existing worker, controller and prepared trial paths
for one frozen discovery task. It returns the selected completed capture and every
trial outcome, without rerunning the winner. Default rounds and repetitions are
one. The frozen deadline covers qualification and execution, respects a shorter
caller deadline, and survives worker takeover. Cancellation attempts an authorized
durable stop. Optimization trial, controller and evaluation provenance cannot
start another task-local optimization.

The trusted evaluator must qualify every criterion as independent of an expected
answer before persistence and at trial execution. Sentinel exports this capability,
including built-in substring fallback, and combines requirements across cases.
Legacy offline plans retain their bindings; task-local plans need explicit
capabilities. The same environment checks apply at dispatch and selection and
preserve every promised guarantee, including read-only access.

Selection rederives scores from the complete stored roster, preserves failures
and blocked probes, and retains the baseline on ties. Cost comparisons require
known original execution charges at the pinned pricing revision and a positive
baseline. Judge charges remain separate. Task evidence can support a contrastive
lesson, but it cannot become an offline confidence estimate or global activation.

`GetTaskResult` rechecks current authority, artifacts and lessons. Explicit
`AdoptArtifacts` requires the frozen adoption policy, exact selected bindings and
current caller/requester grants. The host adapter owns destination authorization
and atomic effect/key/digest/receipt persistence. The fixture loses a commit
response, reconciles the retry without another effect, and rejects changed
content under the same key. A production destination adapter is still required.

The Sentinel task loop uses a metered judge without an expected answer. It reopens
both SQLite stores after lost environment, execution and scoring acknowledgments,
recovers twelve trial/evaluation records, and returns the selected output without
replaying calls or consuming a validation allocation. Conformance covers all four
stores, including deadline qualification, cancellation, forged capabilities,
recursive execution, selection tampering, denied adoption and withdrawn evidence.

Product capability wiring and the later plan tasks remain open.

Review found that evidence withdrawn during a later artifact content read could
escape an earlier retention check. The regression reproduced this with separate
baseline and candidate artifacts. Result retrieval and adoption now recheck every
trial's retained evidence and frozen lessons after external reads. The final task
conformance race tests pass on memory, SQLite, PostgreSQL and MongoDB. The full
Sentinel race suite passed in 222.748s, and the Docker-backed integration suite
passed in 11.872s. The full optimization race suite passed memory in 135.636s,
SQLite in 337.919s, PostgreSQL in 324.102s and MongoDB in 453.116s. After the
retention fix, all four focused task suites, Sentinel task/criteria races, both
module builds and the published-root consumer build/core/task races pass. Scoped
`make f`, all-module `make l` with a private cache and `git diff --check` pass.
No database backend was skipped in the focused conformance runs.

## Current source and runtime compatibility

`NewEngineCompatibility` binds the production engine, its deployed runtime revision
and a host checker for current policy and external dependencies. It validates the
sealed version and exact scope, gives the host a detached copy, then checks pinned
model/tool implementations and stored sources in a current atomic snapshot. A
missing provider or snapshot capability is unavailable; changed source content,
runtime bindings or policy is stale. The original cause remains inspectable.

The optimizer requires this built-in checker and `RunActive` to use the same
engine. Approved derived prompt content keeps its source identity. No model or
tool executes during compatibility checks, and the host still owns provider
routing, external knowledge revisions and current grants.

The real-store integration reproduced an edit during an artifact read reaching
the model. Activation and execution now check compatibility after content reads.
The tests cover both paths, stale rollback refusal, unchanged activation history
and deactivation of a stale route, using a real SQLite runtime source store with
each optimization backend. Core tests cover changes during host checks, source
edits, model/tool/runtime pins, foreign scope, tampering, host denial, detached
host inputs, snapshotless stores, cancellation and engine mismatch.

The runtime API is published in `e03e71c`, and the optimizer pins the verified
module version `v1.8.1-0.20261025170844-e03e71ca03f4`. Module tidying propagates
that root requirement to Sentinel, the extension, Fabriq and integration tests.
A temporary consumer without the local root replacement passes its build, core
races and stale-source activation tests. Core races and the full activation/recovery
group pass on all four optimization stores: memory 32.998s, SQLite 82.252s,
PostgreSQL 82.116s and MongoDB 101.707s. Full Sentinel races pass in 233.560s.
Extension and Fabriq races, Docker-backed integration tests, module builds, scoped
formatting and final all-module lint pass. Product capability wiring and Tasks
14-21 remain open.

## Controller comparison family foundation

Task 14 now has a frozen two-arm roster with complete child templates. Child
creation requires a live parent lease and an exact match for the stored template,
controller, evaluation plan and initial lessons. Parent creation rejects existing
child IDs, changed arm positions, incompatible accounting and nested comparisons.
The roster survives database reopen, so recovery does not reconstruct child
assignments from current controller output.

Every reservation checks both the child's ceiling and the parent's aggregate
ceiling in the same transaction. Aggregate spend comes from the original parent
and child reservations, including all operation classes and conservative unknown
usage. A recovered receipt changes that original debit. A late overrun blocks a
sibling's held dispatch, and one receipt cannot settle calls in both arms.

Child leases record the parent's generation. Parent pause, cancellation, expiry
or takeover stops child execution writes, while a replacement worker can claim a fresh
child generation and reconcile existing dispatch markers. Child deadlines cannot
outlive the parent. Metered operations recheck caller and original requester
grants against both experiment resources.

This is the storage and accounting foundation. The comparison scheduler,
arm-isolated lesson advancement, parent-owned validation across the full task
stream and controller adoption remain open. Ordinary trial and controller phase
handlers refuse comparison parents until that scheduler is available. Initial
child lessons must currently match the parent's frozen lesson IDs.

The full optimization race suite passed: memory 138.773s, SQLite 433.448s,
PostgreSQL 334.206s and MongoDB 460.235s. The full Sentinel race suite passed in
219.076s, with extension and Fabriq races and Docker-backed integration tests
also passing. Final review then reproduced two accounting gaps: a saturated
aggregate could admit a held call at the maximum integer ceiling, and trial
attempt creation checked only its own experiment's budget. Both now use the
same subtraction-based family budget check. After those fixes, the affected
accounting, attempt and recovery race tests passed on all four stores, along
with core races, Sentinel judge/cost tests, module build, scoped formatting and
all-module lint. Persistent recovery tests reopen all three database backends.

## Paired validation across the controller task stream

A comparison now reserves one allocation for its entire frozen child roster.
The store derives each child's held-out case assignment from its saved spec and
checks the controller arm, stream position, scorer revisions, criteria and
objective. A child cannot reserve a separate allocation. Unrelated experiments
cannot form a paired roster, including through the older two-child shortcut.
Changed assignments, reordered children and repeated holdouts at later stream
positions are refused. The parent's trial plan includes both controller arms.

The parent enters validation only after all children freeze their discovery
outputs. Child workers reuse that allocation, schedule their own assigned arm
and wait for the whole stream before entering selection. The store rechecks the
same roster before decisions can be published, which keeps reflections behind
the barrier too. Failed trials remain recorded outcomes; missing evaluations
and unresolved attempts keep the barrier closed.

The shared conformance tests use four children across two stream positions.
They cover immutable assignments, partial progress, foreign-child refusal,
worker scheduling without replay and parent/child phase gates. Recovery closes
and reopens each persistent store, takes over parent and child leases, reuses the
same exposed allocation and completes the remaining cases without changing its
roster or exposure history.

Task 14 still needs the comparison creation/scheduling API, discovery-only lesson
advancement within each controller arm, controller-level aggregate comparison
and controller adoption. The current ordinary handlers still refuse parent
execution. Child validation and its persisted feedback barrier are implemented.

Final checks passed for optimization core, memory, SQLite and PostgreSQL, the full
Sentinel race suite, extension and Fabriq race suites, Docker-backed integration,
module builds, scoped formatting and all-module lint. SQLite completed in
528.524s and PostgreSQL in 461.778s. The first MongoDB package run reached its
final recovery test but exceeded Go's default ten-minute package limit. Only that
finished package was rerun with an explicit thirty-minute timeout.
That full MongoDB race run passed in 741.784s. All optimization packages passed
across the original run and this targeted rerun; the original run itself exited
with the MongoDB timeout failure.

## Controller comparison creation

`CreateImprovementExperiment` now saves a qualified comparison draft from a
host-frozen `ImprovementSpec`. It preserves the two controller versions, complete
child templates and ordered stream. Configure `ImprovementEvaluator` explicitly;
each distinct frozen source is checked once, while paired arms reuse the same
definition. Creation does not start child workers or make model calls.

Parent and child creation grants are checked before evaluator access and again
before persistence. Revocation or a changed caller during qualification stops
creation, including access to the next plan. The evaluator receives a detached
request, so it cannot change the saved tasks through a shared slice. Task-local
creation and trial qualification use the same helper. Engine provenance refuses
recursive creation from trials, controllers and evaluation runs, including the
generic `Create` entry point. A regression using real prepared-run contexts
proved that generic creation previously saved drafts from those restricted
purposes. The shared creation path now rejects them before persistence.

Regression tests reproduced four previously accepted parent/child mismatches:
objectives, repetition counts, scorers and criteria. Storage now rejects all four
at creation. The service tests also cover missing adapters, distinct plan sources,
changed frozen metadata, permission loss and retained immutable templates.

Task 14 still needs parent scheduling, discovery-only lesson advancement within
each arm, aggregate controller comparison and adoption. Ordinary handlers remain
unavailable for parent execution.

The Sentinel integration test registers a metered judge, qualifies a comparison,
reopens both SQLite stores and checks the saved controller roster and plan against
Sentinel's reopened frozen definition. Creation leaves the judge call count and
parent reservations at zero. The creation and recursion race matrix passes on
memory, SQLite, PostgreSQL and MongoDB. Final all-module lint, scoped formatting,
optimization and Sentinel builds, core races, lesson-backed creation checks and
the Sentinel creation/reopen test pass after the shared recursion guard. The full
Sentinel suite passed in 333.284s before that last guard change. The broad
optimization run passed core and memory, but MongoDB and PostgreSQL each reached
the thirty-minute package timeout during late conformance cases. Neither log
reported an assertion or race failure before the timeout. Their remaining
conformance and recovery checks are being run in smaller groups; the original
SQLite package is still running. Those broad checks are not recorded as passing.


## Provisional discovery reflection storage

Each controller-arm task can now save one immutable `DiscoveryLessons` record
before the parent opens validation. The store derives the parent, controller,
arm and stream position from the frozen roster. It checks the current parent
and child lease, requires captured discovery evaluations, and rechecks retained
artifacts from every cited trial attempt. Shared content validation bounds both
provisional drafts and published lessons.

An empty result is durable too. Retrying the same record preserves its digest,
timestamp and audit event; changing its content conflicts. A transaction abort
removes the record and audit together. These records do not enter the global
lesson library or the Fabriq publication outbox.

The shared storage tests cover source binding, cross-scope reads, cross-arm and
missing evidence, stale workers, parent pause, validation already started, empty
results and rollback. Reopen tests take over the parent and child leases, reject
the old worker, retry the saved result and reopen again to check the unchanged
record and single audit event. The base discovery/reopen matrix passes on memory,
SQLite, PostgreSQL and MongoDB. The added rollback and pause cases pass on memory,
SQLite and PostgreSQL; MongoDB is checking them in the remaining recovery run.

This is storage support. Later-task inheritance, current-evidence resolution,
metered discovery reflection and parent scheduling are still pending. Aggregate
controller comparison and adoption also remain in Task 14.

The remaining comparison-creation conformance cases passed on PostgreSQL
(221.694s) and MongoDB (699.966s). Their non-conformance recovery groups and the
original SQLite package are still running. The optimization build and record
codec checks pass. All-module lint passed before the final test additions; the
post-format rerun reached concurrent A2A lint failures in the runtime chat's
files. Those diagnostics were sent to that chat, and its files remain untouched.


## Frozen discovery inheritance

Later children now inherit every earlier discovery reflection from their own
controller arm. Storage derives these references from the parent roster and
includes their digests in the child's specification digest. Creation waits for
those records, including empty reflections. Callers cannot supply references,
and the resolver refuses parent, self, other-arm and altered-reference reads.

The controller worker loads these records into proposal and composition input.
The model adapter checks the complete supplied content against the frozen
references before calling the provider. It also refuses omitted records and
changed content under the original digest. Ordinary experiments have no private
discovery input. The optional digest field preserves existing specification
hashes when there are no inherited records; old later-position drafts without
the required references are refused, not silently given new input.

Resolution rechecks captured discovery evaluations, all-attempt artifact
retention and transitive lesson eligibility. Reservation and dispatch now run
the same eligibility check inside their transaction. A regression reproduced a
held reservation dispatching after its source artifact was deleted. That call
is now refused without adding a dispatch marker or changing the retained debit.
Another regression reproduced unfrozen discovery content reaching the model;
the complete-input check now stops it before provider access.

Reopen tests retain the exact source records, references and specification
hash, then take over parent and child leases and refuse the old worker. Existing
paired-validation fixtures now save empty discovery reflections before creating
the next stream position, preserving the original whole-stream feedback barrier.

Final formatting, all-module lint, optimization build, core races, final memory
boundary checks, and targeted Sentinel cost-provenance, late-judge-receipt and
comparison-reopen checks pass. The inheritance/reopen matrix passes on all four stores, and the paired-validation
regressions pass on SQLite, PostgreSQL and MongoDB. The broader memory suite is
still running.

The preceding verification run also finished: SQLite passed in 1666.639s, and
the remaining PostgreSQL and MongoDB durability/recovery groups passed in
421.195s and 599.345s. The full Sentinel optimization race suite passed in
640.052s. The original broad command still exited with the two database timeout
failures; its completed packages and the passing follow-up groups are recorded
separately.

Task 14 still needs metered discovery-reflection scheduling, the parent worker,
aggregate controller comparison and controller adoption. Tasks 15 onward remain
tracked in the implementation plan.


## Metered discovery reflection

The controller phase handler now reflects when a child has finished discovery
and the parent is still in its baseline phase. It uses the child's existing
worker lease and heartbeat, saves the private result, then leaves the child at
validation until the parent opens its paired allocation. It does not publish a
lesson or create a decision at that point.

The call key is `controller/discovery-reflect/<round>`, separate from final
reflection. Repeated handler calls acknowledge the saved result without another
model invocation. Empty results persist. If the answer is lost, the durable call
marker and debit remain, and a retry returns `ErrUnknownAttempt`. A late receipt
can settle the original charge, but it cannot recover the missing answer or
permit another call.

Before saving, the handler rechecks current caller and requester authority for
both child and parent. A regression revoked authority immediately after model
usage settlement and reproduced an unauthorized reflection write. The new check
refuses that write. The store still checks the lease, discovery evidence and
parent phase in the transaction.

Tests cover real worker execution, successful and empty results, lost answers,
separate final-reflection keys, withholding during validation, database reopen,
lease takeover and late-receipt settlement without answer replay. The first
late-receipt matrix passed all four stores. The final matrix with the authority
regression passes all four stores, including MongoDB in 91.495s. Normal controller
and paired-worker regressions and the optimization build pass. The full Sentinel
optimization suite passed in 310.927s before the final private-write authority
check; that check is covered by the final four-store matrix. Final formatting
and all-module lint pass. The runtime chat corrected the two findings reported
during its concurrent edits, and the clean rerun completed before this commit.

The preceding inheritance verification is complete: its broader memory race
suite passed in 349.024s, and its core and internal-state packages also passed.
Task 14 still needs parent scheduling, aggregate comparison and controller
adoption. Discovery storage, inheritance and the metered child handler are now
implemented.

## Parent discovery and paired-validation scheduler

`NewImprovementPhaseHandler` wraps the child worker configuration. The parent
keeps renewing its lease while the dedicated child worker advances one bounded
step. Children are created and queued from the exact frozen templates, in stream
order. Each private discovery reflection must exist before scheduling continues.
The store now checks those reflections and their current evidence when the parent
moves from discovery into validation.

The parent freezes one paired allocation using the existing finalist selector,
then resumes each child's assigned validation work. Allocation identity, selected
execution versions and case assignments survive takeover. The parent reaches its
decision phase only after the full paired stream has terminal evidence. Generic
`Work` polling skips parent-owned children, so it cannot independently claim them
or run their decisions while the parent is scheduling.

Scheduling rechecks caller and requester authority for each child, including
children whose discovery is already complete. A denied child is not created, a
paused child remains paused, and child budget exhaustion exhausts the parent.
Unknown model calls propagate to the supervisor. Finalist selection, trial listing
and paired readiness share the existing task-mode code.

A longer integration run exposed a heartbeat shutdown race: cancellation during
renewal could turn a completed step into a false `context canceled` failure.
A deterministic regression reproduced it. Heartbeat shutdown now treats its own
cancelled renewal as normal, while the worker retains caller cancellation and
substantive renewal failures.

PostgreSQL recovery also exposed a metering boundary race. Dispatch records the
live lease after a heartbeat renewal, but the model client required its expiry to
match the request's earlier snapshot exactly. A deterministic test reproduced the
refusal, along with refusal of a held reservation taken over before dispatch and
acceptance of a mismatched parent generation. Request content and dispatch
authority are now checked separately: the call must retain experiment, owner,
worker generation and parent generation, while its expiry may extend. A held
reservation retains its creation lease as history; its live dispatch fence
supplies current authority.

The real controller/trial integration covers four ordered children across both
arms, private lesson inheritance, all proposal/reflection/trial charges, and the
holdout barrier. Recovery closes storage during discovery and after allocation,
waits for lease expiry, rebuilds both workers and verifies that provider calls and
the frozen allocation are not repeated. The final race matrix passes memory
(140.005s), SQLite (749.285s), PostgreSQL (191.627s) and MongoDB (304.472s).
It uses the production worker lease and waits for actual expiry at both restart
checkpoints. Earlier two-second fixture leases expired between a returned model
answer and its persistence under concurrent database load. Those runs correctly
refused to replay the unknown answer; dedicated short-lease tests remain.

Metering and worker race regressions, the optimization build, scoped `make f`
and final all-module `make l` pass. Sentinel's judge and cost-provenance race
tests also pass after the dispatch-fence fix. Its sequential/parallel reopen and
cost regressions passed before that fix and are recorded separately. An earlier
database matrix was interrupted when Docker became unavailable; the final matrix
above ran both container-backed stores successfully.

Task 14 still needs aggregate controller comparison and reviewed controller
adoption. The parent handler currently returns `ErrUnavailable` at the decision
phase. It does not create a recommendation or adopt a controller.

## Recorded comparison across frozen plans

Sentinel's `CompareRecordedGroups` now accepts explicit paired job sets for each
frozen plan. It shares the existing evidence loader and paired case bootstrap
with `CompareRecordedSets`. Reports retain every plan ID/digest and job digest;
the singular `PlanDigest` stays empty when several plans contribute.

Tests compare the grouped result with the same captures scored under one plan.
They cover group order, the global distinct-case minimum, pending evaluations,
required failures, changed plan references and foreign scope. Reusing a case
across plans is rejected even when the plans contain disjoint repetitions.
Duplicate input/context pairs under different case IDs are rejected too, while
distinct frozen contexts remain valid.

Sentinel commit `c59b2f9` is pushed. Its full race suite, final comparison race
suite, build, `make f` and `make l` pass locally. CI passed, and the normal release
workflow published `v1.10.0` after its build and race checks. Cortex's Sentinel
adapter and Forge extension now pin that published version. Their race suites
pass, including the full Sentinel optimization suite in 236.156s and extension
contract tests in 25.084s. Both modules build, scoped `make f` and final all-module
`make l` pass, and tidy changes only the Sentinel version and checksums.

This supplies the statistical primitive for parent controller decisions. Cortex
still needs the comparison adapter, transactional evidence validation and
reviewed adoption.

## Parent controller decision evidence

Controller comparison requests now group selected child evaluations by their
frozen plans. `ReadControllerEvidence` loads the original records, and
`BuildControllerComparison` checks the exact child templates, paired allocation,
case assignments and terminal trial evidence. The request retains the parent
spec digest and each plan reference without changing child ownership or execution
versions.

`ValidateControllerDecisionEvidence` validates controller-version decisions
using the shared execution-decision arithmetic and policy checks. The transaction
reloads each child's selected evidence, rechecks the complete paired roster and
bound lessons, and persists the decision with its original evaluation IDs.
Invalid scores, unknown cost ratios, substituted plans and incomplete job lists
are rejected. Artifact checks follow child validation trials both when recording
a qualified decision and when moving the parent into review.

The final race matrix passes memory (16.362s), SQLite (57.526s), PostgreSQL
(55.832s) and MongoDB (68.771s), including reopen recovery and stale parent
fences. Decision, artifact and review regressions pass in memory and SQLite.
Sentinel recovery, metered judge and cost-provenance regressions pass in
136.284s. The optimization build, scoped `make f` and final all-module `make l`
pass. The parent scheduler still stops before comparison; Sentinel adapter
wiring, the decision phase and reviewed controller adoption remain pending.

## Parent comparison adapter and decision worker

`ImprovementPhaseHandler` now handles `PhaseDecide` through the configured
`ControllerEvaluator`. It reads the original selected child evidence, passes a
detached request to the evaluator, rechecks child authority after comparison and
validates the result before returning the transaction. Execution and controller
decisions share the same frozen threshold policy.

The Sentinel adapter reloads the canonical parent allocation and exact plan
roster, then uses `CompareRecordedGroups`. Case statistics, protected criteria,
latency and execution-cost provenance share the existing comparison projection.
A controller arm can select an unchanged execution baseline or a discovered
candidate; both retain their original child ownership. Missing captures and
incomplete scores produce an inconclusive report without another provider call.

The SQLite integration records twelve metered judge results across two plans and
four child experiments, reopens both stores, rebuilds the same report and commits
the parent worker's decision. The malformed judge-output variant preserves the
incomplete result and its original charge. Neither comparison nor reopen dispatches
another judge call. Tests also reject substituted job rosters, changed parent
specifications, foreign scopes, denied children and authority revoked during
comparison. Callback mutation cannot change the frozen request.

The full Sentinel optimization race suite passes in 291.737s. After final edits,
the complete-score and malformed-score reopen tests pass in 68.347s, and the
parent-worker race test passes in 11.585s. Existing decision and artifact-policy
regressions pass in memory and SQLite. Both modules build; scoped `make f` and
final all-module `make l` pass. The controller decision race matrix passes memory
(36.648s), SQLite (86.604s), PostgreSQL (94.526s) and MongoDB (97.359s),
including artifact checks, worker authority and reopen recovery.

Parent reflection, review completion and reviewed controller adoption remain
pending; Task 14 is not complete.
