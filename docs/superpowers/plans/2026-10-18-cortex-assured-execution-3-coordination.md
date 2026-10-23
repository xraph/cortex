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

S3.1 progress: the pure graph/revision/readiness foundation is implemented. `Ready` accepts an explicit observation time and returns malformed-state errors; `ApplyRevision` accepts host graph limits. Immutable artifact producers bind the plan digest and task ID. Immutable revision persistence and conformance now pass on SQLite, PostgreSQL and MongoDB. Typed effect/source manifests and pure sequential/parallel/hierarchical adapters are implemented. Task-state integration, execution-strategy registration and live source capture remain open, so the complete packet above stays unchecked. See the shared progress log for test evidence and host trust boundaries.

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
streamed and approved execution. Durable operation discovery, governed continuation,
retry policy execution, compensation, artifact conflict handling, scheduler fences
and durable events remain open; the full packet stays unchecked.

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
