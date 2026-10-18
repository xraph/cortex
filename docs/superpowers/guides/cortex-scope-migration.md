# Scope validation and legacy inventory

Cortex supports one through three ordered scope levels. Keys must be unique. Keys and values must contain non-whitespace text and cannot contain `/` or `=`. Level order remains part of identity. `ValidateScope` returns `ErrNoScope` for an empty scope, `ErrInvalidScope` for an ambiguous identity and both `ErrInvalidScope` and `ErrUnsupportedScopeDepth` for a deeper hierarchy.

`WithScope` copies the supplied levels. An invalid scope masks the parent context; it never falls back to the parent's authority. `ScopeFromContext` returns zero for an invalid identity. Use `RequireScope` when you need the typed validation error. Returned levels are detached, so changing them cannot change another operation's identity.

Valid prefix lists still let an authorized workspace caller list its projects. Updates, deletes, claims and execution require the resource's exact scope. A parent context is no longer a grant to mutate a child. Hosts must resolve the resource, authorize the caller against that exact identity, then attach the authorized scope for the operation. Never construct this authority from an unverified request body. Existing tool authorization still runs at effect time.

Prepared artifacts with invalid or mismatched agent scope are rejected. Create a fresh prepared version under the reviewed exact scope and evaluate it again. Do not patch serialized scopes or reuse acceptance results from an earlier identity.

## Inventory

Every built-in backend implements `store.ScopeInspector`. Its `InspectScopes(ctx, visit)` method streams `store.ScopeIssue` records containing the resource, ID, canonical scope, indexed values, overflow data and reason. It reads scope metadata only and never changes a row. Callback errors and cancellation stop the scan. PostgreSQL and SQLite discover scoped tables from their catalogs, including pending asks. Mongo scans the current Cortex entity collections in bounded batches.

This is a database maintenance capability across all scopes. Call it from an operator-authorized process with access to the database, not an ordinary application request. There is no public inventory HTTP route. Store the report in the host's restricted audit system. Run ordinary schema migrations first; an incomplete SQL schema stops inventory rather than producing a partial clean report.

Normal entity reads reject records whose canonical identity is invalid or disagrees with indexed columns. Overflow records cannot be updated or claimed through exact-scope operations. A list that encounters invalid identity may return an error, so inventory and reconciliation are part of upgrade preparation. Inventory is a read of current rows, not a consistent database snapshot; pause relevant writers or run it against a consistent backup when you need an authoritative migration report.

## Operator-authorized reconciliation

1. Take and verify a restorable backup. Pause writers affecting the identities being reconciled. Save the inventory and database/schema version.
2. Produce an explicit mapping from each resource ID and its existing stored identity to its intended supported scope. The host owner approves the mapping and the resulting grants. Never drop a fourth level or pick an owner from a matching prefix.
3. Dry-run the complete mapping. Check per-scope name uniqueness, working-memory keys and overlay uniqueness. Include related runs, steps, tool calls, sessions, checkpoints, suspensions and A2A records so references keep the same authorized ownership.
4. Apply the approved mapping through a host migration under a database transaction or a documented recovery protocol. Update canonical and indexed identity together and clear overflow only after its meaning is represented in the approved mapping. Record before/after identity, operator, mapping revision and affected resource IDs in the host audit trail. Abort on a changed source row or a collision.
5. Re-run inventory. Verify expected reads and denied mutations with two principals and two scopes, plus restart and resume for affected runs. Restore writers only after the checks pass. Restore the backup or use the approved rollback mapping if reconciliation fails.

This packet supplies validation and inventory. It does not run a repair migration or infer the host's authorization policy.
