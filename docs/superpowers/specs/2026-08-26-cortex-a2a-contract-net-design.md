# Cortex A2A: Contract Net (spec 3)

- **Status:** Approved, ready for implementation
- **Date:** 2026-08-26
- **Author:** Rex Raphael
- **Repos touched:** `github.com/xraph/cortex`
- **Related:** Third of the three specs decomposed in
  [2026-08-26-cortex-a2a-messaging-design.md](2026-08-26-cortex-a2a-messaging-design.md) §3.
- **Method:** Test-driven.

---

## 1. Goal

Let an agent put work out to tender: announce a task to several agents, collect
what they each say they can do, pick one, and hold the winner to it.

That is the FIPA Contract Net Interaction Protocol, and it is the oldest
task-allocation protocol in multi-agent systems for a reason. It handles the
case a manager cannot: the initiator does not know who is best placed, so it
asks rather than assigns.

```
initiator                          participants
    |-------- cfp ------------------>|  (to several at once)
    |<------- propose / refuse -------|  (each answers, or does not)
    |-------- accept-proposal ------->|  (to the one it picked)
    |-------- reject-proposal ------->|  (to the others)
    |<------- inform / failure -------|  (the winner reports back)
```

## 2. What is already here, and what is missing

Almost all of it exists. Spec 1 carries all four Contract Net performatives and
routes them correctly: `cfp`, `propose` and `accept-proposal` are directives
that start a run, `refuse` and `reject-proposal` are informatives that do not.
The conversation, the hop budget and the deadline are all in place, and
`Protocol` already travels on the envelope.

**One thing is missing: an ask that waits for several answers.**

`Bus.Ask` refuses more than one receiver, because a durable ask correlates one
reply to one waiting run. A call for proposals is exactly the case that
constraint rules out. So the whole of this spec is one change to that rule, plus
the vocabulary and the documentation to use it.

## 3. The change: asks that wait for a quorum

`Ask` accepts several receivers. The pending ask resolves when **every**
recipient has answered, or when the deadline passes, whichever comes first.

Nothing new is stored. The number of answers to expect is `len(Receivers)` on
the ask's own message, which is already persisted, and the answers themselves
are already persisted as messages carrying `in_reply_to`. Counting them is a
read of the conversation rather than a new table.

The resume payload becomes a list:

```go
// AskReply is what a resumed agent_ask returns to the model.
//
// Replies is plural because an ask may have gone to several agents, which
// is what a call for proposals is. A single-recipient ask carries exactly
// one entry, so nothing about the common case changes shape twice.
type AskReply struct {
	Replies []AskReplyItem `json:"replies"`
	// Complete says every recipient answered. False means the deadline
	// arrived first and Replies holds whoever did.
	Complete bool `json:"complete"`
}

type AskReplyItem struct {
	Performative   string `json:"performative"`
	Sender         string `json:"sender"`
	Content        string `json:"content"`
	ConversationID string `json:"conversation_id"`
}
```

That is a breaking change to the tool result an agent sees, and it is worth
taking now rather than shipping two shapes: an agent reading a single reply out
of a one-element list costs a prompt sentence, while a payload that changes
shape depending on recipient count costs every prompt forever.

### 3.1 Partial answers are the normal case

A participant that never answers is ordinary in Contract Net, not exceptional.
The deadline resolves the ask with whoever replied, `Complete` false, and the
initiator picks from what it got. An agent that asked three specialists and
heard from two should be able to proceed, and it can.

### 3.2 Refuse no longer resolves early

Today a `refuse` resolves a waiting ask. With several recipients that is wrong:
one participant declining a tender must not un-pause the initiator while the
others are still thinking. A refusal now counts as an answer from that
participant and nothing more.

## 4. What Contract Net looks like from an agent

No new tools. The protocol is the primitives used in the order the protocol
describes, which is the point: an interaction protocol is a convention, not a
mechanism.

```
1. agent_ask(to: ["a","b","c"], performative: "cfp",
             content: "who can review this migration by 5pm?",
             protocol: "fipa-contract-net")
      -> the run suspends, and resumes with every proposal and refusal

2. agent_send(to: ["b"], performative: "reject-proposal", ...)
   agent_send(to: ["c"], performative: "reject-proposal", ...)

3. agent_ask(to: "a", performative: "accept-proposal",
             content: "yours please, by 5pm")
      -> the run suspends again, and resumes with the winner's result
```

Step 3 is an ask rather than a send because the initiator wants the work, not
just an acknowledgement, and `accept-proposal` is already a directive.

## 5. The Go surface

A small package-level helper for hosts driving the protocol themselves rather
than through an agent's own reasoning:

```go
// ContractNet announces a task, collects what comes back, and reports it.
// Awarding is deliberately left to the caller: choosing a contractor is a
// judgement, and this package has no business making it.
func ContractNet(ctx context.Context, b *Bus, p ContractNetParams) (*Tender, error)

type ContractNetParams struct {
	Initiator  Address
	Recipients []Address
	Content    string
	Ontology   string
	ReplyBy    *time.Time
	AskerRunID id.AgentRunID
	ToolCallID string
}

// Tender is what a call for proposals came back with.
type Tender struct {
	ConversationID id.ConversationID
	ReplyWith      string
	Proposals      []Proposal
	Refusals       []Proposal
	Complete       bool
}

type Proposal struct {
	From    Address
	Content string
}
```

`ProtocolContractNet = "fipa-contract-net"` is stamped on the envelope, so a
reader of a stored conversation can tell a tender from an ordinary exchange, and
a remote peer sees the protocol name the FIPA specification uses.

## 6. Testing

- Multi-recipient ask resolving on the last answer, and on the deadline with
  whoever answered.
- A `refuse` from one participant not resolving an ask with three.
- A single-recipient ask still resolving on its one answer, with a one-element
  list.
- Exactly-once resume under a race between the last answer and the deadline
  sweep, which is the same claim discipline as before and needs to stay true
  with several writers arriving at once.
- An end-to-end tender through the engine with a fake LLM: three participants,
  two propose, one refuses, the initiator awards and gets the work back.

## 7. Out of scope

- **Iterated Contract Net** (FIPA00030), where the initiator counter-proposes
  and rounds repeat. It is expressible with these primitives already; what it
  would need from cortex is nothing.
- **Automatic award.** Cortex will not pick a winner. Choosing is a judgement,
  and an agent or a host is what makes it.
- **Bid formats.** A proposal's content is text, like every other message. A
  host that wants structured bids puts JSON in it and says so in the ontology.
