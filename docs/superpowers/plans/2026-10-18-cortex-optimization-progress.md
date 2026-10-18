# Cortex optimization implementation progress

Plan: [Optimization implementation](2026-10-18-cortex-optimization.md). Scope: all seven phases and 21 tasks, using DRY and TDD. No phase is complete yet.

## Shared contracts and ownership

The user authorized direct coordination with **Improve Cortex agent execution**. That chat owns the root runtime, outcomes, prepared execution, continuation and root backend integration. This chat owns Sentinel's recorded evaluation foundation, optimization, adapters and product integration. We consume the shared runtime commits rather than maintaining two execution loops.

The outcome foundation is on Cortex main in `8bebd82`: incomplete termination, stop and finish reasons, verification status and result references. The runtime owner verified engine/run/SQLite race tests, PostgreSQL/Mongo conformance and historical fixtures, all-module lint, root build and A2A compatibility.

Prepared execution remains in progress with the runtime owner. The agreed surface is `PrepareAgent`, `RunPrepared`, `StreamPrepared`, `PreparedAgent.Digest() string`, detached `Inspect()` and immutable `Derive(PreparedEdits)`. Derivation keeps identity, scope, model, budgets, schemas and implementation pins fixed. It supports section bodies, tool guidance/subsets and finite sampling bounds. Optimization will enforce its narrower edit policy. Do not consume this API until its verified commit lands.

## Current state

| Task                                                                         | State                                                         | Next gate                                                                     |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| 1: prepared execution                                                        | Runtime owner implementing and verifying                      | Consume the verified commit, then publish a root version through the workflow |
| 6: Sentinel foundation                                                       | Implemented in verified slices; release qualification remains | Check exact main CI, then publish Sentinel through its workflow               |
| 2-5: optimization module and persistence                                     | Pending foundation release                                    | Require a real published root version                                         |
| 7-21: adapters, optimizer, activation, learning, dashboard and qualification | Pending                                                       | Follow the plan's dependency gates                                            |

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

CI passed the recorded worker commit `c76234b`. The first comparison commit exposed standalone `gosec` G404: its scanner does not honor the golangci-lint annotation for the seeded statistical RNG. The scanner-specific justification passes CI's pinned gosec v2.28.0 with zero findings, and comparison race tests and tagged lint pass. Do not release until CI passes on the final main commit.

Task 7 must bind frozen task context and environment references explicitly, preserve original usage quality and validate every returned identity. Tasks 5 and 7 must also enforce judge reservations at the actual model-call boundary. Persisting aggregate judge usage alone does not enforce an optimization budget. Automatic evaluation stays unavailable without an explicit binding, persistent queue and running worker.
