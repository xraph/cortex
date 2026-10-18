# Cortex optimization implementation progress

Plan: [Optimization implementation](2026-10-18-cortex-optimization.md). Scope: all seven phases and 21 tasks, using DRY and TDD. No phase is complete yet.

## Shared contract reconciliation

The user authorized direct coordination with the active **Improve Cortex agent execution** chat. That chat owns Cortex root runtime, run outcomes, prepared execution, continuation and root backend integration. This optimization chat owns Sentinel's generic frozen/recorded evaluation foundation and the optimizer, adapters and product integration. Consume the root foundation commits; do not create a second loop or competing prepared API.

- Root: `PrepareAgent`, `RunPrepared`, `StreamPrepared`, versioned immutable `PreparedAgent` with `Digest()`, explicit empty tool allowlists, current authorization on dispatch/resume, frozen source/model/tool revisions and durable provenance. Prepared and ordinary paths share initialization and finalization.
- Outcomes: `run.StopReason`, `StateIncomplete`, provider finish reason and explicit unchecked verification status. Step exhaustion, truncated output and unknown termination cannot become successful validation evidence.
- Provenance: use a root domain type that suspension can reference without importing `engine`; engine exposes context helpers. Persist the prepared snapshot/schema and provenance across suspension. Do not serialize credentials or permissions as durable authority.
- Sentinel: `FreezeEvaluation`, `EvaluateRecorded`, `CompareRecorded`, `EnqueueRecorded`, `WorkRecorded`. Sentinel owns frozen plans, scorer revisions, recorded jobs and source cursors across all four backends. It never imports Cortex and recorded evaluation never calls a target.
- Fabriq: the Cortex adapter owns the lesson envelope. Upstream Fabriq remains a generic guarded-write and recall provider. Implementation follows task 12.

The initial runtime outcome draft from this chat was handed to the runtime owner before commit. Its real SQLite regressions failed before the draft and passed afterward. The subsequent race run compiled the other chat's expanded tests and failed on unfinished verification metadata and incomplete stream events. This is partial foundation evidence, not phase completion. The runtime owner has the exact log and owns the final checks.

## Current state

| Task                   | State                                   | Evidence or next gate                                                               |
| ---------------------- | --------------------------------------- | ----------------------------------------------------------------------------------- |
| 1: prepared execution  | In progress in coordinated runtime chat | Consume and verify final shared foundation commits                                  |
| 6: Sentinel foundation | In progress here                        | Frozen plans shipped in Sentinel `1540974`; recorded scoring/comparison/jobs remain |
| 2-5, 7-21              | Pending                                 | Follow dependency and release gates in the plan                                     |

Sentinel's tracked checkout was fast-forwarded to `1703a5c` (`v1.8.0`) before edits. Existing untracked `_project_files/` remains untouched. New plan storage requires an explicit app and matches tenant exactly. Legacy suite APIs retain their existing behavior.

The first lint invocation reused results referring to a deleted `/tmp/tier2/wt/sentinel` checkout. Verification uses a task-specific linter cache and serial-runner mode to avoid the global linter lock held by concurrent Cortex checks. Do not count a stale-cache result or a lock refusal as a passed check.

Sentinel `1540974` adds immutable plan storage, copied case/context data, scorer revision checks and four-backend conformance tests. The full race suite, build and vet passed. PostgreSQL and MongoDB containers ran without skips. Standard and integration-tagged lint each reported zero issues using serial-runner mode; `make l` itself encountered the shared linter lock. Two integration fixtures also received naming and shadowing fixes exposed by the tagged check.

Captured scoring is the next slice. Ordinary and recorded evaluations will share one scoring function. Required criteria remain individual gates, and scorer errors remain distinct from failed outputs. Recorded requests and result checkpoints need durable storage independent of live suite rows, whose SQL foreign keys cascade on deletion.
