# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [1.7.0] - Unreleased

Replaces cortex's tenant/app string pair with `cortex.Scope`, a host-defined
ordered hierarchy, closing a bug where the ReAct loop passed `""` as the
tenant on every conversation call and every tenant shared one history
bucket. This is a breaking release: hosts must migrate to `cortex.Scope`
before upgrading. See `docs/content/docs/concepts/multi-tenancy.mdx` for the
full model and a worked middleware example.

This ships inside v1 rather than as v2.0.0. The module path is
`github.com/xraph/cortex`, with no `/v2` suffix, and migrating it was
declined. Go rejects a `v2.x` tag on an unsuffixed module path, so the
breaking changes below ship as a v1 minor release instead. Read the
breaking-changes list before upgrading.

### Breaking changes

- **`memory.Store` interface reshaped.** `SaveConversation`,
  `LoadConversation`, `ClearConversation`, `SaveSummary` and
  `LoadSummaries` no longer take a `tenantID` parameter. Scope now comes
  from the context via `cortex.ScopeFromContext`, so there is nothing left
  for a caller to forget to pass — but this breaks every custom
  `memory.Store` implementation, not just the bundled ones.
- **`run.ListFilter.AppID` is gone; use `Exact` instead.** The run store
  now filters by the context's scope. `ListFilter.Exact` (a `bool`)
  controls prefix vs. exact matching against that scope, replacing the old
  `AppID` string filter.
- **`checkpoint.ListFilter.TenantID` is gone**, with no replacement field.
  Checkpoint stores now source scope from context the same way run stores
  do and always prefix-match; there is no exact-match knob for checkpoints
  in this release.
- **`cortex.WithTenant` and `cortex.TenantFromContext` are removed.** Use
  `cortex.WithScope` / `cortex.ScopeFromContext`.
- **`Run.TenantID`, `Checkpoint.TenantID` and `orchestration.Run.TenantID`
  Go struct fields are removed.** The underlying `tenant_id` database
  columns are **not** dropped this release — see "Migration notes" below.
- **`agent.ListFilter` is intentionally unchanged.** Agents (and the
  skill/trait/behavior/persona subsystems) still filter by `AppID`, not
  `Scope`. This phase closes the conversation/run scoping bug; converting
  the entity model is deferred to a later phase. Agent rows do get scope
  columns written at create for forward compatibility, but agent queries
  stay app-keyed.
- **`AppFromContext`, `WithApp` and `Config.AppID` are unchanged.** The app
  vocabulary survives this release alongside the new scope vocabulary.

### Added

- `cortex.Scope` and `cortex.Level`: a host-defined ordered hierarchy (e.g.
  workspace → project → environment). Order is significant — it determines
  which indexed column a level lands in, so a host must keep its ordering
  stable across releases.
- `cortex.WithScope` / `cortex.ScopeFromContext`: attach and read a scope on
  a `context.Context`. `WithScope` validates before storing: a `Level` with
  an empty `Key` or `Value`, or a scope deeper than 3 levels, is rejected by
  returning the context unchanged rather than storing a scope that looks
  populated and isn't discriminating (a `Level` past the indexed columns
  would otherwise be written to `scope_extra` and silently never matched as
  a predicate).
- `cortex.ErrNoScope`: returned by every scope-guarded store method when the
  context carries a zero scope, instead of silently querying across every
  tenant.
- `cortex.ParseCanonical(canon string) (Scope, error)`: reconstructs a
  `Scope` from the `scope_canon` column. New public API. Returns an error
  on a malformed segment rather than silently dropping it — an earlier,
  unreleased version of this function skipped bad segments, which would
  have reconstructed a scope narrower than what was actually stored.
- Checkpoint stores (`postgres`, `sqlite`, `mongo`) are now scope-guarded:
  `CreateCheckpoint`, `GetCheckpoint`, `Resolve`, `ListPending` and
  `CountPending` all require a non-zero context scope and filter by it,
  matching the run store. `Checkpoint.Scope` is written at create and
  reconstructed on every read.
- Mongo gained a compound `scope_l0/scope_l1/scope_l2` index on the
  agents, runs, memories and checkpoints collections, so scoped queries
  use an index instead of a collection scan.

### Fixed

- The ReAct loop's four conversation call sites (plus the dashboard and API
  memory handlers) used to pass `""` as the tenant, so every tenant shared
  one conversation history bucket. All call sites now source scope from
  context; a `scopespy` regression guard in `store/scopespy` and
  `engine/scope_test.go` fails if a future call site drops it.
- `StreamAgent`'s cancel path (both the mock/echo fallback and the real
  ReAct loop) called `UpdateRun` with the context that had just been
  cancelled, so the write failed before it started and the run stayed
  `running` forever instead of recording `Cancelled`. Both now use
  `context.WithoutCancel(ctx)` for that one terminal write.
- **Postgres: every `memory.Store` write failed outright.** `SaveConversation`,
  `SaveWorking` and `SaveSummary` never populated `memoryModel.Metadata`,
  which is a `jsonb` column; the Go zero value (`""`) is invalid JSON, so
  postgres rejected every insert with `invalid input syntax for type json`
  instead of falling back to the column's default. This backend had no
  executable scope tests before this release, which is why it went
  undetected. Fixed by running `Metadata` through the same `mustJSON`
  helper every other model in the package already uses.
- **Mongo: every `memory.Store` read after a write failed to decode.**
  The same three methods never set `memoryModel.ID`, so Mongo assigned its
  own ObjectID to `_id`, which the string-typed `ID` field can't decode
  (`decoding an object ID into a string is not supported by default`).
  Fixed by generating an `id.NewMemoryID()` explicitly, matching every
  other model in the package; `SaveWorking`'s upsert assigns it via
  `$setOnInsert` so re-saving an existing key never tries to touch the
  immutable `_id` field.

### Migration notes

- **`tenant_id` and `app_id` columns survive this release as read-only.**
  Per the design spec, dropping them in the same release as the code that
  stops writing them would be a one-way door. The columns go inert: no
  migration in this release drops them, and no code writes or reads them.
  Expect a follow-up release to remove them once hosts have migrated.
- **Pre-existing rows are NOT backfilled with a scope.** An earlier,
  unreleased version of the migration backfilled `scope_l0` from
  `tenant_id`/`app_id` (e.g. `'tenant=' || tenant_id`), but that invents a
  key vocabulary ("tenant=", "app=") distinct from what new writes use
  (a host's own keys, e.g. "workspace="), and every pre-v2 conversation
  row had `tenant_id = ''` — backfilling those into a literal `'tenant='`
  bucket would have silently orphaned all v1 history into a bucket nothing
  could ever reach. Rows created before this release are left with empty
  scope columns instead. Every scoped query already requires a non-zero
  `cortex.Scope` on the context, so a row with `scope_l0 = ''` simply never
  matches — it is invisible to scoped reads, not deleted. If old history
  matters, the host must re-scope those rows itself.
- **`cortex_orchestration_runs` carries no scope columns.** Orchestration is
  out of scope for this phase and `orchestrationRunToModel` never populated
  the indexed scope levels (only `ScopeExtra`), so the columns would have
  read as coverage that wasn't there. They were removed from the migration
  and the Go model across all three backends before this release shipped.
- **Mongo hosts: pre-`v1.7.0` `cortex_memories` documents may not decode.**
  The mongo `_id` fix above (under Fixed) closes a bug that predates this
  phase entirely — `memoryModel.ID` was never set on
  `SaveConversation`/`SaveWorking`/`SaveSummary` since that code was first
  written, so any document those methods wrote before this release has a
  Mongo-generated ObjectID `_id` that the current, correctly-typed decoder
  still rejects. In practice this should only affect development
  databases: those same pre-existing rows also predate scope and carry
  empty `scope_l0/l1/l2`, so a correctly-scoped post-upgrade read will
  never select them anyway. It only bites a deployment still running old,
  unscoped code directly against a collection that already has these
  rows. If that describes your deployment, plan to drop or reconstruct
  `cortex_memories` rather than assume old documents will read cleanly
  after upgrading.
