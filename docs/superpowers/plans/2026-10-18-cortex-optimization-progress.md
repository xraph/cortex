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
| 6: Sentinel foundation | Complete, published as Sentinel v1.9.0 | Consume v1.9.0 after optimization bootstrap |
| 2: optimization contracts | Harness contracts `8f3f91b` and experiment contracts `cd89669` pushed; CI includes optimization | Wire constructor validation after storage ports |
| 3: atomic memory storage | Draft storage `c80e580`, audit/transitions `12e4a9f`, leases `ddece76`, accounting `f02dec9`; allocation checks pass | Complete resource invariants, terminal reconciliation and activation |
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
