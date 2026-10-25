# Cortex optimization foundations

This module provides scoped experiment storage, metered model controllers, a
host-started worker, artifact lifecycle coordination and usage recovery. You supply
the host authority, model bindings and trial/evaluation handler. The complete
optimization loop and application integrations are still being built.

## Usage accounting

Construct `NewLedgerMeter` with your optimization repository, host authorizer and
identity resolver. Each operation checks the caller's `optimization.start` grant
for the experiment, resolves its original requester and checks that requester's
current grant. Scope and cancellation survive identity resolution. Credentials
stay in the host context.

Use `Reserve` to hold a conservative debit. It does not permit an external call.
Call `BeginCall` at the provider boundary with the same reservation and a SHA-256
digest of the actual frozen request. Your trusted adapter must compute this
binding; do not accept a digest supplied by an agent as proof of request content.
The store commits the debit, dispatch marker and audit event together. Invoke the
provider only after `BeginCall` succeeds and while the call context remains live.

Once a dispatch marker exists, the same call cannot receive another dispatch
permission. An identical retry returns `ErrUnknownAttempt`; changed request
content returns `ErrConflict`. A lost commit acknowledgment may mean the marker
was saved. Reconcile the original operation before deciding whether a new attempt
is allowed. A marker records that the operation may have started, not that the
provider received it or completed it.

Use `Settle` for an authoritative receipt or an explicit uncertainty observation.
Verify receipt provenance in your adapter. Unknown usage retains the reserved
upper bound. A later authoritative receipt can reconcile that debit, and retrying
the same receipt cannot refund it twice. Reported overruns remain in the ledger
and stop subsequent dispatch, including calls with an earlier reservation.

The ledger enforces the current worker lease and experiment state inside each
transaction. A cancelled or expired worker cannot bypass those checks by keeping
an old meter instance. Cancellation after a dispatch commit returns an error and
leaves the conservative debit in place.

## Model calls

Use `NewMeteredClient` around your `llm.Client`, then supply it through
`engine.WithLLM`. You provide the ledger meter, a conservative `UsageEstimator`
and a configuration bound to the exact scope, experiment, attempt, operation and
model revision. The estimator sees a detached copy of the actual request. It
must include input tokens, the output cap and any frozen pricing in its bound.
Give each logical call a durable key that survives retries.

The wrapper hashes the full request with that binding and whether the call is
streaming. It commits dispatch before invoking your provider. Requests for a
different scope or model, missing output caps and oversized request JSON fail
before dispatch. Byte limits bound accepted JSON, not the provider's allocations.
You still need to pin the provider's routing to the configured model revision.

Supply a `ModelUsageReporter` when your provider can establish authoritative
usage. It receives accounting facts and request identity without prompt or output
content. Verify the provider receipt and frozen pricing there. Without a reporter,
token counts remain uncertain: the ledger retains the reservation and records
any observed overrun. Invalid receipts, inconsistent counts and underreporting
cannot release the debit. Reporting must not perform unmetered charged work.

Streaming closes and settles once at EOF or failure. Always close a stream you
abandon. Closing cancels a blocked read, provided your provider honors context
cancellation, and usage available during close is included in accounting. A
settlement error remains an error when Cortex consumes the stream; it cannot
be mistaken for EOF. Settlement uses the original caller context, so caller
cancellation may leave the durable dispatch debit awaiting later reconciliation.

The wrapper is available for model and judge calls. The controller factory below
uses it directly. Judge integration and tool accounting remain open.

`NewTokenPricing` supplies the `Pricing` port for a fixed model and price revision.
You provide approved input and output rates in USD micro-units per million tokens,
plus a fixed request charge. Each token category rounds upward independently.
Arithmetic is exact, and a quote that exceeds the ledger's integer range fails.
Explicit zero rates are supported. Quotes remain estimates, including zero quotes.

Use rates that bound every charge your provider can incur for that request. If
token rates and a fixed fee cannot cover its billing model, supply another
`Pricing` implementation. This adapter does not fetch prices or count tokens.

## Artifact lifecycle

See [artifact lifecycle](artifacts/README.md) for retained reads, deletion receipts
and recovery scans. Artifact recovery and metering share the identity-resolution
helper so both preserve the caller's scope, cancellation and deadline.

## Request estimation

`NewRequestEstimator` computes a reservation from the actual request, including
its messages, tools and positive output cap. Supply a local `InputTokenCounter`
that covers the exact model revision, tokenizer and provider framing. Its `Bounded`
flag is a host assertion that the result is an upper bound. An approximate token
count or unknown model revision cannot authorize dispatch. The module does not
bundle a provider tokenizer or use a generic characters-per-token heuristic.

The estimator checks scope, detaches the input, rejects negative or overflowing
bounds, adds the output cap and quotes frozen pricing. Token-only experiments
omit pricing. For priced experiments, the quote must identify the experiment's
pricing revision and cover every billable category. Counting cannot make an
unmetered charged request.

## Controller and worker wiring

Construct these pieces in your host:

1. Build `NewControllerFactory` with the repository, authorizer, identity resolver,
   model bindings and explicit input/output limits. Each `ModelBinding` supplies
   a provider, model revision, token counter, optional pricing and receipt reporter.
2. Pass that factory to `NewControllerPhaseHandler`. It handles proposal,
   composition and reflection. Supply `NewTrialPhaseHandler` with your environment factory and evaluator
   for baseline, probe, validation and decision work. A missing handler returns
   `ErrUnavailable` for those phases.
3. Build `NewUsageRecovery` with an authoritative receipt lookup provider, then
   supply it with the handler and host `CallerIdentity` to `NewOptimizer`.
4. Use `Create` with a host-frozen `ExperimentBundle`, then `Start`. The host owns
   evaluator freezing before creation. Run `Work` with an exact scoped context,
   or call `WorkOnce` to execute one phase step. Cancel the context on shutdown.

The model controller resolves immutable controller content and exact model
bindings. It checks the baseline, round, edit policy and frozen lessons. Discovery
trials, evaluations and candidates must match stored records. Artifacts from every
execution attempt must remain eligible. Held-out evidence is refused before a
model call. Returned JSON is strict and bounded, and accepted edits pass the
existing edit-policy validator. Controller versions with tools return
`ErrUnavailable` until a metered tool executor exists.

The worker claims one experiment at a time per instance, renews its lease and
rechecks current authority during execution. Different instances receive unique
owners. `Pause`, `Resume` and `Cancel` use revision checks; resume preserves the
original phase and deadline. Persisted pause or cancellation invalidates the
lease immediately, and the next heartbeat cancels active work. Handlers and
providers must honor context cancellation and perform their own environment
cleanup. A budget error commits `budget_exhausted`. Other execution errors leave
durable state for recovery and return to the host supervisor.

Candidate versions and phase advancement commit together. Reflection queues
lesson publications in the same transaction as completion or review. The worker
does not publish to Fabriq or activate a winner. Those adapters remain separate.

## Controller comparison scheduling

For a frozen controller comparison, wrap your child `OptimizerConfig` with
`NewImprovementPhaseHandler`, then use that handler in the parent optimizer's
config. The wrapper creates a separate child worker so the parent can keep
renewing its lease while a child executes. `Work` polls top-level experiments;
the parent schedules its children through `WorkOnce`.

Use `CreateImprovementExperiment` with an `ImprovementEvaluator` to qualify the
parent and child plans before saving the draft. The scheduler creates children
from those exact templates in stream order. Each child finishes discovery and
saves its private reflection before the next child starts. Later tasks inherit
only earlier reflections from their own controller arm.

Once every private reflection is recorded, the parent opens validation and
freezes one allocation with each child's selected execution version. Retries and
takeovers reuse that allocation. The parent reaches its decision phase only after
both arms have captured all assigned validation outcomes. Unknown controller
answers stop work, even if a late receipt has settled their cost. Paused children
stay paused; an exhausted child exhausts the parent.

Parent scheduling currently stops at the decision phase with `ErrUnavailable`.
Aggregate controller comparison and reviewed controller adoption are still
pending. You can run discovery and paired validation, but this handler cannot yet
recommend or adopt a controller version.

## Late receipts and takeover

`UsageRecovery.Recover` checks the caller and original requester's current
`optimization.usage.reconcile` grant before and after receipt retrieval. Your
`UsageReceiptProvider` must verify provenance and return the exact durable request
digest, experiment, attempt, call key, receipt and authoritative usage. It looks
up an existing operation; it cannot execute another one. Supply an actor obtained
from host authentication, never from model output.

Recovery uses `Transaction.ReconcileUsage` without an execution lease. It works
after cancellation or lease expiry, uses the reservation revision to fence races,
and cannot change experiment state or execution authority. Repeating an identical
receipt cannot refund twice. Unknown evidence retains the conservative debit.

Before takeover or resumption, the worker scans bounded reservation pages and
reconciles unresolved dispatches, with a thirty-second lookup deadline per receipt.
A reservation bound to a persisted trial attempt is retained for environment
inspection by the trial handler. Its full debit remains until settlement. Other
unresolved calls require verified receipt recovery before another phase runs. Accounting
recovery does not recover a lost model response: a saved dispatch marker still
returns `ErrUnknownAttempt` if the controller would repeat the call. Such cases
need host reconciliation of the original output before the phase can advance.


## Trial execution

Use `NewTrialPhaseHandler` for sequential, parallel and task-local experiments. You supply an
`EnvironmentFactory`, `Evaluator`, repository, authorization ports and a
`TrialAllowance` covering the entire trial. Your environment must enforce that
allowance across model calls, tools and retrieval, along with the frozen step,
output and time limits. A priced allowance must match the experiment's pricing
revision. Capability flags are host guarantees. They don't qualify a sandbox.

Each worker step persists one boundary before advancing:

1. Schedule the frozen case and repetition, then create its attempt.
2. Open the environment idempotently and commit its receipt.
3. Reserve usage and persist a dispatch marker before execution.
4. Capture the result and confirmed containment receipt in one transition.
5. Settle captured usage, score the stored output and commit the evaluation.

Every attempt gets a stable, separate session ID and the optimization execution
purpose. After a lost response, the handler reconnects and inspects that same
attempt. It never dispatches over an existing marker. An ambiguous inspection or
stop keeps the attempt unresolved and its budget charged. Cancellation triggers
bounded cleanup, and artifacts from earlier attempts must remain eligible before
scoring or selection.

Probe scheduling covers accepted proposals and composed candidates within the
frozen round and candidate limits. The finalist is chosen from complete discovery
scores, with ties retaining the baseline. Validation pins that choice in a durable
allocation and records exposure before executing the held-out cases. Sentinel
supplies the paired statistics; Cortex checks the frozen selection thresholds and
stored evidence before committing a decision for reflection and review.

You can run trials in sequential or parallel mode. Parallel mode admits at most
`Limits.Concurrency` operations per step, counts unresolved attempts against that
limit, and uses the same durable budget ledger. Calls overlap only after each has
its own dispatch marker. The worker commits successful sibling results before
returning a batch error, so you can resume uncertain work without replaying a
completed call. Cancellation or lost authority leaves the batch recoverable.

You can bind the Sentinel implementation from
`github.com/xraph/cortex/sentinel/optimization`; see its
[adapter guide](../sentinel/optimization/README.md).

## Task-local refinement

Call `OptimizeTask(ctx, TaskSpec{...})` with a frozen `ExperimentBundle` containing
one discovery task, its baseline and controller versions, evaluation plan and
host-approved budget. Supply the same trusted evaluator through
`OptimizerConfig.TaskEvaluator` and `TrialConfig.Evaluator`. Cortex qualifies the
plan before persisting work and again at each trial phase. Every criterion must
explicitly support scoring without an expected answer.

`TaskSpec.Isolation` records the guarantees you require from each environment.
The opened environment must satisfy every promised guarantee, including read-only
access when requested. Omitted round and repetition counts default to one. The
total timeout covers evaluator qualification and execution, and a shorter caller
deadline is persisted as the experiment's absolute deadline. Caller cancellation
attempts an authorized durable cancellation; failed cleanup is returned with the
original error. Recursive calls from optimization trials, controllers and judges
are refused.

The result returns a completed capture's output and artifacts, the decision and
all trial outcomes, including failures and blocked probes. Selection preserves the
baseline on a tie. Comparative cost gates require known execution charges at the
frozen pricing revision and a positive baseline cost. Judge charges remain in the
budget ledger separately. A selected task result is exploratory or contrastive
evidence and cannot activate a global harness version.

Use `GetTaskResult` to retrieve a completed result after reconnecting. It rechecks
current authority, retained artifacts and frozen lessons without executing the
task again. Interrupted calls return the experiment ID when creation succeeded;
your worker can reconcile unresolved attempts within the original deadline.

Artifact adoption is explicit. Bind `WithArtifactAdopter` and freeze an
`AdoptionPolicy` in the task before calling `AdoptArtifacts` with the selected
decision, trial, artifact subset, destination and idempotency key. Both the caller
and original requester need the current adoption grant. A nil policy forbids
adoption.

Your host adapter must enforce destination permissions and the pinned policy,
read the exact artifact content, and atomically persist the key, request digest
and receipt with its effects. An identical retry must reconcile the original
commit; changed content under that key must conflict. Cortex validates the
receipt and never retries host effects automatically. The integration tests use
a receipt-producing host fixture; you still need a qualified destination adapter.

## Production activation compatibility

Bind `NewEngineCompatibility` to the same production engine you pass to
`WithActiveEngine`. You supply its deployed runtime revision and a host
`Compatibility` implementation for current policy and external dependencies:

```go
checker, err := optimization.NewEngineCompatibility(optimization.EngineCompatibilityConfig{
	Engine:         productionEngine,
	Host:           hostCompatibility,
	RuntimeVersion: deploymentRevision,
})
if err != nil {
	return err
}
config.Compatibility = checker
optimizer, err := optimization.NewOptimizer(config, optimization.WithActiveEngine(productionEngine))
```

The checker verifies the immutable version digest and scope, then checks host
policy, current model/tool bindings and stored source records. Model and selected
tool revisions must be pinned. Selected tools must still have executable
implementations and matching schemas. Source records are read in one current
store snapshot; a store without that capability returns unavailable. Approved
prompt and tool-description edits retain their original source identity.

Activation, rollback and `RunActive` perform compatibility checks after reading
artifact content, so a source edit during that read cannot authorize a stale
version. Changed sources or runtime bindings require revalidation or deactivation.
Deactivation remains available for a stale route. A checker bound to a different
engine from `RunActive` is rejected during optimizer construction.

These are point-in-time checks and make no model or tool calls. Your host still
owns current grants, policy, remote provider routing and external knowledge
revisions. It must return incompatible when it cannot establish those bindings.

### Controller comparison creation

Use `CreateImprovementExperiment` with an `ImprovementSpec` containing your frozen
parent bundle. Set `OptimizerConfig.ImprovementEvaluator` to the Sentinel adapter
that owns those plans. Creation qualifies each distinct plan, checks permission
to create the parent and every child, and repeats those checks after external
qualification. It returns a saved draft. Workers start separately.

You can persist an `ImprovementPlan` with two pinned controller versions and
ordered pairs of complete child templates. A child's `ParentExperiment` binds
its arm and sequence. Creating a child requires the current parent lease, and both
controller arms consume the parent's aggregate token and money budget. Late
receipts update the original reservations, including after cancellation.

Controller comparisons have durable storage, shared accounting and paired
validation. The scheduler, isolated lesson advancement, controller-level comparison
and controller adoption are still pending. Child validation uses one parent
allocation with frozen case assignments, and selection waits for the whole
stream. Ordinary phase handlers return `ErrUnavailable` for comparison parents.
