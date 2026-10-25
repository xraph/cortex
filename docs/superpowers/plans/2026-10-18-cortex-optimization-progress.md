# Cortex optimization implementation progress

Plan: [Optimization implementation](2026-10-18-cortex-optimization.md). Scope: all seven phases and 21 tasks, using DRY and TDD. No phase is complete yet.

## Shared contracts and ownership

The user authorized direct coordination with **Improve Cortex agent execution**. That chat owns the root runtime, outcomes, prepared execution, continuation and root backend integration. This chat owns Sentinel's recorded evaluation foundation, optimization, adapters and product integration. We consume the shared runtime commits rather than maintaining two execution loops.

The outcome foundation is on Cortex main in `8bebd82`: incomplete termination, stop and finish reasons, verification status and result references. The runtime owner verified engine/run/SQLite race tests, PostgreSQL/Mongo conformance and historical fixtures, all-module lint, root build and A2A compatibility.

Prepared execution was published as Cortex `v1.8.0` from `e798d45`. Root CI `39142591307` and release `39143145756` passed, and the real Go proxy resolves that commit. The API provides `PrepareAgent`, `RunPrepared`, `StreamPrepared`, detached `Inspect()` and immutable `Derive(PreparedEdits)`. Derivation preserves identity, scope, model, budgets, schemas and implementation pins. Optimization applies its narrower edit policy.

Reproducibility still needs a stronger source snapshot guarantee. The runtime owner confirmed that v1.8.0 collects source values without a consistency check. Its follow-up detects changes to the collected read set, but does not promise an atomic multi-source snapshot. Reproducibility-required work stays unavailable until a qualified host provider or consistent store snapshot supplies that guarantee. Source changes are never resolved silently at trial time.

Runtime commit `281d727` adds publication safety and prepared schema 2. Follow-up `77ee8b9` adds read-set conflict detection with explicit atomic-snapshot-unavailable diagnostics. Schema 1 requires new preparation and evaluation before adoption. Optimization tests run against both the published v1.8.0 dependency and the current checkout; the temporary published-dependency modfile is not committed.

## Current state

| Task                                                                         | State                                                         | Next gate                                                                     |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| 1: prepared execution | Published foundation; source consistency qualification remains | Qualified atomic snapshot contract |
| 6: Sentinel foundation | Published as v1.9.2, including job-set comparison | Adapter integration remains Task 7 |
| 2: optimization contracts | Harness contracts `8f3f91b` and experiment contracts `cd89669` pushed; CI includes optimization | Wire constructor validation after storage ports |
| 3: atomic memory storage | Drafts, leases, accounting, allocations, trials and evaluation gates; decisions pushed in `3769466`; activation storage checks pass | Lesson/outbox pushed as `6b7e1f6`; scoped readers verified; durable-store work follows |
| 4: durable persistence | Durable backends, concurrent database operations and indexed resource reads verified | Artifact storage, database pagination, selective transaction loading and throughput qualification |
| 5: durable worker recovery | Metered controller, scoped service, worker leases and late receipts implemented | Complete trial/evaluator adapters, output recovery and end-to-end qualification |
| 7-21: adapters, optimizer, activation, learning, dashboard and qualification | Pending, except Fabriq prerequisites below | Follow dependency gates |

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
