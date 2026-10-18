# Cortex assured execution implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Choose the execution method with the user before implementation; this document does not create chats or authorize agents to message other chats.

**Goal:** Deliver trustworthy execution, evidence-backed verification, isolated evaluation, durable teamwork and governed adaptation in four independently gated stages.

**Architecture:** Extend the existing engine through small root-level ports, shared lifecycle policies and optional durable store capabilities. Keep Sentinel scoring, Fabriq recall and sandbox runtimes in adapters. A result is successful only when its acceptance contract passes against current evidence.

**Tech Stack:** Go 1.26.0, Grove SQLite/Postgres/Mongo, existing Nexus/Shield/Weave/Sentinel/Fabriq integrations, Forge contracts and React dashboard; qualified Linux OCI/gVisor workers for untrusted execution.

**Spec:** [Shared architecture and proposed contracts](../specs/2026-10-18-cortex-assured-execution-design.md).

## Global constraints

- Work directly on `main` in each current primary checkout. Do not create branches or worktrees.
- Preserve concurrent edits. Stage exact owned paths; inspect staged content before every commit. Commit and push verified packets; do not merge or tag by hand.
- Run `GOWORK=off make l`, then `GOWORK=off make f` after final code edits, then lint and relevant tests/builds before committing. One owner runs repository-wide formatting while other writers pause.
- Go 1.26.0 is the current module floor. Heavy dependencies live in separate modules. Never commit `go.work` or sibling-repository replacements.
- Apply rex-voice and humanizer embedded mode to repository prose and commit bodies. No attribution trailers or em dashes.
- All requested capabilities need implementation and live verification evidence. Fixtures, mocks and links alone do not close an integration gate.
- Shared dashboard controls remain compact, accessible and responsive. Use shared ZeroState; a walkthrough requires a registered provider, ID and working targets.
- Use DRY shared domain contracts and TDD for behavior changes. Avoid duplicating sync/stream policy, scoring, storage, optimization or transport implementations.

## Review focus

1. A worker performs an external effect and crashes before recording its result: S3.4 must reconcile instead of blindly repeating it.
2. A result was verified and then its source, artifact, permission or policy changed: S2.1/S2.2 must mark it stale and refuse success publication.
3. A scanner fails or a stream consumer disconnects during publication: S1.3/S2.6 must prevent unchecked leakage and unblock cancellation.
4. A suspended run resumes under new policy or an older binary: S1.2/S2.2 must preserve its snapshot and reject unsupported semantics.
5. An evaluator receives malicious candidate content or the agent modifies its own tests: S2.4/S2.5 must preserve grader isolation and authoritative evidence.

Each item has named tests in its owner stage. Add newly discovered failure classes to the relevant packet before implementation.

## Read and execute in this order

| Document | Deliverable | Entry gate |
| --- | --- | --- |
| [Stage 1](2026-10-18-cortex-assured-execution-1-trust.md) | Correct terminal outcomes, enforceable safety and scope boundaries | F0 contracts frozen |
| [Stage 2](2026-10-18-cortex-assured-execution-2-verification.md) | Acceptance gates, evidence, isolated tests, Sentinel and durable streaming | Stage 1 gate; pure port/scorer work can start earlier |
| [Stage 3](2026-10-18-cortex-assured-execution-3-coordination.md) | Durable plans, task ownership, recovery and accountable delegation | Stage 2 evidence and execution contracts stable |
| [Stage 4](2026-10-18-cortex-assured-execution-4-adaptation.md) | Operational self-model, reflection, qualified learning and controlled improvement | Stage 3 lifecycle and Stage 2 evaluation gates |

All checkboxes start unchecked. Completing this planning package does not complete implementation. The default delivery order is sequential stages; the dependency table below permits parallel development within each stage.

## F0: Freeze shared contracts and allocate ownership

Files: create `docs/superpowers/plans/2026-10-18-cortex-assured-execution-progress.md` when implementation begins; update this roadmap/spec only to resolve accepted interface changes. Do not edit the concurrent optimization design without its owner's coordination.

Interfaces: the spec owns result states, evidence refs, verification request/report, prepared execution, budgets, public events, sandbox jobs, plan revisions and handoffs. Stage packets own implementation files. New domain types cannot import `engine`; engine assembles their inputs.

- [ ] Inventory HEAD, status, active work and current package APIs in Cortex, Sentinel, Fabriq and forge-dashboard. Record baseline SHAs and existing failures.
- [ ] Reconcile `PreparedAgent`, `ExecutionProvenance`, `EvaluateRecorded` and lesson schemas with the optimization workstream. Give each shared symbol one owner; consume its commit if already implemented.
- [ ] Write a dependency/API compatibility test skeleton for proposed packages. Pin JSON fixtures for additive public fields and unsupported-version errors before implementing them.
- [ ] Record schema versions, capability names, chosen safety defaults and scope-depth migration behavior. Mark new APIs as proposed until compiled conformance tests exist.
- [ ] Assign one engine integrator and one persistence owner. Allocate the remaining independent packages using the waves below.
- [ ] Commit the contract/progress baseline after document checks. Expected result: every active packet has one owner, prerequisites, owned paths and a concrete validation command.

## Dependency and parallel-chat schedule

A chat owns a work packet, not an entire stage. Parallel development is safe only after shared interface fixtures compile. Do not run two migrations writers or two ReAct writers in the same checkout.

| Wave | Parallel packets | Must wait for |
| --- | --- | --- |
| 0 | F0 only | Current source and work ownership |
| 1 | S1.1 runtime outcome logic; S1.4 scope boundary; S1.5 validated delegation parser | F0; runtime and scope owners coordinate shared `errors.go` edits |
| 2 | S1.2 continuation/preparation; S1.3 publication safety | S1.1; safety owner builds `safety/`, integrator alone wires `engine/` |
| 3 | S1.6 integration/compatibility | All S1 packets |
| 4 | S2.1 evidence/verifier package; S2.3 Sentinel recorded scoring; S2.4 sandbox port/broker | F0 frozen ports and Stage 1; Sentinel work may start in Wave 1 in its own repo |
| 5a | S2.2 engine gate; S2.5 isolated grading | S2.1; S2.3/S2.4 for grading; integrator serializes engine/store wiring |
| 5b | S2.6 durable event adapter | S2.2 atomic result commit |
| 6 | S2.7 installed product/qualification | S2.2 through S2.6 |
| 7 | S3.1 plan domain; S3.4 effects/reconciliation package | Stage 2; pure packages are independent |
| 8a | S3.3 handoff protocol | S3.1 contract committed |
| 8b | S3.2 scheduler | S3.1/S3.3/S3.4; one persistence writer |
| 8c | S3.5 shared budgets/replanning | S3.2 scheduler and reservations seam |
| 9 | S3.6 integration and recovery qualification | All other S3 packets |
| 10a | S4.1 capability model; S4.4 calibration/benchmark cohorts | Stage 3, recorded evaluation and evidence contracts |
| 10b | S4.3 lessons adapter | S4.1 data contract committed |
| 11 | S4.2 reflection/controller; S4.5 improvement activation bridge | S4.1/S4.3/S4.4, optimization seam availability |
| 12 | S4.6 full system qualification and rollout | All other S4 packets |

The safest first parallel batch is runtime outcomes, scope enforcement, and the hierarchical plan parser. Sentinel's generic recorded-scoring work can run in a separate repository after its interface is agreed. Sandbox design can begin early, but no generated program executes until its security gate passes.

## File ownership and integration rules

| Owner | Exclusive shared paths | Other packets submit |
| --- | --- | --- |
| Runtime integrator | `engine/react.go`, `engine/engine.go`, `engine/resume.go`, `engine/options.go`, `engine/prompt.go`, `suspension/suspension.go`, `config.go`, `errors.go`, `run/run.go` | Tests and changes in their new helper package; a list of required wiring calls |
| Persistence owner | `store/store.go`, all backend `models.go`/`migrations.go`, shared scope helpers, `store/scopespy`, new capability-store wiring | Domain interface, conformance tests, migration invariants |
| Contract owner | `extension/contract/manifest.yaml`, `extension/contract/contract.go`, `api/api.go` | New handler files and JSON fixtures; verify actual service file name before editing |
| UI owner | `forge-dashboard/packages/plugin-cortex/src/index.tsx`, `types.ts`, `schema.ts`, `components.tsx`, shared exports | Isolated new page/component files with contract fixtures |
| Release owner | `go.mod`, `go.sum`, CI/release workflows, `scripts/pin-submodules.sh`, module/tag matrix | Dependency and runtime compatibility evidence |

Each owner has only the listed paths, not permission to edit matching directories broadly. A later packet must read actual HEAD before modifying registrations. Never stage a whole directory when another chat has files there.

Use a progress row per packet: `ID | owner | baseline SHA | prerequisites | state | commit | tests | live evidence | blockers`. States are queued, active, implemented, fixture-tested, backend-tested, live-verified, blocked and accepted. Only the coordinator advances a stage gate after checking the evidence. The progress file has one writer.

## TDD and commit protocol

Each packet follows the same steps so the stage documents do not repeat command boilerplate:

1. Write the named regression/contract tests with the stated assertions. Run the focused command and capture the expected behavioral failure. Compile failures are acceptable only while introducing the specified public type; get to a behavioral failure before claiming a regression test.
2. Implement the smallest shared domain behavior and adapter wiring needed to pass. Tests observe public behavior or durable state, not private helper layout.
3. Run focused tests, add the packet's fault/restart/denial cases and run its backend/module checks. Refactor duplication while tests stay green.
4. Acquire the formatting window. Run initial `GOWORK=off make l`, final `GOWORK=off make f`, inspect formatting changes, rerun `GOWORK=off make l`, then relevant tests and `GOWORK=off go build ./...` in each affected module.
5. Inspect branch, working tree and staged diff. Stage only owned files, commit with the packet's conventional subject, and push verified commits. Record failures precisely; do not call a blocked packet accepted.

A packet can contain a small sequence of focused commits when each commit passes its checks. Cross-package interface and implementation changes that cannot compile independently belong in one coordinated commit.

## Validation commands and evidence

Root focused packages are listed per packet. Root release gate:

```sh
GOWORK=off make l
GOWORK=off make f
GOWORK=off make l
CORTEX_TEST_MONGO_IMAGE=mongo:7 GOWORK=off go test -race -count=1 -v -timeout 30m ./...
GOWORK=off go build ./...
GOWORK=off go vet ./...
```

Run nested-module gates from each touched module, including `api`, `sentinel`, `extension`, `integrations/fabriq`, `a2aremote`, `a2aremote/grpcbind` and the new sandbox module when added: `GOWORK=off go test -race -count=1 ./...` and `GOWORK=off go build ./...`. Run `GOWORK=off make test-integration` for Fabriq changes. `inttest` uses its documented integration build tag. Root globs do not reach nested modules.

Backend test evidence must show Docker-backed Postgres and Mongo tests actually ran. SQLite alone does not qualify those adapters. If the documented macOS `scopespy` hang occurs, retain the timeout evidence, run remaining packages, and require the exact affected commit's Linux CI run before acceptance. A skip or unexplained hang never counts as a pass.

In `docs/`, run `pnpm types:check`, `pnpm lint`, `pnpm build` when published docs or site sources change. For plan-only changes validate links, fences, task coverage, ownership and dependency consistency; Go tests do not validate prose.

In forge-dashboard, read current `AGENTS.md` and the full `packages/plugin/PLAYBOOK.md`. Use current repo `make l`/`make f` targets when present. Package equivalents are `pnpm --filter @forge-go/dashboard-plugin-cortex lint`, `format`, `typecheck`, `test`. Run `tsc -b` too where project references require it, and the documented shell build. Verify real installed flows at desktop and 390px, keyboard navigation, permission denial, empty/loading/error states, retry and restart.

Evidence records include commit, command, exit status, actual environment/runtime versions, dataset/image digests, timestamps, scope, skipped checks and artifact links. Keep sensitive raw outputs in access-controlled storage.

## Integrations and release dependencies

| Integration | Required work | Proof |
| --- | --- | --- |
| Nexus | Capture resolved model/finish reason/usage; preserve stream tool-call assembly; shared budget accounting | Provider contract fixtures plus one configured live provider |
| Shield | Fail policy, redaction application and optional incremental capability | Allow/block/redact/error/cancellation, cross-chunk cases and installed adapter |
| Weave | Evidence provenance, retrieval freshness and immutable source references | Source changed/revoked tests and actual retrieval with authorized scope |
| Sentinel | Frozen evaluation plans, captured-output scoring, durable deduplicated completion jobs, calibrated comparisons | Zero target reruns during recorded scoring; installed Cortex-to-Sentinel trace |
| Fabriq | Typed lesson schema, physical provisioning, guarded publication, filters and revocation | Real database, restart, denied access, duplicate retry and stale recall |
| Forge/Authsome | Host principal/scope mapping, capabilities, command CSRF/idempotency, approval permissions | Two principals/two scopes, revoked grants, installed plugin |
| Audit/metrics | Use existing `audit_hook` and `observability` patterns; durable audit transition/outbox, bounded labels | Failed audit persistence behavior, no secrets in public events or labels |
| Sandbox | Broker identity, isolated OCI runtime, export authorization and verifier jobs | Linux escape-boundary probes, resource/cancel tests, egress policy and cleanup |
| A2A | Structured handoffs, authority attenuation, remote status/stream compatibility | In-process plus real HTTP/SSE and gRPC-binding tests |

Release generic missing Sentinel/Fabriq capabilities upstream first and consume tagged versions. Never add an upstream import of Cortex. Add the new sandbox module to lint, CI, release verification and pinning as an explicit release task, with a decision recorded about its public tag. Expand CI coverage to currently omitted remote modules when their behavior changes. Dispatch releases only after exact-main CI is green; use the existing workflow, never hand-pushed tags.

## Stage gates and rollout

| Gate | Required acceptance evidence |
| --- | --- |
| G1 | No exhausted/truncated/cancelled run reported as successful; safety failures and deep scopes fail as specified; sync/stream/resume agree; legacy decoding works |
| G2 | Required criteria gate success; stale/substituted evidence rejected; untrusted jobs isolated; grader protected; Sentinel scores captured results; safe public replay survives restart |
| G3 | Plans survive restart; stale workers fenced; failed prerequisites block dependents; ambiguous effects reconciled; shared budget enforced; real remote handoff retains scope |
| G4 | Reflection changes behavior within bounds; memory eligibility enforced; calibration reports honest uncertainty; improvement activation is authorized/reversible; system benchmark and live product gates pass |

Use opt-in capability flags per stage, then a fixture deployment, an isolated integration deployment, a limited host canary and wider rollout. Gate advancement is an operator action. Shadow verification can collect reports while labeling answers unchecked; it must never imply checks gated a result when they did not.

Required security corpus cases have zero observed unauthorized operations, secret leaks, artifact substitutions or duplicate non-idempotent effects. Report sample counts; zero observed failures is not proof of zero risk. Quality cohorts, budgets and success thresholds are fixed before candidate runs. Establish latency/cost baselines at G1, then report p50/p95 and regressions by task class through G4. No fixed performance claim is made before measurement.

Rollback disables new scheduling/activation, drains or cancels outstanding work using persisted policy, and preserves verification/audit data. Never rollback by dropping migrations or silently running a weaker safety mode. Reconcile active jobs before retiring a sandbox worker or effect adapter.

## Coverage of the requested capabilities

| Requested behavior | Implementation packets | Acceptance proof |
| --- | --- | --- |
| Accurate execution and honest completion | S1.1, S1.2, S2.2 | Exhaustion/truncation tests and required verification gate |
| Self-understanding and awareness of limits | S4.1, S4.2 | Installed capability snapshot, unresolved assumptions and bounded transitions |
| Planning and getting the task right | S1.5, S3.1, S3.5 | Acceptance coverage, validated DAG and revision history |
| Multi-agent coordination and review | S3.2, S3.3, S3.4 | Fenced claims, evidence-bearing handoffs and independent review |
| Testing and contextual validation | S2.1, S2.3, S2.5 | Protected tests, source/identity/freshness checks and observed state |
| Streaming and safe response evaluation | S1.3, S2.6 | No unsafe public bytes, distinct verification events and restart replay |
| Secure evaluation sandbox | S2.4, S2.5 | Qualified Linux runtime, protected graders and authorized export |
| Recovery, budgets and stopping rules | S1.2, S3.2, S3.4, S3.5 | Crash-window, reconciliation and concurrent-spend tests |
| Memory, learning and corrections | S4.1, S4.3 | Context retention, qualified recall, expiry and revocation |
| Calibrated certainty and measured improvement | S4.4, S4.5 | Cohort evidence, holdout evaluation, controlled activation and rollback |
| Product visibility and operations | S1.6, S2.7, S3.6, S4.6 | Installed API/dashboard, backend, browser and rollout gates |

## Shared work with the optimization plan

The companion [optimization plan](2026-10-18-cortex-optimization.md), committed in `c9bee40`, defines 21 optimizer packets. Its [design](../specs/2026-10-18-cortex-optimization-design.md) remains a proposal with its own review status. These mappings let you reuse its deliverables without requiring either whole program to finish first:

| This roadmap | Optimization task | Single implementation to share |
| --- | --- | --- |
| S1.1/S1.2 | 1 | Prepared execution, stop reasons, provenance and resume |
| S2.3 | 6, 7 | Frozen recorded evaluation and Cortex conversion |
| S2.4/S2.5 | 8 | Sandbox runner consumed by the optimizer's trial-environment adapter |
| S3.4/S3.5 | 5, 8 | Compatible operation keys, receipts and budget semantics; each aggregate retains its own store |
| S4.3 | 12 | Shared evidence/provenance validation; ordinary lessons and experiment lessons retain explicit ownership |
| S4.5 | 2 through 5, 9 through 11, 13 through 15 | Optimizer records, trial search, held-out decisions, controller versions, activation and rollback |
| S4.6 | 16 through 21 | Installed product qualification, release and consumer checks |

S4.5 is a real prerequisite dependency on those optimizer deliverables. They remain required if you execute all four stages. Do not replace them with an empty adapter and mark Stage 4 complete. Optimization can consume a qualified host runner before this program's built-in broker exists; it must report which runner was actually qualified.

## Copyable chat assignment

```text
Implement packet <ID> from docs/superpowers/plans/2026-10-18-cortex-assured-execution-<stage>.md.
Read the roadmap and shared architecture spec first. Verify prerequisite commits and claim ownership in the progress ledger through its coordinator.
Work on main in the primary checkout. You are not alone in this codebase: preserve others' changes and do not revert them.
Own only <exact paths>. Shared engine/store/manifest files belong to <named owner>; provide the required wiring changes for that owner.
Use TDD, reuse the frozen contracts, run focused and required lint/format/module checks, and commit/push exact owned paths after they pass.
Report implementation, test, backend and live evidence separately. Stop this packet at its stated boundary; do not start another packet or create/message chats without authorization.
```

You can implement the entire roadmap in one chat using the same packets. Separate chats are useful for independent packages and repositories, but do not remove the shared-checkout ownership rules.
