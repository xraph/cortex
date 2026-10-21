# Cortex optimization foundations

This module contains experiment contracts, scoped persistence, artifact lifecycle
coordination and usage accounting. The optimizer controller, worker lifecycle and
application integrations are still being built.

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

The wrapper is available for model and judge calls. Controller and judge wiring,
the concrete estimator and pricing adapters, and tool accounting remain open.
Late receipts after the worker loses its running lease also need a recovery path.

## Artifact lifecycle

See [artifact lifecycle](artifacts/README.md) for retained reads, deletion receipts
and recovery scans. Artifact recovery and metering share the identity-resolution
helper so both preserve the caller's scope, cancellation and deadline.
