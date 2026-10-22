# PostgreSQL optimization storage

Pass an open Grove PostgreSQL connection to `postgres.New(db)`, then call
`Migrate(ctx)` before accepting work. Closing the store closes that connection.
Use a separate connection when another service owns its lifecycle.

PostgreSQL and SQLite share record encoding, persistence rules and transaction
validation. PostgreSQL uses numbered parameters and `BYTEA` for record content.
Each update explicitly selects `READ COMMITTED`, locks the exact scope row before
reading state, and checks the previous digest of every changed record. The
callback runs once. The adapter does not replay it after a database failure.

The scope lock is held until commit or rollback. Workers with separate
connections can continue in other scopes while one scope is locked. A cancelled
worker waiting for that lock never enters its callback. One store instance can
serve independent scopes concurrently. Migration and close wait for active
operations before accessing the connection exclusively.

Migrations acquire a transaction-scoped advisory lock before creating schema
objects. This matters during concurrent startup: PostgreSQL's `IF NOT EXISTS`
does not prevent all catalog races. Unknown schema versions refuse migration.

The PostgreSQL tests use real `postgres:16-alpine` containers and fail when the
container cannot start. They include the populated shared conformance suite,
concurrent migrations, independent worker claims, a host whose default isolation
is repeatable read, cancellation while waiting for a scope lock, rollback after
a database write failure and lease takeover from a fresh process. SQLite uses
the same subprocess test helper.

Single-resource getters use the scope and record primary key, then validate the
selected record's identity, schema and content digest. Lists, eligibility checks
and updates still load a complete exact scope and updates write only changed
records. Database pagination, selective transaction loading, artifact retention
and throughput qualification remain open. These storage tests do not establish
production optimization readiness.
