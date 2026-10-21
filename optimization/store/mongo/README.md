# MongoDB optimization storage

Pass an open Grove MongoDB connection to `mongo.New(db)` and call `Migrate(ctx)`
before accepting work. You need a deployment that supports snapshot transactions,
such as a replica set. Migration performs a real transactional query and refuses
a standalone server. Closing the store closes the connection you supplied.

Each update writes an exact-scope fence before reading state. A competing worker
receives `ErrConflict` if it cannot acquire that fence, without entering its
callback. The adapter never replays a callback. Transactions explicitly use
primary reads, snapshot isolation and majority writes, even when your host uses
another default read preference.

If commit acknowledgment fails, the adapter returns `ErrUnavailable`. That error
does not establish whether the commit happened. Your service must reconcile the
persisted operation identities before deciding whether another attempt is safe.

Record metadata and body chunks commit together. Bodies use chunks of at most
4 MiB, so a valid encoded record can exceed MongoDB's single-document limit.
Replacement checks the previous digest and removes superseded chunks in the same
transaction. Reads use one snapshot across the metadata and chunk collections,
then verify chunk identities, order, sizes, scope and the reconstructed content
digest before applying the shared storage rules.

Migration creates the scope collection before accepting work. Creating it inside
the first transaction would let competing transactions use different catalog
snapshots, defeating the scope fence. Concurrent migrations are supported;
unknown schema versions refuse startup.

The tests start real MongoDB containers and fail if Docker is unavailable. You
can select a compatible image with `CORTEX_TEST_MONGO_IMAGE`. Coverage includes
standalone refusal, concurrent startup, independent connections, subprocess lease
takeover, a 17 MiB record, corrupt or missing chunks, rollback after a database
write failure and retries with record writes forbidden.

The adapter currently loads the complete exact scope, and each store instance
serializes its own operations. Selective loading, bounded database pagination,
artifact retention and throughput qualification remain open. These storage checks
do not qualify the optimizer's runtime or production activation path.
