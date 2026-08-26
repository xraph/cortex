# Cortex A2A: Agent-to-Agent Messaging Core

- **Status:** Approved (brainstorming), ready for implementation planning
- **Date:** 2026-08-26
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex` (all changes live here).
- **Related:** Picks up the "message-bus comms" follow-up parked in [2026-06-23-cortex-orchestration-design.md](2026-06-23-cortex-orchestration-design.md) §11. First spec of three (see §3).
- **Method:** Test-driven. Every unit below is written test-first, red to green, no exceptions.

---

## 1. Goal

Give cortex agents the ability to address each other directly: send a message to a named
agent, ask one a question and durably wait for the answer, and read a mailbox of what
arrived while they were busy. The wire semantics are FIPA-ACL, in full, because it is the
one agent-communication vocabulary with thirty years of use behind it and because the
task-allocation protocols we want next (Contract Net especially) are defined in its terms.

After this lands, an agent can do three things it cannot do today:

1. `agent_send` a message to any agent in its scope, fire and forget.
2. `agent_ask` a peer and suspend until the reply comes back, surviving a process restart.
3. `agent_inbox` to drain messages that arrived without starting a run.

The routing rules, the conversation ledger and the delivery guarantees all live in one
new leaf package. Nothing about the envelope is bound to in-process delivery, so the
remote transport in the follow-up spec is an addition, not a rewrite.

### Non-goals

- **Remote transport.** Agent cards, JSON-RPC over HTTP, SSE streaming and the auth story
  for cross-vendor peers are spec 2. This spec defines the `Transport` seam they plug into
  and ships the in-process implementation only.
- **Interaction protocols.** Contract Net, subscribe/notify and the rest are spec 3. This
  spec carries the `Protocol` field and the performatives those protocols need, and stops
  there.
- **Forwarding.** `proxy` and `propagate` are recognised and delivered, but cortex does not
  itself forward on an agent's behalf. See §5.3.
- **Replacing the blackboard.** Orchestration's shared state and its `AgentHandoff` hook
  stay exactly as they are. Messaging is a second mechanism, not a migration.

---

## 2. Background: what is already here

Two things make this cheap, and both were built for other reasons.

Orchestration already solved the import cycle. It is a leaf package that reaches
the engine through an injected `AgentRunner` rather than importing it
([orchestration/orchestrator.go:57](../../../orchestration/orchestrator.go)). The same
trick works here, with the same interface.

Suspension already solved the waiting. [suspension/suspension.go](../../../suspension/suspension.go)
models a run that stopped because something outside it has not returned, and it
stores everything needed to continue: the messages, the assembled system prompt, the step
index, the history boundary, the session, and the resolved run config. `Engine.Resume`
takes one `ToolResult` per pending call and picks the loop back up. When the orchestration
spec rejected a message bus for needing "interruptible agents", that machinery did not exist
yet. It does now.

Two smaller facts the design leans on:

- `executeTool` in [engine/react.go:918](../../../engine/react.go) already has a three-way
  outcome, and the loop collects pending calls across a whole step and suspends **once**,
  after the step. Per-call reason tracking in [engine/suspend.go:63](../../../engine/suspend.go)
  is what keeps an approval and an external call in the same step from corrupting each other.
- Builtin tools are gated on configuration. `knowledge_search` only appears when a knowledge
  provider is set ([engine/tools.go:13](../../../engine/tools.go)), which is the pattern for
  making the three new tools opt-in.

---

## 3. Decomposition

Full agent-to-agent support, including remote peers, is more than one spec's worth of work.
It splits cleanly:

| Spec | Scope | Depends on |
|---|---|---|
| **1. Messaging core** (this one) | Envelope, conversations, mailboxes, delivery, containment, agent tools, persistence | nothing |
| **2. Remote A2A transport** | Agent cards, task lifecycle, JSON-RPC + SSE server and client, peer auth | the `Transport` seam from spec 1 |
| **3. Interaction protocols** | Contract Net task allocation, subscribe/notify, protocol state machines | the performatives and `Protocol` field from spec 1 |

Each gets its own spec, plan and implementation cycle. Spec 1 is written so neither of the
others needs to reshape it.

---

## 4. Architecture

### 4.1 Package

The new package is `a2a/`, not `comms/`. The repo already has `communication/` for tone,
formality and verbosity styles, and two package names one letter apart in the same import
block is a bug waiting to happen. `a2a` also names what spec 2 implements.

`a2a` is a **leaf package**. It imports `cortex` and `id` and nothing else from this repo.
It never imports `engine`, `plugin` or `store`. It owns:

- the FIPA-ACL envelope and the full performative vocabulary,
- the conversation ledger: hop counts, deadlines, status,
- correlation between an ask and its reply,
- mailbox and inbox semantics,
- the `Bus`, which routes an envelope to a recipient and decides what happens on arrival.

### 4.2 Seams

Four injected interfaces, one of which already exists:

| Seam | Status | Purpose |
|---|---|---|
| `AgentRunner` | exists | Start a run for a recipient whose performative demands an answer. Same shape orchestration uses. |
| `Resumer` | new, thin | `Resume(ctx, runID, callID, result string) error`. Wraps the engine's internal resume path (§9.5) so a reply can un-pause the asker. |
| `Store` | new | Envelopes, conversations, deliveries, pending asks. Folds into the composite in [store/store.go:23](../../../store/store.go). |
| `Transport` | new | Delivers an envelope to an address. The in-process implementation ships here; spec 2 adds an HTTP one. |

`AgentRunner` is currently declared in `orchestration`. Two packages needing the identical
interface is the signal to lift it, so the plan moves it to a shared home (root `cortex`
package, beside `ToolAuthorizer` and `Invocation`) and has both packages depend on that.
The engine adapter that satisfies it does not change.

### 4.3 Engine wiring

```
agent A's run loop                       a2a.Bus                       agent B
  │                                         │                             │
  ├─ agent_ask tool call ──────────────────>│                             │
  │  (builtin, returns outcomePending)      ├─ write envelope             │
  │                                         ├─ write pending ask          │
  │<─ suspend(ReasonAgentReply) ────────────┤─ enqueue delivery           │
  │  [run is on disk]                       │                             │
  │                                    dispatcher worker ─ RunAgent ─────>│
  │                                         │<──── output ────────────────┤
  │                                         ├─ reply envelope (in-reply-to)
  │                                         ├─ claim pending ask          │
  │<─ Resume(runID, callID, reply) ─────────┤                             │
  │  [continues from Continuation]          │                             │
```

---

## 5. The envelope

### 5.1 Type

Every one of FIPA-ACL's thirteen message parameters, typed, plus a small namespaced set of
cortex additions:

```go
type Envelope struct {
    cortex.Entity
    ID    id.MessageID
    Scope cortex.Scope

    // FIPA-ACL message parameters
    Performative   Performative
    Sender         Address
    Receivers      []Address
    ReplyTo        []Address
    Content        string
    Language       string
    Encoding       string
    Ontology       string
    Protocol       string
    ConversationID id.ConversationID
    ReplyWith      string
    InReplyTo      string
    ReplyBy        *time.Time

    // cortex additions
    Hops        int
    OriginRunID id.AgentRunID
    Metadata    map[string]any
}

type Address struct {
    Agent string // agent name
    Node  string // "" means an agent in this engine; spec 2 fills this in
}
```

`Node` is the only field remote delivery needs. Everything else about an envelope is
transport-agnostic by construction, which is the property that keeps spec 2 additive.

### 5.2 Performatives

All twenty-two, as a constant block: `accept-proposal`, `agree`, `cancel`, `cfp`, `confirm`,
`disconfirm`, `failure`, `inform`, `inform-if`, `inform-ref`, `not-understood`, `propagate`,
`propose`, `proxy`, `query-if`, `query-ref`, `refuse`, `reject-proposal`, `request`,
`request-when`, `request-whenever`, `subscribe`.

Carrying the whole vocabulary is cheap, since it is a constant block and a validation check. The
cost sits in the routing semantics, so cortex interprets exactly three classes and documents
which performative falls in which.

### 5.3 Routing classes

**Directive.** Arrival starts a run for the recipient, and the run's output becomes the reply:
`request`, `request-when`, `request-whenever`, `query-if`, `query-ref`, `cfp`, `propose`,
`accept-proposal`.

`accept-proposal` sits here on purpose, away from the informatives. In Contract Net it
is the message that makes the contractor actually do the work, so it has to be able to start one.

**Informative.** Queued in the durable inbox, no run started: `inform`, `inform-if`,
`inform-ref`, `confirm`, `disconfirm`, `agree`, `refuse`, `failure`, `not-understood`,
`reject-proposal`, `subscribe`, `proxy`, `propagate`.

`proxy` and `propagate` are validated and delivered, but cortex will not forward them itself.
Forwarding on an agent's behalf is a policy decision with an obvious abuse shape, and a host
that wants it can build it over the inbox. A documented boundary beats a half-forwarder.

**Control.** Just `cancel`. The bus closes the conversation and resolves any waiting ask with a
`failure`. No run starts, because handing an LLM a message about bookkeeping is a waste of a
call.

Two rules that are easy to get backwards and are therefore pinned here:

- `agree` does **not** resolve a waiting ask. It means the peer accepted the task and is
  still working.
- `refuse` and `failure` **do** resolve it.

### 5.4 Correlation

`ReplyWith` on the ask, `InReplyTo` on the reply. FIPA's own mechanism, no cortex-invented
token beside it. A pending ask row is keyed by `reply_with`, so a reply looks up exactly one
row and resumes exactly one run.

---

## 6. Persistence

Four tables, following the entity conventions every other subsystem uses: embed
`cortex.Entity`, carry `Scope`, expose a `Store` interface plus a `ListFilter`, fold into
the composite `store.Store`.

| Table | Holds | Notes |
|---|---|---|
| `a2a_messages` | Envelopes | Immutable once written. |
| `a2a_conversations` | Protocol, initiator, participants, hops used, hop ceiling, deadline, status | Status is `open`, `closed` or `expired`. |
| `a2a_deliveries` | One row per receiver per envelope, with delivery state (`queued`, `delivered`, `failed`) and read state | One envelope to five agents is five rows. Folding this into the message row makes "unread for agent B" unanswerable, and the delivery state is what the dispatcher redrives from after a restart. |
| `a2a_pending_asks` | `reply_with`, asker run id, tool call id, expected responder, deadline, claim state | The correlation ledger. It is about the sender's run, which is why it is not a delivery. |

All four across sqlite, postgres and mongo, same as orchestration.

New TypeID prefixes in [id/id.go:27](../../../id/id.go): `msg` for envelopes, `conv` for
conversations. Both use the existing `New(prefix)` path with no special casing.

---

## 7. Data flow

### 7.1 Enablement

The three tools only exist when the host opts in:

```go
eng, err := engine.New(
    engine.WithStore(st),
    engine.WithA2A(a2a.Options{
        HopCeiling:     8,
        Workers:        4,
        DefaultReplyBy: 5 * time.Minute,
        SweepInterval:  30 * time.Second,
    }),
)
```

`builtinTools()` appends them conditionally, the way it already does for `knowledge_search`.
A host that does not configure A2A sees no new tools, no new tables touched, and no change
in behaviour.

### 7.2 Tools

```
agent_send(to[], performative, content, conversation_id?, ontology?, protocol?, reply_by?)
   → {message_id, conversation_id, deliveries: [{agent, status, error?}]}

agent_ask(to, content, performative="request", ontology?, protocol?, reply_by?)
   → suspends; on resume returns {performative, sender, content, conversation_id}

agent_inbox(limit?, conversation_id?, mark_read=true)
   → [{message_id, sender, performative, content, conversation_id, received_at}]
```

### 7.3 The ask lifecycle

Everything else is a simplification of this path.

1. Agent A's model calls `agent_ask`. `executeTool` consults the authorizer first, unchanged.
   This is an ordinary tool call, so a host's existing `ToolAuthorizer` is what decides who
   may talk to whom.
2. The builtin resolves the recipient inside A's scope, opens or joins a conversation, writes
   the envelope with a fresh `ReplyWith`, writes the pending ask row, enqueues the delivery,
   and returns `outcomePending` with `ReasonAgentReply`.
3. The step finishes. Sibling tool calls in that step complete normally and the loop suspends
   once, after the step.
4. A dispatcher worker takes the delivery, renders the envelope into run input (sender,
   performative, content, prior conversation turns) and starts B's run through `AgentRunner`.
5. B's output becomes a reply envelope: `inform`, `InReplyTo` set to A's `ReplyWith`, `Hops`
   incremented, same conversation.
6. The bus matches `InReplyTo` against the ledger, claims the row, and calls the `Resumer`.
   A continues from its stored `Continuation`, with its original scope, session and step index.

`agent_send` is steps 1 through 4 with no pending ask and no suspension. Informative
performatives stop at step 4 with an inbox row instead of a run.

### 7.4 Three properties worth stating

The resumed run executes on the dispatcher's goroutine, because `Resume` is synchronous. Worker
capacity therefore has to cover resumed askers as well as recipients, and `Workers` defaults
conservatively for that reason.

Nesting is free but bounded. B can ask C while A waits, and nothing prevents it, because A is
paused in the store and not holding a stack frame. The hop ceiling is what keeps the chain finite.

Restart safety is real here and not aspirational. If the process dies at step 4, A's suspension
and its pending ask are both on disk, so on restart the dispatcher redrives undelivered
deliveries, and anything genuinely lost hits `ReplyBy` and resolves into a `failure` that lets
the asking run finish instead of sitting paused until someone notices.

### 7.5 Scope and policy

Scope is a cortex invariant and the bus enforces it structurally: a recipient that does not
resolve inside the sender's `cortex.Scope` is refused, and the refusal is `not-understood`.
An agent that never saw the message must not appear to have turned it down.

Whether two agents that share a scope may talk is host policy, and it goes through
`ToolAuthorizer` like everything else. `cortex.ErrRequiresApproval` works here for free: a
host can make agent-to-agent messaging pause for a human, and it opens a checkpoint the same
way any other gated call does.

---

## 8. Containment

The conversation carries a hop ceiling, default 8. Every derived message inherits the
conversation and increments `Hops`. Past the ceiling the bus refuses delivery, writes a
`failure` envelope back to the sender, and emits `MessageRefused`.

`ReplyBy` gets real teeth. A bus-owned sweep resolves an overdue pending ask into a timeout
`failure` and resumes the asker.

That last point diverges from the existing suspension sweeper on purpose. The engine sweep in
[engine/sweeper.go:105](../../../engine/sweeper.go) *fails the run* when nobody answers in
time. For an agent-reply pause that is the wrong verb. A peer that did not answer is
information the asking agent should get and can act on, not a reason to kill it. So the bus
sweep runs first and resolves asks into failures, and the engine's `WithSuspensionTTL` sweep
stays as the outer backstop for anything the bus missed.

---

## 9. Failure handling

The organising rule is that a peer's problem should reach the asking agent as something it can
read and act on. Every case below either refuses before anything suspends, or resolves a waiting
ask into a message the agent gets back from its tool call.

### 9.1 Refused before suspending

An error string goes back to the model, the run carries on, nothing is paused:

- The recipient does not resolve, or resolves outside the sender's scope.
- Self-addressing. Refused outright, because the loop risk is real and the use case is not.
- The conversation is already `closed` or `expired`.
- Any store write fails during send.

Write ordering is envelope, then pending ask, then enqueue delivery, and `outcomePending` is
returned only after all three succeed. Suspending first and validating later is how you get a
run paused forever on a message that never existed.

### 9.2 Resolved into a reply

The ask un-pauses with something actionable:

- The recipient's run fails, so a `failure` envelope carries the error text.
- The hop ceiling is exceeded, so delivery is refused and `MessageRefused` fires.
- `ReplyBy` elapses, so the sweep writes a timeout `failure`.
- `cancel` arrives, so waiting asks resolve with `failure` and the conversation closes.

### 9.3 Exactly-once resume

Two replies carrying the same `InReplyTo` must resume one run once. The pending ask row is
claimed before the `Resumer` is called, the same discipline `ClaimExpiredSuspension` uses in
[engine/sweeper.go:138](../../../engine/sweeper.go), and for the same reason: the bus sweep, a
late reply and a `cancel` are three writers racing for one row. The loser's message is still
persisted and still lands in the inbox. It just does not resume anything.

### 9.4 Broadcast is per-receiver

`agent_send` to five agents where two do not resolve returns a per-receiver outcome list.
Failing the whole send because one name was wrong throws away four good deliveries and tells
the model nothing useful.

### 9.5 Public Resume refuses agent-reply pauses

A host calling `Resume` on one gets an error, the same shape as resuming an approval pause
today. Only the bus's internal path may answer it. Without that, a host can forge a peer's
reply and the correlation ledger becomes decorative.

---

## 10. Engine changes

Concretely, outside the new package:

1. **Builtin outcome contract.** `executeBuiltinTool` returns `(string, bool)` today, so a
   builtin can only complete. It grows an outcome so `agent_ask` can pend, and `dispatchTool`
   learns that a builtin may pend. This is the most invasive edit in the spec and it is about
   ten lines.
2. **New suspension reason.** `ReasonAgentReply` in
   [suspension/suspension.go:19](../../../suspension/suspension.go). Distinct from
   `ReasonExternalTool` because the meanings differ: external says the caller executes this,
   agent-reply says cortex is waiting on a peer and nobody else should touch it.
3. **Resume gating.** `resume` gains an internal path the bus uses, and the public `Resume`
   rejects agent-reply pauses.
4. **Option.** `engine.WithA2A(a2a.Options)` constructs the bus, the transport and the
   dispatcher, and registers the three builtins.
5. **Lifecycle.** The dispatcher and the ask sweep start and stop with `Engine.Start` and
   `Engine.Stop`, joining rather than signalling, the way `stopSweeper` already does.
6. **Plugin hooks.** `MessageSent`, `MessageDelivered` and `MessageRefused` alongside
   `AgentHandoff` in [plugin/plugin.go:133](../../../plugin/plugin.go). `AgentHandoff` is
   untouched: orchestration handoffs and ACL messages are different events, and collapsing
   them would lie to existing subscribers.
7. **IDs.** `msg` and `conv` prefixes.
8. **`AgentRunner` moves** to the root `cortex` package, with `orchestration` and `a2a` both
   depending on it there.

---

## 11. API surface

Read endpoints under `/v1`, matching every existing handler:

```
GET  /v1/conversations                       list conversations in scope
GET  /v1/conversations/:id                   one conversation with its messages
GET  /v1/agents/:name/inbox                  deliveries for an agent, filterable by read state
POST /v1/agents/:name/messages               send an envelope from outside the engine
```

The POST is what lets a host or a human inject a message into a running system, and it is the
same path spec 2's remote transport terminates into. Everything else here is observability.

---

## 12. Testing

Test-driven throughout: a failing test first, then the code that passes it. Four layers,
matching how `orchestration` is already tested.

**`a2a` unit tests, no LLM and no database.** Fake `AgentRunner`, fake `Resumer`, in-memory
store, following [orchestration/fakes_test.go](../../../orchestration/fakes_test.go). Covers
all twenty-two performatives mapped to a routing class, table-driven, so a performative added
later without a class fails the test. Also correlation matching exactly one pending ask, the
duplicate-reply claim, the hop ceiling, and deadline expiry against an injected clock rather
than wall time.

**Store conformance across all three backends.** New cases in
[store/storetest/conformance.go:38](../../../store/storetest/conformance.go), which sqlite,
postgres and mongo already run. Scope isolation gets the
[store/scopespy](../../../store/scopespy) treatment, because the spy is what proves scope
actually reached the query instead of sitting available and unread.

**Engine integration with a fake LLM.** The behaviours that only exist once the loop is
involved: `agent_ask` suspends with `ReasonAgentReply`; a sibling tool call in the same step
still completes; the reply resumes at the right step index with the original session and
scope; public `Resume` is refused; an authorizer denial never reaches the bus;
`ErrRequiresApproval` on an `agent_ask` opens a checkpoint.

**Race and restart.** `-race` over concurrent dispatch, plus a test that tears down and
rebuilds the bus with the store intact, asserting that undelivered deliveries redrive and
orphaned asks time out.

One constraint falls out of the tests and belongs in the design, not in a test helper:
the dispatcher must be **deterministically drainable**. Tests drain it synchronously, production
runs workers. Without that, an assertion can observe a run mid-flight and the suite goes flaky.

---

## 13. Implementation phases

**Plan 1, the package.** Envelope, performatives and classes, conversation ledger,
correlation, hop and deadline logic, `Bus`, in-process `Transport`, dispatcher with a
synchronous drain mode. Fakes only, no engine and no database.

**Plan 2, persistence and engine.** Store interface and the three backends, conformance and
scopespy cases, the builtin outcome contract, `ReasonAgentReply`, Resume gating, `WithA2A`,
lifecycle, hooks, IDs, and the `AgentRunner` move.

**Plan 3, API, docs and example.** The four endpoints, an `_examples/` walkthrough of two agents
holding a conversation, and docs pages under `docs/content/docs/`.

---

## 14. Out of scope and follow-ups

- **Remote A2A transport** (spec 2). Agent cards, task lifecycle, JSON-RPC and SSE, peer auth.
  Plugs into the `Transport` seam.
- **Interaction protocols** (spec 3). Contract Net first, since `cfp`, `propose`,
  `accept-proposal` and `reject-proposal` are already carried here.
- **Forwarding semantics** for `proxy` and `propagate`, if a host ever needs cortex to do it
  rather than doing it themselves over the inbox.
- **Streaming replies.** An ask resolves on the recipient's final output. Token-level streaming
  between agents would need a different suspension shape and is not obviously worth it.
