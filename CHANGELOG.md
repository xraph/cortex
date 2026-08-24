# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

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
  by asking the store what it holds right now, so an approval that
  expired and took its suspension with it still routes as loop-created
  and still tells the operator so.

  What does change for a row of your own is the read in front of the
  write. A checkpoint whose state is not `pending` is refused instead of
  resolved again, so if you keep states of your own on those rows, or you
  resolve the same one twice and rely on the second call succeeding, that
  call comes back `cortex.ErrInvalidState` now.
- **`Engine.Start` now launches a background goroutine, and
  `Engine.Stop` joins it.** The goroutine is the expiry sweeper: it ticks
  once a minute, reads suspensions past their deadline across every
  scope, and fails those runs. `Stop` cancels it and waits for it to
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
  `POST /runs/:id/cancel` still works and was already allowed; the
  suspension it leaves behind is dropped by the next sweep, which clears
  rows whose run has reached a terminal state.

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

Four gaps ship with this release. None of them corrupts a run and each
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
- **An approval suspension that expires leaves its checkpoint pending
  forever.** The sweeper fails the run and drops the suspension, but it
  has no checkpoint in hand and `checkpoint.Store` has no resolve-by-run-id,
  so the row stays in `ListPending` asking for a decision on a run that
  already failed. It fails in the visible direction. An operator who acts
  on it gets a loud error at the point of use, because there's no
  suspension left to claim. Fixing it needs either a new store method
  across three backends or a `checkpoint_id` column on the suspension and
  a migration.
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
