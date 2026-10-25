# Cortex stage 3: planning and teamwork implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Follow the roadmap's ownership and execution-method rules.

**Goal:** Execute durable plans through accountable agents, recover safely from failures, and accept team results only when their dependencies are verified.

**Architecture:** Add a versioned task DAG and a durable scheduler around existing orchestration and A2A execution. Persist claims, fences, effects and shared budget reservations. Preserve the current strategies through adapters so planning does not become a second agent runtime.

**Tech Stack:** Go 1.26.0, existing A2A/remote transports, Grove capability stores, verification/evidence/event ports from Stage 2, Forge contracts and React dashboard.

**Spec:** [Shared architecture](../specs/2026-10-18-cortex-assured-execution-design.md). [Roadmap and common checks](2026-10-18-cortex-assured-execution.md).

## Global constraints

All roadmap constraints apply. G2 is required for production task acceptance. Plans may change only through validated versioned proposals. Dependencies consume current verified artifacts. Child authority and aggregate budgets cannot exceed the parent's grant. No universal exactly-once claim for external effects.

## Review focus

- Two workers claim the same task and one continues after lease expiry: S3.2.
- A remote peer returns plausible output without the promised artifact/evidence: S3.3.
- A non-idempotent tool succeeds but the result write is lost: S3.4.
- Concurrent children spend the same remaining budget: S3.5.
- Replanning removes a failed requirement or invalidates already completed dependencies: S3.1/S3.5.

## S3.1: Versioned task graphs and acceptance-aware plans

Depends on F0/S2.1. Owner: planning domain implementer.

Files: create `planning/{plan,task,validate,revision,store}.go`, `planning/{validate,revision}_test.go`, `store/storetest/planning.go`; persistence owner creates backend `planning.go` and additive migrations. Add `orchestration/plan_adapter.go` after pure package tests pass.

Interfaces: spec's `Validate`, `Ready`, `ApplyRevision`, `Plan` and `Task`. `planning.Store` exposes typed `CreatePlan`, `GetPlan`, `ListPlans`, `CommitRevision` and `ReadTasks`; immutable plan revision commits use expected revision and scoped identity. Store acceptance-contract refs, artifact constraints and source provenance, not only prose tasks.

- [ ] Write `TestPlanCycleRejected`, `TestUnknownDependencyRejected`, `TestDuplicateTaskRejected`, `TestPlanCannotDropRequiredCriterion`, `TestReadyRequiresVerifiedDependencies`, `TestRevisionConflictPreservesCurrentPlan`, and `TestChangedArtifactInvalidatesDependents`.
- [ ] Run `GOWORK=off go test ./planning -count=1`; confirm failures before implementation. Add property tests for DAG order, stable serialization and revision replay.
- [ ] Implement bounded graph size/depth, typed task/resource/effect requirements, scope consistency, artifact ownership and acceptance coverage checks. A plan proposal may refine tasks but cannot weaken the host acceptance contract, resource grants or budget ceilings.
- [ ] Store immutable revisions with actor, reason, previous digest and affected task set. Retain completed artifacts but mark downstream assumptions stale when dependencies change. Require explicit policy for reusing prior evidence.
- [ ] Adapt sequential/parallel/hierarchical strategies to task graphs where compatible. Keep router/debate/blackboard available as registered task execution strategies. Reuse existing agent runner and S1.5 parser validation; do not introduce another ReAct loop.
- [ ] Run backend conformance and existing orchestration suites. Apply common checks and commit `feat(planning): persist versioned plans with verified dependencies`.

S3.1 progress: the pure graph/revision/readiness foundation is implemented. `Ready` accepts an explicit observation time and returns malformed-state errors; `ApplyRevision` accepts host graph limits. Immutable artifact producers bind the plan digest and task ID. Immutable revision persistence and conformance now pass on SQLite, PostgreSQL and MongoDB. Typed effect/source manifests and pure sequential/parallel/hierarchical adapters are implemented. Exact task-contract projections now preserve assigned checks through verification, submissions, reviews and readiness without requiring unrelated task outputs to pass the same job. Task-state integration, execution-strategy registration and live source capture remain open, so the complete packet above stays unchecked. See the shared progress log for test evidence and host trust boundaries.

## S3.2: Durable task scheduler and fenced ownership

Depends on S3.1, S3.3 protocol fixture and S3.4 effect contract. Owner: coordination implementer; persistence and runtime owners integrate shared stores/engine.

Files: create `coordination/{scheduler,claim,lease,recovery,store}.go`, tests, `engine/planning.go`, `engine/planning_test.go`; modify orchestration run integration and sweeper lifecycle; backend `coordination.go`, conformance and migrations through persistence owner.

Interfaces: spec's `Scheduler.Tick(ctx, planID) error`; store operations `ClaimReadyTask`, `RenewClaim`, `CommitTaskTransition`, `ExpireClaims` use expected revision/fencing token. Each operation updates task state and event/outbox atomically. New workers obtain current identity through a host resolver; persisted principal references are not bearer credentials.

- [ ] Write `TestOnlyOneWorkerClaimsTask`, `TestExpiredFenceCannotCommit`, `TestLeaseRecoveryAfterRestart`, `TestFailedDependencyBlocksChildren`, `TestParentCancelPropagates`, `TestWaitingTaskConsumesNoWorkerSlot`, and `TestRevokedWorkerGrantStopsResume`.
- [ ] Run `GOWORK=off go test ./coordination ./engine -run 'Test(OnlyOne|ExpiredFence|LeaseRecovery|FailedDependency|ParentCancel|WaitingTask|RevokedWorker)' -count=1`. Confirm observable failures.
- [ ] Implement deterministic ready-task ordering, bounded concurrency, claims/renewals, cancellation-aware queue waits and fair per-scope admission. Make clock and worker identity injectable. Do not hold database locks across an agent/tool call.
- [ ] Persist assignment/run/effect relationships before dispatch. Recovery reconciles active execution receipts before retrying; stale workers cannot commit task success. Waiting human/peer/verification states release execution capacity without losing deadlines or reservations.
- [ ] Integrate with existing sweeper and host lifecycle. Shutdown stops new claims, drains bounded work, preserves resumable state and cancels owned jobs. A crashed process does not own an immortal lease.
- [ ] Run race/conformance tests on all backends and kill/restart a real worker at claim, dispatch and completion boundaries. Apply common checks and commit `feat(coordination): schedule durable tasks with fenced claims`.

S3.2 progress: durable ownership reservations, renewal, immutable history,
replan fencing and dispatch-intent persistence are implemented on the shared
transaction capability. Expired unused reservations release; ambiguous dispatches
enter recovery and cannot be reassigned. Ownership and shared capacity can now be
reserved together, with budget checks on renewal and dispatch intent. Verified
task acceptance now atomically completes ownership and retains the receipt with
a metadata outbox intent. `ClaimReadyTask` now reloads the complete accepted
prerequisite chain and atomically reserves ownership, capacity and event intent.
An explicit host admitter fences current requests and authorizes workers,
allocations and prerequisites before storage, including retries.
`DispatchReadyTask` now rechecks prerequisites, fences an active parent, validates
the accepted assignment and current claim, then retains dispatch intent and the
working assignment together. `NewTaskDispatchAdmitter` applies current host
authorization under explicit guards and validates the returned receipt, including
on historical retries. `CreateTaskRun` atomically creates the reserved child,
required capture policy and metadata receipt after revalidating live ownership,
the parent, assignment, budget and prerequisites. The creation receipt contains
no duplicate private input. `NewTaskRunAdmitter` now guards creation with explicit
host fencing and authorization ports and validates the original receipt on every
retry. `Engine.PrepareTaskRun` now builds the exact creation request using the
shared prepared-input path and reserved run identity. It preserves scanning,
private history, capture policy and input evidence, with no execution side
effects. The runtime entry points below consume that request under guarded
admission and per-call authority. Concrete host grant services, scheduler
dispatch, cancellation after intent, recovery settlement and event delivery
remain open. `Scheduler.Tick` now provides bounded local admission around the
guarded claim store, with fresh state loading, deterministic task selection and
fair scope queues. `Engine.NewTaskWorker` connects fresh admission to guarded
dispatch and the prepared runtime. Optional `WithTaskScheduling` now integrates
bounded, scoped discovery with engine and Forge lifecycle handling. It drains
callbacks before shutdown notification, preserves started state on stop timeout,
and blocks restart while draining. Optional `WithClaimRecovery` now owns scoped
expiry discovery and bounded storage pages through the same lifecycle loop. It
retains uncertain dispatches for reconciliation and cancels both services before
drainage. Host resolution, provider/effect reconciliation and installed worker
crash/restart qualification remain open. A SQLite subprocess matrix now kills
an actual task worker at held-claim, committed-dispatch, pending-provider-call and
completed-local-run boundaries. A fresh parent connection runs lifecycle expiry,
checks immutable history and retained call slots, and rejects historical admission
as permission to execute. The fixture uses a controlled lease clock and a fake
provider. Installed adapters, remote workers and late external receipts still
need qualification.

Task-bound provider and tool calls now require an explicit `TaskCallGuard` lease.
The lease binds the exact private invocation, lasts through stream consumption,
and cancels on expiry or revocation. The shared tool-dispatch path also covers
approved resume calls. Host writer fencing, installed lease-service qualification,
and shared usage settlement remain integration requirements. Provider and local
tool calls now reserve a durable logical slot under the lease and retain normal
local-return observations. Replay, ambiguous writes and incomplete streams do not
grant reinvocation. These observations do not settle shared usage or qualify
provider recovery. `execution.NewProviderReconciler` now queries original model
calls through a read-only host adapter, requires current authorization and proof
authentication under an explicit host commit fence, and retains immutable terminal
receipts in all three stores. Replays reauthenticate saved proof without another
provider query. Receipt usage distinguishes known tokens/money from unknown
charges. Late local disagreements preserve both records and fail recovery.
Killed-worker SQLite tests cover pending and locally completed calls followed by
fixture receipt recovery and storage reopen. Concrete provider/routing proof
adapters, installed discovery qualification, claim adoption and shared settlement
remain open; receipt persistence alone does not authorize continuation.
`Engine.NewRunCallSealer` now supplies a guarded permanent journal barrier. All
three stores serialize new call admission with closure on a shared durable anchor.
Acknowledged closure makes call membership stable, including for empty and legacy
journals, while late returns and provider proof remain writable. The engine test
checks that a delayed task worker cannot reach its provider after closure.
Rollback, lost commit acknowledgment and cancellation after commit are covered by
real backend transactions. All deployed writers must participate; closure does
not prove external completion or settle a recovering claim.
`ProviderReconciler.ReconcilePage` now scans bounded sealed-journal pages with
explicit current discovery authorization. It validates the whole page before
provider queries, reuses authenticated single-call recovery and reports terminal,
unresolved and tool calls separately. Errors preserve earlier proof but return no
cursor. Scope, seal and authority are rechecked before acknowledging a page.
`WithProviderRecovery` now runs authorized plan discovery, bounded claim pages
and sealed provider recovery pages through the shared lifecycle service. It skips
active or unsealed runs, isolates run-specific failures so later claims progress,
and drains with scheduling and claim expiry before shutdown notification. Restart
reuses retained proof. Engine/Forge stop-timeout tests and killed-worker SQLite
tests cover this wiring. Installed discovery/provider qualification, guarded
adoption and shared usage settlement remain open.

`RunPreparedTask` and `StreamPreparedTask` now admit the saved request and execute
its captured inputs through the original prepared snapshot and ReAct loop.
They validate current preparation policy and revisions, authorize input reads,
check the persisted child/intent and require a confirmed fresh receipt. Historical
or ambiguous starts return a reconciliation error without invoking a provider.
Host authority and ownership remain guarded by the admission and per-call ports.
`TaskScheduler` now invokes an explicit claimed-task worker only for a validated
fresh admission. Queued and owned work is cancellation-aware; waiting workers
return their local slots without settling shared reservations. Claim expiry bounds
the worker context. The engine adapter preserves fresh dispatch and child creation
before using the existing journaled execution and verification paths. Installed
host qualification and durable recovery remain open.

`RenewTaskExecution` now provides atomic renewal for a task-bound child. It fences
current parent and child revisions, assignment, capture provenance, prerequisites
and shared capacity. The renewed claim and metadata event intent commit together;
retries retain the original receipt with `Fresh: false`. `NewTaskClaimRenewer`
reuses the host authority guard for every attempt. Optional scheduler renewal now
uses a watchdog that cancels at the last confirmed expiry. The engine activates
it only after confirming fresh child creation. It rejects ambiguous or historical
renewal acknowledgements, drains in-flight renewal on worker return and shutdown,
and preserves the separate expiry of each provider/tool authorization lease.
The SQLite scheduler-to-engine test runs beyond its original ownership expiry
with confirmed durable renewal. Host services and live call-lease qualification
remain open.


## S3.3: Structured handoffs and independent review

Depends on S2.1 and S3.1 contract. Owner: A2A/remote protocol implementer.

Files: create `coordination/{handoff,review,authority}.go`, tests; `a2a/handoff.go`, `a2a/handoff_test.go`; update existing envelope/content adapters, `engine/a2a_tools.go`, `a2aremote/{mapping,types,service}.go` and remote tests through their owners. Generated protobuf files are regenerated only when a legitimate source contract requires it; do not edit vendored A2A spec types for local fields.

Interfaces: versioned structured handoff content contains assignment/task/plan IDs, revisions, exact scope, actor identities, attenuated grant, deadline, artifact refs and acceptance contract. Handoff lifecycle is proposed, accepted, working, blocked, submitted, reviewed, rejected or cancelled. Submitting an answer does not mark a task succeeded.

- [ ] Write `TestHandoffPreservesScopeAndDeadline`, `TestChildCannotEscalateAuthority`, `TestUnknownHandoffVersionRejected`, `TestPeerSuccessWithoutEvidenceRejected`, `TestReviewerCannotSelfApproveRestrictedTask`, `TestPeerInjectionDoesNotChangePolicy`, and `TestLegacyTextMessageStillSupported`.
- [ ] Run A2A/coordination tests and `GOWORK=off go test -race -count=1 ./...` in `a2aremote` and `a2aremote/grpcbind`; establish failures for new content type.
- [ ] Implement typed handoffs over existing durable A2A envelopes and delivery deduplication. Reuse conversation hop limits and cancellation. A remote identity maps through the host resolver; peer-supplied scope or grant cannot override local authority.
- [ ] Add producer/reviewer separation where policy requires it. Reviewers receive artifact/evidence views and their own criteria, with independent task context and no effect tools by default. Same-model agreement is not a verifier pass; required checks still use the verification contract.
- [ ] Retain contradictions as explicit findings. A coordinator can request additional evidence or human adjudication; majority voting cannot override deterministic failure, missing evidence or authority denial.
- [ ] Run in-process and real HTTP/SSE loopback workflows, malformed/duplicate/late replies, cancellation, deadline expiry and transport restart. Apply common checks and commit `feat(a2a): carry accountable task handoffs and review evidence`.

S3.3 progress: sealed handoff proposals, current host authority validation,
restricted review grants and bounded host admission are implemented. A2A uses a
leaf-package handler port, with no fallback to ordinary agent execution. Remote
mapping preserves parent runs and exact deadlines; HTTP/SSE loopback tests use
SQLite messages/plans and fixture authorization/acceptance callbacks. The built-in
stores retain progress and evidence-bound submissions with immutable history,
exact-scope reads, idempotent replay and plan/run/evidence transactions. Prepared
execution retains handoff lineage through resume. Immutable review observations
now bind separate reviewer execution to submitted evidence, preserve contradictions
and leave verifier verdicts unchanged. Task acceptance now checks committed
verification and the complete prerequisite chain under the plan/run/evidence
transaction. An explicit host acceptor now keeps authorization and current
request checks inside a host-supplied fence for the full prerequisite chain.
Review completion/adjudication, concrete grant and scheduler-ownership fences,
stronger producer independence and installed qualification remain open.
The packet and its acceptance checklist remain incomplete.

## S3.4: Effect reconciliation and artifact conflict handling

Depends on S1.2/S2.1. Owner: execution/effects implementer; runtime integrator wires tools.

Files: create `execution/{effect,reconcile,compensation,store}.go`, tests; `engine/effects.go`, `engine/effects_test.go`; `coordination/conflict.go`, tests; backend `effects.go` and conformance tests through persistence owner.

Interfaces: `execution.EffectAdapter` exposes `Execute(ctx, EffectRequest) (EffectReceipt, error)` and `Reconcile(ctx, EffectRequest) (EffectStatus, error)`. Requests include stable logical operation key, args digest, authority ref and attempt ID. Receipt records external ID/version and known/applied/not-applied/unknown outcome. Existing trusted handlers opt into effect metadata through an adapter; no implicit retry promise.

- [ ] Write `TestCrashAfterExternalEffectReconciles`, `TestUnknownNonIdempotentEffectIsNotRetried`, `TestSameKeyDifferentArgumentsRejected`, `TestIdempotentRetryReturnsSameReceipt`, `TestCompensationRequiresAuthorization`, `TestConflictingArtifactWritesRequireResolution`, and `TestStaleWorkerCannotExport`.
- [ ] Run `GOWORK=off go test ./execution ./coordination ./engine -run 'Test(CrashAfter|UnknownNonIdempotent|SameKey|IdempotentRetry|Compensation|ConflictingArtifact|StaleWorker)' -count=1`; confirm failures.
- [ ] Persist planned effects before dispatch. Use a stable key across transport retries and worker replacement. If the provider supports idempotency, pass the key through. Otherwise probe authoritative state via Reconcile; unknown results stop for reconciliation or human review.
- [ ] Implement effect classes read-only, idempotent mutation, non-idempotent mutation and compensatable mutation. Retries use bounded backoff with jitter and deadline; an agent cannot reclassify a tool. Explicit compensation records a new authorized effect and preserves the original history.
- [ ] Give artifact updates expected versions and explicit ownership. Conflicting writers produce separate candidates or a conflict record; no silent last-write-wins on accepted artifacts. Shared blackboard observations include provenance and revision.
- [ ] Run injected network ambiguity, duplicate delivery, broker restart and process-kill tests against an instrumented external-effect fixture. Count actual side effects, not tool return messages. Apply common checks and commit `feat(execution): reconcile ambiguous effects before retrying tasks`.

S3.4 progress: optional effect persistence and an explicit host executor now
reserve operations before dispatch and recover through read-only reconciliation.
All three built-in stores share the immutable reservation/receipt implementation.
The executor pins trusted handler metadata and rechecks current authority. It
never automatically repeats a mutation. A killed-process fixture checks that a
completed external file write is reconciled without a second write. Explicit engine tool handlers now use that executor, and uncertainty stops normal,
streamed and approved execution. Bounded exact-scope operation discovery and reconciliation are implemented.
Governed continuation, retry policy execution, compensation, artifact conflict handling, scheduler fences
and durable events remain open; the full packet stays unchecked.

Guarded tool effects retain the exact original `RunCallRequest` in their existing
operation journal. Built-in admission validates that call and serializes with
local completion and the permanent run seal. Recovery uses the full retained
request; legacy unbound effects cannot be relabelled. This supplies durable
call-to-effect identity. `RecoverSealedPage` now discovers bounded effect pages under current exact-run
authority and rechecks the retained seal before returning a cursor. It reuses the
existing reconciliation path and preserves legacy unbound records. Claim
adoption and shared usage settlement remain open. Lifecycle effect recovery
supervision is implemented through `engine.WithEffectRecovery`; it shares
claim traversal, fairness and shutdown with provider recovery. The subprocess
fixture covers recovery after an external write and before the tool reply.
Installed host ports still need qualification.

## S3.5: Shared budgets and bounded replanning

Depends on S3.1/S3.2/S3.4. Owner: budget/planning implementer; integrator serializes shared runtime edits.

Files: create `execution/{reservation,budget_store}.go`, tests; `planning/{replan,progress}.go`, tests; `coordination/budget.go`; modify existing prepared budget hooks and scheduler. Add backend reservation operations/conformance through persistence owner.

Interfaces: `Reserve(ctx, execution.ReservationRequest) (execution.Reservation, error)` and `Settle(ctx, execution.Settlement) error` are store operations with idempotent keys and parent accounting. `planning.ProposeReplan(snapshot ProgressSnapshot, policy ReplanPolicy) ReplanDecision` returns a bounded reason/action proposal; `ApplyRevision` remains the only graph mutation path.

- [ ] Write `TestConcurrentChildrenCannotOverspend`, `TestSettlementIdempotent`, `TestUnknownUsageRetainsReservation`, `TestDeadlineIncludesPausedTime`, `TestRepeatedFailureTriggersBoundedReplan`, `TestNoProgressStopsAtReplanLimit`, and `TestReplanCannotWeakenAcceptance`.
- [ ] Run `GOWORK=off go test ./execution ./planning ./coordination -count=1`; confirm failures with concurrent reservation tests under the race detector.
- [ ] Reserve tokens/cost/tool calls/fan-out before dispatch across the plan's budget tree. Refund only known unused capacity. Keep integer currency units and pricing version; unavailable cost estimates disable monetary guarantee and surface that limitation.
- [ ] Detect repeated equivalent failures, contradictory prerequisite evidence, excessive idle/waiting, deadline pressure and no new artifacts/checks. Use bounded reason-specific retry/replan policies, not unbounded reflection loops. Default replan cap is two per plan, host-configurable within a finite ceiling.
- [ ] Validate model-produced plan revisions using the same deterministic graph/authority/acceptance validators. Preserve completed work, cancel superseded assignments, reconcile outstanding effects and invalidate dependent reports before continuing.
- [ ] Run multi-worker stress, crash/recovery, budget exhaustion during verification and cancellation tests. Apply common checks and commit `feat(planning): enforce shared budgets and bounded replanning`.

S3.5 progress: shared parent/child accounting, idempotent reserve/settle,
conservative unknown usage, overrun propagation, integer monetary bounds and
replan-safe account retention are implemented. Atomic claim/capacity acquisition
and budget checks on claim renewal/dispatch are implemented. The deterministic bounded replan proposal policy now consumes the existing shared
account, binds progress observations to the current graph and rejects a cap wider
than its configured ceiling. Unknown charges and plan revisions cannot reset the
allowance. Runtime accounting, provider usage qualification, durable proposal
admission and automatic replan execution remain open.

`SnapshotTaskUsage` now reads the current recovering/completed claim, its exact
sealed call journal and the complete verifier-job history under one plan/run/journal
transaction. It rejects stale ownership, changed journals and oversized or malformed
history. `SummarizeTaskUsage` combines provider and verifier observations, includes
failed work, preserves known lower bounds when other usage is missing and marks
saturated dimensions as inexact. It does not settle a reservation. Closing future
verifier admission, authenticating final billing and committing settlement against
this exact evidence remain required before capacity can be refunded.

`TaskUsageCloser` now permanently closes verifier admission for an accepted task
with terminal verifier jobs. Explicit host authorization and fencing precede an
atomic snapshot comparison and durable closure write. New enqueue, claim, renewal
and metered-call admission check the closure under the run guard. Retries recover
the original receipt after uncertain commits. This closes the local admission gap;
external quiescence, billing authentication and guarded budget settlement remain
open. Closure never refunds capacity.

`Settlement.Minimum` now retains authenticated partial overruns even when other
receipts are missing. Unknown dimensions charge the larger of allocation and
observed minimum. `Settlement.Overflow` preserves saturation within a single leaf
and blocks new admission throughout its account. These accounting fields do not
authenticate billing or authorize final settlement; the guarded task operation
and its host proof integration remain open.

`TaskUsageSettler` now gates settlement through explicit host billing authority
and a synchronous fence. Storage verifies the original budgeted claim binding,
accepted ownership, admission closure and exact current combined usage under
plan/run/journal guards. It derives the charge and writes both accounting and the
guarded receipt in the same transaction, reusing ordinary budget settlement.
Historical retries retain the first receipt and require current authorization.
Live billing authentication, enforced bounds for unknown external work, concrete
host integration and live qualification remain open.

Explicit closure can now bind a failed, cancelled or incomplete run revision
while its claim remains recovering. Fresh closure and settlement recheck the
original dispatch and terminal run under the existing guards. Accounting does
not accept a result, release ownership or grant retry authority. This path uses
the same authenticated billing and conservative unknown-usage rules as accepted
work. Managed unsuccessful-run discovery, completed-but-unaccepted disposition
and governed claim retirement remain open.

`TaskSettlementService` and `engine.WithTaskSettlement` connect authorized
discovery of completed, already sealed claims to admission closure, proof
resolution and guarded accounting. They reuse the recovery scheduler and engine
lifecycle, retain pending debt, and retry committed receipts with current
authorization after a lost acknowledgment or restart. Attempt timeouts bound
cooperative callbacks; shutdown retains ownership until every callback returns.
Store tests exercise this path on SQLite, PostgreSQL and MongoDB with fixture
authority. Optional `TaskCallSealFence` and `TaskCallSealAuthorizer` ports now let
the service seal ordinary accepted runs before settlement. `SealTaskCalls`
compares the current completed claim, acceptance and committed run under the
plan/run/journal guards, then reuses the existing journal seal write. Rollback,
lost acknowledgment and concurrent late admission tests cover all three stores.
Settlement now pages immutable acceptances within each discovered plan, so a
replacement claim cannot hide earlier accepted debt. Historical accounting and
ordinary call sealing check the original claim revision, acceptance and dispatch;
replacement ownership and its reserved capacity remain unchanged. Host discovery
must still include completed/cancelled plans with debt. Concrete billing adapters,
unaccepted terminal-task accounting and deployment qualification remain open.

## S3.6: Team review UI and recovery qualification

Depends on S3.1 through S3.5. Owner: integration coordinator and UI owner.

Files: `api/planning_handler.go`, `extension/contract/planning.go`, `coordination.go`, manifest and registration tests; dashboard `src/pages/planning.tsx`, `src/task-review.tsx`, existing operations pages/types; audit/metrics hooks, `inttest/coordination_recovery_test.go`, execution/orchestration docs.

Interfaces: plan/task queries and authorized propose/accept-revision, cancel, reassign, retry, reconcile and resolve-conflict commands. Reassignment changes claims with expected revision; it cannot duplicate an active external effect. Commands carry idempotency and invalidate their dependent views.

- [ ] Write contract/UI tests for ownership, stale revisions, blocked dependencies, failed review, exhausted budgets, denied reassignment and unavailable transport. Assert task success requires verified output and plan success requires every required task/criterion.
- [ ] Run failing tests, then implement a compact task table with owner, status, dependency, budget and next action. Offer a graph for dependency inspection without replacing the working list. Put blockers and review summaries above it; use shared ZeroState and accessible navigation.
- [ ] Run installed workflows with three agents: planner, producer and independent reviewer. Exercise failure/replan, contradictory evidence, human approval, external effect ambiguity, cancellation and a remote worker restart.
- [ ] Kill workers at each durable boundary and inspect claims, effect receipts, reservations, event order and exported artifacts after recovery. Test a late stale worker response after reassignment and a backend outage during completion.
- [ ] Check desktop/390px, long plans, keyboard navigation, live updates and denial/retry paths. Record end-to-end latency/cost and fairness across two scopes.
- [ ] Run G3 matrix and all touched remote/nested suites. Apply common checks and commit `feat(dashboard): expose durable plans and accountable teamwork`.

## G3 acceptance and handoff

- [ ] Plan revisions and task ownership survive restart with stale-worker fencing.
- [ ] Required dependencies consume current verified artifacts; failed workers cannot disappear into a successful synthesis.
- [ ] Unknown external outcomes enter reconciliation, with zero observed duplicate non-idempotent effects in the failure corpus.
- [ ] Parent budgets and grants constrain every worker, reviewer and replan.
- [ ] In-process and remote installed flows preserve scope, status and evidence; all UI/runtime gates pass.

Deliver plan/assignment fixtures, crash-window evidence, operation-key contracts and measured coordination overhead. Stage 4 can now choose adaptive actions through these controlled operations.


S3.2 recovery snapshot progress: the built-in stores expose a bounded atomic
snapshot of sealed call and effect journals. Terminal provider/effect writers now
share the journal transaction guard with local completions. The digest retains
unresolved and contradictory evidence and pins complete call bindings. Guarded
claim adoption uses the transaction and explicit proof contracts described below.
Reading a snapshot alone cannot accept a task or release its reservation.


Recovery adoption transaction support: SQLite, PostgreSQL and MongoDB now let an
existing plan transaction lock a run journal and compare the complete retained
snapshot. `assurance.LockRunRecoverySnapshot` is internal storage machinery. It
requires a live transaction, keeps the journal lock until the outer commit or
rollback and returns no evidence when the seal or expected digest differs.
Standalone snapshot reads share the same decoding and validation code.

Guarded adoption now uses that primitive to validate the current claim,
assignment, committed result and prerequisite chain. `TaskRecoveryAdopter` requires
separate host recovery fencing and authentication ports. Its proof binds the
original claim, seal and snapshot, with permanent worker/tool quiescence and
terminal provider/effect evidence. The completed claim retains a recovery digest
without renewing its expired lease. Ordinary live-claim acceptance rejects recovery
requests. Later prerequisite use rechecks the journal and current verifier evidence.

Shared storage tests cover transaction rollback, lost acknowledgment, proof expiry
at commit, invocation binding, contradictory and late evidence, and dependency use
after the adoption window. These fixtures do not authenticate real quiescence.
`TaskAdoptionService` and `engine.WithTaskAdoption` now connect authorized
plan discovery, sealed snapshots, proof resolution and guarded adoption. They reuse
recovery traversal and lifecycle ownership, bound each attempt, and preserve
unready claims for later passes. Local engine integration tests exercise the
built-in stores and completed-claim restart behavior. SQLite process-kill tests
now cover death after all adoption writes but before commit, and death after commit
before acknowledgment. A fresh connection and engine resume recovery, retain one
acceptance/outbox identity, and preserve the original lease, fence, run and absent
local provider return. Concrete host proof/fence implementations, other backend
process faults and deployment qualification remain open. Accepted-task usage
settlement uses the guarded accounting and history traversal described above.
