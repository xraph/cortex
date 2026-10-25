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
Cortex. It also records whether each criterion requires an expected answer,
including the fallback used by `contains` and `not_contains` when no substring
is configured. If any case requires an expected answer, that requirement applies
to the frozen criterion. Supported scorers are the pinned local built-ins: `exact`, `contains`,
`not_contains`, `regex`, `json_valid`, `length`, `latency` and `cost`.

For a model judge, construct `NewMeteredJudge` with your repository, authorizer,
identity resolver and `optimization.ModelBinding`. Set explicit input bytes,
response bytes and output token limits. Call `judge.Register(engine.Scorers())`
before freezing the suite, use its scorer name with `{"rubric": "your criteria"}`,
and include that judge in `Config.Judges`. Other custom scorers remain unavailable.

Your model binding supplies the conservative input counter, frozen pricing and
provider receipt reporter. The judge reserves the full request before dispatch,
including input framing and the output cap. It charges `OperationJudge` under the
requesting worker's lease and rechecks the caller and original requester. You must
pin provider routing to the configured model revision and enforce the output cap.
The scorer revision includes the model revision and limits; the plan pins the rubric.

Judges return strict JSON with a numeric `score` between zero and one and an
optional `reason`. Scores of at least 0.5 pass the criterion. Malformed, duplicate,
missing and truncated answers produce incomplete evidence. The adapter does not
substitute a neutral score. Each case may use a judge name once; register separate
names if you need multiple rubrics for the same model.

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

Sentinel v1.10.0 implements a 95% interval with 2,000 bootstrap samples. Other frozen
confidence settings return `ErrUnavailable`. Too few distinct cases produce
inconclusive evidence. Repetitions do not increase the distinct-case count.

Supply `Config.Repository` to enable cost ratios. The adapter joins each Sentinel
job to its stored Cortex evaluation and sealed trial capture, checks the report,
experiment and plan identities, and compares the original execution charges only
when every charge is known and uses the experiment's frozen pricing revision.
Unknown or estimated charges, token-only budgets and a zero baseline leave the
ratio unavailable. Conflicting captures fail the comparison.

`CompareControllers` requires `Config.Repository`. It reloads the parent allocation
and original child records, checks the exact frozen plan groups and compares their
selected executions through Sentinel's recorded group API. Reports retain all
plan references; `PlanDigest` is empty when several plans contribute. Missing
captures or scores produce an inconclusive report without another judge call.

Judge charges remain separate from trial costs. `Evaluation.JudgeUsage` reports
ledger observations with an aggregate receipt reference; individual provider
receipts and their request digests remain in the reservations. It does not treat
unreported usage as free.

## Recovery

A completed recorded job reuses its score without another model call. If a judge
answer is lost before Sentinel commits it, the ledger's dispatch marker prevents
replay. The evaluation stays incomplete. Use `optimization.UsageRecovery` with
your receipt verifier to reconcile a late charge, including after cancellation or
lease loss. A receipt establishes usage, not the missing answer.

For task-local refinement, supply this evaluator as both
`OptimizerConfig.TaskEvaluator` and `TrialConfig.Evaluator`. Cortex verifies the
frozen capabilities before admitting work and rejects unknown or answer-dependent
criteria. Existing offline plans that predate capability metadata retain their
bindings, but you must freeze a qualified plan to use task-local selection.

## Verification boundary

The integration test uses both SQLite stores, the real optimizer worker and
controller handler, and Sentinel's recorded scoring and comparison. It closes
and reopens both stores after lost provisioning, execution and scoring acknowledgements,
checks that execution and committed judge calls are not replayed, verifies separate
judge reservations and a priced cost-ratio gate, and advances through review.

Parallel variants exercise the same recovery boundaries. The task-local variant
uses one discovery case without an expected answer, recovers twelve recorded
executions and judge results across database reopen, and returns the selected
capture without a holdout allocation or a repeated execution.

The controller comparison test records twelve metered judge results across two
plans and four child experiments. It reopens both stores, rebuilds the same report
and commits the parent worker's decision without dispatching another judge.

Its model and environment are fixtures. Live sandbox qualification, hosted model
calls, destination artifact adoption, Fabriq publication and dashboard application
flows require their own integration checks.
