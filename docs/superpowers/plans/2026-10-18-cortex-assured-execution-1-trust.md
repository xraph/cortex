# Cortex stage 1: trustworthy execution implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Follow the roadmap's ownership and execution-method rules.

**Goal:** Make termination, policy enforcement and continuation behavior accurate before adding autonomous planning.

**Architecture:** Introduce shared finalization and publication helpers used by synchronous, streaming and resumed execution. Add explicit compatibility fields and enforce supported scope boundaries. Keep the current authorizer and checkpoint paths.

**Tech Stack:** Go 1.26.0, existing root engine and three Grove stores, Shield/Nexus adapters, Forge contract and A2A adapters.

**Spec:** [Shared architecture](../specs/2026-10-18-cortex-assured-execution-design.md). [Roadmap and common checks](2026-10-18-cortex-assured-execution.md).

## Global constraints

All roadmap constraints apply: primary `main` only, preserve concurrent work, exact staging, DRY/TDD, final format/lint/tests, no heavy root dependency, versioned additive APIs, current authorization on every side effect. No packet can claim verified task success before Stage 2 exists.

## Review focus

- A final tool call uses the last step and the model never supplies an answer: S1.1.
- A run pauses at its budget boundary and resumes after config changes: S1.2.
- Redaction succeeds but original bytes remain in stream/history/logs: S1.3.
- A fourth scope level or ambiguous canonical separator reaches a backend directly: S1.4.
- A manager emits malformed or partial JSON and workers can mutate external state: S1.5.

## S1.1: Correct outcomes and centralize finalization

Depends on F0. Owner: runtime integrator, with persistence owner for model/migration changes.

Files: create `run/outcome.go`, `run/outcome_test.go`, `engine/finalize.go`, `engine/outcome_test.go`; modify `run/run.go`, `engine/react.go`, `engine/resume.go`, backend run conversion/model files and `store/storetest` run conformance cases. Add migration entries through each backend's `migrations.go`.

Interfaces: produces `run.StopReason`, `StateIncomplete`, additive result fields, and engine helper `finalizeRun(ctx context.Context, r *run.Run, outcome run.Outcome) error`. `run.Outcome` contains stop reason, candidate output, finish reason, step/token usage and terminal time. The helper performs durable terminal update before emitting completion. It does not assign a passed verification status.

- [x] Write `TestRunStepLimitIsIncomplete`, `TestRunLengthFinishIsIncomplete`, `TestRunFinalAnswerIsUnchecked`, `TestRunTerminalWriteFailureDoesNotEmitDone` and table-driven sync/stream equivalents. Assert `StateIncomplete` with `step_limit` for tool-only exhaustion, no successful done event on persistence failure, and legacy completed decoding yields unchecked.
- [x] Run `GOWORK=off go test ./run ./engine -run 'TestRun(StepLimit|LengthFinish|FinalAnswer|TerminalWrite)' -count=1`. Confirm the current completion behavior fails the regression assertions.
- [x] Implement shared terminal outcome selection. Respect provider finish reasons, nil/malformed response failures, cancellation and limits. Use a bounded uncancelled context for terminal persistence. A persistence outage returns an error and retains a recoverable incomplete lifecycle, not a fabricated durable success.
- [x] Extend all backend conversions and migrations; historical rows retain original state with missing assurance fields interpreted as unchecked. Add compatibility fixtures for old run JSON and unknown future state/schema.
- [x] Run root `run`/`engine` tests with race detection and store conformance against SQLite, Postgres and Mongo. Test repeated finalization idempotency and completion plugin emission only after durable state.
- [x] Apply common quality/commit protocol. Subject: `fix(engine): distinguish incomplete runs from successful termination`.

## S1.2: Freeze prepared execution and enforce cumulative limits

Depends on S1.1. Owner: runtime integrator. Coordinate this packet with the optimization owner; these APIs are implemented once.

Files: create `engine/prepared.go`, `engine/prepared_run.go`, `engine/execution_context.go`, `engine/prepared_test.go`, `execution/budget.go`, `execution/budget_test.go`; modify `engine/prompt.go`, `engine/options.go`, `engine/react.go`, `engine/resume.go`, `suspension/suspension.go`; persistence owner handles continuation schema fixtures.

Interfaces: produces `PrepareAgent`, `RunPrepared`, `StreamPrepared` and `PreparedRunOptions` from the spec; `execution.Budget` and `execution.Usage` provide finite cumulative limits. `PreparedAgent` is immutable serialized configuration with digest and schema version. `RunOverrides` gains a representation that distinguishes absent tools from an explicit empty allowlist without breaking old callers.

- [x] Write `TestPreparedEmptyToolsDenyAll`, `TestPreparedSourcesCannotMutateMidRun`, `TestResumeRetainsPreparedDigestAndUsage`, `TestResumeRechecksRevokedGrant`, `TestResumeUnknownSchemaRejected`, `TestTotalTokenBudgetAcrossSteps` and `TestUnknownUsageCannotResetBudget`. Assert no tool authorization widening and no step/token reset after pause or restart.
- [x] Run `GOWORK=off go test ./engine ./execution ./suspension -run 'Test(Prepared|Resume|TotalToken|UnknownUsage)' -count=1`. Confirm failures before changing the runtime path.
- [x] Extract one configuration/prompt preparation path used by normal and prepared runs. Record source/tool/model revisions, explicit missing optional inputs and trusted execution purpose. Fail preparation when a required input has no usable snapshot. Avoid temporary edits to agent, overlay or global tool registration.
- [x] Enforce per-run cumulative steps/tokens/deadline before provider/tool dispatch. Preserve per-call provider output limits as a separate setting. Record estimated versus actual usage; model budget estimates do not create hard limits. Stage 3 adds durable reservations across workers.
- [x] Extend continuation schema and backward decoding. Old continuations can retain legacy unchecked behavior only when their saved policy permits it; unsupported required policies fail with a typed compatibility error. Verify pending last-step calls never fabricate a final answer.
- [x] Run engine resume/approval/scope suites and controlled provider-boundary fixtures (this checkout has no Nexus adapter tests; live provider qualification remains S1.6); prove prepared and ordinary paths use identical tool authorization and finalization. Apply common checks and commit `feat(engine): freeze execution inputs and preserve cumulative budgets`.


## S1.3: Enforce safety before public output

Depends on S1.1; S1.2 before final resume wiring. Owner: safety package implementer; runtime integrator owns loop edits.

Files: create `safety/policy.go`, `safety/publication.go`, `safety/publication_test.go`, `engine/publication.go`, `engine/publication_test.go`; modify `safety/safety.go`, `safety/shield/adapter.go`, `engine/react.go`, `engine/options.go`, `engine/prompt.go`, `llm/nexus/adapter.go` only if provider evidence requires it. Keep provider-specific logic out of engine.

Interfaces: consumes the spec's `PublicationGate`; produces `PublicationRequest`, `PublicationDecision` and versioned policy with buffered, checked-segment and explicit-disabled modes. Internal candidate text and public events use different types so accidental publication is visible in review.

- [x] Write `TestStreamBlockedOutputNeverPublishesBytes`, `TestScannerErrorFailsRequiredPolicy`, `TestInputRedactionReachesProvider`, `TestSegmentBoundarySecretHeld`, `TestToolOutputCannotOverridePolicy`, `TestSlowConsumerCancellationCompletes`, and `TestPublicHistoryContainsOnlyApprovedOutput`. Assert original redacted secrets are absent from public events, logs and ordinary conversation history.
- [x] Run `GOWORK=off go test ./safety/... ./engine -run 'Test(StreamBlocked|ScannerError|InputRedaction|SegmentBoundary|ToolOutput|SlowConsumer|PublicHistory)' -count=1` and capture failures.
- [x] Route all output through the publication gate. Buffer by default when scanning is configured; require explicit capability for segment decisions. Honor cancellation with select-aware sends. Bound buffers and tool-output sizes; oversized content fails or uses an explicitly configured truncation policy with a visible marker.
- [x] Apply redactions to actual input messages. Scan newly introduced untrusted tool/retrieval/peer content using provenance. Keep policy and tool authority outside that content. Scan errors, missing required scanners and unknown decisions fail closed; explicit permissive policy emits degraded status.
- [x] Reuse the same helpers on normal, prepared, resume and stream paths. Protect raw candidates through a restricted evidence path; Stage 2 supplies the durable repository. Until configured, do not retain raw sensitive candidates in ordinary step output.
- [x] Run race tests, a malicious output corpus, tool-call delta assembly regressions, approval/resume and installed Shield adapter tests. Test final-response redaction does not claim to undo earlier publication. Commit `fix(safety): gate response publication before exposing content` after common checks.

## S1.4: Enforce supported scope identity everywhere

Depends on F0. Owner: persistence/security boundary owner. Coordinate additions to `errors.go` and API route registrations with their owners.

Files: create `scope_validation.go`, `scope_validation_test.go`, `store/storetest/scope_boundary.go`; modify `scope.go`, backend context/scope helpers, `store/postgres/scope.go`, `store/sqlite/memory.go`, `rescope.go`, `extension/contract/contract.go`, `api` request scope adapter and A2A credential/scope mapping. Locate each current helper before editing; do not add a second resolver.

Interfaces: produces `ValidateScope(s Scope) error`, `ErrInvalidScope`, `ErrUnsupportedScopeDepth`; uses existing `Scope.Covers`, `Canonical`, `ScopeFromContext`. Zero scope remains `ErrNoScope`. Supported depth is one through three; keys are unique, key/value nonempty, canonical separators `/` and `=` rejected until a separately versioned codec exists.

- [ ] Write conformance cases `ScopeDepthFourRejected`, `ScopeDuplicateKeyRejected`, `ScopeSeparatorRejected`, `ExactMutationDoesNotUseAncestorGrant`, and `PrefixListStillWorksWithAuthorization`. Test direct store calls as well as engine/API/remote entry points.
- [ ] Run `GOWORK=off go test ./... -run 'Test.*Scope' -count=1 -timeout 10m` or the targeted package list if Docker suites need their documented configuration. Record expected failures before enforcing checks.
- [ ] Add one shared validation function and call it at storage and execution boundaries. Preserve ordered-prefix list semantics for valid supported scopes. Resource mutations must validate exact stored resource identity plus current authorization; an ancestor context alone is not an automatic grant.
- [ ] Add a read-only inventory/report helper for existing unsupported/ambiguous scopes. Quarantine unsafe records from normal access. Document an operator-authorized migration with identity mapping, dry-run report and audit; never truncate a deep scope silently.
- [ ] Run all backend conformance and rescope tests, plus two principals/two scopes through API, dashboard and A2A. Verify no fallback manufactures a missing scope.
- [ ] Apply common checks and commit `fix(scope): reject unenforceable resource scopes`.

## S1.5: Validate manager plans before dispatch

Depends on F0. Owner: orchestration parser implementer. Avoid engine edits.

Files: create `orchestration/delegation_plan.go`, `orchestration/delegation_plan_test.go`; modify `orchestration/hierarchical.go`, `orchestration/hierarchical_test.go`, `orchestration/config.go` for a bounded repair setting if needed.

Interfaces: produces `parseDelegations(output string, workers []Participant) ([]delegation, error)` and a typed invalid-plan error. One schema-valid response or at most one configured repair attempt is allowed by default. This is a compatibility fix; Stage 3 replaces the task representation with `planning.Plan` through an adapter.

- [ ] Write `TestMalformedPlanDispatchesNoWorkers`, `TestUnknownWorkerRejected`, `TestPartialPlanDoesNotDropRequiredAssignment`, `TestPlanRepairConsumesBudget`, and `TestWorkerFailureCannotYieldUnqualifiedSuccess`. Assert malformed plans never broadcast the original request to all workers.
- [ ] Run `GOWORK=off go test ./orchestration -run 'Test(MalformedPlan|UnknownWorker|PartialPlan|PlanRepair|WorkerFailure)' -count=1` and confirm failure.
- [ ] Replace permissive extraction/fallback with strict structured validation, bounded size/task counts and known worker identity. Repair receives validation errors as data. No side effects run before the complete plan validates.
- [ ] Propagate worker failures and missing outputs into the orchestration result. A synthesis may explain partial work but cannot conceal unresolved required workers.
- [ ] Run all orchestration strategy tests and cancellation/race cases. Apply common checks and commit `fix(orchestration): reject invalid delegation plans before dispatch`.

## S1.6: Integrate status, compatibility and the first release gate

Depends on S1.1 through S1.5. Owner: integration coordinator with separate UI owner.

Files: modify `api/run_handler.go`, `api/requests.go`, `extension/contract/execution.go`, `extension/contract/stream.go`, manifest and registration files, `a2aremote/mapping.go`, `a2aremote/types.go`, `audit_hook/`, `observability/`; dashboard `src/pages/chat.tsx`, `operations.tsx`, `types.ts`, `schema.ts`; add domain/API/contract compatibility tests and `docs/content/docs/execution/` guidance.

Interfaces: expose stop reason, verification status and publication status without changing existing response keys. Map incomplete outcomes to supported remote protocol states with explanatory metadata; never invent a protocol enum. New capabilities describe scanner presence, preparation support and assurance availability truthfully.

- [ ] Write API/contract fixtures for old and new runs, exhausted runs, scanner outage and permission denial. Add UI assertions that unchecked, incomplete and blocked outcomes cannot render as verified success.
- [ ] Run focused tests to confirm the missing status fields and rendering fail, then implement mappings and compact summaries through the existing contract transport.
- [ ] Add audit events for policy failures and incomplete termination with bounded metric labels. Raw prompt/tool content and evidence identifiers must not become metric labels.
- [ ] Run installed workflows: ordinary answer, tool answer, last-step exhaustion, truncated generation, approval/resume, scanner failure, stream disconnect, restart and revoked grant. Inspect durable state and public bytes, not just UI badges.
- [ ] Check desktop and 390px layouts, keyboard use, shared ZeroState, loading/denied/error distinctions and retry. Do not add an unregistered walkthrough.
- [ ] Run G1 matrix, update progress evidence and compatibility notes, then common checks and commit `feat(cortex): expose trustworthy execution outcomes`.

## G1 acceptance and handoff

- [ ] Sync, stream, prepared and resume paths pass the same outcome and safety cases.
- [ ] SQLite/Postgres/Mongo preserve new fields; legacy fixtures stay readable and unchecked.
- [ ] Unsupported scopes are denied at direct store and external entry points; migration inventory is available.
- [ ] No malformed plan starts worker effects; partial failure is visible.
- [ ] Required scanner outage and stream cancellation behave as specified in a live adapter deployment.
- [ ] Exact commit passes root and touched nested-module checks plus installed dashboard validation.

Deliver the commit list, API fixtures, migration notes and evidence receipts to Stage 2. Do not label this stage as contextual result verification; it establishes trustworthy execution and safety semantics.
