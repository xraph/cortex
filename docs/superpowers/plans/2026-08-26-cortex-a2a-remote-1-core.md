# Cortex A2A Remote, Plan 1: core, cards, JSON-RPC and the client

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cortex agents reachable by remote A2A clients and able to call remote peers, over JSON-RPC, with FIPA-ACL semantics surviving the hop.

**Architecture:** A new module `a2aremote` holds one `Service` with every semantic decision in it; bindings are format translation over that service. Outbound is a `Client` implementing the `a2a.Transport` interface spec 1 already defined. The core module gains two small things: transports actually consulted at delivery time, and a way to register one after the bus exists.

**Tech Stack:** Go 1.26, standard library `net/http` and `encoding/json`. No new dependency in the core module. The new module depends only on the root module.

**Spec:** [docs/superpowers/specs/2026-08-26-cortex-a2a-remote-transport-design.md](../specs/2026-08-26-cortex-a2a-remote-transport-design.md)

**Depends on:** spec 1, complete and merged on this branch.

## Deviation from the fully-worked format

Like the second plan of spec 1, this carries full code where a wrong choice is expensive (the security rules, the mapping tables, the transport hand-off) and interfaces plus test names where the work is mechanical. The reference implementations are in this repo already.

## Global Constraints

- **TDD**, failing test first, every unit.
- **The core module gains no dependency.** `a2aremote` may import the root module; nothing in the root module may import `a2aremote`.
- **Scope comes from the `PeerResolver` and nowhere else.** No message field, no header, no query parameter may influence which cortex scope an inbound request acts in. A test asserts this directly.
- **Wire names come from the normative proto**, `github.com/a2aproject/A2A/specification/a2a.proto`: JSON field names are `lowerCamelCase` of the proto field names (`messageId`, `contextId`, `taskId`, `referenceTaskIds`, `protocolBinding`, `supportedInterfaces`, `defaultInputModes`). Do not invent casing.
- **Method strings are PascalCase**: `SendMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`, `GetExtendedAgentCard`.
- **Agent cards are served at `/.well-known/agent-card.json`**, never `/.well-known/agent.json`.
- `make lint` clean, `gofmt` clean, conventional commits, no AI attribution.

---

### Task 1: Delivery consults the transports

Spec 1 shipped a `Transport` seam that `deliverOne` never asked. A remote address was checked for routability at send time and then delivered by running a local agent of the same name.

**Files:**
- Modify: `a2a/deliver.go`, `a2a/bus.go`
- Test: `a2a/deliver_remote_test.go`, `a2a/fakes_test.go`

**Interfaces:**
- Produces: `(*Bus).AddTransport(t Transport)`, and `deliverOne` routing a non-local receiver to the transport that handles it.

- [ ] **Step 1: Write the failing test**

```go
// fakeTransport records what it was asked to carry.
type fakeTransport struct {
	mu       sync.Mutex
	node     string
	carried  []*Envelope
	err      error
}

func (f *fakeTransport) Handles(addr Address) bool { return addr.Node == f.node }

func (f *fakeTransport) Deliver(_ context.Context, e *Envelope, _ Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.carried = append(f.carried, e)
	return nil
}

func TestRemoteReceiverGoesToTheTransport(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	tr := &fakeTransport{node: "peer.example"}
	b.AddTransport(tr)

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
		Performative: Request, Content: "over there please",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The local runner must not have been used. A remote address that ran
	// a local agent of the same name is the bug this test exists for.
	if runner.callCount() != 0 {
		t.Fatalf("the local runner ran %d times for a remote address", runner.callCount())
	}
	if len(tr.carried) != 1 || tr.carried[0].Content != "over there please" {
		t.Fatalf("transport carried %+v, want the message", tr.carried)
	}
	_ = st
}

func TestRemoteDeliveryFailureDoesNotRunLocally(t *testing.T) {
	b, st, runner, _, hooks, _ := newTestBus(t)
	ctx := testCtx()
	tr := &fakeTransport{node: "peer.example", err: errors.New("peer unreachable")}
	b.AddTransport(tr)

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
		Performative: Request, Content: "hello?",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if runner.callCount() != 0 {
		t.Fatal("a failed remote delivery must never fall back to a local agent")
	}
	if hooks.refused() != 1 {
		t.Fatalf("MessageRefused fired %d times, want 1", hooks.refused())
	}
	rows, _ := st.ListQueuedDeliveries(ctx, 10)
	if len(rows) != 0 {
		t.Fatalf("%d rows left queued after a failed delivery", len(rows))
	}
}

// A remote ask must still be answerable: the transport carries the
// question, and the answer arrives later through Send.
func TestRemoteAskIsResolvedByAReplyThroughTheBus(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	b.AddTransport(&fakeTransport{node: "peer.example"})

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
			Content: "status?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The remote peer answers, and the client feeds it back in here.
	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "worker", Node: "peer.example"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "all clear over here",
		ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); err != nil {
		t.Fatalf("reply Send: %v", err)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./a2a/ -run 'TestRemote'`
Expected: FAIL, `b.AddTransport undefined`, and once that compiles, the local runner is called for a remote address.

- [ ] **Step 3: Implement**

In `a2a/bus.go`:

```go
// AddTransport registers a transport after the bus exists.
//
// It exists because construction cannot be circular: a remote transport
// needs the bus (a peer's reply is fed back through Send so it can
// resolve a waiting ask), and the bus needs the transport. The host
// builds the bus, builds the transport with it, and registers it here.
func (b *Bus) AddTransport(t Transport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transports = append(b.transports, t)
}
```

`Bus` gains a `mu sync.RWMutex` guarding `transports`, and `routable` and the new `transportFor` take the read lock. Registration happens at startup and reads happen per delivery, so a plain mutex is right.

In `a2a/deliver.go`, `deliverOne` asks about the receiver before it asks about the performative:

```go
	// A remote receiver is carried by a transport, whatever the
	// performative says. Routing by class first would run a LOCAL agent
	// named like the remote one, which is how a message addressed to
	// somebody else's system gets answered by yours.
	if !d.Receiver.IsLocal() {
		return b.deliverRemote(ctx, d, e)
	}
```

and:

```go
// deliverRemote hands one delivery to whichever transport claims the
// address. A transport failure is a failed delivery, never a fallback:
// falling back to a local agent of the same name would answer another
// system's question with your own agent.
func (b *Bus) deliverRemote(ctx context.Context, d *Delivery, e *Envelope) error {
	t := b.transportFor(d.Receiver)
	if t == nil {
		return b.failDelivery(ctx, d, fmt.Errorf("%w: %s", ErrUnroutable, d.Receiver))
	}
	if err := t.Deliver(ctx, e, d.Receiver); err != nil {
		if askErr := b.resolveAskWithFailure(ctx, e.ReplyWith, err.Error()); askErr != nil {
			return askErr
		}
		return b.failDelivery(ctx, d, err)
	}
	return b.finishDelivery(ctx, d, e, id.AgentRunID{})
}
```

Note what `deliverRemote` does NOT do: it does not wait for a reply. The transport's job is to get the envelope there; the answer comes back later as an ordinary inbound message carrying `InReplyTo`, which is the same path a local reply takes.

- [ ] **Step 4: Verify**

Run: `go test ./a2a/ -race -count=2`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/
git commit -m "fix(a2a): carry remote receivers through their transport

The transport seam existed and the delivery path never asked it, so an
envelope addressed to another system would have been answered by a local
agent that happened to share the name."
```

---

### Task 2: The module, the wire types, and the errors

**Files:**
- Create: `a2aremote/go.mod`, `a2aremote/doc.go`, `a2aremote/types.go`, `a2aremote/errors.go`
- Test: `a2aremote/types_test.go`, `a2aremote/errors_test.go`

**Interfaces:**
- Produces: `Message`, `Part`, `Role`, `Task`, `TaskStatus`, `TaskState` (+ the eight constants), `Artifact`, `SendMessageResult`; `Error` with `Code`/`Message`/`Data`, the nine A2A codes, and `func ErrTaskNotFound(id string) *Error` style constructors.

Module file:

```
module github.com/xraph/cortex/a2aremote

go 1.26.0

replace github.com/xraph/cortex => ../

require github.com/xraph/cortex v1.6.1
```

- [ ] **Step 1: Write the failing test**

```go
// The JSON names are the protocol's, not ours. A field renamed by a
// careless refactor is a peer that silently stops understanding us, so
// the wire shape is pinned here rather than trusted.
func TestMessageJSONNames(t *testing.T) {
	m := Message{
		MessageID: "msg_1", ContextID: "conv_1", TaskID: "arun_1",
		Role: RoleAgent, Parts: []Part{{Text: "hello"}},
		Extensions: []string{FIPAExtensionURI}, ReferenceTaskIDs: []string{"arun_2"},
		Metadata: map[string]any{"k": "v"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"messageId", "contextId", "taskId", "role", "parts", "extensions", "referenceTaskIds", "metadata"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing wire field %q in %s", key, b)
		}
	}
}

func TestTaskStateStringsAreTheProtocolsOwn(t *testing.T) {
	want := map[TaskState]string{
		TaskStateSubmitted:     "TASK_STATE_SUBMITTED",
		TaskStateWorking:       "TASK_STATE_WORKING",
		TaskStateCompleted:     "TASK_STATE_COMPLETED",
		TaskStateFailed:        "TASK_STATE_FAILED",
		TaskStateCanceled:      "TASK_STATE_CANCELED",
		TaskStateRejected:      "TASK_STATE_REJECTED",
		TaskStateInputRequired: "TASK_STATE_INPUT_REQUIRED",
		TaskStateAuthRequired:  "TASK_STATE_AUTH_REQUIRED",
	}
	if len(want) != 8 {
		t.Fatalf("the protocol defines 8 task states, table has %d", len(want))
	}
	for state, s := range want {
		if string(state) != s {
			t.Errorf("state = %q, want %q", state, s)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	cases := []struct {
		err  *Error
		code int
	}{
		{ErrTaskNotFound("arun_1"), -32001},
		{ErrTaskNotCancelable("arun_1"), -32002},
		{ErrPushNotificationNotSupported(), -32003},
		{ErrUnsupportedOperation("GetExtendedAgentCard"), -32004},
		{ErrContentTypeNotSupported("file"), -32005},
		{ErrInvalidAgentResponse("no parts"), -32006},
		{ErrExtendedCardNotConfigured(), -32007},
		{ErrExtensionSupportRequired("urn:x"), -32008},
		{ErrVersionNotSupported("0.1"), -32009},
	}
	for _, tc := range cases {
		if tc.err.Code != tc.code {
			t.Errorf("%s: code = %d, want %d", tc.err.Message, tc.err.Code, tc.code)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails.** `cd a2aremote && go test ./...`

- [ ] **Step 3: Implement** the types with exact JSON tags, and the errors as a `*Error` implementing `error` with a `Code int`, `Message string`, `Data []map[string]any`. Constructors name the offending value in the message, because a peer debugging an integration reads that string.

- [ ] **Step 4: Verify.** `cd a2aremote && go test ./... -v`

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): add the A2A wire types and error codes"
```

---

### Task 3: Mapping

The heart of the interop: cortex shapes in, protocol shapes out, and back.

**Files:**
- Create: `a2aremote/mapping.go`
- Test: `a2aremote/mapping_test.go`

**Interfaces:**
- Consumes: the types from Task 2, `a2a.Envelope`, `run.Run`.
- Produces:
  - `const FIPAExtensionURI = "https://cortex.xraph.dev/a2a/extensions/fipa-acl/v1"`
  - `func MessageFromEnvelope(e *a2a.Envelope) Message`
  - `func EnvelopeParamsFromMessage(m Message, sender a2a.Address, receiver a2a.Address) (a2a.SendParams, error)`
  - `func TaskFromRun(r *run.Run, contextID string) Task`
  - `func taskState(s run.State) TaskState`

- [ ] **Step 1: Write the failing test**

```go
func TestTaskStateProjection(t *testing.T) {
	cases := map[run.State]TaskState{
		run.StateCreated:   TaskStateSubmitted,
		run.StateRunning:   TaskStateWorking,
		run.StateCompleted: TaskStateCompleted,
		run.StateFailed:    TaskStateFailed,
		run.StateCancelled: TaskStateCanceled,
		// A paused run is waiting on something outside itself, which is
		// exactly what INPUT_REQUIRED means. The peer learns that it is
		// waiting without learning whose business it is waiting on.
		run.StatePaused: TaskStateInputRequired,
	}
	for state, want := range cases {
		if got := taskState(state); got != want {
			t.Errorf("%s -> %s, want %s", state, got, want)
		}
	}
}

func TestEnvelopeRoundTripsThroughAMessage(t *testing.T) {
	deadline := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	e := &a2a.Envelope{
		ID: id.NewMessageID(), ConversationID: id.NewConversationID(),
		Performative: a2a.CFP, Sender: a2a.Address{Agent: "planner"},
		Receivers: []a2a.Address{{Agent: "worker", Node: "peer.example"}},
		Content:   "who can take this?", Ontology: "ops", Protocol: "fipa-contract-net",
		ReplyWith: "rw-1", ReplyBy: &deadline,
	}
	m := MessageFromEnvelope(e)

	if m.ContextID != e.ConversationID.String() {
		t.Errorf("contextId = %q, want the conversation id", m.ContextID)
	}
	if len(m.Parts) != 1 || m.Parts[0].Text != "who can take this?" {
		t.Errorf("parts lost the content: %+v", m.Parts)
	}
	if len(m.Extensions) != 1 || m.Extensions[0] != FIPAExtensionURI {
		t.Errorf("the message must declare the extension it used: %+v", m.Extensions)
	}

	params, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "planner", Node: "peer.example"}, a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Performative != a2a.CFP {
		t.Errorf("performative = %s, want cfp", params.Performative)
	}
	if params.Ontology != "ops" || params.Protocol != "fipa-contract-net" || params.ReplyWith != "rw-1" {
		t.Errorf("ACL parameters did not survive: %+v", params)
	}
	if params.ReplyBy == nil || !params.ReplyBy.Equal(deadline) {
		t.Errorf("replyBy did not survive: %v", params.ReplyBy)
	}
}

// A peer that has never heard of FIPA still has to work. This is the
// test that keeps the extension optional in practice and not just in
// the card.
func TestMessageWithoutFIPAMetadataGetsASensibleDefault(t *testing.T) {
	plain := Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "do the thing"}}}

	params, err := EnvelopeParamsFromMessage(plain, a2a.Address{Agent: "someone", Node: "peer.example"}, a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Performative != a2a.Request {
		t.Fatalf("performative = %s, want request for a plain inbound message", params.Performative)
	}
	if params.Content != "do the thing" {
		t.Fatalf("content = %q", params.Content)
	}
}

func TestMessageWithSeveralTextPartsIsJoined(t *testing.T) {
	m := Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "first"}, {Text: "second"}}}
	params, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "s", Node: "n"}, a2a.Address{Agent: "w"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Content != "first\n\nsecond" {
		t.Fatalf("content = %q, want the parts joined", params.Content)
	}
}

func TestUnsupportedPartTypeIsRefused(t *testing.T) {
	m := Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{File: &FilePart{URL: "https://x/y.png"}}}}
	_, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "s", Node: "n"}, a2a.Address{Agent: "w"})
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != -32005 {
		t.Fatalf("err = %v, want ContentTypeNotSupportedError", err)
	}
}

func TestTaskFromRun(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted, Output: "done and dusted"}
	task := TaskFromRun(r, "conv_1")

	if task.ID != r.ID.String() {
		t.Errorf("task id = %q, want the run id verbatim", task.ID)
	}
	if task.ContextID != "conv_1" {
		t.Errorf("contextId = %q", task.ContextID)
	}
	if task.Status.State != TaskStateCompleted {
		t.Errorf("state = %s", task.Status.State)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].Parts[0].Text != "done and dusted" {
		t.Errorf("the run output must arrive as an artifact: %+v", task.Artifacts)
	}
}

func TestFailedRunCarriesItsErrorIntoTheStatus(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateFailed, Error: "the model refused"}
	task := TaskFromRun(r, "")
	if task.Status.State != TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
	if task.Status.Message == nil || !strings.Contains(task.Status.Message.Parts[0].Text, "the model refused") {
		t.Fatalf("a failed task must say why: %+v", task.Status)
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** The FIPA metadata lives under one namespaced key so it cannot collide with a peer's own metadata:

```go
// fipaMeta is the ACL parameter set as it travels in Message.metadata,
// under FIPAExtensionURI as its key.
//
// conversationId is deliberately absent: it is contextId, a native A2A
// field, and carrying it twice would create two places to disagree.
type fipaMeta struct {
	Performative string `json:"performative,omitempty"`
	ReplyWith    string `json:"replyWith,omitempty"`
	InReplyTo    string `json:"inReplyTo,omitempty"`
	Ontology     string `json:"ontology,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	Language     string `json:"language,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	ReplyBy      string `json:"replyBy,omitempty"`
}
```

`EnvelopeParamsFromMessage` reads that key when present and falls back to `a2a.Request` when absent, and it **always** takes the sender from its argument rather than from anything in the message. The signature makes that hard to get wrong: the caller passes the sender the resolver assigned.

- [ ] **Step 4: Verify.** `cd a2aremote && go test ./... -race`

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): map envelopes to A2A messages and runs to tasks"
```

---

### Task 4: Agent cards

**Files:**
- Create: `a2aremote/card.go`
- Test: `a2aremote/card_test.go`

**Interfaces:**
- Produces: `AgentCard`, `AgentInterface`, `AgentCapabilities`, `AgentExtension`, `AgentSkill`, `AgentProvider`; `func BuildCard(a *agent.Config, skills []*skill.Skill, opts CardOptions) AgentCard`; `func (s *Service) CardHandler() http.Handler`; `func FetchCard(ctx context.Context, c *http.Client, baseURL string) (AgentCard, error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildCardDeclaresTheAgentAsATenant(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "db-expert", Description: "knows the database"}, nil, CardOptions{
		BaseURL: "https://cortex.example/a2a", Version: "1.0.0",
		Provider: AgentProvider{Organization: "acme", URL: "https://acme.example"},
	})

	if card.Name != "db-expert" {
		t.Errorf("name = %q", card.Name)
	}
	if len(card.SupportedInterfaces) == 0 {
		t.Fatal("a card with no interface tells a client nothing about how to reach it")
	}
	iface := card.SupportedInterfaces[0]
	if iface.ProtocolBinding != "JSONRPC" {
		t.Errorf("protocolBinding = %q, want JSONRPC", iface.ProtocolBinding)
	}
	// tenant is how one endpoint serves many agents, and it is the
	// protocol's own mechanism rather than a cortex convention.
	if iface.Tenant != "db-expert" {
		t.Errorf("tenant = %q, want the agent name", iface.Tenant)
	}
}

func TestCardDeclaresTheFIPAExtensionAsOptional(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, nil, CardOptions{BaseURL: "https://x/a2a"})
	var found *AgentExtension
	for i := range card.Capabilities.Extensions {
		if card.Capabilities.Extensions[i].URI == FIPAExtensionURI {
			found = &card.Capabilities.Extensions[i]
		}
	}
	if found == nil {
		t.Fatal("the card must declare the extension its messages use")
	}
	// Required would refuse conversations we can hold perfectly well: a
	// peer that ignores the extension still gets valid A2A and reads the
	// text.
	if found.Required {
		t.Fatal("the FIPA extension must be optional")
	}
}

func TestCardDeclaresWhatIsNotSupported(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, nil, CardOptions{BaseURL: "https://x/a2a"})
	if card.Capabilities.PushNotifications {
		t.Error("push notifications are not implemented and must not be advertised")
	}
	if card.Capabilities.ExtendedAgentCard {
		t.Error("the extended card is not implemented and must not be advertised")
	}
	if card.Capabilities.Streaming {
		t.Error("streaming lands in plan 3, so it must not be advertised yet")
	}
}

func TestCardSkillsComeFromTheAgentsSkills(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, []*skill.Skill{
		{Name: "sql-review", Description: "reviews SQL migrations"},
	}, CardOptions{BaseURL: "https://x/a2a"})
	if len(card.Skills) != 1 || card.Skills[0].Name != "sql-review" {
		t.Fatalf("skills = %+v", card.Skills)
	}
	// An agent with no skills still needs one entry: skills is REQUIRED
	// in the schema, and an empty list makes the agent look useless.
	bare := BuildCard(&agent.Config{Name: "a", Description: "does things"}, nil, CardOptions{BaseURL: "https://x/a2a"})
	if len(bare.Skills) != 1 {
		t.Fatalf("a skill-less agent needs one synthesised skill, got %+v", bare.Skills)
	}
}

func TestCardHandlerServesTheWellKnownPath(t *testing.T) {
	// Serve, fetch, and compare. The path is the one a 1.0 client looks
	// at; 0.x used /.well-known/agent.json and nothing looks there now.
	// (Full test in the file: httptest.Server + FetchCard round trip.)
}
```

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** `CardHandler` serves `/{prefix}/agents/{name}/.well-known/agent-card.json` for every exposed agent, and the default agent's card at `/.well-known/agent-card.json`. It returns 404 for an agent that exists but was not exposed, because a card is public and exposure is opt-in.

`FetchCard` sets `Accept: application/json`, honours `Cache-Control: max-age` for the caller's cache, and refuses a body over 1 MiB.

- [ ] **Step 4: Verify.**

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): build, serve and fetch agent cards"
```

---

### Task 5: The service

Every semantic decision lives here, so that three bindings can share one implementation and one set of security rules.

**Files:**
- Create: `a2aremote/seams.go`, `a2aremote/service.go`
- Test: `a2aremote/service_test.go`, `a2aremote/fakes_test.go`

**Interfaces:**
- Produces: `Gateway`, `PeerResolver`, `Credentials`, `Peer`, `Options`, `NewService`, and the four methods `SendMessage`, `GetTask`, `ListTasks`, `CancelTask`, each taking `(ctx, Credentials, <request>)`.

```go
type Gateway interface {
	SendMessage(ctx context.Context, p a2a.SendParams) (*a2a.SendResult, error)
	GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error)
	ListRuns(ctx context.Context, f *run.ListFilter) ([]*run.Run, error)
	CancelRun(ctx context.Context, runID id.AgentRunID) error
	GetAgentByName(ctx context.Context, name string) (*agent.Config, error)
}
```

- [ ] **Step 1: Write the failing tests**

```go
func TestSendMessageUsesTheResolversScopeAndNotTheMessages(t *testing.T)
func TestSendMessageRefusesAnUnauthenticatedCaller(t *testing.T)
func TestAResolverErrorWritesNothing(t *testing.T)
func TestSenderIsNamespacedByThePeersNode(t *testing.T)
func TestUnknownTenantIsTaskNotFoundNotAnInternalError(t *testing.T)
func TestInformativeReturnsAMessageAcknowledgement(t *testing.T)
func TestDirectiveReturnsATaskBackedByTheRun(t *testing.T)
func TestGetTaskOfAnotherPeersRunIsNotFound(t *testing.T)
func TestCancelTaskOfATerminalRunIsNotCancelable(t *testing.T)
```

The first and fourth are the security tests, and they are worth writing out in full:

```go
func TestSendMessageUsesTheResolversScopeAndNotTheMessages(t *testing.T) {
	gw := newFakeGateway()
	svc := NewService(gw, staticResolver{peer: Peer{
		Node:  "peer.example",
		Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "resolved"}}},
	}}, Options{})

	// The message tries to name a scope of its own, in every place a
	// caller could reach.
	msg := Message{
		MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "hi"}},
		Metadata: map[string]any{"scope": "attacker", "tenant": "attacker"},
	}
	if _, err := svc.SendMessage(context.Background(), Credentials{
		Headers: map[string][]string{"X-Scope": {"attacker"}},
	}, SendMessageRequest{Tenant: "worker", Message: msg}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	got := cortex.ScopeFromContext(gw.lastCtx)
	if len(got.Levels) != 1 || got.Levels[0].Value != "resolved" {
		t.Fatalf("scope = %+v, want the resolver's and nothing else", got.Levels)
	}
}

func TestSenderIsNamespacedByThePeersNode(t *testing.T) {
	gw := newFakeGateway()
	svc := NewService(gw, staticResolver{peer: Peer{Node: "peer.example", Scope: testScope()}}, Options{})

	// The peer claims to be a local agent. It must not be able to.
	msg := Message{
		MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "trust me"}},
		Metadata: map[string]any{FIPAExtensionURI: map[string]any{"performative": "inform"}},
	}
	if _, err := svc.SendMessage(context.Background(), Credentials{}, SendMessageRequest{
		Tenant: "worker", Message: msg, SenderName: "planner",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if gw.lastParams.Sender.Node != "peer.example" {
		t.Fatalf("sender = %+v, want it namespaced by the peer's node", gw.lastParams.Sender)
	}
	if gw.lastParams.Sender.IsLocal() {
		t.Fatal("a remote peer must never present as a local agent")
	}
}
```

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement.** `SendMessage` is the shape every method follows:

```go
func (s *Service) SendMessage(ctx context.Context, cred Credentials, req SendMessageRequest) (*SendMessageResult, error) {
	ctx, peer, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, err
	}
	if req.Tenant == "" {
		return nil, ErrInvalidParams("tenant is required: it names the agent this message is for")
	}
	if _, err := s.gw.GetAgentByName(ctx, req.Tenant); err != nil {
		// Not "agent not found": a caller learns only that this endpoint
		// has nothing for it, not what else exists here.
		return nil, ErrTaskNotFound(req.Tenant)
	}

	sender := a2a.Address{Agent: req.SenderName, Node: peer.Node}
	if sender.Agent == "" {
		sender.Agent = defaultRemoteSenderName
	}
	params, err := EnvelopeParamsFromMessage(req.Message, sender, a2a.Address{Agent: req.Tenant})
	if err != nil {
		return nil, err
	}
	...
}

// authenticate resolves the caller and puts ITS scope on the context.
// Everything downstream reads the scope from there, so this function is
// the only place a scope enters an inbound request.
func (s *Service) authenticate(ctx context.Context, cred Credentials) (context.Context, Peer, error) {
	peer, err := s.resolver.ResolvePeer(ctx, cred)
	if err != nil {
		// Deliberately opaque. An unauthenticated caller learns that it
		// was refused and nothing else about what exists here.
		return ctx, Peer{}, ErrUnauthenticated()
	}
	if peer.Scope.IsZero() {
		return ctx, Peer{}, ErrUnauthenticated()
	}
	return cortex.WithScope(ctx, peer.Scope), peer, nil
}
```

- [ ] **Step 4: Verify.** `cd a2aremote && go test ./... -race`

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): add the service every binding shares"
```

---

### Task 6: The JSON-RPC binding

**Files:**
- Create: `a2aremote/jsonrpc.go`
- Test: `a2aremote/jsonrpc_test.go`

**Interfaces:**
- Produces: `func (s *Service) JSONRPCHandler() http.Handler`.

- [ ] **Step 1: Write the failing tests**

```go
func TestJSONRPCSendMessage(t *testing.T)               // golden request and response body
func TestJSONRPCUnknownMethodIs32601(t *testing.T)
func TestJSONRPCMalformedBodyIs32700(t *testing.T)
func TestJSONRPCServiceErrorKeepsItsCode(t *testing.T)  // -32001 survives the binding
func TestJSONRPCNotificationGetsNoResponse(t *testing.T) // id absent = notification
func TestJSONRPCRejectsAnUnknownA2AVersion(t *testing.T) // -32009
```

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement.** A `POST`-only handler that decodes `{jsonrpc, id, method, params}`, dispatches on the PascalCase method name, and encodes `{jsonrpc:"2.0", id, result}` or `{jsonrpc:"2.0", id, error:{code,message,data}}`. The `A2A-Version` header is checked when present; absent means the current version.

The binding does no semantics. If a reviewer sees a policy decision in this file, it belongs in the service.

- [ ] **Step 4: Verify.**

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): add the JSON-RPC binding"
```

---

### Task 7: The outbound client

**Files:**
- Create: `a2aremote/client.go`, `a2aremote/peers.go`
- Test: `a2aremote/client_test.go`

**Interfaces:**
- Produces: `PeerConfig{Node, BaseURL, Header http.Header}`, `NewClient(peers []PeerConfig, sink ReplySink, opts ClientOptions) *Client`, and `Client` satisfying `a2a.Transport`. `ReplySink` is `func(ctx context.Context, p a2a.SendParams) error`, which the engine wires to `Bus.Send`.

- [ ] **Step 1: Write the failing tests**

```go
func TestClientHandlesOnlyRegisteredPeers(t *testing.T)  // an invented hostname is not handled
func TestDeliverSendsAMessageAndFeedsTheReplyBack(t *testing.T)
func TestDeliverPollsANonTerminalTask(t *testing.T)
func TestDeliverSurfacesAPeerError(t *testing.T)
func TestClientCachesTheAgentCard(t *testing.T)          // one fetch across two deliveries
func TestClientPrefersJSONRPCAmongInterfaces(t *testing.T)
```

`TestClientHandlesOnlyRegisteredPeers` is the security one: an agent's own output must not be able to make cortex call an arbitrary host.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement.** `Deliver` fetches the card (cached per node), picks the interface, posts `SendMessage`, and turns the answer into a `ReplySink` call carrying `InReplyTo` set to the outbound envelope's `ReplyWith`. A `Task` that is not terminal is polled with `GetTask` on a backoff, and polling stops at the envelope's `ReplyBy`, because the ask's own deadline already turns silence into a readable failure.

- [ ] **Step 4: Verify.**

- [ ] **Step 5: Commit**

```bash
git add a2aremote/
git commit -m "feat(a2aremote): add the outbound client"
```

---

### Task 8: Engine wiring

**Files:**
- Create: `a2aremote/gateway.go` (the engine adapter lives on this side of the seam, so the core module keeps knowing nothing about A2A)
- Test: `a2aremote/gateway_test.go`

**Interfaces:**
- Produces: `func Gateway(eng *engine.Engine) Gateway` and `func Attach(eng *engine.Engine, opts AttachOptions) (*Service, error)`, which builds the service, builds the client from the configured peers, and registers it with `eng.A2A().AddTransport`.

- [ ] **Step 1: Write the failing test**

```go
func TestAttachRegistersTheTransport(t *testing.T)
func TestAttachRefusesAnEngineWithoutMessaging(t *testing.T) // WithA2A was never set
```

- [ ] **Step 2 to 5:** as before.

```bash
git add a2aremote/
git commit -m "feat(a2aremote): attach the remote transport to an engine"
```

---

### Task 9: The loopback test

Two engines, over the wire. This is the test the whole plan exists to make pass.

**Files:**
- Test: `a2aremote/loopback_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestTwoEnginesTalkOverTheWire stands up engine B behind an httptest
// server and has an agent on engine A ask it a question. A's run
// suspends, the answer travels back over JSON-RPC, and A resumes.
//
// Every piece of this is unit tested somewhere. What is not, and cannot
// be, is that the pieces line up: the ids, the extension metadata, the
// sender namespacing, the reply correlation, and the resume.
func TestTwoEnginesTalkOverTheWire(t *testing.T) {
	// engine B: serves "specialist" over JSON-RPC
	// engine A: has "planner", whose model calls agent_ask on
	//           specialist@peer-b, with peer-b registered at srv.URL
	// assert: A's run completes carrying B's answer, and the ask's tool
	//         call result contains it
}
```

- [ ] **Step 2 to 5:** as before.

```bash
git add a2aremote/
git commit -m "test(a2aremote): prove two engines hold a conversation over the wire"
```

---

## Self-review

**Spec coverage.** §2.1 to Task 2, §2.2 to Task 5, §2.3 to Task 1, §3.1 and §3.2 to Tasks 3 and 4, §3.3 and §3.4 to Task 3, §3.5 to Task 2, §4.1 to Tasks 5 and 6, §4.2 to Task 7, §5 to Tasks 5 and 7 (the two security tests are named and written out), §6 to every task plus Task 9.

Spec §7's plans 2 and 3, the REST and gRPC bindings and streaming, are deliberately absent: they are their own plans and they change nothing here.

**One thing this plan adds to the spec.** The spec says an unknown tenant is an error; the plan makes it `TaskNotFound` rather than a distinct "no such agent", so a caller cannot enumerate which agents exist by probing names. That is a security improvement, and it is worth carrying back into the spec if anyone reads the two together.
