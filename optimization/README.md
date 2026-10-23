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
   composition and reflection. Supply your existing trial/evaluation `PhaseHandler`
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
Missing verification stops the worker before another phase runs. Accounting
recovery does not recover a lost model response: a saved dispatch marker still
returns `ErrUnknownAttempt` if the controller would repeat the call. Such cases
need host reconciliation of the original output before the phase can advance.
