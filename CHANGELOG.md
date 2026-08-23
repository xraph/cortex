# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [1.8.0] - Unreleased

Finishes the scope conversion that v1.7.0 started. That release moved runs,
conversations, and checkpoints onto `cortex.Scope` but left the entity model
(agents, skills, traits, behaviors, personas) keyed on `AppID`, a separate
axis with its own context helper. This release removes that axis entirely.
`AppID` is gone from every entity, `cortex.WithApp` and
`cortex.AppFromContext` are gone from the package, and a host that still
wants an application dimension declares it as an ordinary scope level, the
same as tenant, workspace, or anything else it needs. There is one scoping
model now, not two.

As with v1.7.0, this ships inside v1 rather than as v2.0.0. The module path
is `github.com/xraph/cortex`, carries no `/v2` suffix, and migrating it was
declined. Go refuses to resolve a `v2.x` tag against an unsuffixed module
path, so a minor version is the only release channel available, and the
breaking changes below are enumerated here instead of signaled by a major
version bump. Read this whole section before upgrading.

### Breaking changes

- **`cortex.WithApp` and `cortex.AppFromContext` are removed**, along with
  the unexported `appKey` context key that backed them. Attach an `app`
  scope level with `cortex.WithScope` instead, and read it back with
  `scope.Get("app")`.
- **`AppID` is removed from every entity struct**: `agent.Config`,
  `skill.Skill`, `trait.Trait`, `behavior.Behavior`, `persona.Persona`,
  `orchestration.Config`, and `orchestration.Run`. Each of these already
  carries a `Scope` field, populated from context on create the same way
  runs and checkpoints have since v1.7.0.
- **The `appID` parameter is gone from every by-name lookup**:
  `agent.Store.GetByName`, `skill.Store.GetSkillByName`,
  `trait.Store.GetTraitByName`, `behavior.Store.GetBehaviorByName`,
  `persona.Store.GetPersonaByName`, and
  `orchestration.ConfigStore.GetOrchestrationByName` now all take just
  `(ctx, name)`. `orchestration.Service.Run` and `engine.Engine.RunOrchestration`
  drop the same parameter, now `(ctx, name, input)`.
- **`engine.Engine`'s facade methods drop `appID` too**, the same change
  applied one layer up from the stores above: `RunAgent`, `StreamAgent`,
  `CloneAgent`, `ClonePersona`, `GetAgentByName`, `GetSkillByName`,
  `GetTraitByName`, `GetBehaviorByName`, `GetPersonaByName`, and
  `GetOrchestrationByName` all take one fewer argument now, scope coming
  from context like everywhere else. `engine.Engine.RunAgent(ctx, appID,
  agentName, input, overrides)` becoming `RunAgent(ctx, agentName, input,
  overrides)` is the single most-called break in this release; if you
  only fix one call site before upgrading, fix this one.
- **Every `ListFilter` that used to carry `AppID` now carries `Exact bool`
  instead**: `agent.ListFilter`, `skill.ListFilter`, `trait.ListFilter`,
  `behavior.ListFilter`, `persona.ListFilter`,
  `orchestration.ConfigListFilter`, and `orchestration.RunListFilter`.
  `Exact` narrows a list or count to rows stored at precisely the caller's
  scope depth, instead of everything beneath it; scope itself still comes
  from context, not the filter. `checkpoint.ListFilter` is unaffected. It
  never carried `AppID` and still has no `Exact` field: checkpoints always
  prefix-match, a v1.7.0 decision this release does not revisit.
- **Uniqueness moved from `(app_id, name)` to a partial
  `UNIQUE (scope_canon, name) WHERE scope_canon != ''`** on
  `cortex_agents`, `cortex_skills`, `cortex_traits`, `cortex_behaviors`,
  `cortex_personas`, and `cortex_orchestration_configs`, across all three
  backends. The `WHERE scope_canon != ''` clause matters: it excludes rows
  left unscoped by this release's migration (see "Migration notes") from
  the uniqueness check entirely, rather than letting every one of them
  collide on the empty string.
- **`store.Store.Migrate` and every backend's `Migrate` are now variadic**:
  `Migrate(ctx context.Context, opts ...cortex.MigrateOption) error`. A
  custom `store.Store` implementation with the old `Migrate(ctx) error`
  signature no longer satisfies the interface; add the `opts` parameter
  even if your implementation ignores it.
- **`engine.Engine.BuildSystemPrompt` now returns `(string, error)`**,
  where it used to return just `string`. A requested persona, skill, or
  trait that fails to resolve now aborts the whole call instead of
  silently dropping its fragment from the prompt: an agent that can't
  become who it's configured to be should fail loudly, not quietly
  degrade with no signal anywhere.
- **`orchestration.AgentRunner.RunAgent` drops its `appID` parameter**,
  now `(ctx, agentName, input, opts)`. Every bundled strategy
  (sequential, parallel, router, hierarchical, debate) drops the matching
  `appID` field it carried internally, and `orchestration.Build` drops
  its own `appID` parameter to match. None of these ever read the value
  they carried once the engine's adapter started discarding it with `_`,
  so removing it is a signature change with no behavioral change behind
  it.
- **`sentinel.WithCortexAppID` is removed**, along with the `cortexAppID`
  field it set on `pluginOptions`. `sentinel.NewAgentClient` drops its
  trailing `appID` parameter too, now just `NewAgentClient(eng)`; that
  parameter had already been reduced to `_` internally for one release,
  so this finishes a removal that was already underway.

### Added

- `cortex.Rescoper`: the interface a host implements to map a legacy
  `(appID, tenantID)` pair onto a `cortex.Scope` during migration. See the
  [rescope guide](/docs/guides/v1.8-rescope) for a worked implementation.
- `cortex.MigrateOption` and `cortex.WithRescoper(r)`: the option type and
  constructor for passing a `Rescoper` into `Migrate`.
- `cortex.ApplyMigrateOptions(opts ...MigrateOption) MigrateOptions`: folds
  variadic options into a value; a zero `MigrateOptions` is valid; a fresh
  install has no unscoped rows and never calls the rescoper.
- `cortex.ValidateRescopedScope(s Scope) error`: applies the same rules
  `WithScope` applies to a scope returned from a `Rescoper`, so a buggy
  implementation can't write a scope the rest of the system would refuse
  to construct in the first place.
- `cortex.ErrNoRescoper`: returned when a migration finds unscoped rows
  and no `Rescoper` was supplied. The migration aborts before writing
  anything rather than guessing.

### Migration notes

- **Hosts relying on per-app Shield policies must declare an `app` scope
  level.** `safety.ScanRequest.AppID` is now sourced from the host's own
  `Scope`, specifically from an `app` level if one exists
  (`scope.Get("app")`). A host with no such level gets the same canonical
  string already used for `TenantID`, which collapses the app and tenant
  dimensions Shield treats as distinct. Add an `app` level to your
  `scope_levels` before upgrading if per-app Shield policies matter to
  you, or they will silently stop matching.
- **Rows written before this release carry empty scope columns**, and
  stay that way after upgrading unless you rescope them. Every
  scope-guarded query already requires a non-zero `cortex.Scope` on the
  context and filters by it, so a row with `scope_canon = ''` is simply
  invisible to a scoped read. It is not deleted, and it is not
  reachable until you supply a `cortex.Rescoper` and run the migration
  with `cortex.WithRescoper(r)`. Read the
  [rescope guide](/docs/guides/v1.8-rescope) before upgrading a database
  that has agents, skills, traits, behaviors, or personas created before
  v1.8.0.
- **`app_id` and `tenant_id` columns are still not dropped.** Per the
  v1.7.0 note, dropping them in the same release as the code that stops
  reading them would be a one-way door, and this release's own migration
  still reads `app_id`/`tenant_id` off unscoped rows to feed your
  `Rescoper`. Expect a later release to drop them once hosts have
  finished migrating.

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
- **`AppFromContext`, `WithApp` and `Config.AppID` are unchanged in this
  release** — the app vocabulary survives here alongside the new scope
  vocabulary. That does not last: the scope-completion work that follows
  this release removes `AppFromContext` and `WithApp` outright, along
  with the `AppID` field on every entity and the `appID` parameter on
  every by-name lookup. A host wanting an app dimension declares it as a
  scope level instead.

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
- **Hosts relying on per-app Shield policies must declare an `app` scope
  level.** Once `AppFromContext`/`WithApp` are removed, `safety.ScanRequest.AppID`
  (and the `shield.WithApp` dimension it feeds) is sourced from the host's own
  `Scope` — specifically from an `app` level, if one exists (`scope.Get("app")`).
  A host with no such level gets the same canonical string already used for
  `TenantID`, which collapses the app and tenant dimensions Shield sees as
  distinct. A host with per-app Shield policies must add an `app` level to its
  `scope_levels` before upgrading, or those policies silently stop matching.
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
