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
| 3: atomic memory storage | Drafts, leases, accounting, allocations, trials and evaluation gates; decisions pushed in `3769466`; activation storage checks pass | Lesson/outbox checks pass; remaining readers and terminal reconciliation |
| 4-5: durable persistence and recovery | Pending | Finish store contracts |
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
`39516036204` at `e51dea8`; this lesson slice still needs its own CI run.

This is the memory-store contract. Durable backends, publisher claims and
acknowledgments, bounded remote retry, Fabriq schema provisioning and actual
remote revocation are still open. The worker must stop external work and
reconcile unresolved attempts after local invalidation; storage tests do not
prove that operational behavior. Current actor grants and retained artifacts
remain service checks.
