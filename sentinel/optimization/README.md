# Sentinel optimization adapter

Use this package as the `optimization.Evaluator` supplied to Cortex's trial
handler. It scores captured executions through Sentinel v1.9.2 and compares exact
recorded job sets. Cortex keeps ownership of execution, budgets and selection.

## Wiring

1. Freeze your suite with Sentinel's `Engine.FreezeEvaluation`.
2. Create a `Binding` containing the exact Cortex scope, Sentinel app and tenant,
   frozen plan reference, and each public `TaskCase` paired with its Sentinel case
   ID. The plan reference uses its ID, digest and schema version as the revision.
3. Supply those bindings, the Sentinel engine and your `ScopeMapper` to
   `NewEvaluator`. The mapper must preserve the Cortex scope and resolve exactly
   the app and tenant in the binding. Existing conflicting identities are denied.
4. Call `Freeze` with the same source, tasks and required criteria. Put its result
   in the experiment bundle before creation, then pass the evaluator to
   `optimization.NewTrialPhaseHandler`.
5. Wrap the trial handler with `NewControllerPhaseHandler` and run the optimizer
   worker under your authenticated scope.

You own both stores and their migrations. No global Sentinel worker starts when
you construct this adapter. Scoring uses `EvaluateRecorded` synchronously under
the optimization worker's context, with Sentinel's persistent recorded-job queue
providing idempotency and recovery.

## Evidence and scoring

Private rubric bytes stay in Sentinel. The binding verifies exact task inputs and
frozen plan identities, then projects criterion IDs and scorer revisions into
Cortex. Supported scorers are the pinned local built-ins: `exact`, `contains`,
`not_contains`, `regex`, `json_valid`, `length`, `latency` and `cost`.

LLM judges and custom scorers are unavailable here until an adapter can reserve
and settle their actual charged calls. This prevents a suite from adding judge
spend outside the optimization budget. Unknown scorer revisions are refused too.

Recorded jobs preserve original output, duration, usage quality and Cortex run
identity. Artifact bindings and step/tool-call IDs remain opaque references.
Missing trace payloads are not fabricated. The cost scorer requires known usage;
unknown spend cannot pass as zero. Failed or incomplete execution keeps
its failure classification; missing scores never become a valid zero.

Retries reuse the same recorded job and report. A reused idempotency key with a
changed capture fails. Once Sentinel has committed a score, reopening either
store and retrying can link that score without calling the agent again.

## Comparison

The adapter calls Sentinel's paired comparison over explicit baseline and
candidate job IDs. It maps case IDs back to the frozen public tasks, retains
criterion failures and averages repetitions within each case. Protected criterion
regressions and latency ratios come from the recorded results and original
execution durations.

Sentinel v1.9.2 implements a 95% interval with 2,000 bootstrap samples. Other frozen
confidence settings return `ErrUnavailable`. Too few distinct cases produce
inconclusive evidence. Repetitions do not increase the distinct-case count.

Cost quality remains visible, but Sentinel's recorded usage does not retain the
Cortex pricing revision. This adapter therefore leaves `CostRatio` unavailable;
a configured cost-ratio gate cannot recommend a candidate without that evidence.
Trial and controller spend still count against their own reservations.

## Verification boundary

The integration test uses both SQLite stores, the real optimizer worker and
controller handler, and Sentinel's recorded scoring and comparison. It closes
and reopens both stores after lost provisioning, execution and scoring acknowledgements,
checks that execution is not replayed, and advances through review.

Its model and environment are fixtures. Live sandbox qualification, hosted model
calls, metered judges, parallel/task-local scheduling, Fabriq publication and
dashboard application flows are separate work.
