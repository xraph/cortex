# Cortex optimization implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. This document authorizes no implementation. The user requested planning only; wait for a separate instruction to build.

**Goal:** Deliver an optional Cortex optimizer with isolated experiments, Sentinel evaluation, Fabriq learning, explicit activation and a working dashboard.

**Architecture:** `cortex/optimization` owns immutable harness versions and the experiment lifecycle. Cortex root supplies prepared execution, Sentinel scores captured executions, and the existing Fabriq integration publishes and recalls validated lessons. Adapter dependencies point into optimization; the root engine never imports it.

**Tech Stack:** Go 1.26.0, Cortex, Sentinel, Fabriq core ports, Grove, PostgreSQL/SQLite/Mongo, Forge dashboard contracts, React, TypeScript, Vitest and the existing dashboard kit.

**Spec:** [Cortex optimization design](../specs/2026-10-18-cortex-optimization-design.md). Read both documents. Names marked as proposed in the spec become the contract for implementation after approval.

## Global constraints

- Do not start implementation until the user requests it. All implementation checkboxes below remain pending in this planning change.
- Work directly on `main` in each primary checkout. No task branches or additional worktrees. Inspect status and preserve concurrent edits before each change.
- All new Go modules declare Go 1.26.0 and use `GOWORK=off` for checks.
- Cortex root never imports optimization, Sentinel, Fabriq or Forge for this feature. Optimization core never imports Sentinel, Fabriq or Forge.
- The first mode is offline sequential. Task-local refinement, parallel search, improvement-controller evaluation and approved orchestration presets are later required phases.
- Initial defaults: two rounds, six non-baseline candidates, 500 total trials, concurrency two, 30-minute experiment deadline, two-minute trial timeout, 12 trial steps and three repetitions per offline case. Reserve final-validation capacity before discovery.
- Activation is explicit and revision-checked. `RunActive` consumes it. Existing `RunAgent` calls do not acquire optimizer behavior implicitly.
- Validate one to three scope levels and exact resource scope. Trial identity is metadata, never an extra scope level.
- Model-backed starts require finite positive usage limits or an explicit token-only accounting mode. Unknown cost is never known zero.
- Holdout validation is hidden from controllers and lesson recall. Require 20 distinct validation cases, a mean paired improvement of at least 0.02, a 95% bootstrap lower bound greater than zero and protected-dimension regression no greater than 0.01 for the default activation recommendation.
- Use the existing Cortex contributor, scope-relative React routes, shared `ZeroState`, kit density variants and command invalidation. Do not add a tour button without a verified onboarding provider and registered walkthrough.
- Read `rex-voice` before repo prose and commit bodies, then apply humanizer in embedded mode. No em dashes or attribution trailers.
- Before code commits, run the repository's lint command, fix owned failures, run format after final edits, inspect its diff, rerun lint and relevant tests/build. Commit exact owned paths and push verified commits. Never merge or dispatch releases without the implementation request covering that operation.

## Review focus

1. A crash after an external effect but before persistence must remain an unknown attempt until reconciled, without duplicate effects. Tasks 5, 8 and 20 test this.
2. Scope mappings that are empty, malformed, conflicting or stale must refuse before Sentinel/Fabriq calls; a same-count wrong-tenant response is a failure. Tasks 3, 4, 7, 12 and 16 test identities.
3. Changing referenced skills, tools, evaluator settings or host policy midway through an experiment must not silently change comparison inputs. Tasks 1, 6, 9 and 13 cover this.
4. Concurrency, unknown usage and retries must not bypass the aggregate budget or charge one receipt twice. Tasks 5 and 8 race-test this.
5. Dashboard fixtures must not conceal missing live activation, learning or evaluation integration. Tasks 16, 18 and 20 verify the persistent backend and explicit unavailable states.

## Repository map

Paths below use these aliases only within this plan:

| Alias | Primary checkout                                |
| ----- | ----------------------------------------------- |
| `C`   | `/Users/rexraphael/Work/xraph/forgery/cortex`   |
| `S`   | `/Users/rexraphael/Work/xraph/forgery/sentinel` |
| `F`   | `/Users/rexraphael/Work/TwinOS/fabriq`          |
| `D`   | `/Users/rexraphael/Work/xraph/forge-dashboard`  |

Read each repository's current instructions before editing it. A source path below is an ownership boundary for that task, not permission to revert concurrent work in the file.

Before task 1 or 6 starts, reconcile shared contracts with F0 in the concurrent local draft `docs/superpowers/plans/2026-10-18-cortex-assured-execution.md`. Assign one owner each for prepared execution/outcomes/provenance, Sentinel recorded evaluation, environment receipt contracts and Fabriq lesson envelopes. Reuse compatible prerequisite commits. These plans do not authorize two writers for the same runtime, store migration or manifest. The full assured-execution roadmap is not a prerequisite for optimization.

## Phase map and dependency order

| Phase                                   | Tasks | Deliverable and exit gate                                                                                            |
| --------------------------------------- | ----- | -------------------------------------------------------------------------------------------------------------------- |
| 1. Execution and evaluation foundations | 1, 6  | Root executes frozen agents; Sentinel scores recorded outputs without calling targets; backward compatibility passes |
| 2. Module, persistence and usage        | 2-5   | Importable optimization module, four store backends, durable transitions and enforced accounting                     |
| 3. Offline optimizer                    | 7-10  | Baseline/propose/probe/compose/validation yields a reproducible recommendation or retained baseline                  |
| 4. Explicit activation                  | 11    | Version activation, opt-in execution, stale-source detection and rollback work across restart                        |
| 5. Learning and broader refinement      | 12-15 | Guarded Fabriq lessons, parallel/task-local refinement, controller versions and orchestration presets                |
| 6. Product integration                  | 16-19 | Authorized API, Cortex dashboard, Sentinel evidence links and public docs                                            |
| 7. Qualification and release            | 20-21 | Persistent browser flow, live provider evidence, all module/consumer checks and release readiness                    |

Task numbers group ownership; follow dependencies rather than numeric order alone. Recommended sequence is 1, 6, upstream release checkpoint, 2-5, optimization bootstrap release checkpoint, then 7-21. Phase 1 consists of independently releasable foundation changes. Phase 2 can be published with no configured worker or feature enablement.

These are delivery gates, not calendar estimates. Re-estimate after the foundation review and live integration qualification. No phase is complete solely because its types compile.

## Verification commands used by the tasks

Run commands from the stated working directory. The initial lint run detects existing failures; final format/lint and relevant tests happen after edits. Report unrelated blockers without broad cleanup. Run database tests with verbose output and verify that containers actually ran.

| Gate          | Commands                                                                                                                                                                                                                                                           |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `C-root`      | In `C`: `GOWORK=off make l`; after final edits `make f`; then `GOWORK=off make l`, `GOWORK=off go test -race -count=1 ./engine ./prompt ./suspension ./plugin`, `GOWORK=off go build ./...`                                                                        |
| `C-module(M)` | In `C`: initial `GOWORK=off make l`, final `make f`, final `GOWORK=off make l`; in the affected module `M`: `GOWORK=off go test -race -count=1 ./...` and `GOWORK=off go build ./...`                                                                              |
| `S-all`       | In `S`: `GOWORK=off make l`, final `make f`, final `GOWORK=off make l`, `GOWORK=off go test -race -count=1 ./...`, `GOWORK=off go build ./...`; if aliases differ use documented `make lint`/`make fmt` and record them                                            |
| `S-stores`    | `GOWORK=off go test -tags integration -race -count=1 -v ./store/...`, `GOWORK=off go vet -tags integration ./store/...`, `GOWORK=off golangci-lint run --build-tags integration --max-same-issues=0 --max-issues-per-linter=0 ./store/...`                         |
| `F-all`       | In `F`: `GOWORK=off make lint`, final `make fmt`, final `GOWORK=off make lint`, `GOWORK=off make test`; for changed adapter behavior `GOWORK=off make test-integration`; use documented equivalents if targets change                                              |
| `D-plugins`   | In `D`: `pnpm --filter @forge-go/dashboard-plugin-cortex lint` and the Sentinel equivalent; final scoped `format` for changed packages, repeat lint, `typecheck` and `test` for both packages; `pnpm --filter @forge-go/dashboard-shell build` (includes `tsc -b`) |
| `D-shared`    | In `D`: `pnpm lint`, scoped final format, `pnpm lint`, `pnpm typecheck`, `pnpm -r --no-bail test`, `pnpm build`; for project-reference packages also run their documented `tsc -b` check                                                                           |
| `C-inttest`   | In `C`: `GOWORK=off make test-integration`; tests must execute against real Postgres/Fabriq, not skip                                                                                                                                                              |
| `Docs`        | In the changed Fumadocs site: `pnpm types:check`, `pnpm lint`, `pnpm build`; use its documented formatter for changed prose                                                                                                                                        |

For every implementation task: inspect the final diff, verify the current branch and staging area, stage only the listed owned files, commit the coherent change, and push only after its gate passes. If tests cannot run, record exact blockers and do not call the slice verified. Root full-suite qualification is additionally required in task 20; the documented local `store/scopespy` hang can be isolated with explicit reporting and Linux CI evidence.

## Task 1: Prepare immutable engine execution

**Depends on:** approved design.

**Files:** Create `C/engine/prepared.go`, `prepared_run.go`, `execution_context.go`, `prepared_test.go`, `prepared_resume_test.go`. Modify `C/engine/react.go`, `resume.go`, `prompt.go`, `suspension/suspension.go` and relevant run metadata persistence tests. Keep root public APIs backward compatible.

**Interfaces:** Produce `PrepareAgent`, `RunPrepared`, `PreparedAgent`, `PreparedRunOptions` and `ExecutionProvenance` from spec 5.1. Add `WithExecutionProvenance(ctx context.Context, p ExecutionProvenance) context.Context` and `ExecutionProvenanceFromContext(ctx context.Context) (ExecutionProvenance, bool)` for lifecycle adapters. `PreparedAgent.Digest()` returns `(string, error)` over canonical execution content. Source revision reads use existing entity timestamps/content hashes plus host tool/environment revisions, with before/after checks.

- [ ] Write `TestPreparedRunFreezesReferencedContent`: prepare an agent, edit its skill, trait, persona and overlay, execute the snapshot, and assert exact old prompt bytes and source digest.
- [ ] Write `TestPreparedRunEmptyToolsDeniesAll`, `TestPreparedRunRechecksAuthorization`, `TestPreparedRunRejectsWrongScope` and `TestPreparedResumeKeepsDigest`. Assert zero tool calls on denial, no tool expansion from an empty allowlist, and identical persisted/resumed provenance.
- [ ] Write `TestPreparedRunUsesSharedReActLoop` with a fake provider returning a tool call then a final answer. Assert the same sequence, step persistence and failure semantics as ordinary execution.
- [ ] Reconcile the shared outcome/stop-reason contract and add `TestPreparedStepExhaustionIsNotSuccessfulCompletion`, timeout/cancellation/suspension cases and historical-unknown serialization coverage. Trial conversion must not infer success from a legacy `completed` flag alone. Consume the runtime owner's compatible implementation if it already exists.
- [ ] Run `GOWORK=off go test ./engine -run 'TestPrepared' -count=1`; new APIs must fail to compile or assertions must fail before implementation.
- [ ] Implement preparation and snapshot execution through the existing loop. Persist prepared state in continuation without replacing authorization with stored permissions. Add strict source-drift and knowledge availability diagnostics.
- [ ] Run `C-root`, the relevant root store serialization tests, and compatibility tests for existing overrides/approval/session behavior. Commit `feat(engine): add immutable prepared execution`.

## Task 2: Define the optimization module and immutable records

**Depends on:** task 1 root APIs released through the workflow; choose a real resolvable version.

**Files:** Create `C/optimization/go.mod`, `go.sum`, `doc.go`, `ids.go`, `errors.go`, `types.go`, `version.go`, `policy.go`, `controller.go`, `evaluation.go`, `environment.go`, `lessons.go`, `artifacts.go`, `identity.go`, `options.go` and focused tests. Add `C/optimization/internal/canonical/` for deterministic encoding.

**Interfaces:** Produce spec 5.2 records and spec 5.3 ports, plus `CanonicalVersion(v HarnessVersion) ([]byte, error)`, `ValidateProposal(base HarnessVersion, p Proposal, policy EditPolicy) (HarnessVersion, error)` and `ValidateSpec(spec ExperimentSpec) error`. Public sentinel errors include `ErrInvalidSpec`, `ErrPolicyDenied`, `ErrConflict`, `ErrNoEnvironment`, `ErrUnavailable`, `ErrBudgetExceeded`, `ErrUnknownAttempt`, `ErrStaleVersion`, `ErrIncompleteEvidence` and `ErrNotFound`.

- [ ] Write table tests `TestVersionCanonicalDigest`, `TestProposalCannotChangeHostPolicy`, `TestProposalRejectsUnknownAndDuplicateFields`, `TestSpecRejectsSplitOverlap` and `TestSpecPreflightCountsTrials`.
- [ ] Assert ordered prompt changes alter the digest; display metadata does not; empty tools remain explicit; duplicate input hashes across splits fail; the planned maximum trial count includes composed and baseline validation trials and refuses a count above 500 unless the host raises its limit. Assert the ten-discovery/twenty-validation-case example reserves 120 validation trials within its 330-trial maximum.
- [ ] Run `GOWORK=off go test ./...` inside `optimization` before bodies are implemented and confirm the contract tests fail.
- [ ] Implement strict DTO decoding, ID parsing, canonical content hashing, bounded policy validation and constructor validation. Define usage quality (`known`, `estimated`, `unknown`) and evidence class (`validated`, `contrastive`, `exploratory`) as distinct types.
- [ ] Add a dependency boundary test using `go list -deps` in CI to reject Forge, Sentinel and Fabriq imports in the optimization core package and optimizer imports in Cortex root.
- [ ] Run `C-module(optimization)`. Commit `feat(optimization): define harness and experiment contracts`.

## Task 3: Implement atomic store contracts and memory backend

**Depends on:** task 2.

**Files:** Create `C/optimization/store/store.go`, `transaction.go`, `memory/store.go`, `storetest/conformance.go`, `storetest/scope.go`, `storetest/lifecycle.go`, `storetest/usage.go`, `storetest/activation.go`, and backend tests.

**Interfaces:** `Store` exposes `Transact(ctx context.Context, fn func(Tx) error) error`, `GetExperiment(ctx, id)`, `ListExperiments(ctx, ListQuery)`, resource readers, `ListEvents(ctx, experimentID, afterSequence, limit)`, `Migrate(ctx) error`, `Ping(ctx) error`, `Close() error`. Readers return typed records or `Page[T]` plus error. `Tx` exposes `CreateExperiment(ExperimentBundle) error`, `ClaimWork(ClaimRequest) (WorkLease, error)`, `RenewLease(WorkLease) error`, `CommitTransition(Transition) error`, `Reserve(UsageReservation) (Reservation, error)`, `Settle(UsageSettlement) error`, `CompareAndSwapActivation(ActivationChange) (Activation, error)` and `PutPublication(Publication) error`. Transaction methods inherit the transaction context and scope. Resource insert/update sets are carried by `Transition`, which includes expected revision, lease generation and an allowlisted state change.

`ExperimentBundle` contains experiment, frozen versions/task/evaluation references and initial event. `WorkLease` contains experiment ID, owner, generation and expiry. `Transition` contains next phase/state, newly immutable candidate/trial/decision rows, event payloads and revision-checked mutable attempt updates. Unsupported record combinations are rejected by the service and store invariants. Artifact bytes use a separate blob table through the database artifact adapter.

Add `Tx.AllocateValidation(ValidationAllocationRequest) (ValidationAllocation, error)` and `Tx.ExposeValidation(id.ID) error`, keyed by exact scope/agent/case identity and normalized input hash. Add `Tx.RevokeLesson(LessonRevocation) error`, which updates local eligibility and creates a revocation publication atomically. Allocation, exposure and revocation records participate in the same populated backend conformance suite.

- [ ] Write the conformance tests first: `ScopeIsolation`, `TransactionRollback`, `DuplicateStartIdempotency`, `StaleLeaseCannotCommit`, `ActivationCAS`, `ReservationRace`, `PublicationAtomicWithDecision`, `ValidationAllocationCannotBeReused`, `RevocationOutboxAtomic`, `CursorPaging` and populated JSON round trips for every record.
- [ ] Assert entity IDs from another exact scope fail; three-level scope works; a fourth, blank value, repeated key or canonical separator refuses before a store operation. Assert identity sets, not row counts alone.
- [ ] Run `GOWORK=off go test ./store/...`; confirm missing behavior fails.
- [ ] Implement copy-on-read/write memory storage and atomic transactions without exposing mutable maps. Store timestamps come from an injected clock in tests. Enforce unique compound keys and nonnegative budget balances.
- [ ] Run `C-module(optimization)` including race tests. Commit `feat(optimization): persist experiments with atomic transitions`.

## Task 4: Add PostgreSQL, SQLite and Mongo persistence

**Depends on:** task 3.

**Files:** Create `C/optimization/store/{postgres,sqlite,mongo}/{store.go,migrations.go,records.go,work.go,usage.go,activation.go,conformance_test.go}` and `C/optimization/artifacts/database.go`, `database_test.go`. Split large record implementations by domain when necessary.

**Interfaces:** `New(db *grove.DB) *Store` in each backend, implementing task 3. `database.New(store ArtifactBackend) optimization.ArtifactStore` implements scoped content-addressed artifact storage. `ArtifactBackend` provides scoped blob put/get/delete and reference queries with transactional metadata.

- [ ] Run the task 3 suite against fresh databases before backend implementation; require actual failures, not an empty package pass.
- [ ] Add `LeaseTakeoverAcrossProcesses`, `ActivationAuditAtomic`, `MissingArtifactBlocksDecision` and migration forward-compatibility tests. Mongo tests must use transactions on a replica set; refuse startup if required atomicity is unavailable.
- [ ] Implement scoped tables/collections and indexes. Use Grove transactions for SQL and verified Mongo transaction support. Do not emulate activation or usage atomicity with a read followed by an unguarded write.
- [ ] Add and run `GOWORK=off go test -race -count=1 -v ./store/... ./artifacts/...` in `optimization`, with testcontainers for Postgres/Mongo and the supported Mongo image override if needed.
- [ ] Inspect verbose output to prove all backends ran. Run `C-module(optimization)`. Commit `feat(optimization): add durable store backends`.

## Task 5: Enforce reservations and durable worker recovery

**Depends on:** tasks 3-4.

**Files:** Create `C/optimization/budget.go`, `worker.go`, `recovery.go`, `service.go`, `metered_llm.go`, `budget_test.go`, `worker_test.go`, `recovery_test.go`.

**Interfaces:** Produce `Create`, `Start`, `Get`, `List`, `Cancel`, `Resume`, `Work` from spec 5.3; `Meter.Reserve` and `.Settle` from spec 9; `UsageEstimator.Estimate(ctx context.Context, req *llm.Request) (UsageReservation, error)`; `Pricing.Price(ctx context.Context, usage UsageEstimate) (MoneyEstimate, error)`. `UsageEstimate` carries resolved model, prompt/output token upper bounds and pricing revision. `UsageSettlement` identifies reservation, receipt, actual/estimated counts and completeness.

- [ ] Write `TestConcurrentReservationsStayWithinLimit`, `TestDuplicateReceiptSettlesOnce`, `TestUnknownUsageKeepsReservation`, `TestLeaseFenceRejectsOldWorker`, `TestCancelStopsScheduling`, `TestShutdownLeavesRecoverableWork` and `TestRevokedIdentityPreventsResume`.
- [ ] Simulate a 100-unit budget with concurrent 60-unit reservations and assert only one starts. Advance the fake clock past 30 seconds and assert the previous generation cannot persist. Keep unknown debits until an authoritative receipt is supplied.
- [ ] Run the focused tests and confirm red, then implement service transitions and worker lifecycle without unbounded goroutines. Resolve current host identity before work; never serialize credentials.
- [ ] Add a controller/judge wrapper contract test proving every model call and metered adapter operation reserves usage, including calls that fail before producing output.
- [ ] Run `C-module(optimization)` and backend reservation/lease conformance tests. Commit `feat(optimization): enforce budgets and recover durable work`.

## Task 6: Extend Sentinel for immutable recorded evaluation

**Depends on:** approved design; independent of Cortex implementation.

**Files:** Create `S/engine/recorded.go`, `recorded_worker.go`, `evaluation_plan.go`, `recorded_test.go`, `recorded_worker_test.go`, `evaluation_plan_test.go`, `S/evalplan/{plan.go,store.go}`, `S/comparison/recorded.go`, `recorded_test.go`. Modify `S/engine/runner.go`, `S/evalrun/{run.go,result.go,settings.go}`, `S/store/store.go`, all four store backends/migrations and `S/store/storetest/`.

**Interfaces:** Produce spec 10 `FreezeEvaluation`, `EvaluateRecorded`, `CompareRecorded`, `EnqueueRecorded` and `WorkRecorded`. Share a private `scoreCapturedCase` implementation between recorded evaluation and ordinary `evaluateCase`. `EvaluationPlan` stores case snapshots and scorer configuration/revision content. `RecordedCase` identifies case and repetition and includes original execution status, output, timing, trace, input digest and usage quality. `RecordedRunRequest` has no target function. Sentinel's store owns recorded-job claim/lease/complete transactions and per-binding source reconciliation cursors.

- [ ] Write `TestEvaluateRecordedNeverCallsTarget`, `TestRecordedUsesFrozenCaseAndScorer`, `TestRecordedRetryUsesSameResult`, `TestHistoricalZeroCostIsUnknown`, `TestExecutionFailureDiffersFromScorerError` and `TestRecordedCancellationKeepsPartialResults`.
- [ ] Add durable queue conformance tests for restart, duplicate key with matching or conflicting payload, lease fencing, bounded retries, cursor advancement only after acknowledgment and zero target calls during worker retry. Advertise persistent readiness only with a durable store and running worker.
- [ ] Write comparison fixtures with exact paired case/repetition IDs, deterministic 2,000-resample bootstrap output, small-sample inconclusive status, missing trial refusal, duplicate case refusal and protected criterion failure despite a high average.
- [ ] Run `GOWORK=off go test ./engine ./comparison ./store/...`; confirm new tests fail before implementation.
- [ ] Extract scoring from target execution, add frozen plan persistence and idempotent recorded run ingestion. Keep original execution metrics separate from scoring metrics and judge usage.
- [ ] Run `S-all`, `S-stores`, and existing target/prompt-version/regression tests. Commit `feat(evaluation): score and compare recorded executions`.
- [ ] At the authorized release checkpoint, publish Sentinel via its workflow after green CI. Record the real tag for task 7.

## Foundation and module bootstrap release checkpoints

- [ ] After task 1 passes and code release is authorized, publish Cortex root prepared APIs through `release.yml`. Do not invent or hand-push a tag.
- [ ] Before publishing optimization, add it to every module list in task 21's release plumbing, including verification before tagging, pinning, proxy resolution and install docs. Test the changes without dispatch first.
- [ ] Publish tasks 2-5 as an opt-in foundation module with a real Cortex requirement, before any existing module requires the new path. It may expose contracts and persistence while controller/environment capability checks report unavailable.
- [ ] Only then add optimizer requirements to `cortex/sentinel`, `cortex/integrations/fabriq`, `cortex/api` and `cortex/extension`. Never use an untagged future require as a shortcut around CI proxy validation.

## Task 7: Bridge Sentinel and Cortex without feedback loops

**Depends on:** tasks 1, 2, 6 and published bootstrap tags.

**Files:** Create `C/sentinel/optimization/{adapter.go,mapping.go,adapter_test.go}`, `C/sentinel/recorded.go`, `recorded_test.go`, `auto_eval.go`, `auto_eval_test.go`. Modify `C/sentinel/{adapter.go,plugin.go,options.go,go.mod,go.sum}` and root run-provenance hook tests as needed.

**Interfaces:** `optimization.NewEvaluator(eng *sentinelengine.Engine, mapping ScopeMapper, meter opt.Meter) (opt.Evaluator, error)` in the adapter package. `ScopeMapper` exposes `Map(ctx context.Context, scope cortex.Scope) (context.Context, error)`. `ConvertExecution(record opt.ExecutionRecord) (sentinelengine.RecordedCase, error)` preserves output/trace identities and metrics. Automatic evaluation calls Sentinel's `EnqueueRecorded`; the host starts Sentinel's `WorkRecorded`. `CompletionReconciler.Work(ctx context.Context) error` scans explicitly bound Cortex runs and persists its per-binding cursor through Sentinel's source-cursor store.

- [ ] Write `TestEvaluatorPreservesOriginalDurationAndUnknownCost`, `TestMapperRejectsMissingAndConflictingScope`, `TestCompletionHookDoesNotRerunAgent`, `TestTrialAndEvaluationPurposeDoNotReenqueue` and `TestAutoEvalRetryDeduplicates`.
- [ ] Use a counting agent runner and assert it is called exactly once for a source run and zero times for recorded scoring/retry. Assert no Sentinel read on failed mapping.
- [ ] Run `GOWORK=off go test ./...` in `C/sentinel`, confirm red, implement the adapter and explicit suite bindings. Preserve `NewAgentClient` for existing callers.
- [ ] Use Sentinel's durable recorded-job queue. Add `TestCompletionReconcilerRecoversLostHook` and `TestReconcileCursorDoesNotSkipFailedEnqueue`, including delayed completion persistence and restart. Use a scope/run/binding digest idempotency key and overlapping scans; report unavailable without an explicit binding, persistent queue and worker. Do not make task 8 depend on global auto-eval.
- [ ] Run `C-module(sentinel)` and `C-module(extension)` compatibility checks. Commit `feat(sentinel): evaluate captured Cortex runs`.

## Task 8: Execute and reconcile isolated trials

**Depends on:** tasks 1-5 and 7.

**Files:** Create `C/optimization/executor.go`, `environment.go` implementation helpers, `trial.go`, `trial_test.go`, `isolation_test.go`, `reconcile_test.go`, `C/optimization/optimizationtest/{environment.go,controller.go,evaluator.go}`. Add real host test setup in `C/inttest/optimization_environment_test.go`.

**Interfaces:** `Executor.RunTrial(ctx context.Context, req TrialRequest) (*Trial, error)` and `Executor.Reconcile(ctx context.Context, trialID id.ID) (*Trial, error)` use the environment ports. `TrialRequest` pins case/environment/version, attempt ID, fresh session ID, deadline, identity and budget. `AttemptStatus` distinguishes running, completed with receipt, terminated and unknown.

Implement `EnvironmentFactory.Recover(ctx, receipt)` and `.Stop(ctx, receipt, fence)` from the spec. `TrialEnvironment.Receipt()` exposes the durable provider/environment ID and capabilities before execution. Persist it before the call. Idempotent `Open` must recover an ambiguous provisioning result using the same attempt ID. A new process can reattach, inspect and stop the original environment without access to its old Go object.

- [ ] Write `TestTrialsHaveFreshSessionsAndResetState`, `TestTrialCannotAccessProductionTools`, `TestToolRevisionMismatchRefuses`, `TestCrashAfterEffectDoesNotReplay`, `TestLateReceiptReconcilesOnce`, `TestRecoverEnvironmentInNewProcess`, `TestOpenAfterLostResponseDoesNotProvisionTwice`, `TestUnconfirmedStopPreventsReplacement` and `TestUnexpectedSuspensionBlocksTrial`.
- [ ] Assert two repetitions start with the same frozen memory/environment state but different session IDs. Assert an unknown external effect cannot be reissued by a replacement worker without termination/idempotency evidence.
- [ ] Run focused tests to red. Implement persist-before-call attempt creation, captured output/artifacts, cleanup, cancellation and receipt reconciliation.
- [ ] Add an isolated local filesystem environment only to the test harness, with a disposable task-owned directory, fake side-effect tool and explicit cleanup. Production environments are injected by hosts; no invented sandbox security claim.
- [ ] Run `C-module(optimization)` and focused real-backend `C-inttest`. Commit `feat(optimization): execute isolated recoverable trials`.

## Task 9: Implement sequential propose, probe and compose

**Depends on:** task 8.

**Files:** Create `C/optimization/search.go`, `sequential.go`, `candidate.go`, `selection.go`, `controller_llm.go`, `search_test.go`, `selection_test.go`, `controller_llm_test.go` and controller prompt fixtures under `C/optimization/testdata/controllers/`.

**Interfaces:** `Controller` from spec 5.3. `NewLLMController(client llm.Client, spec ControllerSpec, meter Meter) (Controller, error)` constructs a frozen structured-output controller. `Select(ctx context.Context, req SelectionRequest) (Decision, error)` applies host constraints to evaluator comparisons. `SelectionRequest` contains immutable objective/policy plus evidence references, not arbitrary model-reported scores.

- [ ] Write `TestSequentialRunsGeneralDomainAndComposition`, `TestCompositionMustBeExecuted`, `TestInvalidCandidateConsumesUsageNotTrial`, `TestNoImprovementRetainsBaseline`, `TestHoldoutNotVisibleToController` and `TestSourceDriftInvalidatesRecommendation`.
- [ ] Use deterministic fake controllers with an instrumented input log. Assert private expected answers and validation outputs never appear in proposal/reflection input. Assert the composed candidate receives its own probes.
- [ ] Run `GOWORK=off go test ./... -run 'TestSequential|TestComposition|TestNoImprovement|TestHoldout'` before implementation and confirm failure.
- [ ] Implement the persisted phase sequence from spec 8. Strictly decode model JSON, reject unknown fields, cap proposal size, retain rejected candidates and count every controller attempt in the ledger.
- [ ] Pin structured controller prompts and versions. On malformed output allow one configured retry, charged to the same experiment; then mark the phase failed with evidence. No unbounded repair loop.
- [ ] Run `C-module(optimization)`. Commit `feat(optimization): add sequential harness refinement`.

## Task 10: Add reproducible validation and evidence review

**Depends on:** tasks 6-9.

**Files:** Create `C/optimization/validation.go`, `evidence.go`, `comparison.go`, corresponding tests and fixture manifests under `C/optimization/testdata/experiments/`. Extend store projections for comparison DTOs.

**Interfaces:** `BuildComparison(ctx context.Context, experimentID id.ID) (ComparisonView, error)` and `FinalizeDecision(ctx context.Context, experimentID id.ID, expectedRevision int64) (*Decision, error)`. `ComparisonView` includes both quality and completeness, per-case paired results, hard criteria, confidence bounds, usage quality and evidence links.

- [ ] Write tests for the exact default thresholds in spec 8: 19 validation cases is exploratory; 20 cases and three repetitions is eligible for the confidence calculation; 0.019 mean gain cannot recommend; protected dimension regression above 0.01 rejects; zero lower bound is inconclusive.
- [ ] Write `TestValidationPlanMutationRefused`, `TestMissingRequiredTrialIsInconclusive`, `TestUnknownCostDisablesCostGate` and `TestValidationReuseRequiresNewAllocation`. Retry the same case hashes under new experiment/harness/task-family identifiers and assert refusal after exposure; a retry of the identical frozen finalist within its original allocation remains permitted.
- [ ] Run focused tests red, then implement the final baseline/finalist validation stage and deterministic decision record. Preserve the original baseline and decline to overstate contrastive task-local results.
- [ ] Run `C-module(optimization)` and Sentinel comparison tests. Commit `feat(optimization): persist validation and adoption evidence`.

## Task 11: Activate versions and roll back safely

**Depends on:** task 10.

**Files:** Create `C/optimization/activation.go`, `active_run.go`, `compatibility.go`, their tests and store activation conformance extensions.

**Interfaces:** Produce `Activate`, `Rollback`, `RunActive` from spec 5.3 and `Deactivate(ctx context.Context, req DeactivationRequest) (*Activation, error)`. Add `WithActiveEngine(*engine.Engine) Option` to bind the production execution path. Requests carry agent/channel/scope, expected generation, decision/version and reason. `Compatibility.Check(ctx context.Context, version HarnessVersion) (CompatibilityReport, error)` compares current source/policy revisions.

- [ ] Write `TestActivationRequiresPermissionAndCompleteEvidence`, `TestActivationRequiresExactQualifyingDecision`, `TestExploratoryAndFailedDecisionsCannotActivate`, `TestConcurrentActivationConflicts`, `TestRunActivePinsVersionAtStart`, `TestResumeIgnoresLaterActivation`, `TestRollbackChecksCurrentPolicy`, `TestOrdinaryRunAgentUnaffected` and `TestStaleAgentEditBlocksActiveRun`. A recommend decision for a different candidate, agent, scope, allocation or policy must fail; no force flag exists.
- [ ] Add `TestRunActiveUsesOnlyBoundProductionEngine` and `TestRunActiveUnavailableWithoutEngine`. Run red, then implement activation pointer transactions and opt-in execution. `RunActive` calls the bound production engine's `RunPrepared`, never the trial environment factory. Deactivation is a new history event, not deletion.
- [ ] Race-test two reviewer commands, repeat requests with the same key and restart between activation and execution. Assert exactly one current generation and matching audit events.
- [ ] Run `C-module(optimization)` and all backend activation tests. Commit `feat(optimization): activate and roll back harness versions`.

## Task 12: Publish and recall Fabriq optimization lessons

**Depends on:** tasks 5, 9-10 and published optimization bootstrap.

**Files:** Create `C/integrations/fabriq/optimization/{adapter.go,schema.go,mapping.go,publisher.go,adapter_test.go,publisher_test.go}`, `C/optimization/reflection.go`, `publication.go` and tests. Modify `C/integrations/fabriq/plugin.go`, its tests and module pins. Add `C/inttest/optimization_learning_test.go`. In `F`, add focused regression tests to `core/agent/write_test.go`, `recall_test.go` and relevant Postgres command/recall integration tests; edit generic implementation only if those tests expose a missing guarantee.

**Interfaces:** `NewLessonStore(toolkit Toolkit, fabric query.Fabric, mapper ScopeMapper, cfg Config) (opt.LessonStore, error)`, `LessonSpec(name string) registry.EntitySpec`, `LessonWritePolicy(name string) agent.WritePolicy`. `Toolkit` is the narrow Remember/Recall interface. `ScopeMapper` resolves tenant plus exact scope filters from authenticated host context. `Publisher.Work(ctx context.Context) error` drains optimization publication rows.

- [ ] Prove the pinned Fabriq version supports explicit-ID create, expected version zero, authoritative row lookup and recall filters. Write `TestPublishAmbiguousCreateReadsDigest`, `TestConflictingDigestRefused`, `TestRecallRejectsForeignExpiredRevokedEvidence`, `TestTrialRunsDoNotWriteOrdinaryMemory` and `TestLearningOutageKeepsDecision`.
- [ ] Write upstream generic regression tests for duplicate create conflict, stale expected version, filtered hydration and revoked/deleted search hits. No Cortex types enter Fabriq core.
- [ ] Run tests red where new behavior is required. Implement the lesson entity with filterable columns, provision its schema in the integration fixture, and add durable publication retries. Failed publication remains visible and does not fail a completed experiment.
- [ ] Use explicit ID create plus digest reconciliation. Repeated upsert is not treated as exactly-once event publication. Return incomplete recall warnings explicitly; do not silently treat missing required knowledge as an empty successful search.
- [ ] Write `TestRevocationLocallyBlocksStaleRemoteRecall`, `TestRevocationPausesFrozenConsumer`, `TestRevokedLessonInvalidatesActivation` and `TestPublishRetryCannotUndoRevocation`. Persist local revocation plus remote outbox atomically; retry remote propagation independently. Paused affected experiments require a new experiment, not resumption with a modified lesson set.
- [ ] Run `F-all` when upstream code/tests change, `C-module(integrations/fabriq)`, `C-module(optimization)` and `C-inttest` with a real non-superuser tenant context.
- [ ] Commit upstream changes separately as `test(agent): pin guarded lesson storage guarantees` or an appropriate fix subject. Release fixes before repinning. Commit Cortex work as `feat(optimization): retain evidence-backed Fabriq lessons`.

## Task 13: Add bounded parallel and task-local refinement

**Depends on:** tasks 8-12.

**Files:** Create `C/optimization/parallel.go`, `task.go`, `parallel_test.go`, `task_test.go`; extend worker phase handling and capability reporting.

**Interfaces:** Produce `OptimizeTask(ctx context.Context, spec TaskSpec) (*TaskResult, error)`. `TaskSpec` carries baseline, one task, evaluation criteria, isolation capability, artifact adoption policy and budget. `TaskResult` contains the selected completed trial's output/artifacts, evidence class, decision and experiment ID. `AdoptArtifacts(ctx context.Context, req ArtifactAdoptionRequest) (ArtifactAdoptionReceipt, error)` is an explicit host adapter operation, outside automatic trial selection.

- [ ] Write `TestParallelOrderIndependentEvidence`, `TestParallelReservationsBoundConcurrency`, `TestTaskReturnsChosenOutputWithoutRerun`, `TestTaskRejectsNonRepeatableEnvironment`, `TestTaskCannotActivateGlobalVersion` and `TestArtifactAdoptionNeedsHostAuthorization`.
- [ ] Run red, then implement bounded scheduling and contrastive selection with no expected-answer dependency. All trial outcomes, including failed and blocked probes, remain in the evidence bundle.
- [ ] Assert the default one-round task mode, hard total budget and caller deadline. Do not allow optimization to trigger from its own trial or controller provenance.
- [ ] Run `C-module(optimization)` and a persistent integration test for crash/reconnect of parallel work. Commit `feat(optimization): add parallel and task-local refinement`.

## Task 14: Version and evaluate the improvement controller

**Depends on:** tasks 10, 12-13.

**Files:** Create `C/optimization/improvement.go`, `improvement_test.go`, `testdata/improvement/`; extend version, experiment and lesson projections.

**Interfaces:** `CreateImprovementExperiment(ctx context.Context, spec ImprovementSpec) (*Experiment, error)` compares controller versions over an ordered frozen task stream. `ImprovementSpec` has baseline/controller candidate IDs, disjoint discovery/validation task sets and the shared aggregate budget. Execution harness versions are outputs of each controller's sub-experiments, all charged to the parent ledger.

Extend `ValidationAllocation` with a parent-owned paired roster: frozen controller arm versions, permitted child experiment IDs and their case assignments. Both arms share this allocation; an unrelated child, changed arm or later experiment cannot. Freeze each child's selected execution harness before its validation dispatch. Withhold all validation results and reflections until the paired evaluation finishes; learning across earlier discovery tasks stays isolated per arm.

- [ ] Write `TestControllerFrozenDuringExperiment`, `TestLaterTaskReadsOnlyEarlierLessons`, `TestControllerValidationWithholdsTaskAnswers`, `TestImprovementCannotSpawnAnotherMetaLevel` and `TestParentBudgetIncludesChildTrials`.
- [ ] Add store conformance tests `TestPairedControllerArmsShareFrozenAllocation`, `TestValidationRejectsUnrelatedChildOrChangedArm` and `TestControllerValidationFeedbackWaitsForBothArms`. Restart the parent mid-comparison and prove the same roster resumes without opening holdout reuse to other experiments.
- [ ] Run red. Implement one permitted meta level with explicit parent experiment links, isolated lesson libraries per controller arm and controller version adoption through the same review policy.
- [ ] Keep evidence-supported lesson adoption distinct from controller-prompt changes. Neither can alter host policy, private evaluation definitions or model provider credentials.
- [ ] Run `C-module(optimization)`. Commit `feat(optimization): evaluate versioned improvement controllers`.

## Task 15: Support approved orchestration presets

**Depends on:** tasks 1, 8, 10 and 13.

**Files:** Create `C/optimization/orchestration.go`, `orchestration_test.go`; add prepared orchestration execution in `C/engine/prepared_orchestration.go`, tests and root orchestration seams where required.

**Interfaces:** `OrchestrationPreset` contains immutable strategy/config, participant prepared versions, allowed delegation instructions and host limits. `PrepareOrchestration(ctx context.Context, configID id.OrchestrationConfigID) (*PreparedOrchestration, error)` and `RunPreparedOrchestration(ctx context.Context, p *PreparedOrchestration, input string, opts PreparedRunOptions) (*orchestration.Run, error)` preserve the existing orchestration strategies. The optimizer's trial record can link either an agent run or an orchestration run explicitly.

- [ ] Write `TestPresetPinsEveryParticipant`, `TestChildRunsShareBudgetAndPurpose`, `TestDelegationCannotExpandToolsOrScope`, `TestUnapprovedPresetRefused`, `TestOrchestrationFailureKeepsPartialEvidence` and a real parallel-strategy trace conversion test.
- [ ] Run red, then implement approved preset selection and guidance edits. Child sessions and environments are isolated, and A2A peer discovery is restricted to the experiment roster. No generated graph or arbitrary external peer access is supported.
- [ ] Run `C-root`, `C-module(optimization)`, orchestration strategy tests and adapter trace tests. Commit `feat(optimization): refine approved orchestration presets`.

## Task 16: Expose authenticated API and dashboard contracts

**Depends on:** tasks 11-15.

**Files:** Create `C/api/optimization_handler.go`, tests; modify `C/api/{api.go,go.mod,go.sum}`. Create `C/extension/contract/optimization.go`, `optimization_catalog.go`, `optimization_test.go`, `optimization_permissions_test.go`; modify `contract.go`, `manifest.yaml`, `runtime.go`, `contract_test.go`, extension options/lifecycle and module pins.

**Interfaces:** Add `WithOptimization(*optimization.Optimizer)` to API options and Forge extension options, and `Optimization *optimization.Optimizer` to contract dependencies. Implement every query/command in spec 13 with shared DTOs. Authorizer actions map one-to-one to read/manage/run/activate/artifacts permissions. The optional evaluation catalog resolver enforces its own mapped Sentinel access.

- [ ] Write table-driven HTTP/contract tests for every intent, permission, malformed scope, foreign ID, unavailable dependency, stale revision, duplicate idempotency key and cursor boundary.
- [ ] Pin `meta.invalidates` as exact names. For example activation invalidates `optimization.activations.list`, `optimization.versions.list`, `optimization.versions.detail`, `optimization.experiments.detail`, `optimization.capabilities` and `optimization.events.list`.
- [ ] Run focused tests red, then implement thin service handlers. Use existing command envelope CSRF/idempotency; never execute a long experiment inside an HTTP request.
- [ ] Expose production activation routing readiness, trial environment readiness, pricing quality, Sentinel/Fabriq mapping and publication state in capabilities. Missing integration must not become an empty catalog or an enabled Start button.
- [ ] Run `C-module(api)`, `C-module(extension)` and real HTTP checks against persistent storage. Commit `feat(contract): expose Cortex optimization workflows`.

## Task 17: Build Cortex optimization review pages

**Depends on:** task 16.

**Files:** Create `D/packages/plugin-cortex/src/pages/optimization/{list,new,detail,candidate,trial,version}.tsx`, `src/optimization/{types,schemas}.ts`, focused shared components under `src/optimization/`, and tests under `test/optimization/`. Modify plugin `src/index.tsx`. If a reusable diff component is needed, add it in kit and migrate existing callers without introducing plugin-to-plugin imports.

**Interfaces:** Routes and intents match spec 13. Use `PluginPageProps` for route params, `useQuery`/`useCommand`, kit dialogs/tables and scope-relative `PluginLink`. Comparison DTO fields retain known/unknown usage and completeness; no frontend recomputation of adoption policy.

Use shared `usePoll` for active-query refetch, preserving visibility handling. Resume event reads from the durable cursor and honor command invalidations; add refresh/reconnect coverage without component-local polling timers.

- [ ] Write rendered workflow tests for create/preflight/start, no qualifying candidate, incomplete evidence, unknown cost, activation conflict, rollback denial, publication retry and scope changes.
- [ ] Run tests red, then implement compact pages. Keep phase, budget, review alert and next action above the working table. Use left-aligned illustrated `ZeroState` for empty/filtered results; failed requests and unavailable integrations have their own states.
- [ ] Test activation error inside the open dialog, double-click prevention, focus restoration, keyboard table access and route mounting with production-shaped params. A fixture throwing `ContractError` must drive the actual failure path.
- [ ] Add lazy routes for diff and chart views; verify build output contains separate chunks. Do not add a tour affordance because no shared onboarding provider was verified.
- [ ] Run `D-plugins`; if kit/runtime changes, run `D-shared`. Commit `feat(cortex): add optimization review dashboard`.

## Task 18: Connect Sentinel evidence and persistent browser fixtures

**Depends on:** tasks 16-17.

**Files:** Modify `S/extension/contract/handlers_runs.go` or the current run handler owning provenance, manifest and tests; modify `D/packages/plugin-sentinel/src/pages/{run-detail,result-detail}.tsx`, types and tests. Create `D/packages/fixture-server/cortex-optimization-fixtures.mjs` and its HTTP verification script; wire into the existing fixture server. Add a persistent demo setup under `C/inttest/testdata/optimization-demo/` and an integration executable in the inttest module for local browser qualification.

**Interfaces:** Sentinel exposes recorded-source provenance and authorized origin references. The host resolves cross-extension navigation; a Cortex page never addresses Sentinel intents through its own scoped client. Fixture handlers implement the same DTOs and state transitions as task 16.

- [ ] Write `TestRecordedOriginAuthorization`, frontend inaccessible-origin tests, HTTP fixture write/read tests, and refresh/reconnect tests against the persistent backend.
- [ ] Run red, then add origin links and provenance labels. A missing Sentinel plugin renders local captured evidence with an unavailable external-link explanation.
- [ ] Verify fixture start changes experiment state, activation changes the next active read and rollback creates another generation. Run every new intent over HTTP.
- [ ] Start the persistent Go backend with SQLite, deterministic labelled providers and the real contract dispatcher. Walk create/start/review/activate/RunActive/rollback, restart the backend and verify state survives.
- [ ] Run `S-all` for changed Go, `D-plugins`, `D-shared` when fixture/runtime tests require it, and browser flows at 1440x900 and 390x844. Commit each repository's coherent change separately.

## Task 19: Retention, audit, operations and user documentation

**Depends on:** tasks 11-18.

**Files:** Create `C/optimization/retention.go`, `audit.go`, `metrics.go`, tests, `C/docs/content/docs/optimization/{index,experiments,evaluation,learning,activation,operations}.mdx`, matching navigation metadata and examples. Update `C/README.md`, `AGENTS.md`, `MIGRATION.md`, Sentinel recorded-evaluation docs and Fabriq integration docs where behavior changed.

**Interfaces:** `Retention.Sweep(ctx context.Context, policy RetentionPolicy) (RetentionReport, error)` honors active evidence and lesson revocation. `EventSink.Record(ctx context.Context, event Event) error` forwards durable optimization events; forwarding failure is retried and does not erase the original event. Metrics use bounded dimensions defined in spec 12.

- [ ] Write `TestRetentionProtectsActiveEvidence`, `TestDeletionRevokesDependentLesson`, `TestAuditForwardRetry`, `TestExportRequiresArtifactPermission` and log-redaction tests with sentinel secret values.
- [ ] Run red, then implement 30-day raw-artifact default, indefinite metadata retention, explicit deletion status and redacted export. No automatic cleanup of another task's processes or paths.
- [ ] Document host setup with the actual final APIs, all required adapters, pricing limitations, scope mappings, failure recovery and capability reporting. Clearly distinguish fixture verification from live quality results.
- [ ] Run `C-module(optimization)`, applicable adapter tests and `Docs` for changed sites. Review prose with rex-voice and humanizer. Commit `docs(optimization): document operation and verification` with code split into its own focused commit if appropriate.

## Task 20: Qualify the full system

**Depends on:** tasks 1-19.

**Files:** Create `C/inttest/optimization_e2e_test.go`, `optimization_recovery_test.go`, `optimization_scope_test.go`, `optimization_live_test.go` with explicit live-provider opt-in; add reproducible task manifests and an acceptance record under `C/docs/verification/optimization/`. Extend dashboard browser tests in its established harness.

**Interfaces:** Tests consume only public APIs and authenticated contracts. Live-provider configuration comes from host credentials; no credentials or raw sensitive traces enter fixtures or git.

- [ ] Run deterministic end-to-end trials with real stores and Fabriq command/recall, including both successful refinement and retained baseline. Assert linked Cortex, Sentinel and lesson identities.
- [ ] Inject crashes at reservation, external call, artifact write, evaluation persistence, activation CAS and publication. Restart separate workers and prove no unacknowledged effect is blindly replayed.
- [ ] Test two tenants and two projects within one tenant. Attempt foreign run/evaluation/lesson/artifact access, including same-count result sets and absent scope mappings.
- [ ] Exercise a live provider for executor, proposer, judge and reflector. Record actual model revisions, prompts by digest, task manifest, repetitions, usage quality, latency, confidence interval and final decision. With missing credentials, record this gate as blocked, not passed.
- [ ] Run root tests/build, every touched nested module's race tests/build, all store conformance/integration suites, `C-inttest`, `S-all`, `S-stores`, relevant `F-all`, `D-shared` and Fumadocs checks. Run gosec/govulncheck as CI requires.
- [ ] Run browser desktop/narrow denial/error/retry/refresh tests on the persistent backend. Verify the tour button is absent, external links are authorized and no page labels incomplete evidence as validated.
- [ ] Write an acceptance table with implemented, unit-tested, real-store-tested, browser-verified and live-model-verified columns. Commit `test(optimization): qualify integrated refinement workflows`.

## Task 21: Complete module release and consumer verification

**Depends on:** bootstrap portions run before task 7; final qualification depends on task 20.

**Files:** Modify `C/.github/workflows/{ci.yml,release.yml}`, `scripts/pin-submodules.sh`, add `scripts/pin-submodules_test.sh` or an equivalent shell-test fixture, module dependency pins, `updatemod.sh` only if discovery misses the module, release/install docs and dependency automation. Change upstream workflow files only where a new generic API requires their own normal release.

**Interfaces:** Published module list includes `optimization` alongside `api`, `sentinel`, `extension` and `integrations/fabriq`. Pin all intra-Cortex requirements in every published module. New adapters' local replaces point only within Cortex. The root tag and submodule pin-commit behavior remain explicit.

- [ ] Write a temporary-fixture test of the pin script that verifies all five modules, preserves intended local replaces, rejects placeholders and checks the optimizer's own root requirement. The test must not mutate real module files.
- [ ] Run workflow/static checks and bootstrap consumer tests before a release dispatch. Verify `go list -deps` keeps optional integrations out of root and optimizer core imports.
- [ ] Release upstream generic Sentinel/Fabriq changes first, then root foundation, optimization bootstrap and adapter consumers as described above. Choose real versions from current tags. Require green CI before each authorized dispatch.
- [ ] In a throwaway module outside the checkout, `go get` each published module at its actual tag through `proxy.golang.org`, compile a small host wiring all adapters and run a deterministic experiment. This probe must have no local replace directives.
- [ ] Verify every relevant CI job, not only an aggregate status. Record tag/commit differences for root and submodules and the consumer probe output.
- [ ] Commit final pins/docs after lint/format/checks. Update the acceptance record with remaining live qualification limits. Do not call the feature ready while any required integration remains a stub.

## Coverage map

| Design section                           | Implemented by              |
| ---------------------------------------- | --------------------------- |
| 1-4: scope, ownership, gaps, defaults    | 1-2, 6, 21                  |
| 5-6: contracts and immutable edits       | 1-4, 7, 9                   |
| 7: lifecycle and recovery                | 3-5, 8, 20                  |
| 8: search and selection                  | 9-10, 13-15                 |
| 9: budgets, scope and isolation          | 3-5, 7-8, 12, 16, 20        |
| 10: Sentinel                             | 6-7, 10, 18                 |
| 11: Fabriq and learning                  | 12, 14, 19-20               |
| 12: activation, retention and operations | 3-5, 11, 19                 |
| 13: API and dashboard                    | 16-18                       |
| 14: qualification                        | all task tests, 20          |
| 15: release                              | foundation checkpoints, 21  |
| 16: proposed choices                     | design review before task 1 |

## Planning review and handoff

Before implementation, review the proposed defaults in design section 16. The plan can be revised without writing product code. Once implementation is requested, use the phase gates to keep each slice independently reviewable and preserve the dependency release order.

The design's independent builder check and this plan's coverage/type/step review are document reviews. They are not evidence that any runtime behavior, integration or test described above already exists.
