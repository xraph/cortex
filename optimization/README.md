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

Model, judge and tool wrappers still need to call this boundary for every metered
operation. These foundations do not establish that integration on their own.

## Artifact lifecycle

See [artifact lifecycle](artifacts/README.md) for retained reads, deletion receipts
and recovery scans. Artifact recovery and metering share the identity-resolution
helper so both preserve the caller's scope, cancellation and deadline.
