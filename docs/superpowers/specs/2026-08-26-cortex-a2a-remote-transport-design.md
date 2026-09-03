# Cortex A2A: Remote Transport (spec 2)

- **Status:** Approved (brainstorming), ready for implementation planning
- **Date:** 2026-08-26
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex`, plus a new module `github.com/xraph/cortex/a2aremote`.
- **Related:** Second of the three specs decomposed in
  [2026-08-26-cortex-a2a-messaging-design.md](2026-08-26-cortex-a2a-messaging-design.md) §3. Plugs into the `Transport` seam that spec defined.
- **Method:** Test-driven. Failing test first, every unit.

---

## 1. Goal

Let cortex agents talk to agents that are not cortex agents, in both directions,
over the Agent2Agent protocol.

After this lands:

1. A remote A2A client can address any cortex agent, and gets back a Task it can
   poll, cancel and subscribe to.
2. A cortex agent can `agent_ask` a peer at `worker@peer.example` and the answer
   comes back through the same suspension and resume path a local peer uses.
3. Both sides speak all three protocol bindings: JSON-RPC 2.0, gRPC, and
   HTTP+JSON.
4. The FIPA-ACL semantics from spec 1 survive the hop, as a declared A2A
   extension rather than a private convention.

### The protocol, as it actually is today

Checked against the normative sources rather than memory, because the protocol
moved: A2A is at **1.0.0**, governed by the Linux Foundation, and the canonical
data model is [`specification/a2a.proto`](https://github.com/a2aproject/A2A) with
the three bindings derived from it.

- **Method names are PascalCase**: `SendMessage`, `SendStreamingMessage`,
  `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`,
  `CreateTaskPushNotificationConfig` and siblings, `GetExtendedAgentCard`. The
  `message/send` style is 0.x and a 1.0 server should not answer to it.
- **Agent Cards live at `/.well-known/agent-card.json`.** 0.x used
  `/.well-known/agent.json`, and a 1.0 client never looks there.
- **`AgentInterface` carries `url`, `protocol_binding` (`JSONRPC`, `GRPC`,
  `HTTP+JSON`) and `tenant`.** `tenant` is "an opaque string used for routing
  requests to a specific agent or tenant when multiple agents are served behind
  a single A2A endpoint", and every request message carries a `tenant` field.
- **Task states**: `SUBMITTED`, `WORKING`, `COMPLETED`, `FAILED`, `CANCELED`,
  `REJECTED`, `INPUT_REQUIRED`, `AUTH_REQUIRED`.
- **Errors** are `-32001` through `-32009`, listed in §5.

### Non-goals

- **Push notifications.** The four config methods plus webhook delivery are a
  large surface, and `AgentCapabilities.push_notifications: false` is an honest
  declaration rather than a gap. A host that needs callbacks has the plugin
  hooks from spec 1.
- **Agent card signatures.** `AgentCardSignature` earns its keep in a public
  registry. Peers you configured by hand are already authenticated by the
  credentials you configured with them.
- **`GetExtendedAgentCard`.** Declared unsupported, and it returns
  `-32007 ExtendedAgentCardNotConfiguredError`.
- **Interaction protocols.** Contract Net is spec 3, and it rides on this
  unchanged.

---

## 2. Architecture

### 2.1 A new module

`a2aremote/`, module `github.com/xraph/cortex/a2aremote`, with a `replace` back
to the root the way [api/go.mod](../../../api/go.mod) already does.

It is a separate module for one concrete reason: gRPC pulls in `grpc-go` and
`protobuf`, and a host that only ever wanted in-process messaging should not
inherit that dependency graph. The core module's dependencies do not move at all.

```
a2aremote/
  service.go     the semantics: every binding funnels through this
  seams.go       Gateway, PeerResolver, PeerRegistry
  card.go        AgentCard types, building one from an agent, serving, fetching
  mapping.go     Envelope <-> Message, Run -> Task, the FIPA extension
  types.go       Message, Part, Task, TaskStatus, Artifact
  errors.go      the A2A error codes and their mapping
  jsonrpc.go     the JSONRPC binding
  rest.go        the HTTP+JSON binding
  client.go      outbound: implements a2a.Transport
  grpcbind/      the GRPC binding, in a subpackage so its deps stay contained
    a2apb/       generated from the normative proto
```

### 2.2 Seams

| Seam | Purpose |
|---|---|
| `Gateway` | The cortex surface the service needs: `SendMessage`, `GetRun`, `ListRuns`, `CancelRun`, `ListMessages`, `GetAgentByName`, `ListAgents`. The engine satisfies it; tests pass a fake. |
| `PeerResolver` | Authenticates an inbound caller and returns the `Peer` it is, including the scope its messages land in. Host-implemented. Cortex ships no authentication of its own. |
| `PeerRegistry` | Outbound: maps a `Node` to a base URL and credentials, from `WithA2APeer` config. |

```go
// Credentials is what a binding could learn about a caller. It is
// transport-neutral on purpose: headers cover HTTP, gRPC metadata lands in
// the same map, and TLS covers mutual-TLS peers.
type Credentials struct {
    Headers    map[string][]string
    RemoteAddr string
    TLS        *tls.ConnectionState
}

// Peer is who the caller turned out to be.
type Peer struct {
    // Node is how this caller appears as a2a.Address.Node. Every sender
    // name a peer claims is namespaced by it, which is what stops a peer
    // presenting itself as a local agent.
    Node  string
    Scope cortex.Scope
}

type PeerResolver interface {
    ResolvePeer(ctx context.Context, cred Credentials) (Peer, error)
}
```

### 2.3 Two changes to the core module

Both are small, and the first is a bug spec 1 shipped.

**`deliverOne` never consulted the transports.** It routes by performative and
calls the local runner, so an envelope addressed to `worker@peer.example` would
have been "delivered" by running a local agent called `worker`. Routability was
checked at send time and then ignored at delivery time. Delivery now asks
whether the receiver is local, and hands a remote one to the transport that
handles it.

**`Bus.AddTransport`.** The outbound client needs the bus (a remote reply is fed
back in through `Send` so it can resolve a waiting ask), and the bus needs the
client. Construction cannot be circular, so the engine builds the bus, builds
the client with it, then registers the transport.

---

## 3. The wire mapping

### 3.1 Addressing: tenant is the agent

Cortex hosts many agents behind one endpoint, which is exactly the case
`AgentInterface.tenant` exists for. **The tenant string is the cortex agent
name.** Each agent gets its own Agent Card, whose `supported_interfaces` all
carry `tenant: "<agent name>"`, and an inbound request's `tenant` field selects
the recipient.

Cards are served per agent:

```
GET /{prefix}/agents/{name}/.well-known/agent-card.json
```

and, when a host names a default agent with `WithA2ADefaultAgent`, that agent's
card is also served at the root `/.well-known/agent-card.json` so plain
discovery finds something.

### 3.2 Identifiers

| A2A | cortex | Why |
|---|---|---|
| `Task.id` | `id.AgentRunID` (`arun_…`) verbatim | A task is a view over a run, so the run's id IS the task's id. A peer quoting one back names a row we can read. |
| `Message.context_id` | `id.ConversationID` (`conv_…`) | A2A's own words for `contextId` are "logically groups multiple related Task and Message objects", which is what a conversation is. |
| `Message.message_id` | `id.MessageID` (`msg_…`) | Same idea, one hop down. |

TypeID prefixes make all three self-describing on the wire, which is a pleasant
accident of the identity scheme rather than a design goal.

### 3.3 Task is a projection, never a stored entity

```go
func taskState(r *run.Run) TaskState {
    switch r.State {
    case run.StateCreated:   return TaskStateSubmitted
    case run.StateRunning:   return TaskStateWorking
    case run.StateCompleted: return TaskStateCompleted
    case run.StateFailed:    return TaskStateFailed
    case run.StateCancelled: return TaskStateCanceled
    case run.StatePaused:    return TaskStateInputRequired
    }
}
```

`INPUT_REQUIRED` is the interesting row. A paused run is waiting on something
outside itself, which is precisely what that state means, and it is true whether
the run paused on an approval checkpoint, an external tool, or an ask to a third
agent. The peer learns "this is waiting on somebody" without learning whose
internal business it is waiting on.

There is no task table and no task lifecycle of cortex's own. Two state machines
over one piece of work drift, and a task claiming `WORKING` for a run that failed
an hour ago is a lie the peer has no way to detect.

`Task.artifacts` carries the run's output as one `TextPart` artifact.
`Task.history` carries the conversation's messages when the caller asks for them.

### 3.4 Messages, and where the performative goes

A2A's `Message` has `role` and `parts` and no speech act. It also has
`extensions` ("the URIs of extensions that are present or contributed to this
Message") and `metadata`, which is the designed-in place for exactly this.

The extension URI is:

```
https://cortex.xraph.dev/a2a/extensions/fipa-acl/v1
```

It is declared in each card under `AgentCapabilities.extensions` as an
`AgentExtension{uri, description, required: false}`. **`required: false` is
deliberate**: a peer that has never heard of FIPA still receives a valid A2A
message and reads the text, and a peer that has gets the whole ACL envelope.
Declaring it required would refuse conversations we can perfectly well hold.

The ACL parameters ride in `Message.metadata` under one namespaced object:

```json
{
  "https://cortex.xraph.dev/a2a/extensions/fipa-acl/v1": {
    "performative": "request",
    "replyWith": "msg_01j...",
    "inReplyTo": "",
    "ontology": "ops",
    "protocol": "fipa-request",
    "language": "",
    "encoding": "",
    "replyBy": "2026-08-26T12:05:00Z"
  }
}
```

`conversationId` is absent from that object on purpose: it is `context_id`, a
native A2A field, and duplicating it would create two places to disagree.

An inbound message with no FIPA metadata is treated as `request` when it expects
an answer and `inform` when it does not, which is the reading that makes a
non-cortex peer work without knowing anything about us.

### 3.5 Errors

| A2A error | Code | Raised when |
|---|---|---|
| `TaskNotFoundError` | `-32001` | No run with that id in the peer's scope. |
| `TaskNotCancelableError` | `-32002` | The run is already terminal. |
| `PushNotificationNotSupportedError` | `-32003` | Any push-notification method. |
| `UnsupportedOperationError` | `-32004` | A method this binding does not implement. |
| `ContentTypeNotSupportedError` | `-32005` | A part type we cannot read (file or data parts, for now). |
| `InvalidAgentResponseError` | `-32006` | Outbound: a peer answered with something unparseable. |
| `ExtendedAgentCardNotConfiguredError` | `-32007` | `GetExtendedAgentCard`. |
| `ExtensionSupportRequiredError` | `-32008` | A peer demanded an extension we do not implement. |
| `VersionNotSupportedError` | `-32009` | An `A2A-Version` we do not speak. |

Standard `-32600` / `-32601` / `-32602` / `-32603` cover malformed requests,
unknown methods, bad params and internal failures.

---

## 4. Flow

### 4.1 Inbound

1. The binding parses the request into `Credentials`, a `tenant` and a `Message`.
2. `PeerResolver.ResolvePeer` authenticates and returns the `Peer`. A resolver
   error is `-32600` with no detail: an unauthenticated caller learns nothing
   about what exists.
3. The service puts **the resolver's scope** on the context. Not the message's,
   not a header's, not a field a caller can set.
4. The message maps to an `a2a.SendParams` whose sender is
   `{Agent: <claimed name>, Node: peer.Node}` and whose receiver is
   `{Agent: tenant}`, then goes through `Gateway.SendMessage`, which is the same
   bus path a local `agent_send` takes. Hop budget, conversation status and
   recipient resolution all apply exactly as they do locally.
5. An informative returns a `Message` acknowledgement. A directive returns a
   `Task` projected from the run the delivery started.

### 4.2 Outbound

1. An agent calls `agent_ask` with `to: "worker@peer.example"`. The address
   parses into `{Agent: "worker", Node: "peer.example"}`.
2. `Client.Handles` claims any non-local address whose `Node` is a registered
   peer. Anything else stays unroutable, so an agent cannot invent a hostname
   and have cortex call it.
3. `Client.Deliver` fetches the peer's Agent Card (cached, honouring
   `Cache-Control`), picks the first `supported_interfaces` entry whose
   `protocol_binding` we speak, preferring `JSONRPC`, and calls `SendMessage`
   with the tenant that interface declares.
4. A `Message` response is the answer. A `Task` response in a terminal state
   carries the answer in its artifacts. A non-terminal `Task` is polled with
   backoff until the ask's own deadline, which spec 1 already enforces and
   already turns into a readable failure.
5. Either way the answer re-enters through `Bus.Send` carrying `InReplyTo`,
   which resolves the pending ask and resumes the waiting run. **The local and
   remote paths converge here**, so a resumed agent cannot tell whether its peer
   was in this process or another company's data centre.

---

## 5. Security

**Scope comes from the resolver and nowhere else.** A message body cannot name
its own scope, and a header cannot either. This is the single load-bearing rule
of the inbound path.

**A peer cannot impersonate a local agent.** Every sender a peer claims is
namespaced with the `Node` the resolver assigned, so the best a hostile peer can
do is claim to be a different agent at its own node.

**Cortex authenticates nothing itself.** It ships the `PeerResolver` seam and a
documented bearer-token reference implementation in the docs. Hosts already have
identity; a second, weaker identity system inside cortex would be a liability
rather than a convenience.

**Outbound trust is configuration, not data.** Peers come from
`WithA2APeer(node, endpoint, credentials)` at construction. An agent's own output
cannot introduce a new peer, which means a prompt-injected agent can at worst
misuse a peer you already trusted.

**Agent cards are public.** A card names an agent, its description and its
skills, so anything in an agent's description or skill list is disclosed to
anyone who can reach the endpoint. Card serving is opt-in per agent through
`WithA2AExposed(names...)`, defaulting to none.

---

## 6. Testing

**Mapping tests, no network.** Table-driven over every run state to task state,
envelope to message and back, and an inbound message with no FIPA metadata
landing on the right default performative.

**Service tests with fakes.** A fake `Gateway` and a fake `PeerResolver` cover:
the resolver's scope reaching the gateway call, a resolver error refusing before
anything is written, an informative returning a `Message` while a directive
returns a `Task`, and every error code being raised by the condition that names
it.

**Binding tests.** Golden request and response bodies per binding, asserting the
three produce identical outcomes for identical semantics. That is the property
the shared `Service` exists to give, so it is worth a test rather than a comment.

**The loopback test.** Two engines, each with its own sqlite store, one served
over `httptest`. An agent on engine A asks an agent on engine B, over the wire,
and A's run resumes with B's answer. Nothing about this can be proven by unit
tests, and everything about the feature depends on it.

**Interop check, by hand.** The card and one `SendMessage` exchange validated
against the published A2A JSON schema, so a mistyped field name is caught by the
spec's own artifact rather than by a peer.

---

## 7. Implementation phases

**Plan 1, the core.** The module, `Service`, seams, types, mapping, the FIPA
extension, cards, the JSONRPC binding, the outbound client, and the two core
module changes. Ends with the loopback test passing over JSON-RPC.

**Plan 2, the other two bindings.** `HTTP+JSON` handlers and the gRPC service
generated from the normative proto, both over the same `Service`, plus the
binding-equivalence tests.

**Plan 3, streaming.** `SendStreamingMessage` and `SubscribeToTask` over SSE,
mapped from `Engine.StreamAgent` events, and `AgentCapabilities.streaming: true`
once they work.

---

## 8. Out of scope and follow-ups

- Push notifications, card signatures and the extended card, all declared
  unsupported rather than silently missing.
- **Contract Net** (spec 3), which needs nothing from here beyond the
  performatives already carried.
- A peer registry with CRUD. Trust stays in configuration for now; making it
  runtime-mutable data is a change in blast radius that deserves its own
  decision.
- Cross-process delivery redrive. A delivery to a peer that is down fails and is
  answered by the ask deadline; it is not retried with backoff. That is worth
  revisiting once there is operational evidence about how peers actually fail.
