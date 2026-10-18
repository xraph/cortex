# Cortex stage 4: adaptive behavior implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Follow the roadmap's ownership and execution-method rules.

**Goal:** Let agents recognize capability limits, learn from qualified evidence and improve behavior through bounded, evaluated changes.

**Architecture:** Build an operational self-model from trusted runtime observations. Route reflection decisions through the existing planner, verifier and authorizer. Store qualified lessons through Fabriq, calibrate reliability from versioned evaluation cohorts, and reuse the optimization workstream for controlled configuration improvement.

**Tech Stack:** Go 1.26.0, cognitive/behavior/persona value objects, Stage 2 evaluation/evidence ports, Stage 3 scheduling/budgets, Sentinel and Fabriq adapters, Forge and React dashboard.

**Spec:** [Shared architecture](../specs/2026-10-18-cortex-assured-execution-design.md). [Roadmap and common checks](2026-10-18-cortex-assured-execution.md).

## Global constraints

All roadmap constraints apply. G3 is required before enabling adaptive execution in production. Model-authored confidence is not a calibrated probability. Reflection cannot change acceptance criteria, privileges or hard limits. Lessons preserve evidence and uncertainty. Proposed improvements do not activate themselves.

## Review focus

- An agent claims a capability its installed tools do not provide: S4.1.
- Reflection repeats criticism without collecting new evidence: S4.2.
- A revoked or cross-tenant lesson remains in a semantic recall result: S4.3.
- A reliability score comes from a small or leaked evaluation cohort: S4.4.
- Activation changes while an older run is paused, or rollback occurs during work: S4.5.

## S4.1: Operational capabilities, assumptions and context management

Depends on S2.1/S3.1/S3.5. Owner: cognitive domain implementer, runtime integrator owns prompt/loop wiring.

Files: create `cognitive/{snapshot,capability,assumption,context,store}.go`, tests; `engine/cognitive.go`, `engine/context_budget.go`, tests; extend `cognitive/cognitive.go` and prompt assembly through existing owners. Capability store conversion and migrations belong to persistence owner.

Interfaces: `cognitive.BuildSnapshot(input SnapshotInput) (Snapshot, error)` combines trusted tool/permission/provider capabilities, task progress, evidence, assumptions and remaining budget. `cognitive.ContextPolicy.Select(input ContextInput) (ContextSelection, error)` returns bounded context with retained provenance. Assumption records carry origin, evidence refs, state and expiry; an agent proposal is an unverified hypothesis.

- [ ] Write `TestCapabilityRequiresInstalledAuthorizedTool`, `TestRevokedGrantRemovesCapability`, `TestAssumptionIsNotPromotedWithoutEvidence`, `TestContradictoryFactsRemainVisible`, `TestContextCompactionRetainsConstraintsAndSources`, and `TestUntrustedMemoryCannotBecomePolicy`.
- [ ] Run `GOWORK=off go test ./cognitive ./engine -run 'Test(Capability|RevokedGrant|Assumption|ContradictoryFacts|ContextCompaction|UntrustedMemory)' -count=1`; confirm failures.
- [ ] Build snapshots from runtime/store observations, never persona claims. Distinguish unavailable capability, denied permission, temporary outage and insufficient evidence. Attach measured reliability references only when a compatible cohort exists.
- [ ] Add token-aware context selection with pinned objective/constraints, acceptance criteria, current plan, unresolved blockers and source refs. Summaries retain source digests and uncertainty; critical truncated information is recorded and triggers retrieval or escalation. Do not silently continue after required conversation-history load fails.
- [ ] Persist short decision summaries and hypothesis transitions with provenance. Do not store or expose hidden chain-of-thought. Recheck recalled information before a side effect or verification decision if its freshness policy requires it.
- [ ] Run long-conversation, restart, revoked-source, contradictory-context and scope tests. Apply common checks and commit `feat(cognitive): track operational capabilities and evidence gaps`.

## S4.2: Bounded reflection and real cognitive transitions

Depends on S4.1 and Stage 3 control operations. Owner: cognitive controller implementer; runtime integrator handles shared loop changes.

Files: create `cognitive/{decision,controller,transition}.go`, tests; `engine/reflection.go`, tests; extend `behavior/behavior.go`, persona cognitive config validation, plugin cognitive event emission and existing metrics/audit hooks.

Interfaces: `cognitive.Decide(snapshot Snapshot, policy Policy) Decision` selects continue, retrieve, verify, replan, delegate, ask or stop. Optional `ReflectionProvider.Propose(ctx context.Context, input ReflectionInput) (Proposal, error)` returns a schema-bound proposal, never executable code. Deterministic validators apply capability, authority, acceptance and budget checks before adoption.

- [ ] Write `TestOnStuckTriggersOneBoundedReflection`, `TestReflectionWithoutNewEvidenceStops`, `TestPhaseTransitionEmitsOnce`, `TestReflectionCannotRaiseBudget`, `TestReflectionCannotRemoveRequiredCheck`, `TestUnsupportedStrategyRejected`, and `TestUserInterventionSupersedesPendingProposal`.
- [ ] Run `GOWORK=off go test ./cognitive ./behavior ./engine -run 'Test(OnStuck|Reflection|PhaseTransition|UnsupportedStrategy|UserIntervention)' -count=1`; capture failures.
- [ ] Implement existing phase transitions (`after_steps`, `on_stuck`, `on_plan_complete`, `on_error`) as observable controller behavior. Register supported strategies explicitly; analytical/reflective/collaborative labels map to actual actions and checks or are rejected as unsupported. Do not promise a separate reasoning algorithm merely because an enum exists.
- [ ] Trigger reflection on conflicting evidence, repeat failure, no progress, changed requirements or approaching limits. Default to at most two reflection calls per run, bounded by parent budget and the plan's replan cap. Use a host-configurable finite ceiling and a reason record.
- [ ] Route retrieval, verification, delegation and replanning through existing controlled services. Ask for human input only when authority, unavailable information or unresolved risk requires it. Preserve an authorized user correction across pending/paused actions using revision checks.
- [ ] Run ablations against the same prepared tasks with reflection disabled/enabled. Check improvement and cost separately; no-progress detection should reduce loops without increasing false success. Apply common checks and commit `feat(cognitive): execute bounded evidence-driven reflection`.

## S4.3: Qualified lessons and governed Fabriq recall

Depends on S2.1/S2.2 and S4.1 data contract. Owner: Fabriq adapter chat. Coordinate lesson schema and outbox ownership with optimization work.

Files: create root `learning/{lesson,eligibility,store}.go`, tests; existing Fabriq module `lessons.go`, `lesson_schema.go`, `lesson_publisher.go`, `lesson_recall.go`, tests; modify `integrations/fabriq/plugin.go`, `memory.go`, `config.go`; add `inttest/qualified_learning_test.go` and host migration fixtures.

Interfaces: `learning.Store` exposes `Publish(ctx, Lesson) (Receipt, error)`, `Recall(ctx, Query) (Page, error)`, `Revoke(ctx, RevokeRequest) error`. Define compatibility mapping to any optimization lesson interface, sharing lower-level schema/provenance checks. Ordinary activity memory remains observation-class; only current passed evidence can support a validated-result lesson.

- [ ] Write `TestUncheckedCompletionIsObservation`, `TestFailedRunCannotBecomeSuccessLesson`, `TestExpiredRevokedLessonExcluded`, `TestRecallFiltersBeforeModelSeesContent`, `TestCrossScopeHydrationRejected`, `TestPublicationRetryDoesNotDuplicate`, and `TestEvaluationRunsDoNotPolluteOrdinaryMemory`.
- [ ] Run `GOWORK=off go test ./learning -count=1`, then module tests from `integrations/fabriq`; confirm failures.
- [ ] Register and physically provision a typed lesson entity through host migrations. Store applicability, domain, exact scope, evidence class, source reports/digests, expiry, revocation, correction and supersession. Verify read/write/schema capabilities at startup; a registered schema without a physical table is unavailable.
- [ ] Publish accepted lessons through a durable outbox committed with acceptance. Use a stable aggregate ID and expected version; on ambiguous response, read authoritative state and compare digest before retry. Fabriq outage leaves a visible pending/failed publication and does not erase run results.
- [ ] Apply scope/domain/expiry/active/version filters before recall reaches the model, then verify them again after hydration. Check provenance authorization and report revocation. Return conflicting lessons with applicability and evidence instead of merging them into certainty. Support authorized deletion with audit tombstones and retention policy.
- [ ] Skip ordinary learning for evaluation, optimization trial and optimization controller purposes. Publish experiment lessons only through the optimization acceptance path. Use shared provenance validation to prevent circular self-citation from becoming evidence.
- [ ] Run real Fabriq/Postgres integration with restart, retries, deny paths, corrected facts and revoked sources. If pinned Fabriq lacks a generic guarantee, add upstream regression/fix/release work and repin; no sibling replace. Commit `feat(memory): recall lessons with verified provenance and expiry` after common checks.

## S4.4: Calibration, task benchmarks and regression evidence

Depends on S2.3 and prepared execution/evidence. Owner: evaluation/calibration chat; Sentinel owns statistical operations.

Files: create Cortex `verification/calibration.go`, tests and `inttest/assurance_benchmark_test.go`; fixture manifests under `inttest/testdata/assurance/`; Sentinel comparison/calibration helpers and tests in its repo; benchmark runner/reporting scripts and docs. Store benchmark artifacts in the injected evidence repository, not committed large output dumps.

Interfaces: versioned `CalibrationProfile` identifies task class, model/provider revision, prompt/tool/controller versions, dataset hash, time window, trial counts, score bins, observed success and uncertainty. `ReliabilityFor(binding, taskClass)` returns compatible measurements or unknown. Sentinel provides deterministic calculation over captured immutable trial results.

- [ ] Write `TestSparseCohortReturnsUnknown`, `TestModelRevisionInvalidatesCalibration`, `TestDiscoveryHoldoutOverlapRejected`, `TestMissingTrialIncludedInFailureAccounting`, `TestConfidenceCannotBypassRequiredCheck`, and `TestBenchmarkSeedReproducesAggregation`.
- [ ] Run focused tests and a deliberately miscalibrated fixture before implementing aggregation. Pin expected Brier score/reliability-bin results from hand-computable examples.
- [ ] Build versioned cohorts covering coding, structured data, research/source claims, external state changes, multi-agent work and long-context correction. Include wrong-scope, stale-source, plausible-but-irrelevant, prompt-injection, interruption and unavailable-service cases. Freeze success criteria and dataset splits before runs.
- [ ] Run repeated trials with fixed prepared inputs and record model variation, environment noise, execution failures, grader errors and cancellations separately. Preserve denominators and missing evidence; do not discard inconvenient trials or tune on the holdout.
- [ ] Report false-success rate, verified task success, selective accuracy versus abstention, calibration error, recovery success, unsafe publication, duplicate effects, p50/p95 latency, total cost and cost per verified result. Unknown usage remains unknown. Include sample counts and intervals, not only percentages.
- [ ] Compare single-agent versus team, verification versus unchecked baseline, reflection on/off and memory on/off to identify which additions help each task class. Calibrate model judges with blinded human review samples and record disagreement/adjudication.
- [ ] Run repeatable fixture CI and a configured live model cohort. Keep live credentials/budget requirements explicit. Apply common checks and commit `test(evaluation): measure assurance and calibration across task cohorts`.

## S4.5: Controlled improvement and activation bridge

Depends on S4.3/S4.4 and the separate optimization module's frozen contracts. Owner: optimization integration owner. This packet consumes the optimizer, not a second search implementation. Prerequisites are the concurrent optimization plan tasks 2 through 5, 9 through 11 and 13 through 15; shared foundation tasks 1, 6, 7, 8 and 12 can be satisfied through this roadmap's mapped packets. See the roadmap's shared-work table.

Files: keep active-version resolution in `optimization/activation.go` and `optimization/run_active.go`, owned by the optimization workstream. Add `cognitive/improvement.go` for proposal metadata only, Cortex Sentinel/Fabriq adapter conformance tests, activation contract/UI tests and deployment qualification fixtures. Final root ownership must match F0.

Interfaces: prepared execution and provenance from S1.2; recorded evaluation from S2.3; lessons from S4.3. Optimization owns harness versions, candidates, experiments, budgets, decisions, activation and rollback. An `ImprovementProposal` references observed failure classes, proposed allowed config patch and evidence; it cannot edit tool code, policy or evaluator definitions.

- [ ] Write `TestImprovementCannotEditPolicyOrGrader`, `TestActivationRequiresAuthorizedDecision`, `TestActivationCASRejectsConcurrentChange`, `TestPausedRunKeepsOriginalVersion`, `TestRollbackAffectsOnlyFutureRuns`, `TestRejectedCandidateCannotPublishValidatedLesson`, and `TestControllerCannotRecursivelyOptimizeItself`.
- [ ] Run integration contract tests against optimizer fixtures first, then its real module when available. If unavailable, mark this packet blocked with the exact prerequisite; fixtures do not complete it.
- [ ] Submit proposals to bounded offline experiments with separate discovery/holdout datasets and fresh isolated trial sessions. Freeze baseline/controller/retrieval/environment versions. Share common evidence, budget and sandbox ports through adapters; do not reexecute live production effects to search for a better answer.
- [ ] Require deterministic safety/required-check passes and a predeclared quality/cost decision policy. Report statistically inconclusive experiments honestly. Use the optimizer's selection/calibration implementation; do not duplicate comparison math here.
- [ ] Activate only through an explicit authorized expected-revision command. New opt-in runs resolve activation once into PreparedAgent; paused/in-flight runs retain their digest. Host config/policy changes can stale an activation. Rollback selects an earlier compatible version and preserves the audit history.
- [ ] Test canary activation, concurrent operators, runtime restart, permission revocation and rollback with real evaluation/learning adapters. If the optimizer is not built yet, its complete experiment/adoption plan remains a prerequisite, not work silently omitted from this program.
- [ ] Apply common checks and commit `feat(cortex): connect qualified improvements to controlled activation`.

## S4.6: Operational review, full acceptance and rollout

Depends on S4.1 through S4.5. Owner: integration/release coordinator with UI owner.

Files: `extension/contract/cognitive.go`, `learning.go`, calibration queries and manifest; dashboard `src/pages/cognitive.tsx`, `learning.tsx`, existing configuration/operations pages; capability/audit/metrics integration; docs for planning, verification, reflection, memory and operations; full acceptance corpus in `inttest/`.

Interfaces: read-only capability/assumption/decision/calibration views; authorized lesson revoke/correct, policy-bound retry and optimization review links. Expose remaining uncertainty and actual available actions. Configuration fields unsupported by the runtime are rejected or visibly unavailable.

- [ ] Write contract/UI tests for unknown reliability, contradicted assumptions, unavailable recall, revoked lessons, bounded reflection, pending improvement and unauthorized activation. Never display a model's self-score as measured confidence.
- [ ] Run failing tests, then build compact capability/uncertainty summaries, chronological decision records, evidence-linked lessons and evaluation cohort details. Keep primary work and next actions visible; use shared ZeroState and registered walkthroughs only if available.
- [ ] Run full installed journeys: initial task -> plan -> team execution -> isolated verification -> publication -> qualified lesson -> later recall -> changed fact -> correction/revocation. Separately demonstrate failed improvement and authorized activation/rollback.
- [ ] Run adversarial and failure-injection matrix across all four stages, including scanner/broker/store/LLM outages, remote peer disconnect, exhausted budgets, revoked grant, stale artifact, hostile test output and worker death.
- [ ] Compare the fixed benchmark baseline and candidates. Record required security corpus results, quality/cost tradeoffs, known limitations and sample uncertainty. A regression in required safety/authorization checks blocks rollout regardless of mean quality improvement.
- [ ] Validate all backends/modules, real remote transports, Linux sandbox and desktop/390px product flows. Execute fixture -> isolated deployment -> limited canary -> wider rollout, preserving rollback and drain procedures.
- [ ] Publish documentation and release through existing workflows after exact-main CI. Apply common checks and commit `feat(cortex): expose governed adaptive execution`.

## G4 acceptance and program completion

- [ ] Declared capabilities and cognitive transitions match observable runtime behavior.
- [ ] Reflection stays within budgets and produces measurable actions or stops; unproductive loops are bounded.
- [ ] Qualified learning survives restart, enforces scope/freshness/revocation and distinguishes observations from validated results.
- [ ] Reliability reports have compatible cohorts, sample counts and uncertainty; sparse evidence remains unknown.
- [ ] Improvement trials use isolated environments, protected evaluators and held-out cases; activation/rollback is authorized and auditable.
- [ ] All G1/G2/G3 regressions remain green; installed integrations and live evaluation evidence are recorded.
- [ ] Operators can disable adaptation, reconcile active effects, drain jobs and inspect historical evidence without weakening safety.

Program completion means all required gates and integrations have evidence. Record credential, deployment, training-data or runtime blockers explicitly; do not substitute fixture results or a stored activation record for live behavior.
