# Governed lessons

Use `NewLessonStore` when an ordinary run's verified result should become reusable
knowledge. A completed run alone produces an observation. Failed runs produce
`failed_approach` observations, and evaluation, optimization trial and controller
runs stay out of the ordinary learning plugin.

The root `learning` package owns the shared lesson value and digest. Optimization
keeps its cohort evidence rules and publication lifecycle. Both adapters use the
same Fabriq schema, strict toolkit and authoritative write reconciliation.

## Connect a host

You provide these dependencies explicitly:

- A root SQLite, Postgres or Mongo store implementing `learning.Repository` and
  `verification.ResultContentStore`.
- A `learning.Authorizer` for current read, publish, revoke, retry and delete
  grants. It must resolve the caller from trusted context and enforce exact scope.
- Your existing result content authorizer, synchronous commit fence and clock.
  The fence must hold current source, policy and authority revisions while its
  callback runs. A callback that simply echoes the supplied binding is only a
  test fixture.
- A tenant mapper that preserves the host's scope boundary, plus a real embedder
  and a physically provisioned entity from `LessonSpec("qualified_lessons")`.

Construct a `learning.Guard`, then `NewLessonToolkit` and `NewLessonStore` with
those dependencies. Run `CheckReady` against the worker's scoped context. It checks
physical columns and strict recall, but does not perform a test write or establish
historical retention support.

To expose lessons to an agent, construct `NewLessonProvider(adapter, domain)` and
pass it to `engine.WithKnowledge`. The domain is fixed at construction. Retrieval
returns each statement with its applicability, contraindications and report refs;
it does not merge conflicting lessons. Collection overrides cannot broaden that
entity, and score thresholds are rejected because this adapter does not expose a
calibrated relevance score.

## Accept and deliver

Call `guard.Accept` with a new `lesson` ID, domain, applicability, contraindications,
expiry, trusted audit actor and `verification.ResultReadRequest`. The repository
loads the saved result itself. The statement must be the exact verified output,
and expiry cannot outlive any source report. A derived claim needs verification
of its own. A run that consumed recalled lessons cannot produce an ordinary
validated lesson, even when its verification result passes. The engine records
exact recalled lesson identities before exposing their content. Acceptance and
recall check that saved provenance, so omitting sources from a report cannot
remove the restriction. Conversation history inherits the same restriction;
history without a verifiable origin also makes the run ineligible.

Acceptance and its outbox commit together. Construct `learning.NewPublisher` with
the repository, adapter, guard and worker ID, then schedule `WorkPage` in each
exact scope. Pass its cursor until the page is exhausted and restart the scan on
the next cycle. Claims have a 30-second lease, delivery calls are bounded, and each
cycle permits five attempts. Failures remain visible in the publication record.
An operator with the current retry grant can use `Publisher.Retry` at the expected
generation.

Every retry uses the same remote aggregate ID and expected version. The adapter
reads authoritative state after ambiguous writes, so a lost success response does
not create another lesson. Source evidence and host grants are checked again
around delivery and recall. An outage never grants eligibility.

## Correct, withdraw and delete

Set `AcceptRequest.Corrects` to the old lesson's exact ID, revision and digest to
publish a correction. The transaction accepts the replacement, withdraws the old
revision and saves both outboxes. The caller needs permission to publish the new
lesson and revoke the old one. Local withdrawal takes effect before Fabriq catches
up, including when its vector index still contains the old row.

`Guard.Revoke` records an explicit withdrawal. `LessonAdapter.Revoke` also attempts
its remote delivery. Neither operation needs the old source content to remain
available. They keep history for audit.

Use `LessonAdapter.Delete` for a host-authorized retention decision. Supply a
bounded reason code such as `retention_expired`. Local deletion replaces every
lesson, publication and delivery body with an empty body in one transaction, then
keeps a content-free tombstone with the identity, digest, actor, reason and time.
The identity cannot be accepted again. The adapter also removes the active Fabriq
row and vector. Schedule `SweepDeleted` over durable tombstones to resume cleanup
after an outage or restart. The retention worker needs its own current delete
grant; the original publisher's old permission is insufficient.

Fabriq core v1.10.0 supplies `command.AggregateEraser`. When your installed facade
implements that capability, configure it with
`fabriq.WithErasureAuthorizer`. That authority must hold a current deletion grant
for the exact tenant, entity, aggregate and Fabriq scope throughout its callback.
The adapter submits `ExpectedVersion: 0` after deleting the row and always uses
`cortex_retention` as the remote reason. Retries reconcile that same request and
receipt, including after a lost acknowledgement. The default `LessonSpec` uses
Cortex's `scope_key` and has no native Fabriq `scope_id` column. Its native Fabriq
scope must be empty. If you provision a scoped entity, its retained rows and
projections must carry that same scope. The adapter never removes a caller's
native scope to make erasure succeed.

`LessonDeletion.LocalPayloadsErased` confirms four named local targets: the
aggregate row, command outbox payloads, embeddings and spatial rows. `LocalErasure`
contains the checked receipt. Fabriq keeps a content-free tombstone that blocks
writes under the erased identity. A missing grant returns a denial; a changed
target returns a conflict. A lost acknowledgement stays unavailable until your
worker reconciles the same request under current authority. Older transports
without that capability still report active row/vector removal only.

`HistoryErased` remains false. The receipt lists streams, graph, search, cache,
live subscribers and backups as unconfirmed. Source result artifacts, database
backups and transaction logs follow their own authorized retention policies;
deleting a lesson does not change those policies or erase delivered remote copies.

## Verification

The shared store conformance suite exercises acceptance, outbox claims, local
withdrawal, correction, source deletion, retention tombstones and purpose checks.
It runs on SQLite and real Postgres and Mongo containers. The integration test
`TestQualifiedLearningInstalledFabriq` uses the installed Fabriq API and a real
Postgres destination to check restart, lost responses, guarded retrieval,
prepared and runtime engine recall, rejection of promotion from recalled context,
correction, stale recall, active-copy deletion and idempotent cleanup.
`TestQualifiedLessonRetentionInstalledFabriq` checks the optional erasure capability,
current grants, lost row-delete and erasure acknowledgements, exact retry receipts,
local outbox/vector cleanup and irreversible identity fences. These are
local integration results, not live host qualification.
