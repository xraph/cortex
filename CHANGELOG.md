# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [1.12.0] - Unreleased

A system prompt stops being one opaque string. It's an ordered set of
addressable sections now, and a persona, its skills and its traits emit
`prompt.Section` values instead of string fragments. A `prompt.Overlay`
stored at a scope patches those sections by id, so a tenant can rewrite
one piece of an agent's prompt without the platform handing over the
whole thing. The overlay carries per-scope run settings too: `Model`,
`Temperature`, `MaxTokens`, `ToolsAdded` and `ToolsRemoved`.

Overlays are inherited by walking a run's scope ancestry, broadest
first, so the narrowest one patches last and wins. A `Locked` section
refuses a `replace` patch and still accepts an `append`, which is the
pair that lets you pin a safety preamble and still let a tenant extend
it.

**Your prompts do not change.** An agent that only ever set
`SystemPrompt`, with a persona, skills and traits behind it, assembles
to a byte-identical string. That is pinned by a golden test,
`engine.TestBuildSystemPrompt_MatchesTheLegacyPrompt`, whose literal was
captured by running the previous release's `BuildSystemPrompt` before
any producer was touched. It calls the real pipeline, so a reordering
anywhere inside assembly fails it. It's the first
question every upgrading host asks, and the answer is a test rather than
a promise.

Two byte-level divergences sit outside that promise, and if you diff
prompts across the upgrade you'll see both. An agent with an **empty**
`SystemPrompt` used to get a leading newline, because the persona chunk
was then the first part and every part after the first began with one.
Section assembly starts at `## Identity` instead. And a knowledge block
used to be followed by three newlines where every other part had two,
because the block ended its last bullet with a newline and the join then
added its own on top; a section carries no trailing separator, so a
prompt with knowledge in it loses one blank line.
`engine.TestBuildSystemPrompt_KnowledgeBlockLosesOnlyItsTrailingNewline`
pins the legacy string and tolerates exactly that one byte.

`SystemPrompt` and `Sections` can both be set, so they need a stated
precedence, and it's this: sections are the truth. A non-empty
`Sections` derives `SystemPrompt` from itself and overwrites whatever
you put there. An empty `Sections` with a non-empty `SystemPrompt`
lowers into a single section with id `"role"`, which is what keeps every
agent written before this release assembling the way it always did, and
what gives an overlay an address to reach it by.

As with v1.7.0 through v1.11.0, this ships inside v1 rather than as
v2.0.0. The module path is `github.com/xraph/cortex`, carries no `/v2`
suffix, and migrating it was declined. Go refuses to resolve a `v2.x`
tag against an unsuffixed module path, so a minor version is the only
release channel available, and the breaking changes below are enumerated
here instead of signaled by a major version bump. Read this whole
section before upgrading.

### Breaking changes

- **`store.Store` now embeds `prompt.Store`**, seven additional methods
  (`CreateOverlay`, `GetOverlay`, `GetOverlayForAgent`,
  `GetOverlayForAgentAt`, `UpdateOverlay`, `DeleteOverlay`,
  `ListOverlays`) on top of everything the composite already required. A
  custom `store.Store` implementation (see
  `docs/content/docs/guides/custom-store.mdx`) no longer satisfies the
  interface until it implements these too. The three bundled backends
  already do.

  Two of those seven are the ones to read twice. `GetOverlayForAgent`
  matches the caller's scope **exactly** and `GetOverlayForAgentAt`
  matches a named ancestor of it exactly. Neither prefix-matches, unlike
  almost every other read in this codebase, and that asymmetry is
  deliberate, not an oversight. `store/storetest`'s conformance suite
  covers all seven and asserts the exact-match behavior, so changing it
  later takes editing a test on purpose. Run it against your backend.
- **`agent.Config` gained `Sections []prompt.Section`**, and
  `SystemPrompt` is a derived field whenever that slice is non-empty.
  If you read `SystemPrompt` expecting it to be the authoritative
  string a host wrote, you now get a value assembled from the sections,
  and a write of your own to that field is overwritten, not merged.
  Nothing changes for an agent with no sections: there the string is the
  source, and the derivation is a no-op.

  `Engine.CreateAgent`, `Engine.UpdateAgent` and `Engine.CloneAgent`
  call `Config.SyncSystemPrompt()` before the store write to keep the
  column in step. If you write agents straight through `store.Store` and
  skip the engine you get no sync, so call it yourself or the stored
  string drifts from the sections it claims to come from.
- **`suspension.RunConfig` gained `ToolsRestricted bool`.** A resumed
  run rebuilds its config from the continuation and from nowhere else,
  so the flag that says "this empty tool list is a decision" has to
  travel with it. Without that, a pause and resume would undo an
  overlay's withdrawal and hand the run back more tools than it was
  suspended with. The field is `omitempty` and decodes as `false` on
  rows written before it existed, which is the behavior those runs
  already had. It can therefore be lost across an upgrade and can never
  be spuriously set, which is the safe direction: losing it resumes an
  old run under the rules it was suspended under, where inventing it
  would strip tools from a run nobody restricted.
- **Two migrations on Postgres and SQLite.** `20260826000001`
  (`create_overlays`) creates `cortex_overlays`, scoped from birth, with
  a partial unique index on `(agent_id, scope_canon)`. `20260826000002`
  (`add_agent_sections`) adds a `sections` column to `cortex_agents`,
  `NOT NULL DEFAULT '[]'`. Mongo has no counterpart to the second
  because it has no DDL, and gains the overlay collection's indexes
  through `Store.Migrate()` like every other collection.
- **A prompt with retrieved knowledge in it loses one blank line**, as
  described above. If you pin assembled prompts in your own tests and
  your fixture has a skill with a `KnowledgeRef` behind it, that's the
  diff you'll see, and it's the only one.

### Added

- **The `prompt` package.** `prompt.Section` is one addressable piece of
  a system prompt: an `ID`, an informational `Source`, an optional
  `Title` prefixed on assembly, a `Body`, an `Order`, and `Locked`.
  `prompt.Patch` is an overlay's change to one section, matched by `ID`,
  applied in `PatchReplace` or `PatchAppend` mode. `ApplyOverlay` runs
  the algebra and returns the resulting sections **plus the patches it
  declined**, which is the second return value and is not to be
  discarded. `Assemble` sorts by `Order`, breaks ties by `ID`, and joins
  with a blank line. The package is pure: no store, no logger, no engine.
- **`prompt.Overlay` and `prompt.Store` on the composite store**, backed
  by a new `cortex_overlays` table on Postgres and SQLite and a
  collection of the same name on Mongo. An overlay is a per-scope delta
  on one agent, never a replacement, so deleting it restores the agent's
  own behavior exactly. One per agent per scope. Its `Scope` is stamped
  from the creating context and is immutable afterwards, because letting
  an update move it would silently retarget an overlay somebody already
  approved.
- **`Sections(order int) []prompt.Section` on `Persona`, `Skill` and
  `Trait`.** The order is a parameter rather than a fixed band because
  assembly falls back to sorting by `ID` when two sections share an
  order, and a fixed band would put an agent's skills in alphabetical
  order instead of the order the agent lists them. A caller walks a list
  starting at the band and advancing by the number of sections each
  producer actually returned. A persona with no `Identity`, a skill with
  no fragment and a trait with no prompt injection each return nothing,
  so the counts do not line up with the list lengths.
- **`agent.Config.PromptSections()`, `agent.Config.SyncSystemPrompt()`
  and `agent.RoleSectionID`.** The first two are the two directions of
  the precedence rule above. `RoleSectionID` is `"role"`, the id a plain
  `SystemPrompt` lowers into and therefore the address an overlay uses
  to reach a legacy agent's instructions.
- **`cortex.Scope.Covers(other Scope) bool`**, reporting whether the
  receiver is `other` or an ancestor of it. A scope covers itself. This
  asks "is s somewhere in other's own ancestry", which is the upward
  question, and it is deliberately not the downward prefix matching the
  stores do on a list.
- **`cortex.ErrOverlayNotFound`**, returned when an agent has no overlay
  at the scope asked for. That is the ordinary case for most agents on
  most runs, so the ancestor walk treats it as "keep going", not as a
  failure.
- **`id.OverlayID`, `id.NewOverlayID` and `id.ParseOverlayID`**, prefix
  `ovl`, the same identifier shape every other entity has.

### Changed

- **Overlay inheritance is an explicit ancestor walk, not prefix
  matching.** A run at `[workspace=A, project=B]` asks
  `GetOverlayForAgentAt` for `[A]`, then for `[A, B]`, and applies what
  it finds in that order. At most one lookup per scope level.

  The obvious alternative is wrong in a way that reads fine at the call
  site. Prefix matching in this codebase widens **downward**, so listing
  overlays from `[A]` hands back every project's overlay inside that
  workspace, and feeding those into one run mixes a sibling project's
  instructions and tool grants into a run that must never see them.
  `engine.TestBuildSystemPrompt_ASiblingProjectsOverlayNeverApplies` is
  the test that catches it, and swapping the walk for a listing
  reproduces the leak in one line.

  Broadest first comes out of the walk itself, never from a sort
  applied afterwards: the loop index is the scope depth. One walk per
  run feeds both the prompt and the run config, so the two can never
  disagree about which overlays applied.
- **A declined patch is logged, not raised.** A `replace` against a
  `Locked` section is dropped and the run proceeds with the section as
  the host wrote it. The engine logs it at Warn as `prompt overlay
  patches declined`, carrying `overlay_id`, `agent_id`, `scope`, and a
  comma-separated `sections` field naming exactly which patches were
  dropped. If a host swears its overlay is not taking effect, that line
  is the first place to look.
- **A `Mode` outside `replace` and `append` is declined, not applied.**
  The check on a `Locked` section now names the one mode it accepts.
  Written the other way round, as a list of what to refuse, it left
  every value nobody had thought of permitted. A `PatchMode` is a bare
  string persisted as JSON with nothing validating it on the way in, so
  `"Replace"` or a plain typo sailed past the refusal and overwrote the
  body a host had pinned. The same validation runs on unlocked sections,
  for a different reason: a mode this build cannot apply is as likely to
  be a typo as an intention, and reading it as a replace would turn
  every mode name somebody invents later into a destructive operation
  against today's code. An empty `Mode` still means `replace`, which is
  what every patch written before `Mode` existed depends on. A patch
  declined this way creates no section either, so a garbage mode against
  an id nobody emitted adds nothing to the prompt.
  `prompt.TestApplyOverlay_UnrecognizedModeCannotTouchALockedSection`
  and `prompt.TestApplyOverlay_ModeIsValidatedOnUnlockedSectionsToo`
  cover both halves.
- **A created section lands last, and `Patch` has no `Order` field.** A
  patch whose id matches no section becomes a new section rather than an
  error, and it goes after everything already in the set; several of
  them in one overlay come out in patch order. That position is a
  security property, not a gap waiting to be filled. A created section
  sitting at order 0 would land ahead of every producer band, so an
  overlay naming an id nobody emitted could slide text above a `Locked`
  safety preamble without ever replacing it, and the lock would hold
  while being outranked. Adding `Patch.Order` reopens that. Position is
  the host's call. Put the section on the agent with the order you want,
  then patch it from the overlay by id.
- **A per-run `SystemPrompt` override replaces the agent's whole
  contribution**, its stored `Sections` included, and arrives as the
  `role` section so an overlay targeting `role` still reaches it. That
  is what overriding a prompt for one run has always meant.

  Read that alongside `Locked`, because the two meet and the answer is
  not the one a host guesses. Locked sections go with the rest, and what
  the caller passed comes back as a single unlocked `role` section. The
  lock covers what an overlay can do and stops at the edge of it. If you
  need a preamble held against a run's own caller too, hold it at
  whatever API boundary lets that field be set. The `Locked` doc comment
  and the guide both say so now.
- **Skills are resolved once per prompt build instead of twice.** The
  old code looked a skill up for its fragment, aborting on error, and
  again for its knowledge references, silently skipping on error. The
  second lookup could never see an error the first had not already
  aborted on, so folding them removes a lookup whose error handling
  contradicted its sibling. No behavior change, one less round trip per
  skill.

### Fixed

Three holes in tool resolution, each of which turned a removal into a
grant. All three are reachable from an ordinary overlay and none of them
needed anything unusual to trigger.

- **An agent that named no tools was immune to `ToolsRemoved`.** An
  empty `agent.Config.Tools` means "every registered tool", so
  subtracting from it subtracted from an empty list and did nothing at
  all, silently. The implicit list is now written out before a delta
  applies to it.
- **Withdrawing the last tool granted every tool.** Tool resolution read
  an empty name list as "no filter", so an overlay that removed an
  agent's only tool produced an empty list, which read as every
  registered tool. A removal became the broadest possible grant. An
  explicit restriction flag now distinguishes "nobody named a list" from
  "somebody named one and it is empty".
- **A narrower overlay adding one unrelated tool re-granted everything.**
  The materialize step above asked only whether the list was empty, not
  whether an overlay had already emptied it, so the flag guarded
  resolution but not the next iteration of the loop it sat in. Agent
  `["alpha"]`, a workspace overlay removing `alpha`, a project overlay
  adding `beta`, and the run received `[alpha beta gamma]`: the tool the
  workspace explicitly withdrew, plus every registered tool nobody
  named. Once an overlay has spoken, an empty list is a decision and
  stays one.

  `engine.TestEffectiveConfig_ToolDeltasResolveAsDocumented` covers all
  three, plus the six other combinations around them.

### Known limitations

- **There is no HTTP endpoint and no dashboard screen for overlays in
  this release.** You write them through the composite store, which you
  reach with `eng.Store()`. Nothing's hiding behind a route you haven't
  found.
- **Tool deltas resolve narrowest-wins, and that is a policy choice with
  a stated limit.** A narrower scope can withdraw what a broader one
  granted and can re-grant what a broader one withdrew, provided it
  names the tool. A workspace-level removal is therefore not a floor a
  project overlay is unable to lift. That default is right while
  overlays are written by the host at scopes the host controls, which is
  the only way they can be written today. It'd be the wrong default the
  moment you let tenants author overlays in their own sub-scopes, since
  a tenant could then restore a tool you withdrew above them. Delegating
  would need broader removals to become floors, and that is a policy
  change, not a bug fix.
- **A resumed run keeps the overlay settings it was suspended with**,
  the same way it keeps the prompt it was suspended with. Fix an overlay
  in the middle of an outage and a run that is already paused will not
  see the fix until it is started again.

### Migration notes

- **`sections` backfills to `[]` and nothing copies `system_prompt` into
  it.** The column is added `NOT NULL DEFAULT '[]'`, and on both SQL
  backends an `ADD COLUMN` with a non-null default fills every existing
  row as part of the statement. So the moment the migration finishes,
  every agent written before this release has an empty section list and
  a `system_prompt` untouched byte for byte, which is exactly the state
  the compatibility promise needs. Backfilling a synthetic section
  wrapping the old string would have been worse than useless: it is only
  byte-identical if the title and ordering are exactly right, and it
  would make every legacy agent's prompt patchable by an overlay its
  owner never opted into.
- **The overlays table is created scoped and needs no backfill.**
  Overlays are new this release, so there is no unscoped legacy shape to
  carry forward. The unique index is partial on `scope_canon` on all
  three backends, following the convention every other index here uses.
- **Read the guide before you write your first overlay.**
  `docs/content/docs/concepts/prompt-composition.mdx` covers what a
  section is and how to address one, what the producers emit, writing an
  overlay, what `Locked` means and where a declined patch shows up, the
  ancestor walk and why a sibling's overlay never reaches you, and the
  tool delta rules with a worked two-level example.

## [1.11.0] - Unreleased

Adds run suspension. A run can now stop in the middle of a step, persist
everything it needs to carry on, and be picked up later by whoever it was
waiting for. One primitive serves two reasons. There's the tool the
caller executes and the engine doesn't, registered with
`engine.WithExternalTool`, and there's the tool call a human has to
approve, escalated by an authorizer returning
`cortex.ErrRequiresApproval`. Both pause the loop the same way, both write
one `suspension.Suspension` row, and both come back through
`Engine.Resume`. An expiry sweeper fails a run whose resumer never came
back, so nothing sits paused forever.

`ErrRequiresApproval` completes the authorizer's three outcomes. v1.10.0
shipped allow and deny, and said in its own changelog and in
`docs/content/docs/concepts/authorization.mdx` that the third was arriving
here. It was held back on purpose. With nowhere to suspend into, an
authorizer returning it would've read as an ordinary denial, and "ask
somebody" would have quietly become "no".

Two things that already existed finally do something. `run.StatePaused`
has been declared since v1.x and had never once been assigned; the engine
assigns it now. Checkpoints have had an entity, a store, REST endpoints
and two plugin hooks for just as long, and nothing in the tree ever
created one, so `EmitCheckpointCreated` had exactly one occurrence before
this release and that was its own definition. An approval pause writes a
real checkpoint, `ListPendingCheckpoints` returns it, and
`ResolveCheckpoint` carries the decision back to the run it belongs to.

As with v1.7.0 through v1.10.0, this ships inside v1 rather than as
v2.0.0. The module path is `github.com/xraph/cortex`, carries no `/v2`
suffix, and migrating it was declined. Go refuses to resolve a `v2.x` tag
against an unsuffixed module path, so a minor version is the only release
channel available, and the breaking changes below are enumerated here
instead of signaled by a major version bump. Read this whole section
before upgrading.

### Breaking changes

- **`store.Store` now embeds `suspension.Store`**, seven additional
  methods (`CreateSuspension`, `GetSuspension`, `ClaimSuspension`,
  `DeleteSuspension`, `ListExpired`, `ListExpiredAcrossScopes`,
  `ClaimExpiredSuspension`) on top of everything the composite already
  required. A custom `store.Store` implementation (see
  `docs/content/docs/guides/custom-store.mdx`) no longer satisfies the
  interface until it implements these too. The three bundled backends
  already do. `ClaimSuspension` and `ClaimExpiredSuspension` are the hard
  part of that work: both have to perform the run's paused-to-running
  transition and the suspension read as one atomic operation, because
  that gap is the race two concurrent resumes would otherwise both win.
  `store/storetest`'s conformance suite covers all seven. Run it against
  your backend.
- **`Engine.ResolveCheckpoint` now carries the decision to the run.** It
  used to be a one-line passthrough to `store.Resolve` that always
  succeeded. It now reads the checkpoint first and refuses one that is
  not `pending` with `cortex.ErrInvalidState`. For a checkpoint the loop
  opened it then approves by claiming the run's suspension and resuming
  it, or rejects by claiming the suspension and failing the run with
  `decision.Reason`, and it records the decision only once that has taken
  effect. An approval resumes synchronously, the same way `RunAgent` runs
  synchronously, so the call returns when the run does and a REST client
  holds its request open for the length of the rest of the run.

  A checkpoint you wrote yourself is still recorded and nothing else is
  touched. Nothing in the tree created a checkpoint before this release,
  so every checkpoint that exists today is one of yours, and none of them
  has a paused run behind it. The two kinds are told apart by a
  `suspension_id` key the loop stamps into the row's metadata rather than
  by asking the store what it holds right now, so a row that outlived its
  suspension still routes as loop-created and still tells the operator
  so.

  What does change for a row of your own is the read in front of the
  write. A checkpoint whose state is not `pending` is refused instead of
  resolved again, so if you keep states of your own on those rows, or you
  resolve the same one twice and rely on the second call succeeding, that
  call comes back `cortex.ErrInvalidState` now.
- **`Engine.Start` now launches a background goroutine, and
  `Engine.Stop` joins it.** The goroutine is the expiry sweeper: it ticks
  once a minute, reads suspensions past their deadline across every
  scope, fails those runs, and closes the checkpoint of any approval
  among them. `Stop` cancels it and waits for it to
  exit before emitting `OnShutdown`, so `Stop` can now block for as long
  as a sweep already in flight takes. A second `Start` is a no-op rather
  than a second loop. Two consequences worth checking for: a host that
  calls `Start` and never `Stop` leaks a ticker, and a host that wants no
  background work at all sets `engine.WithSuspensionTTL(0)`, which turns
  off the deadlines and the loop together.
- **A run's state can now really be `paused`.** The constant has been in
  `run.State` since v1.x with nothing ever assigning it, so no dashboard,
  list filter or state machine written against this API has met the value
  in practice. It'll meet it now. Anything of yours that switches
  exhaustively on run state, or that assumes a run still in flight reads
  as `running`, needs the case. Cancelling a paused run through
  `POST /runs/:id/cancel` still works, and it now takes the same claim
  every other writer to a paused run takes. If a resume or a decision got
  there first the cancel loses, and you get a 409 rather than a run
  stomped out from under whoever already owned it. A cancel that wins
  drops the suspension itself and resolves whatever checkpoint the pause
  opened, so an operator's queue does not fill up with decisions about
  runs that already ended.

### Added

- **The `suspension` package.** `suspension.Suspension` is a paused run:
  its `RunID`, the `Scope` the run started under, a `SuspendReason` of
  `approval` or `external_tool`, the `PendingCall`s it is waiting on
  (id, name and the model's verbatim `Arguments`), a `Continuation`, and
  an optional `ExpiresAt`. `suspension.Continuation` is what the loop
  needs to pick up where it stopped: the messages, the assembled system
  prompt, the step index, tokens used, the index where the run's own
  messages begin, the session id, and the resolved `RunConfig` the run
  was executing under. Typed fields, not an untyped metadata map: a
  malformed continuation is a scan error at the boundary, so you find out
  there and not three steps into a half-restored run.
- **`suspension.Store` on the composite store**, backed by a new
  `cortex_suspensions` table on Postgres and SQLite (migration
  `20260825000001`) and a collection of the same name on Mongo.
- **`engine.WithExternalTool(def llm.Tool)`**, registering a tool the
  engine advertises but never runs. The definition reaches the model
  exactly like a `WithTool` registration and the authorizer gates a call
  to it exactly like any other. Only dispatch differs: there is no
  handler, so the call goes pending and the run suspends. External tools
  are subject to `cfg.Tools` name filtering, unlike the builtins v1.10.0
  exempted. A builtin exists because of engine configuration an agent
  never named, so filtering it by name would silently kill an agent's
  knowledge search; an external tool is a host registration like any
  other, and `cfg.Tools` is exactly how an agent picks among those.
- **`Engine.Resume(ctx, runID, ResumeInput) (*run.Run, error)`** and
  **`Engine.ResumeStream(ctx, runID, ResumeInput, chan<- StreamEvent) error`**,
  with `engine.ResumeInput` and `engine.ToolResult`. You supply exactly
  one `ToolResult` per pending call, keyed by `ToolCallID`: an extra, a
  duplicate or a missing one is `cortex.ErrResultsMismatch` and the run
  stays paused so you can fix the call and try again. `Content` carries
  what the tool returned, `Error` says your own execution failed and
  feeds the model the same payload shape an engine-side failure produces,
  and `Execute` asks the engine to run the call itself, which is what an
  approval hands back. A resume continues under the scope stored on the
  suspension, not the scope of whoever called `Resume`.

  Both answer an external-tool pause. A run paused for approval is not
  resumable this way at all: it is waiting on a decision, and
  `ResolveCheckpoint` is the only thing that carries one, so `Resume`
  against it is `cortex.ErrRequiresApproval` and the run stays paused.
  Gating only `Execute` would leave you free to answer an escalated call
  with content you made up, feed the model output no tool ever produced,
  and finish the run with its checkpoint still sitting in somebody's
  queue.
- **`cortex.ErrRequiresApproval`.** Return it from `ToolAuthorizer.Authorize`,
  or wrap it with `fmt.Errorf("...: %w", cortex.ErrRequiresApproval)` to
  say why, and the call is not denied: nothing goes back to the model,
  the run pauses, and a checkpoint carries the call to whoever decides.
  It's matched with `errors.Is`.
- **`cortex.ErrNotSuspended`, `cortex.ErrSuspensionExpired`,
  `cortex.ErrResultsMismatch` and `cortex.ErrInvalidContinuation`.** The
  first is what the losing side of a double resume gets, and what a
  resume against a run that was never paused gets. The second is a resume
  past the deadline, which leaves the run paused for the sweeper. The
  third is the bijection rule above. The fourth is a continuation the
  loop cannot run, which today means one carrying no step budget, and it
  gets its own sentinel, not `ErrMaxStepsReached`, so nobody goes off
  raising a budget that was never the problem.
- **`engine.WithSuspensionTTL`, `engine.WithSuspensionSweepInterval` and
  `engine.WithSuspensionSweepLimit`**, defaulting to 24 hours, one minute
  and 100 rows per sweep. `WithSuspensionTTL(0)` disables expiry: no
  deadline is written and no sweeper runs. A negative TTL is refused
  rather than clamped, since it reads as a deadline already passed and
  would sweep everything on the first tick. The deadline is stamped when
  the run pauses, not worked out at sweep time, so changing the TTL never
  moves the deadline of a run somebody has already been asked to answer.
- **`engine.EventSuspended`**, the terminal stream event of a run that
  paused without finishing. Its data carries `run_id`, `reason` and
  `pending`. A streaming consumer that does not handle it sees the
  channel close and reads a paused run as a finished one.
- **`Engine.CancelRun(ctx, runID)`**, which is what `POST
  /runs/:id/cancel` calls now. The handler used to read the run, check
  its state and write `cancelled` back unconditionally, which made it a
  fourth writer to a paused run with none of the gating the other three
  have. Cancelling a paused run claims its suspension first, so losing
  that claim is `cortex.ErrNotSuspended` and a 409, and winning it drops
  the suspension and resolves whatever checkpoint the pause opened. A run
  that is merely running is cancelled the way it always was. One that has
  already ended is `cortex.ErrInvalidState`, and still a 400.
- **`id.SuspensionID`, `id.NewSuspensionID` and `id.ParseSuspensionID`**,
  prefix `sus`, the same identifier shape every other entity has.

### Changed

- **`engine.Dispatch` refuses external and escalated tools explicitly.**
  It shares `executeTool` with the ReAct loop, and a pending call can
  only be answered by suspending a run, which `Dispatch` does not have.
  Rather than hand back the empty pending result, which reads like a tool
  that ran and said nothing, it returns an error saying which kind of
  pause the call needs. Either a result you supply, or a checkpoint
  somebody decides.
- **`checkpoint.Decision.DecidedAt` is stamped by the engine when you
  leave it zero.** Nothing filled it in before, so a decision recorded
  through the REST endpoint carried no timestamp at all.

### Fixed

- **The REST checkpoint resolve endpoint had no `reason` field.**
  `ResolveCheckpointRequest` carried `decision` and `decided_by` and
  nothing else, so `checkpoint.Decision.Reason` arrived empty on every
  call that came in over HTTP. "Fail the run with the decision's reason"
  would have failed every run with an empty string. `POST
  /checkpoints/:id/resolve` now accepts `reason`, and a rejection puts it
  on the run's `Error` where whoever finds the run later will read it.
- **`plugin.OnCheckpointCreated` and `plugin.OnCheckpointResolved` now
  fire.** Both hooks have been declared, documented and implemented by
  the bundled audit and metrics extensions for releases, and
  `EmitCheckpointCreated` had no caller anywhere in the tree. If you
  implemented either one and never saw it run, you'll see it now. `created`
  fires once the run is genuinely paused, not when the row is written, so
  a subscriber is never told about a checkpoint that a failed pause then
  cleaned up.

### Known limitations

Three gaps ship with this release. None of them corrupts a run and each
one fails where you can see it, but you should know they're there.

- **An approval on an external tool cannot be closed by the engine.** The
  authorizer runs before the external check, so it can escalate a call to
  a tool the engine does not own. Approve that and the engine is being
  asked to run something it has no handler for, so the call ends as a
  failure whose message tells you to answer it by supplying the result
  yourself. The run carries on and the model sees the failure. Closing it
  properly means the loop has to accept a pre-seeded pending set plus a
  fresh continuation that folds in the sibling calls which already ran,
  because resumed calls are recorded before the loop re-enters while the
  classify-and-collect block lives inside the per-step iteration over the
  model's tool calls. That is real design work, not a routing change.
- **A step cannot pend for both reasons at once.** Ask the model for a
  call the authorizer escalates and a call the host executes in the same
  step, and the run fails right there with an error naming both tools and
  both reasons. A suspension carries one reason and everything
  downstream reads it as the reason for every call under it, so neither
  reason is right for both. Approval hands back `Execute` for the whole
  set, which fails the external call for something it never did.
  External drops the checkpoint, which lets a call somebody asked to have
  reviewed go ahead with nobody ever asked. Until a run can hold more
  than one suspension there is nothing better to do than stop, so the
  step stops. Keep the two kinds apart by narrowing the agent's tool
  list, or register the external tool with `WithTool` so the engine runs
  it and only the approval is left pending.
- **A decision whose `Resolve` write then fails leaves a pending
  checkpoint on a run that already moved.** `ResolveCheckpoint` acts
  first and records afterwards, so this is the leftover that ordering
  cannot remove. It was chosen over its inverse deliberately. Recording
  first would mean a decision that could not be carried out drops out of
  `ListPending` while the run stays paused, invisible and recoverable
  only by a direct `Resume` call. This way round the row is still there,
  still listed, and deciding again returns the run's own "not suspended"
  error, not silence.

### Migration notes

- **The suspension table is created by `Store.Migrate()` and needs no
  backfill.** Suspensions are new this release, so there is no unscoped
  legacy shape to carry forward: the table is created with its scope
  columns already in place, and its partial unique index on `run_id`
  follows the `scope_canon` convention every other index in this codebase
  uses.
- **Mongo gains no new replica-set requirement.** On that backend the
  paused-to-running transition is a single `FindOneAndUpdate` against the
  run document, which Mongo applies atomically on its own, so neither
  claim needs a multi-document transaction. v1.9.0's requirement for
  conversation writes still stands and is unchanged.
- **Read the guide before you register an external tool.**
  `docs/content/docs/concepts/suspension.mdx` covers registering one,
  what a suspended run looks like, the one-result-per-pending-call rule,
  why a resume runs under the scope the run started with, and how to
  switch expiry off.

## [1.10.0] - Unreleased

Adds tool authorization: a host-implemented `cortex.ToolAuthorizer` that
decides which tools a model gets to see and which calls it's actually
allowed to dispatch. Before this release cortex had no seam for a host's
own permission model to reach a tool call at all; any registered tool was
visible to every run and dispatched for every call, whatever the caller
was or wasn't allowed to do.

This release ships two of the authorizer's three outcomes. `Authorize`
returning nil allows the call; returning any error denies it, and the
error's text is fed back to the model as the tool result, so the model
can react to the refusal instead of the run just failing. The third
outcome, escalating a call to a human for approval, needs a suspension
mechanism that doesn't exist yet. It's arriving in v1.11.0, not this one:
see "Deferred to v1.11.0" below before you build against it.

As with v1.7.0, v1.8.0 and v1.9.0, this ships inside v1 rather than as
v2.0.0. The module path is `github.com/xraph/cortex`, carries no `/v2`
suffix, and migrating it was declined. Go refuses to resolve a `v2.x`
tag against an unsuffixed module path, so a minor version is the only
release channel available, and the breaking changes below are
enumerated here instead of signaled by a major version bump. Read this
whole section before upgrading.

### Breaking changes

- **`engine.ToolHandler` now takes `(ctx context.Context, inv
  cortex.Invocation)` instead of `(ctx context.Context, arguments
  string)`.** Every `WithTool` registration has to change: pull the
  arguments out of `inv.Call.Arguments` rather than a second parameter.
  The builtin `knowledge_search` handler moved onto the same contract
  internally, so this isn't a shape the engine keeps around anywhere as
  a fallback. Update anything that implements `ToolHandler`, not just
  the literal `WithTool(...)` call sites.

### Added

- **`cortex.ToolAuthorizer`**, the interface a host implements to gate
  tools. `Visible(ctx, Subject, []llm.Tool) []llm.Tool` filters the list
  a model is shown; `Authorize(ctx, Subject, llm.ToolCall) error` gates
  one dispatch. They run at different points, deliberately. `Visible`
  runs once per step, inside `resolveTools`, before the model is asked
  to pick anything. `Authorize` runs on every dispatch, including one
  naming a tool `Visible` never returned: a model can name a tool it was
  never shown, so trusting `Visible` to have already filtered would
  leave `Authorize` doing nothing. Install one with
  `engine.WithToolAuthorizer`. A nil authorizer, or none set at all,
  allows everything, so a host that doesn't configure one sees no
  behavior change.
- **`cortex.Subject`**: who's asking. Carries `Scope`, `Principal`,
  `AgentID` and `RunID`. `Principal` is `any`, and cortex never
  interprets it: a host puts its own identity object there and gets the
  identical value back at both `Visible` and `Authorize`.
- **`cortex.Invocation`**: one tool call about to run. Embeds `Subject`
  and adds `Call llm.ToolCall`, so a `ToolHandler` receives scope and
  principal explicitly instead of reaching into the context for them.
- **`cortex.WithPrincipal(ctx, principal any) context.Context`** and
  **`cortex.PrincipalFromContext(ctx) any`**, attaching and reading the
  host's caller identity on the context, next to `WithScope` and
  `ScopeFromContext` and following the same contract (a missing
  principal returns nil, not a panic). Nothing populates
  `Subject.Principal` unless a host calls `WithPrincipal` on the request
  context; skip it and an authorizer only has scope, agent and run to
  decide on.
- **`plugin.ToolDenied`**, a hook fired when the authorizer refuses a
  call: `OnToolDenied(ctx, runID, toolName, reason string) error`. It's
  observational, not a veto point. A hook can log or alert on a denial;
  it can't undo one, since the authorizer has already decided by the
  time the hook runs.
- **`Registry.EmitToolDenied`**, the emitter `executeTool` calls on a
  denial, notifying every registered `ToolDenied` hook.

### Changed

- **`engine.Dispatch` now goes through the authorizer.** Its signature
  is unchanged, so nothing forces you to look at it, but it shares
  `executeTool` with the ReAct loop and therefore picks up the
  `Authorize` gate along with it. Once you install an authorizer, a
  direct `Dispatch` call it refuses returns the denial as its result
  string instead of running the tool. The subject a `Dispatch` builds
  carries the scope and principal on the context and nothing else: a
  direct call has no agent and no run of its own, so `AgentID` and
  `RunID` arrive zero-valued. Write an authorizer that keys on either
  one to handle that case, or it will treat every host-driven dispatch
  as coming from the same nameless agent.
- **A tool call now fires exactly one terminal plugin event.** The ReAct
  loop used to emit `ToolCompleted` as soon as `executeTool` returned,
  including for calls that had just fired `ToolDenied` or `ToolFailed`,
  so a subscriber counting completions counted every denial and every
  failure as a success. Completed, failed and denied are now mutually
  exclusive, in the streaming loop as well as the synchronous one, and a
  tool name matching nothing fires `ToolFailed`, where before it went
  down as a success. The result string the model reads is unchanged in
  all three cases. If you built a metric on `ToolCompleted` counts, expect
  the number to drop by however many denials and failures your agents
  were quietly logging as wins.

### Fixed

- **`plugin.ToolFailed` now actually fires.** It's been a declared hook
  since an earlier release, but `executeTool` never called
  `EmitToolFailed` when a handler returned an error, so a host that
  implemented `ToolFailed` had registered a callback nothing invoked.
  This release wires the call in while `executeTool` was already being
  touched for the authorizer. If you implemented `ToolFailed` and never
  saw it run, you'll start seeing it now; this is a genuine behavior
  change even though it's a fix, not a feature.
- **`resolveTools` now honors `cfg.Tools`.** The parameter existed but
  was discarded on arrival (`resolveTools(_ []string)`) since the
  function was first written, so `cfg.Tools` filtering has never worked
  on any release up to and including v1.9.0: an agent configured with a
  restricted tool list was handed every registered tool anyway. It works
  now, which means an agent that sets `cfg.Tools` sees a smaller tool
  list than it saw on v1.9.0. Check what your agents name there before
  you upgrade, because a tool an agent had been using without listing
  will disappear from the list the model is shown.

  Builtins are exempt from that filtering. `cfg.Tools` enumerates the
  tools you registered with `WithTool`; it was never meant to enumerate
  the ones cortex ships itself, and filtering them with it would mean an
  agent with Knowledge configured lost `knowledge_search` the moment it
  named a single registered tool. Withholding a builtin is
  `ToolAuthorizer.Visible`'s job, which runs after this and still sees
  the whole list.

### Deferred to v1.11.0

- **Escalating a tool call to a human approver is not available in this
  release.** `Authorize` can allow or deny a call; it has no way to
  suspend a run so a person can approve it later. That needs
  `ErrRequiresApproval` and a suspension mechanism to go with it, and
  both are landing together in v1.11.0, so no half of that feature ships
  on its own. Returning a custom error from `Authorize` today just
  denies the call; there's no approval path for it to fall into.

## [1.9.0] - Unreleased

Adds sessions: a real, first-class thread that groups an agent's
conversation messages, instead of the one-conversation-per-agent-per-scope
shape every earlier release assumed. An agent can now hold many sessions
in the same scope, a run can target one explicitly through
`RunOverrides.SessionID`, and one session per agent per scope is marked
default — the thread a run lands in when its caller names none. The
default is a real row, created lazily on an agent's first unsessioned
run, not an empty-string sentinel standing in for "the shared one"; see
`docs/content/docs/concepts/sessions.mdx` for the full model.

As with v1.7.0 and v1.8.0, this ships inside v1 rather than as v2.0.0.
The module path is `github.com/xraph/cortex`, carries no `/v2` suffix,
and migrating it was declined. Go refuses to resolve a `v2.x` tag
against an unsuffixed module path, so a minor version is the only
release channel available, and the breaking changes below are
enumerated here instead of signaled by a major version bump. Read this
whole section before upgrading.

### Breaking changes

- **`memory.Store`'s `SaveConversation`, `LoadConversation` and
  `ClearConversation` gained a `sessionID id.SessionID` parameter**,
  ahead of the `limit`/`messages` argument each already took. Scope
  still comes from context, but which thread a conversation call reads
  or writes no longer does — this breaks every custom `memory.Store`
  implementation, not just the three bundled ones.
- **`engine.Engine.LoadConversation` and `ClearConversation` gained the
  same `sessionID id.SessionID` parameter**, one layer up from the store
  methods above, for the same reason.
- **`store.Store` now embeds `session.Store`**, six additional methods
  (`CreateSession`, `GetSession`, `UpdateSession`, `DeleteSession`,
  `ListSessions`, `CountSessions`) on top of everything the composite
  interface already required. A custom `store.Store` implementation
  (see `docs/content/docs/guides/custom-store.mdx`) no longer satisfies
  the interface until it implements these too; the three bundled
  backends already do.

### Added

- **The `session` package**: the `session.Session` entity
  (`ID`, `AgentID`, `Scope`, `Title`, `Metadata`, `MessageCount`,
  `LastMessage`, `IsDefault`, `BackfilledBy`) and `session.Store`,
  implemented by all three backends. `session.ListFilter` follows the
  `Exact`/scope-prefix convention v1.8.0 established for the entity
  stores, plus `AgentID`, `DefaultOnly`, and `Search`.
- `id.SessionID`, `id.NewSessionID`, `id.ParseSessionID`, and
  `id.ParseOptionalSessionID`, the same identifier shape every other
  entity in this package already has.
- `cortex.ErrSessionNotFound`, returned by every session store method
  that can't find the row it was asked for, and mapped to a 404 by the
  API layer's `mapStoreError`.
- `RunOverrides.SessionID` and `run.Run.SessionID`: a run can now target
  an explicit session, and every run records which session it belongs
  to. With no override, the engine resolves (and lazily creates, on the
  first unsessioned run) the agent's default session for the caller's
  scope.
- Six REST endpoints under `/v1/agents/:name/sessions`: `POST` (create),
  `GET` (list), `GET /count` (count), `GET /:id` (get), `PUT /:id`
  (update), and `DELETE /:id` (delete). A session id in the path is
  always checked against the agent named in the same path — a mismatch
  reads as the same `ErrSessionNotFound` a genuinely missing session
  does, rather than a distinct "forbidden" response that would leak the
  existence of another agent's session. This is the same ownership check
  `resolveConversationSession` added to the memory endpoints already
  had; the new session endpoints follow it from the start rather than
  needing a follow-up fix.
- `DeleteSession` cascades: deleting a session also deletes every
  conversation message it owns, on all three backends. This is an
  explicit application-level delete, not a database `FOREIGN KEY` —
  `cortex_memories.session_id` is shared by every memory kind, and
  working/summary rows never set it at all, so a real foreign key would
  reject their writes outright.

### Fixed

- **The reasoning loop re-saved a run's entire reloaded conversation
  history as new rows on every turn.** `runReAct` and `streamReAct`
  passed the whole reloaded history back into `SaveConversation`
  alongside each run's actual new turn, so a real N-turn conversation
  could hold far more physical rows than logical messages (a
  three-turn conversation could reach 14 rows where it should hold 6).
  Both now save only the messages a run actually added. See "Migration
  notes" below for what this means for `message_count` on a session
  backfilled from before this fix.

### Migration notes

- **MongoDB requires a replica set (or sharded cluster) in production.**
  This is new as of this release: `SaveConversation` now writes inside a
  multi-document transaction, because a session's `message_count` and
  `last_message` counters have to commit or roll back together with the
  message rows they describe, or the counter drifts from the rows it's
  supposed to summarize. A standalone `mongod` cannot run transactions
  at all, and every conversation write will fail against one.
  `store/postgres` and `store/sqlite` have no such requirement.
- **Existing conversations are backfilled into a per-agent-per-scope
  default session.** Sessions didn't exist before this release, so every
  pre-existing `cortex_memories` row with `kind = 'conversation'` has no
  session to belong to; without a backfill, `LoadConversation`'s
  session-scoped filter would make that history permanently
  unreachable. A pre-v1.9.0 conversation was, by construction, the only
  conversation an agent had in a given scope, so "the default session"
  is the exact description of that history, not an invented one.
- **The backfill runs on every `Migrate()` call, after the rescope pass,
  rather than as a one-shot migration.** A one-shot version would only
  ever see a legacy row's scope as it stood at that exact migration's
  Up — and a host jumping from pre-v1.8.0 straight to this release in
  one `Migrate()` call has every legacy conversation row still unscoped
  at that point, because the rescope pass that assigns scope runs
  separately, in the same call. Backfilling from a one-shot migration
  would have left that entire jump's conversation history permanently
  unreachable, with no second chance to fix it: grove never retries a
  recorded migration version once applied. Running the backfill directly
  from `Store.Migrate`, after rescope, on every boot, closes that gap —
  a row only ever needs a real scope by the time the backfill inspects
  it, regardless of which release wrote that scope. Rows whose scope is
  never filled in at all (no `Rescoper` was ever supplied) are skipped
  and stay unreachable, exactly as before.
- **A legacy session's `message_count` is a `DISTINCT` count of
  `(role, content)` pairs, not a raw row count.** Until the fix above
  landed, the reasoning loop re-saved a run's entire history on every
  turn, so pre-existing conversation rows are duplicated, and the
  duplication compounds with every turn. Backfilling `message_count` as
  a raw row count would report a number known to be wrong from the very
  first read of every upgraded deployment. Counting distinct
  `(role, content)` pairs instead means two things for a host reading a
  legacy session: its `message_count` may read lower than its physical
  row count (`LoadConversation` still returns every row, duplicates
  included), and a user who genuinely sent the same message twice in a
  legacy conversation is counted once. Conversations created after this
  release count normally — the duplication this works around no longer
  happens.

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
- **Rolling back this release's Postgres or SQLite migrations is one-way
  once any two scopes hold a same-named agent, skill, trait, behavior,
  persona, or orchestration config.** Every `Down` in this phase
  recreates the old `UNIQUE (app_id, name)` index, and every row written
  under v1.8.0 has `app_id = ''`. The first time two such rows share a
  name, `Down` fails outright on the index build rather than corrupting
  anything — it just means rollback stops being available from that
  point on. Mongo carries no rollback path for this phase at all. Plan
  accordingly: if you might need to roll back, do it before two scopes
  accumulate a same-named entity, not after.

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
