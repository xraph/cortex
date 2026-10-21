# SQLite optimization storage

Open a Grove SQLite connection, pass it to `sqlite.New(db)`, then call
`Migrate(ctx)` before accepting work. Set WAL mode and a busy timeout in your
connection string:

```text
optimizer.db?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)
```

Use a separate connection when another service owns its lifecycle. Closing the
optimization store closes the Grove connection you supplied.

Every read uses the exact context scope. Writes acquire SQLite's database write
lock before reading current state, apply the shared optimization rules, and
commit changed records with their audit entries. The transaction checks each
record's previous digest before updating it. A retry that changes nothing does
not rewrite the record history.

Records carry a schema version, their original compound identity and a content
digest. Each scope has a bounded hash key, with the complete canonical scope
checked separately. This allows long scope values without using them as primary
index keys. The content digest detects inconsistent stored bytes; it is not a
signature against a database administrator who can rewrite both content and
digest.

Migrations are transactional and repeatable. An unknown schema version refuses
startup. Unknown record kinds, unsupported record versions and malformed stored
content refuse reads and writes, so an older worker cannot silently discard
fields it does not understand.

Single-resource getters use the scope and record primary key, then validate the
selected record's identity, schema and content digest. Lists, current activation
eligibility and updates still load a complete exact scope. Large scopes increase
the cost of those operations. Bounded database pagination, selective transaction
loading and throughput qualification remain part of Task 4. SQLite also permits
one database writer at a time, even when workers operate in different scopes.

The database artifact adapter and service recovery remain in progress.
PostgreSQL and MongoDB have separate adapters. This store does not establish sandbox,
evaluation or production activation readiness.
