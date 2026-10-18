# Cortex stage 2: evidence and evaluation implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Follow the roadmap's ownership and execution-method rules.

**Goal:** Verify task outcomes against current context, run generated code and authoritative checks in isolation, and publish durable evidence-backed results.

**Architecture:** Add immutable evidence references, acceptance contracts and verifier ports to the root module. Gate engine success through these ports. Sentinel scores captured outputs, an isolated runner executes candidate/test jobs, and the existing contract/API transports expose reports and replayable public events.

**Tech Stack:** Go 1.26.0; Grove backends; Sentinel; Shield; Nexus/Weave adapters; a separate Linux OCI/gVisor sandbox module; Forge and React dashboard.

**Spec:** [Shared architecture](../specs/2026-10-18-cortex-assured-execution-design.md). [Roadmap and common checks](2026-10-18-cortex-assured-execution.md).

## Global constraints

All roadmap constraints apply. G1 is required before enabling runtime gates. Pure verifier, Sentinel and sandbox development may start after F0. Never execute a captured task again just to score it. Generated code, graders and publication authority are separate trust domains. Fixture runners cannot advertise live isolation.

## Review focus

- Artifact or source changes between verification and publication: S2.1/S2.2.
- A judge or state probe times out and produces a plausible partial result: S2.1/S2.3.
- A generated program tampers with test sources, expected answers or grader output: S2.4/S2.5.
- Restart occurs between durable result commit and public event delivery: S2.6.
- Evidence retrieval, replay or download crosses scopes or uses a revoked grant: S2.1/S2.6/S2.7.

## S2.1: Evidence references and acceptance contracts

Depends on F0 and S1.4. Owner: evidence/verifier implementer; persistence owner integrates stores.

Files: create `evidence/{artifact,repository,digest}.go`, `evidence/artifact_test.go`, `verification/{contract,verifier,report,aggregate,freshness,store}.go`, corresponding tests, `store/storetest/verification.go`; add `verification.go` and `evidence.go` to all three backend directories with migrations/model conversion owned centrally.

Interfaces: the spec's `evidence.Repository`, `verification.Verifier`, `Aggregate` and `CheckFreshness`. `verification.Store` exposes `CreateContract`, `GetContract`, `SaveReport`, `GetReport`, and `ListReports` with scoped immutable identifiers; `SaveReport` rejects digest reuse with different content. Define typed request structs in this packet so later callers do not invent argument shapes.

- [ ] Write `TestRequiredCriterionMissingIsInconclusive`, `TestOptionalScoreCannotOverrideRequiredFailure`, `TestArtifactDigestMismatchRejected`, `TestContextRevisionMakesReportStale`, `TestEvidenceDeniedAcrossScope`, `TestRevocationInvalidatesReport`, and `TestLegacyCompletedRemainsUnchecked`.
- [ ] Run `GOWORK=off go test ./evidence ./verification -count=1`, confirming missing implementation then behavioral failures.
- [ ] Implement canonical versioned encodings and content digests, ordered criterion evaluation, exact scope checks, evidence retention/tombstones and per-verifier revision binding. Reject unsupported schemas, duplicate criterion IDs and nonfinite scores. Use a fake clock for freshness; do not trust model-supplied timestamps.
- [ ] Implement all backend capability stores and conformance tests. Evidence bytes have a size limit and sensitivity policy; large artifacts use the injected repository. Digest verification occurs on read, not only on upload. An authorized read rechecks access to referenced resources.
- [ ] Add schema, executable-test, external-state and human-approval verifier adapters with typed registrations. Human verdicts bind actor, decision revision and exact evidence; an expired approval cannot pass a new artifact.
- [ ] Run root package/race tests and backend conformance, then common checks and commit `feat(verification): bind acceptance reports to immutable evidence`.

## S2.2: Gate run success and prevent stale publication

Depends on S1.2/S1.3 and S2.1. Owner: runtime integrator and persistence owner.

Files: create `engine/verification.go`, `engine/verification_test.go`, `engine/result_commit.go`, `engine/result_commit_test.go`, `verification/binding.go`; modify shared finalization/prepared/resume files, `run/` result references and backend atomic commit methods.

Interfaces: produces `WithVerification(v verification.Verifier) Option` and engine result commit request containing expected run revision, candidate digest, contract digest, report IDs and current binding. Store method `CommitVerifiedResult(ctx context.Context, req verification.CommitRequest) (*verification.CommitReceipt, error)` atomically records result decision and durable event/outbox. It consumes a report; it cannot manufacture a passed report.

- [ ] Write `TestNoFinalAnswerBypassesRequiredVerification`, `TestFailedTestPreventsSuccess`, `TestEvidenceChangedBeforeCommitRejected`, `TestRevokedAuthorityBeforeCommitRejected`, `TestReportForOtherRunRejected`, `TestVerificationTimeoutIsInconclusive`, and `TestVerificationResumeDoesNotRepeatToolEffects`.
- [ ] Run `GOWORK=off go test ./engine -run 'Test(NoFinal|FailedTest|EvidenceChanged|RevokedAuthority|ReportFor|Verification)' -count=1`; establish failures.
- [ ] Build the verification request from trusted runtime records. Persist candidate/contract/report bindings and transition to pending verification. On failure allow a bounded repair under the remaining budget, explicit human review, or a committed explanation of incomplete work. None yields `ResultSucceeded` without all required checks passing.
- [ ] Recheck local revisions and authorization atomically at publication. For external mutable state, bind version/ETag and require a conditional effect or immediate freshness recheck; if the provider offers neither, state the observation window and do not claim an atomic guarantee. New artifact/source/policy revisions stale prior reports.
- [ ] Persist verification jobs/leases so restart resumes checks without repeating generation or tools. Separate `RunExecutionFinished` from `ResultCommitted` hooks. Keep legacy `RunCompleted` meaning stable and include verification fields for consumers; learning uses the new qualified hook.
- [ ] Test all sync/stream/prepared/resume paths and backend crash windows. Apply common checks and commit `feat(engine): require current evidence before verified success`.

## S2.3: Score recorded executions through Sentinel

Depends on F0 frozen request contracts and S2.1 for Cortex adapter. Owner: Sentinel integration chat; upstream Sentinel edits belong to its own primary checkout.

Files: in Sentinel, create `engine/frozen_evaluation.go`, `engine/recorded.go`, `engine/recorded_test.go`, `comparison/recorded.go`, `comparison/recorded_test.go`; extend existing scorer runner helpers, evalrun provenance/store models and memory/SQLite/Postgres/Mongo adapters. In Cortex: create `sentinel/verification.go`, `sentinel/recorded.go`, `sentinel/queue.go`, tests; modify `sentinel/plugin.go`, `options.go`, `adapter.go` to share conversion, not behavior.

Interfaces: upstream `Engine.FreezeEvaluation(ctx, FreezeRequest) (*EvaluationPlan, error)`, `Engine.EvaluateRecorded(ctx, RecordedRunRequest) (*RunResult, error)` and `comparison.CompareRecorded(ctx, RecordedComparisonRequest) (*RecordedComparison, error)`. Reconcile with the optimizer owner. Cortex's verifier adapter maps exact authorized scope and frozen criteria to these APIs. Existing target-running API remains supported.

- [ ] Write `TestRecordedEvaluationNeverCallsTarget`, `TestFrozenScorerRevisionRequired`, `TestExecutionErrorSeparateFromJudgeError`, `TestUnknownCostIsNotFree`, `TestRecordedComparisonRejectsMismatchedCases`, `TestAutoEvalDeduplicatesAfterRestart`, and `TestEvaluationPurposeDoesNotRecurse`.
- [ ] Run upstream focused `engine`/`comparison` tests and Cortex Sentinel module tests; confirm the new captured-result path is absent before implementation.
- [ ] Extract reusable scoring/persistence logic from Sentinel's runner. Recorded requests carry captured outputs, trace/artifact refs, original usage/duration and execution outcome, with no target callback. Record judge cost/latency separately. Require explicit suite binding; no name-based guessing.
- [ ] Implement a durable deduplicated completion-job queue using the shared run/event outbox, frozen suite digest and bounded retries. Missing binding is unconfigured, not success. Exclude evaluation/optimization purpose runs; online required verification is independent from optional background auto-evaluation.
- [ ] Validate factual/source checks, rubric judges and human calibration against fixed cases. Unknown scorer, missing expected state, incomplete trace and judge errors remain explicit. Redact sensitive trace fields before crossing service boundaries.
- [ ] Release required generic Sentinel APIs through its workflow, pin the tagged module in Cortex and run `GOWORK=off go test -race -count=1 ./...` inside `sentinel`. Apply both repos' quality gates and commit `feat(sentinel): evaluate captured Cortex results without rerunning tasks`.

## S2.4: Qualified sandbox port, broker and runtime

Depends on F0; S1 safety/scope before deployment. Owner: sandbox chat. Release owner handles module registration.

Files: create root `sandbox/{runner,job,policy,receipt,conformance}.go` and tests. Create separate module `integrations/sandbox/` with `client.go`, `broker.go`, `auth.go`, `oci/runner.go`, `oci/policy.go`, `cmd/cortex-sandboxd/main.go`, `testdata/`, `compatibility.json`, integration tests and README. Add `go.mod`/`go.sum` through release owner. No implementation in root `cmd/cortex`.

Interfaces: spec's `sandbox.Runner`; remote client/broker implement the same contract. `Capabilities` lists actual runtime/isolation/network/limit support and qualification version. The broker rejects jobs requiring unsupported controls. Receipts bind immutable job/image/runtime digests and are authenticated to the trusted host.

- [ ] Write `TestJobPolicyRejectsPrivilegedMount`, `TestSubmitSameKeySameDigestIsIdempotent`, `TestSubmitKeyConflictRejected`, `TestNoHostFallback`, `TestCancelKillsDescendants`, `TestResourceLimitsEnforced`, `TestEgressPolicyAppliesToDNSAndRedirects`, `TestArtifactExportRejectsTraversalAndSymlinks`, and `TestTenantCannotInspectOtherJob`.
- [ ] Run root port tests and sandbox-module unit tests; then write Linux integration tests that fail when required isolation capabilities are absent. Missing runtime means an explicitly blocked qualification gate, not a green skipped result.
- [ ] Implement an authenticated broker with exact scope/principal mapping, bounded request/output size, pinned image allowlist, command argv handling, read-only inputs and disposable writable scratch. Do not expose the container socket or broker credentials to jobs. Broker identity can submit jobs only within its own host grant.
- [ ] Implement OCI execution using a qualified `runsc` runtime. Enforce dropped capabilities, no privileged mode, controlled mounts, process/memory/CPU/disk/wall limits and external egress denial. Protect host and cloud metadata access. Apply network constraints outside the sandbox and test DNS rebinding/redirect/private-range behavior within authorized local fixtures.
- [ ] Persist job receipt/status before execution; reconcile ambiguous starts through Inspect. Idempotent cancel/collect and a crash-safe reaper clean orphan workloads. Inspect exported files by descriptor/path policy, reject devices/unsafe links, cap archive expansion and preserve immutable digests.
- [ ] Pin tested runtime, image and client-library versions in `compatibility.json` with host/kernel requirements and test evidence. Run Linux integration probes in CI plus deployed broker restart/resource tests. Add module build/test/lint/release coverage. Commit `feat(sandbox): isolate evaluation jobs behind an authenticated broker` after common checks.

## S2.5: Isolated candidate execution and protected grading

Depends on S2.1/S2.4 and S2.3 for semantic scoring. Owner: evaluation harness implementer.

Files: create `verification/sandbox_test_verifier.go`, `verification/state_verifier.go`, `verification/evaluation_manifest.go`, related tests; sandbox module `grading/runner.go`, `grading/export.go`, adversarial fixtures; `inttest/assured_evaluation_test.go` under the integration tag.

Interfaces: `sandbox_test_verifier` consumes `sandbox.Runner` and `evidence.Repository` without importing the heavy sandbox adapter. A versioned `EvaluationManifest` names candidate digest, immutable test bundle, fixture dataset, permitted state probes, required checks and expected result schema. Only host-registered manifests may execute commands.

- [ ] Write `TestCandidateCannotModifyAuthoritativeTests`, `TestForgedStdoutPassDoesNotPass`, `TestGraderHasNoProductionCredentials`, `TestEvaluationTargetsExactArtifact`, `TestCorrectOutputWrongTenantFails`, `TestPassingTestsWrongRequirementFails`, and `TestExportRequiresCurrentApproval`.
- [ ] Run `GOWORK=off go test ./verification -count=1`; run sandbox grading integration tests on the qualified Linux worker and record expected failures.
- [ ] Run candidate code and grader in separate disposable jobs. Give the grader a read-only export and protected expected answers; keep assessment orchestration outside both. Keep authoritative grading control outside the candidate process: drive candidate behavior through a bounded process/RPC interface and verify outcomes from a trusted supervisor. Unit tests that import candidate code share its execution risk, so treat their reports as untrusted until independently corroborated; read-only test files alone do not prevent test-runner manipulation. The result parser checks authenticated job status and protected supervisor artifacts, not candidate stdout. Semantic checks receive candidate material as untrusted data and no effect tools.
- [ ] Add contextual checks for objective coverage, source freshness, tenant/resource identity, schema/units, environment revision and external state. Include a correct-but-irrelevant answer case so semantic plausibility cannot stand in for task completion.
- [ ] Export approved artifacts through a separate host command that rechecks digest, destination, permissions and safety. A sandbox result alone cannot deploy, publish or mutate production state. Record observation windows for external state checks.
- [ ] Run real failure, timeout, resource-limit, stale-source, artifact-swap, scanner-outage and replay cases. Apply common checks and commit `feat(verification): run protected contextual evaluation in isolated jobs`.

## S2.6: Durable safe event replay

Depends on S1.3/S2.1/S2.2. Owner: event adapter implementer, with runtime and persistence owners handling shared files.

Files: create `event/{event,store,cursor}.go`, `event/cursor_test.go`, `engine/events.go`, `engine/events_test.go`; backend `events.go` and conformance tests; modify `extension/contract/stream.go`, API streaming handler, `a2aremote/stream.go`, SSE and gRPC mapping tests.

Interfaces: `event.Store` exposes `AppendPublicEvents`, `ReadPublicEvents` and `ReadEventWatermark` using scoped typed requests and stable cursor sequences. Append and result transitions share the S2.2 commit/outbox boundary. Read requests carry cursor, bounded limit and expected schema; expired cursors return an explicit retention gap plus current snapshot reference.

- [ ] Write `TestReplayAfterRestartPreservesSequence`, `TestCommittedResultEventNotLostOnCrash`, `TestPublicReplayHasNoRawCandidate`, `TestReplayReauthorizesSubscriber`, `TestCursorGapIsExplicit`, `TestDuplicateDeliveryDeduplicatesByEventID`, and `TestDisconnectDoesNotDeadlockProducer`.
- [ ] Run root event/engine tests and contract/remote module tests; confirm restart and safety invariants fail with the temporary buffer.
- [ ] Implement append-only public events, atomic outbox publication and retention policy. Persist safe content only. Event IDs provide at-least-once delivery deduplication; do not claim exactly-once network delivery. Slow consumers have bounded buffers and reconnect using cursors.
- [ ] Update contract polling, SSE and supported remote bindings to consume the same event log. Preserve external protocol schemas and expose verification detail through supported metadata/artifact fields. Recheck authorization for long-lived subscribers at policy changes or bounded intervals.
- [ ] Test execution-finished, verification-pending/failed/passed, artifact-created and result-committed ordering, retention gaps and cancellation under load. Run remote gRPC tests explicitly because root CI does not cover them today.
- [ ] Apply common checks and commit `feat(streaming): persist authorized result events for safe replay`.

## S2.7: Installed integration, evidence review and release qualification

Depends on S2.1 through S2.6. Owner: integration coordinator with UI, CI and release owners.

Files: new API verification/evidence handlers; `extension/contract/verification.go`, `evidence.go`, manifest registrations/tests; dashboard `src/pages/verification.tsx`, `src/evidence.tsx`, existing run/chat pages and shared types; `docs/content/docs/execution/verification.mdx`, `sandboxing.mdx`; CI Linux sandbox job and nested-module coverage.

Interfaces: query intents for acceptance contract, reports, evidence metadata, sandbox status and runtime capabilities; commands for authorized retry, review and export. Each command declares invalidations, idempotency and capability. Content downloads resolve authorized refs server-side; no arbitrary URL fetch or public object-store URL bypass.

- [ ] Write contract tests for reader/operator/reviewer roles, two scopes, unavailable verifier/broker, stale report and denied export. Add UI tests that distinguish unchecked, pending, failed, inconclusive and stale; no invented confidence percentage.
- [ ] Run failing tests, then implement compact acceptance summaries above the artifact/result content, expandable evidence, source versions, observation times, errors and next actions. Use shared ZeroState and existing host routing.
- [ ] Wire Nexus model/usage, Shield policy, Weave source provenance, Sentinel recorded scoring, sandbox broker, host identity and audit/metrics in an installed demo. Missing integrations display unavailable with a concrete cause.
- [ ] Run the full journey: task -> candidate -> sandbox/tests -> contextual report -> committed response -> restart -> replay -> revoke source -> stale result -> authorized retry. Demonstrate a failed required criterion and a denied artifact export.
- [ ] Browser-check desktop/390px, focus/keyboard, long evidence lists, slow checks, disconnected streams and permission changes. Verify rendered artifact safety; untrusted HTML must not execute in the dashboard origin.
- [ ] Execute G2 corpus and all affected module/CI gates. Publish upstream dependencies first; release Cortex only after exact-main CI and sandbox qualification pass. Commit `feat(dashboard): expose evidence-backed task verification`.

## G2 acceptance and handoff

- [ ] Required checks gate success in every run path, including resume; stale evidence cannot be committed.
- [ ] Sandbox and grader isolation pass on the pinned Linux runtime, with no skipped required probes.
- [ ] Sentinel recorded scoring performs zero target executions; auto-eval survives restart without recursion or duplicates.
- [ ] Public events and evidence remain scoped and authorized after restart, reconnect and permission revocation.
- [ ] Installed integrations and desktop/narrow UI have evidence receipts; unavailable credentials or runtimes remain explicit blockers.

Handoff includes frozen contracts, environment manifests, evidence/report fixtures, migration compatibility records and the measured G1/G2 latency/cost baseline. Stage 3 can now use verified artifacts as task dependencies.
