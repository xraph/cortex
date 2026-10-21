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

This package currently provides lifecycle coordination. The built-in database
content adapter, pending-deletion scan and scheduled retention worker are not yet
implemented. It does not import the unreleased root evidence APIs.
