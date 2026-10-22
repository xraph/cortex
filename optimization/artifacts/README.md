# Artifact lifecycle

Use `RetainedReader` when reading optimization evidence. It combines the existing
content verifier with retention metadata, checking eligibility before the provider
read and again before returning bytes. The provider must enforce current artifact
permissions and bound its allocations. Returned creation and expiry dates must
match the registered metadata.

The final metadata read is an observation point. It does not grant a lease over
the returned bytes. Decisions and activation still need the retention checks in
their own database transaction.

`DeletionService.Delete` first checks host authority, resolves the attributed
actor and commits the deletion request. That transaction invalidates evidence and
queues dependent lesson revocations before the provider receives a delete call.
Pass an actor supplied by your authenticated host boundary. Do not accept the
subject from an agent or an unchecked request body.

The provider implements `Tombstoner`. It must persist a permanent tombstone,
including for an absent object, so a delayed upload cannot restore the content.
Return a `DeletionReceipt` only after verifying the full reference binding,
request ID, actor and reason against that stored tombstone. An object-store 404 or
an accepted delete request does not establish this proof.

Each invocation calls the provider at most once. If the result is uncertain or
the final metadata write fails, the deletion stays pending. Call `Recover` with
the same reference to resume it, including after reopening the database. Recovery
resolves the saved actor and checks current grants; the recovering worker does
not replace that actor. Identical provider requests must be idempotent.

Caller cancellation and deadlines survive identity resolution. Provider success
followed by revocation or cancellation leaves the local deletion pending for a
later authorized reconciliation. Completed retries still check authority, but do
not repeat the provider operation.

Use `Recovery.RunPage` to discover pending deletions and expired artifacts in one
exact scope. Keep the cutoff fixed while following `NextCursor`. Rows are ordered
by their immutable registration time and reference binding, so completing a
deletion does not shift the remaining pages. After the last page, start another
sweep with a new cutoff and empty cursor to pick up failed work and records that
became eligible behind the previous cursor.

Configure the recovery runner with your host's retention identity. It uses that
identity for new expiry requests and resolves the saved original actor for pending
requests. Each item is reread before acting, then passes through `DeletionService`.
Concurrent manual deletion requests keep their actor and request identity.

A page visits at most 500 items (100 by default). Each item gets up to 30 seconds,
or a shorter timeout you configure. Providers must honor cancellation. The runner
does not launch a goroutine that abandons an ongoing provider operation. Check
every `Outcomes` entry: an individual failure stays pending while later items can
proceed. If `RunPage` returns an error, retry the same input cursor; do not advance
from a partial result.

You own scheduling, authorized scope enumeration, continuation persistence and
failure reporting. The runner starts no background service. Page size bounds
provider work, but the current list implementation still loads the whole scope
from durable storage. Bounded database queries and throughput qualification
remain open.

The built-in database content adapter and scheduled retention worker are not yet
implemented. This package does not import the unreleased root evidence APIs.
