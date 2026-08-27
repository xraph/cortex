# Cortex A2A Plan 1: the messaging package

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `a2a` leaf package: FIPA-ACL envelopes, conversations, mailboxes, correlation, containment and a dispatcher, driven entirely by test doubles.

**Architecture:** `a2a` imports only `cortex` and `id`. It reaches the host through four injected seams (`cortex.AgentRunner`, `Resumer`, `Store`, `Transport`). This plan wires none of them to the engine or a database. Every test uses fakes, so the whole package is provable without an LLM or a running Postgres.

**Tech Stack:** Go 1.25, standard library only. Tests are table-driven with `testing` and `t.Run` subtests. No new dependencies.

**Spec:** [docs/superpowers/specs/2026-08-26-cortex-a2a-messaging-design.md](../specs/2026-08-26-cortex-a2a-messaging-design.md)

## Global Constraints

- **Leaf package.** `a2a` may import `github.com/xraph/cortex` and `github.com/xraph/cortex/id` and nothing else from this module. It must never import `engine`, `plugin`, `store` or `orchestration`. A test that greps the import block enforces this.
- **TDD, no exceptions.** Failing test first, watch it fail, minimal implementation, watch it pass, commit. Every task below is written in that order.
- **No wall-clock reads inside the package.** All time comes from an injected `Clock`. `time.Now()` appears in exactly one place, the default clock. This is what makes deadline tests deterministic.
- **Determinism.** The dispatcher must be drainable synchronously in tests. Any test that sleeps to wait for a worker is a defective test.
- **Scope on the context.** Every store call takes `ctx` and the scope rides on it, per `cortex.ScopeFromContext`. Never pass a scope as a bare argument.
- **Errors are sentinels.** New package errors are `var Err... = errors.New("cortex: a2a: ...")` and are matched with `errors.Is`. Follow [errors.go](../../../errors.go).
- **Lint.** `make lint` must pass. The repo runs golangci-lint with the config in [.golangci.yml](../../../.golangci.yml). Exported identifiers need doc comments.
- **Commit style.** Conventional commits, no `Co-Authored-By` trailer, no AI attribution.

---

### Task 1: Lift `AgentRunner` to the root package

Two packages now need the identical interface. Move it once, alias it where it lived, and no caller changes.

**Files:**
- Create: `runner.go`
- Modify: `orchestration/orchestrator.go` (delete the three type declarations, replace with aliases)
- Test: `runner_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `cortex.RunOpts`, `cortex.AgentResult`, `cortex.AgentRunner`. Every later task in every plan depends on these names.

- [ ] **Step 1: Write the failing test**

```go
package cortex_test

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/orchestration"
)

// stubRunner satisfies cortex.AgentRunner. The compile-time assertion below
// is the real test: orchestration.AgentRunner must be the same type, so a
// single implementation satisfies both names.
type stubRunner struct{}

func (stubRunner) RunAgent(context.Context, string, string, *cortex.RunOpts) (*cortex.AgentResult, error) {
	return &cortex.AgentResult{AgentName: "a", Output: "ok"}, nil
}

var (
	_ cortex.AgentRunner        = stubRunner{}
	_ orchestration.AgentRunner = stubRunner{}
)

func TestAgentRunnerIsSharedAcrossPackages(t *testing.T) {
	var r cortex.AgentRunner = stubRunner{}
	got, err := r.RunAgent(context.Background(), "a", "in", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if got.Output != "ok" {
		t.Fatalf("Output = %q, want %q", got.Output, "ok")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestAgentRunnerIsSharedAcrossPackages`
Expected: FAIL to compile, `undefined: cortex.AgentRunner`.

- [ ] **Step 3: Write minimal implementation**

Create `runner.go`:

```go
package cortex

import (
	"context"

	"github.com/xraph/cortex/id"
)

// RunOpts is the subset of run overrides a caller needs when invoking an
// agent through AgentRunner. The engine maps it to its own RunOverrides.
type RunOpts struct {
	Model        string
	Temperature  *float64
	MaxSteps     int
	SystemPrompt string
}

// AgentResult is the caller-facing view of one completed agent run.
type AgentResult struct {
	AgentName string        `json:"agent_name"`
	RunID     id.AgentRunID `json:"run_id,omitempty"`
	Output    string        `json:"output"`
	Err       error         `json:"-"`
}

// AgentRunner is the one host capability the coordination packages depend
// on: run a named agent and hand back its result. The engine satisfies it
// through a thin adapter, which is what keeps orchestration and a2a from
// importing the engine.
type AgentRunner interface {
	RunAgent(ctx context.Context, agentName, input string, opts *RunOpts) (*AgentResult, error)
}
```

In `orchestration/orchestrator.go`, delete the `RunOpts`, `AgentResult` and `AgentRunner` declarations and put aliases in their place:

```go
// RunOpts, AgentResult and AgentRunner moved to the root cortex package
// when a2a came to need the same seam. They are aliased here so every
// existing caller and every stored strategy keeps compiling unchanged.
type (
	RunOpts     = cortex.RunOpts
	AgentResult = cortex.AgentResult
	AgentRunner = cortex.AgentRunner
)
```

`orchestration` already imports `cortex`, so no import change is needed. Delete the now-unused `id` import only if nothing else in that file uses it (`Handoff` and `Result` do, so keep it).

- [ ] **Step 4: Run the full suite**

Run: `go test ./...`
Expected: PASS. Nothing else should change, because aliases are the same type.

- [ ] **Step 5: Commit**

```bash
git add runner.go runner_test.go orchestration/orchestrator.go
git commit -m "refactor(cortex): lift AgentRunner to the root package

a2a needs the identical seam orchestration already had, so the interface
moves up and orchestration aliases it. Aliases keep every existing caller
and the engine adapter compiling untouched."
```

---

### Task 2: Performatives and routing classes

**Files:**
- Create: `a2a/performative.go`
- Test: `a2a/performative_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `a2a.Performative` (string type), the 22 constants, `a2a.Class` (string type) with `ClassDirective`, `ClassInformative`, `ClassControl`, and `func (p Performative) Class() (Class, bool)`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import "testing"

// The table is exhaustive on purpose. A performative added later without a
// routing class fails here rather than silently defaulting to the inbox.
func TestPerformativeClass(t *testing.T) {
	cases := map[Performative]Class{
		Request:        ClassDirective,
		RequestWhen:    ClassDirective,
		RequestWhenever: ClassDirective,
		QueryIf:        ClassDirective,
		QueryRef:       ClassDirective,
		CFP:            ClassDirective,
		Propose:        ClassDirective,
		AcceptProposal: ClassDirective,

		Inform:         ClassInformative,
		InformIf:       ClassInformative,
		InformRef:      ClassInformative,
		Confirm:        ClassInformative,
		Disconfirm:     ClassInformative,
		Agree:          ClassInformative,
		Refuse:         ClassInformative,
		Failure:        ClassInformative,
		NotUnderstood:  ClassInformative,
		RejectProposal: ClassInformative,
		Subscribe:      ClassInformative,
		Proxy:          ClassInformative,
		Propagate:      ClassInformative,

		Cancel: ClassControl,
	}

	if len(cases) != 22 {
		t.Fatalf("table covers %d performatives, FIPA-ACL defines 22", len(cases))
	}
	for p, want := range cases {
		got, ok := p.Class()
		if !ok {
			t.Errorf("%s: not classified", p)
			continue
		}
		if got != want {
			t.Errorf("%s: class = %s, want %s", p, got, want)
		}
	}
}

func TestAllPerformativesAreClassified(t *testing.T) {
	for _, p := range AllPerformatives() {
		if _, ok := p.Class(); !ok {
			t.Errorf("%s has no routing class", p)
		}
	}
	if len(AllPerformatives()) != 22 {
		t.Fatalf("AllPerformatives returned %d, want 22", len(AllPerformatives()))
	}
}

func TestUnknownPerformativeIsNotClassified(t *testing.T) {
	if _, ok := Performative("shout").Class(); ok {
		t.Fatal("an invented performative must not classify")
	}
}

// ResolvesAsk is the pair most likely to be got backwards: agree means the
// peer took the job and is still working, so it must not un-pause the asker.
func TestResolvesAsk(t *testing.T) {
	resolving := []Performative{Inform, InformIf, InformRef, Confirm, Disconfirm, Refuse, Failure, NotUnderstood, RejectProposal}
	for _, p := range resolving {
		if !p.ResolvesAsk() {
			t.Errorf("%s should resolve a waiting ask", p)
		}
	}
	for _, p := range []Performative{Agree, Subscribe, Request, CFP} {
		if p.ResolvesAsk() {
			t.Errorf("%s must not resolve a waiting ask", p)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestPerformative`
Expected: FAIL, the package does not exist yet.

- [ ] **Step 3: Write minimal implementation**

```go
// Package a2a gives cortex agents direct, addressable communication:
// FIPA-ACL messages, durable conversations, mailboxes, and an ask that
// suspends the sender's run until a peer answers.
//
// It is a leaf package. It depends only on cortex and id, and reaches host
// capability (running an agent, resuming a paused run, persistence,
// delivery) through injected interfaces, never by importing the engine.
package a2a

// Performative is the speech act a message performs. The 22 constants
// below are the complete FIPA-ACL set.
type Performative string

// The FIPA-ACL performatives.
const (
	AcceptProposal  Performative = "accept-proposal"
	Agree           Performative = "agree"
	Cancel          Performative = "cancel"
	CFP             Performative = "cfp"
	Confirm         Performative = "confirm"
	Disconfirm      Performative = "disconfirm"
	Failure         Performative = "failure"
	Inform          Performative = "inform"
	InformIf        Performative = "inform-if"
	InformRef       Performative = "inform-ref"
	NotUnderstood   Performative = "not-understood"
	Propagate       Performative = "propagate"
	Propose         Performative = "propose"
	Proxy           Performative = "proxy"
	QueryIf         Performative = "query-if"
	QueryRef        Performative = "query-ref"
	Refuse          Performative = "refuse"
	RejectProposal  Performative = "reject-proposal"
	Request         Performative = "request"
	RequestWhen     Performative = "request-when"
	RequestWhenever Performative = "request-whenever"
	Subscribe       Performative = "subscribe"
)

// Class is how cortex routes a performative on arrival.
type Class string

// The three routing classes.
const (
	// ClassDirective starts a run for the recipient; its output is the reply.
	ClassDirective Class = "directive"
	// ClassInformative lands in the recipient's inbox and starts nothing.
	ClassInformative Class = "informative"
	// ClassControl is interpreted by the bus itself and reaches no agent.
	ClassControl Class = "control"
)

// classes maps every performative to its routing class. A performative
// missing from this map is not deliverable.
var classes = map[Performative]Class{
	Request:         ClassDirective,
	RequestWhen:     ClassDirective,
	RequestWhenever: ClassDirective,
	QueryIf:         ClassDirective,
	QueryRef:        ClassDirective,
	CFP:             ClassDirective,
	Propose:         ClassDirective,
	// accept-proposal is a directive, not an informative: in Contract Net
	// it is the message that makes the contractor do the work.
	AcceptProposal: ClassDirective,

	Inform:         ClassInformative,
	InformIf:       ClassInformative,
	InformRef:      ClassInformative,
	Confirm:        ClassInformative,
	Disconfirm:     ClassInformative,
	Agree:          ClassInformative,
	Refuse:         ClassInformative,
	Failure:        ClassInformative,
	NotUnderstood:  ClassInformative,
	RejectProposal: ClassInformative,
	Subscribe:      ClassInformative,
	// proxy and propagate are carried and delivered, but cortex does not
	// forward them on an agent's behalf. A host that wants forwarding
	// builds it over the inbox.
	Proxy:     ClassInformative,
	Propagate: ClassInformative,

	Cancel: ClassControl,
}

// Class returns the routing class for p, and whether p is a performative
// cortex recognises at all.
func (p Performative) Class() (Class, bool) {
	c, ok := classes[p]
	return c, ok
}

// Valid reports whether p is one of the 22 FIPA-ACL performatives.
func (p Performative) Valid() bool {
	_, ok := classes[p]
	return ok
}

// ResolvesAsk reports whether a reply carrying p un-pauses a waiting ask.
//
// agree is deliberately excluded. It means the peer accepted the task and
// is still working on it, so an asker that treated it as an answer would
// resume on a message carrying no answer.
func (p Performative) ResolvesAsk() bool {
	switch p {
	case Inform, InformIf, InformRef, Confirm, Disconfirm, Refuse, Failure, NotUnderstood, RejectProposal:
		return true
	default:
		return false
	}
}

// AllPerformatives returns every recognised performative, for validation
// and for exhaustive tests.
func AllPerformatives() []Performative {
	out := make([]Performative, 0, len(classes))
	for p := range classes {
		out = append(out, p)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -run TestPerformative -v && go test ./a2a/ -run TestResolvesAsk -v && go test ./a2a/ -run TestAll -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/performative.go a2a/performative_test.go
git commit -m "feat(a2a): add the FIPA-ACL performatives and routing classes"
```

---

### Task 3: Address, Envelope and validation

**Files:**
- Create: `a2a/envelope.go`
- Test: `a2a/envelope_test.go`

**Interfaces:**
- Consumes: `Performative` from Task 2.
- Produces: `a2a.Address{Agent, Node string}`, `a2a.Envelope` (all 13 ACL parameters plus `Hops`, `OriginRunID`, `Metadata`), `func (e *Envelope) Validate() error`, and the sentinels `ErrInvalidPerformative`, `ErrNoReceivers`, `ErrSelfAddressed`, `ErrNoSender`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"errors"
	"testing"
)

func TestEnvelopeValidate(t *testing.T) {
	base := func() *Envelope {
		return &Envelope{
			Performative: Request,
			Sender:       Address{Agent: "planner"},
			Receivers:    []Address{{Agent: "worker"}},
			Content:      "do the thing",
		}
	}

	t.Run("valid", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("unknown performative", func(t *testing.T) {
		e := base()
		e.Performative = "shout"
		if !errors.Is(e.Validate(), ErrInvalidPerformative) {
			t.Fatal("want ErrInvalidPerformative")
		}
	})

	t.Run("no receivers", func(t *testing.T) {
		e := base()
		e.Receivers = nil
		if !errors.Is(e.Validate(), ErrNoReceivers) {
			t.Fatal("want ErrNoReceivers")
		}
	})

	t.Run("no sender", func(t *testing.T) {
		e := base()
		e.Sender = Address{}
		if !errors.Is(e.Validate(), ErrNoSender) {
			t.Fatal("want ErrNoSender")
		}
	})

	// Self-addressing is refused outright. The loop risk is real and the
	// use case is not.
	t.Run("self addressed", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: "planner"}}
		if !errors.Is(e.Validate(), ErrSelfAddressed) {
			t.Fatal("want ErrSelfAddressed")
		}
	})

	t.Run("self addressed among others", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: "worker"}, {Agent: "planner"}}
		if !errors.Is(e.Validate(), ErrSelfAddressed) {
			t.Fatal("a broadcast that includes the sender is still self-addressed")
		}
	})

	t.Run("receiver with empty agent", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: ""}}
		if !errors.Is(e.Validate(), ErrNoReceivers) {
			t.Fatal("want ErrNoReceivers")
		}
	})
}

func TestAddressIsLocal(t *testing.T) {
	if !(Address{Agent: "a"}).IsLocal() {
		t.Fatal("an empty Node means an agent in this engine")
	}
	if (Address{Agent: "a", Node: "peer.example"}).IsLocal() {
		t.Fatal("a Node means a remote peer")
	}
}

func TestAddressEqualIgnoresNothing(t *testing.T) {
	a := Address{Agent: "x"}
	if a.Equal(Address{Agent: "x", Node: "n"}) {
		t.Fatal("same agent name on a different node is a different address")
	}
	if !a.Equal(Address{Agent: "x"}) {
		t.Fatal("identical addresses must compare equal")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestEnvelope`
Expected: FAIL, `undefined: Envelope`.

- [ ] **Step 3: Write minimal implementation**

```go
package a2a

import (
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Envelope validation errors.
var (
	// ErrInvalidPerformative means the envelope names a speech act that is
	// not one of the 22 FIPA-ACL performatives.
	ErrInvalidPerformative = errors.New("cortex: a2a: unknown performative")
	// ErrNoReceivers means the envelope addresses nobody, or addresses an
	// entry with an empty agent name.
	ErrNoReceivers = errors.New("cortex: a2a: envelope has no receivers")
	// ErrNoSender means the envelope carries no sender.
	ErrNoSender = errors.New("cortex: a2a: envelope has no sender")
	// ErrSelfAddressed means the sender is among the receivers. Cortex
	// refuses it outright: it is a loop with no use case behind it.
	ErrSelfAddressed = errors.New("cortex: a2a: envelope is self-addressed")
)

// Address identifies one messaging endpoint. Node empty means an agent in
// this engine; a non-empty Node names a remote peer and is the only field
// remote delivery needs.
type Address struct {
	Agent string `json:"agent"`
	Node  string `json:"node,omitempty"`
}

// IsLocal reports whether the address resolves inside this engine.
func (a Address) IsLocal() bool { return a.Node == "" }

// IsZero reports whether the address names nothing.
func (a Address) IsZero() bool { return a.Agent == "" && a.Node == "" }

// Equal reports whether two addresses name the same endpoint.
func (a Address) Equal(other Address) bool {
	return a.Agent == other.Agent && a.Node == other.Node
}

// String renders the address as agent or agent@node.
func (a Address) String() string {
	if a.Node == "" {
		return a.Agent
	}
	return a.Agent + "@" + a.Node
}

// Envelope is one FIPA-ACL message. The first block is the ACL parameter
// set, verbatim; the second is cortex's own additions, kept apart from it.
type Envelope struct {
	cortex.Entity
	ID    id.MessageID `json:"id"`
	Scope cortex.Scope `json:"scope"`

	Performative   Performative      `json:"performative"`
	Sender         Address           `json:"sender"`
	Receivers      []Address         `json:"receivers"`
	ReplyTo        []Address         `json:"reply_to,omitempty"`
	Content        string            `json:"content"`
	Language       string            `json:"language,omitempty"`
	Encoding       string            `json:"encoding,omitempty"`
	Ontology       string            `json:"ontology,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with,omitempty"`
	InReplyTo      string            `json:"in_reply_to,omitempty"`
	ReplyBy        *time.Time        `json:"reply_by,omitempty"`

	Hops        int            `json:"hops"`
	OriginRunID id.AgentRunID  `json:"origin_run_id,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Validate checks everything about an envelope that can be decided without
// touching the store: the performative is real, somebody sent it, somebody
// receives it, and the sender is not among the receivers.
func (e *Envelope) Validate() error {
	if !e.Performative.Valid() {
		return ErrInvalidPerformative
	}
	if e.Sender.IsZero() {
		return ErrNoSender
	}
	if len(e.Receivers) == 0 {
		return ErrNoReceivers
	}
	for _, r := range e.Receivers {
		if r.Agent == "" {
			return ErrNoReceivers
		}
		if r.Equal(e.Sender) {
			return ErrSelfAddressed
		}
	}
	return nil
}
```

Add the two TypeIDs this file references to [id/id.go](../../../id/id.go), beside the existing prefixes:

```go
	PrefixMessage      Prefix = "msg"
	PrefixConversation Prefix = "conv"
```

and the aliases plus constructors beside the existing ones:

```go
// MessageID identifies one a2a envelope.
type MessageID = ID

// ConversationID identifies one a2a conversation.
type ConversationID = ID

// NewMessageID returns a new message TypeID.
func NewMessageID() MessageID { return New(PrefixMessage) }

// NewConversationID returns a new conversation TypeID.
func NewConversationID() ConversationID { return New(PrefixConversation) }
```

Check [id/id.go](../../../id/id.go) for how the existing `New*ID` helpers are written and match them exactly, including whether a `Parse*` helper is generated per type.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ ./id/ -v`
Expected: PASS. The id package has its own round-trip test; make sure the two new prefixes are added to any exhaustive list it keeps.

- [ ] **Step 5: Commit**

```bash
git add a2a/envelope.go a2a/envelope_test.go id/id.go
git commit -m "feat(a2a): add the ACL envelope, addresses and validation"
```

---

### Task 4: Conversations, deliveries, pending asks, and the store seam

**Files:**
- Create: `a2a/conversation.go`, `a2a/delivery.go`, `a2a/pendingask.go`, `a2a/store.go`
- Test: `a2a/conversation_test.go`, `a2a/memstore_test.go`

**Interfaces:**
- Consumes: `Envelope`, `Address` from Task 3.
- Produces:
  - `a2a.Conversation` with `Status` (`StatusOpen`, `StatusClosed`, `StatusExpired`), `HopCeiling`, `HopsUsed`, `Deadline`.
  - `a2a.Delivery` with `State` (`DeliveryQueued`, `DeliveryDelivered`, `DeliveryFailed`) and `ReadAt`.
  - `a2a.PendingAsk` with `ReplyWith`, `AskerRunID`, `ToolCallID`, `Expected`, `Deadline`, `ClaimedAt`.
  - `a2a.Store` interface, listed in full below.
  - `memStore`, the in-memory test double every later task uses.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func testCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "tenant", Value: "acme"}},
	})
}

func TestMemStoreRoundTripsAConversation(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	c := &Conversation{
		ID:         id.NewConversationID(),
		Status:     StatusOpen,
		HopCeiling: 8,
		Initiator:  Address{Agent: "planner"},
	}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := s.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Status != StatusOpen || got.HopCeiling != 8 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
}

// The claim is the whole point of the pending-ask table. Two callers race
// for one row and exactly one of them may resume the run.
func TestClaimPendingAskSucceedsOnce(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	ask := &PendingAsk{
		ReplyWith:  "rw-1",
		AskerRunID: id.NewAgentRunID(),
		ToolCallID: "call-1",
		Expected:   Address{Agent: "worker"},
	}
	if err := s.CreatePendingAsk(ctx, ask); err != nil {
		t.Fatalf("CreatePendingAsk: %v", err)
	}

	first, err := s.ClaimPendingAsk(ctx, "rw-1")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.ToolCallID != "call-1" {
		t.Fatalf("claim returned the wrong row: %+v", first)
	}

	if _, err := s.ClaimPendingAsk(ctx, "rw-1"); !errorsIs(err, ErrAskAlreadyClaimed) {
		t.Fatalf("second claim: err = %v, want ErrAskAlreadyClaimed", err)
	}
}

func TestClaimUnknownAsk(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	if _, err := s.ClaimPendingAsk(ctx, "nope"); !errorsIs(err, ErrAskNotFound) {
		t.Fatalf("err = %v, want ErrAskNotFound", err)
	}
}

func TestListInboxReturnsUnreadOnly(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	msgID := id.NewMessageID()
	d := &Delivery{MessageID: msgID, Receiver: Address{Agent: "worker"}, State: DeliveryDelivered}
	if err := s.CreateDelivery(ctx, d); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	got, err := s.ListInbox(ctx, "worker", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(got))
	}

	if err := s.MarkDeliveryRead(ctx, got[0].ID); err != nil {
		t.Fatalf("MarkDeliveryRead: %v", err)
	}
	got, err = s.ListInbox(ctx, "worker", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox after read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d unread after marking read, want 0", len(got))
	}
}

func TestListQueuedDeliveriesIsWhatRedriveReadsFrom(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	queued := &Delivery{MessageID: id.NewMessageID(), Receiver: Address{Agent: "w1"}, State: DeliveryQueued}
	done := &Delivery{MessageID: id.NewMessageID(), Receiver: Address{Agent: "w2"}, State: DeliveryDelivered}
	for _, d := range []*Delivery{queued, done} {
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
	}
	got, err := s.ListQueuedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueuedDeliveries: %v", err)
	}
	if len(got) != 1 || got[0].Receiver.Agent != "w1" {
		t.Fatalf("redrive must see only queued rows, got %+v", got)
	}
}
```

Add a tiny helper so the tests above read cleanly, in `a2a/memstore_test.go`:

```go
func errorsIs(err, target error) bool { return errors.Is(err, target) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestMemStore`
Expected: FAIL, `undefined: Conversation`.

- [ ] **Step 3: Write minimal implementation**

`a2a/conversation.go`:

```go
package a2a

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Conversation statuses.
const (
	// StatusOpen means messages may still be delivered on it.
	StatusOpen = "open"
	// StatusClosed means a cancel closed it, or it ran to completion.
	StatusClosed = "closed"
	// StatusExpired means its deadline passed with an ask still waiting.
	StatusExpired = "expired"
)

// Conversation is one thread of messages between agents. It carries the
// containment budget: every derived message increments HopsUsed, and the
// bus refuses delivery once HopsUsed would exceed HopCeiling.
type Conversation struct {
	cortex.Entity
	ID           id.ConversationID `json:"id"`
	Scope        cortex.Scope      `json:"scope"`
	Protocol     string            `json:"protocol,omitempty"`
	Initiator    Address           `json:"initiator"`
	Participants []Address         `json:"participants,omitempty"`
	Status       string            `json:"status"`
	HopCeiling   int               `json:"hop_ceiling"`
	HopsUsed     int               `json:"hops_used"`
	Deadline     *time.Time        `json:"deadline,omitempty"`
}

// IsOpen reports whether the conversation still accepts messages.
func (c *Conversation) IsOpen() bool { return c.Status == StatusOpen }

// HasParticipant reports whether addr already took part.
func (c *Conversation) HasParticipant(addr Address) bool {
	for _, p := range c.Participants {
		if p.Equal(addr) {
			return true
		}
	}
	return false
}

// AddParticipant records addr as a participant, ignoring duplicates.
func (c *Conversation) AddParticipant(addr Address) {
	if !c.HasParticipant(addr) {
		c.Participants = append(c.Participants, addr)
	}
}
```

`a2a/delivery.go`:

```go
package a2a

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Delivery states. The state is what the dispatcher redrives from after a
// restart, which is why it is separate from the read state below it.
const (
	// DeliveryQueued means the bus accepted it and nobody has delivered it.
	DeliveryQueued = "queued"
	// DeliveryDelivered means it reached the recipient: an inbox row for an
	// informative, a started run for a directive.
	DeliveryDelivered = "delivered"
	// DeliveryFailed means delivery was attempted and could not complete.
	DeliveryFailed = "failed"
)

// Delivery is one envelope's arrival at one receiver. A message addressed
// to five agents is five deliveries, which is what makes "unread for agent
// B" an answerable question.
type Delivery struct {
	cortex.Entity
	ID          id.DeliveryID  `json:"id"`
	Scope       cortex.Scope   `json:"scope"`
	MessageID   id.MessageID   `json:"message_id"`
	Receiver    Address        `json:"receiver"`
	State       string         `json:"state"`
	Error       string         `json:"error,omitempty"`
	DeliveredAt *time.Time     `json:"delivered_at,omitempty"`
	ReadAt      *time.Time     `json:"read_at,omitempty"`
	RunID       id.AgentRunID  `json:"run_id,omitempty"` // the run a directive started
}
```

Add `PrefixDelivery Prefix = "dlv"`, `type DeliveryID = ID` and `NewDeliveryID()` to `id/id.go` the same way Task 3 added the other two.

`a2a/pendingask.go`:

```go
package a2a

import (
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Pending-ask errors.
var (
	// ErrAskNotFound means no pending ask carries that reply-with token.
	ErrAskNotFound = errors.New("cortex: a2a: pending ask not found")
	// ErrAskAlreadyClaimed is the losing side of a race between a reply, a
	// deadline sweep and a cancel. Exactly one of them may resume the run.
	ErrAskAlreadyClaimed = errors.New("cortex: a2a: pending ask already claimed")
)

// PendingAsk is one suspended run waiting on a peer's answer. It is keyed
// by ReplyWith, which is FIPA's own correlation token, so a reply carrying
// InReplyTo finds exactly one row.
type PendingAsk struct {
	cortex.Entity
	Scope          cortex.Scope      `json:"scope"`
	ReplyWith      string            `json:"reply_with"`
	ConversationID id.ConversationID `json:"conversation_id"`
	MessageID      id.MessageID      `json:"message_id"`
	AskerRunID     id.AgentRunID     `json:"asker_run_id"`
	AskerAgent     string            `json:"asker_agent"`
	ToolCallID     string            `json:"tool_call_id"`
	Expected       Address           `json:"expected"`
	Deadline       *time.Time        `json:"deadline,omitempty"`
	ClaimedAt      *time.Time        `json:"claimed_at,omitempty"`
}
```

`a2a/store.go`:

```go
package a2a

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// InboxFilter controls an inbox listing. Scope arrives on the context.
type InboxFilter struct {
	UnreadOnly     bool
	ConversationID id.ConversationID
	Limit          int
	Offset         int
}

// MessageListFilter controls a message listing. Scope arrives on the
// context; Exact narrows to rows stored at precisely that depth.
type MessageListFilter struct {
	Exact          bool
	ConversationID id.ConversationID
	Limit          int
	Offset         int
}

// ConversationListFilter controls a conversation listing.
type ConversationListFilter struct {
	Exact  bool
	Status string
	Limit  int
	Offset int
}

// Store is persistence for the messaging subsystem. It folds into the
// composite store.Store the same way orchestration's two interfaces do.
type Store interface {
	CreateMessage(ctx context.Context, e *Envelope) error
	GetMessage(ctx context.Context, msgID id.MessageID) (*Envelope, error)
	ListMessages(ctx context.Context, filter *MessageListFilter) ([]*Envelope, error)

	CreateConversation(ctx context.Context, c *Conversation) error
	GetConversation(ctx context.Context, convID id.ConversationID) (*Conversation, error)
	UpdateConversation(ctx context.Context, c *Conversation) error
	ListConversations(ctx context.Context, filter *ConversationListFilter) ([]*Conversation, error)

	CreateDelivery(ctx context.Context, d *Delivery) error
	UpdateDelivery(ctx context.Context, d *Delivery) error
	ListInbox(ctx context.Context, agentName string, filter InboxFilter) ([]*Delivery, error)
	ListQueuedDeliveries(ctx context.Context, limit int) ([]*Delivery, error)
	MarkDeliveryRead(ctx context.Context, deliveryID id.DeliveryID) error

	CreatePendingAsk(ctx context.Context, a *PendingAsk) error
	// ClaimPendingAsk takes ownership of the ask carrying replyWith. It
	// returns ErrAskNotFound when no such row exists and
	// ErrAskAlreadyClaimed when another caller got there first. Claiming
	// before resuming is what keeps a run from being resumed twice.
	ClaimPendingAsk(ctx context.Context, replyWith string) (*PendingAsk, error)
	ListExpiredAsks(ctx context.Context, now time.Time, limit int) ([]*PendingAsk, error)
}
```

Then write `newMemStore()` in `a2a/memstore_test.go`: a mutex-guarded struct of maps implementing every method above, with `ClaimPendingAsk` setting `ClaimedAt` under the lock and returning `ErrAskAlreadyClaimed` when it is already set. Keep insertion order for listings by storing a slice of ids alongside the maps, because tests assert on order.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS, including the race detector.

- [ ] **Step 5: Commit**

```bash
git add a2a/conversation.go a2a/delivery.go a2a/pendingask.go a2a/store.go a2a/conversation_test.go a2a/memstore_test.go id/id.go
git commit -m "feat(a2a): add conversations, deliveries, pending asks and the store seam"
```

---

### Task 5: Options, clock and the remaining seams

**Files:**
- Create: `a2a/options.go`, `a2a/seams.go`
- Test: `a2a/options_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `a2a.Options{HopCeiling, Workers, DefaultReplyBy, SweepInterval}` with `func (o Options) withDefaults() Options`; `a2a.Clock` interface with `Now() time.Time`, `a2a.systemClock`, and the test `fakeClock`; `a2a.Resumer`, `a2a.HookEmitter` (plus `noopHooks`), `a2a.Transport`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"testing"
	"time"
)

func TestOptionsDefaults(t *testing.T) {
	got := Options{}.withDefaults()
	if got.HopCeiling != DefaultHopCeiling {
		t.Errorf("HopCeiling = %d, want %d", got.HopCeiling, DefaultHopCeiling)
	}
	if got.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want %d", got.Workers, DefaultWorkers)
	}
	if got.DefaultReplyBy != DefaultReplyBy {
		t.Errorf("DefaultReplyBy = %s, want %s", got.DefaultReplyBy, DefaultReplyBy)
	}
	if got.SweepInterval != DefaultSweepInterval {
		t.Errorf("SweepInterval = %s, want %s", got.SweepInterval, DefaultSweepInterval)
	}
}

func TestOptionsKeepExplicitValues(t *testing.T) {
	in := Options{HopCeiling: 2, Workers: 1, DefaultReplyBy: time.Second, SweepInterval: time.Minute}
	if got := in.withDefaults(); got != in {
		t.Fatalf("withDefaults changed explicit values: %+v", got)
	}
}

func TestOptionsRejectNegatives(t *testing.T) {
	got := Options{HopCeiling: -3, Workers: -1}.withDefaults()
	if got.HopCeiling != DefaultHopCeiling || got.Workers != DefaultWorkers {
		t.Fatalf("negatives must fall back to defaults, got %+v", got)
	}
}

func TestFakeClockDoesNotMove(t *testing.T) {
	c := &fakeClock{now: time.Unix(1000, 0).UTC()}
	first := c.Now()
	if !c.Now().Equal(first) {
		t.Fatal("the fake clock must not advance on its own")
	}
	c.advance(time.Minute)
	if !c.Now().Equal(first.Add(time.Minute)) {
		t.Fatal("advance must move the fake clock by exactly the delta")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestOptions`
Expected: FAIL, `undefined: Options`.

- [ ] **Step 3: Write minimal implementation**

`a2a/options.go`:

```go
package a2a

import "time"

// Defaults for Options. The hop ceiling is the containment budget: eight
// derived messages is generous for a real delegation chain and short of
// anything that reads as a runaway.
const (
	DefaultHopCeiling    = 8
	DefaultWorkers       = 4
	DefaultReplyBy       = 5 * time.Minute
	DefaultSweepInterval = 30 * time.Second
)

// Options tunes the messaging subsystem.
type Options struct {
	// HopCeiling caps how many messages one conversation may derive.
	HopCeiling int
	// Workers is the dispatcher's concurrency. It has to cover resumed
	// askers as well as recipients, because Resume runs synchronously on
	// the worker that delivered the reply.
	Workers int
	// DefaultReplyBy is the deadline stamped on an ask that names none.
	DefaultReplyBy time.Duration
	// SweepInterval is how often overdue asks are resolved into failures.
	SweepInterval time.Duration
}

func (o Options) withDefaults() Options {
	if o.HopCeiling <= 0 {
		o.HopCeiling = DefaultHopCeiling
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.DefaultReplyBy <= 0 {
		o.DefaultReplyBy = DefaultReplyBy
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = DefaultSweepInterval
	}
	return o
}
```

`a2a/seams.go`:

```go
package a2a

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// Clock is the package's only source of time. Everything reads it, so a
// test can move deadlines without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Resumer un-pauses a run that stopped on an agent_ask. It wraps the
// engine's internal resume path, not the public Resume, because a host
// must not be able to forge a peer's reply.
type Resumer interface {
	ResumeAgentReply(ctx context.Context, runID id.AgentRunID, callID, result string) error
}

// Transport delivers an envelope to one receiver. The in-process
// implementation resolves agents in this engine; a remote implementation
// resolves a Node.
type Transport interface {
	// Deliver hands the envelope to the receiver. Returning an error marks
	// the delivery failed; it does not fail the sender's run.
	Deliver(ctx context.Context, e *Envelope, receiver Address) error
	// Handles reports whether this transport can reach the address.
	Handles(addr Address) bool
}

// HookEmitter receives messaging lifecycle events. The engine adapts
// plugin.Registry to it; tests pass a recorder.
type HookEmitter interface {
	MessageSent(ctx context.Context, msgID id.MessageID, from, to string, performative string)
	MessageDelivered(ctx context.Context, msgID id.MessageID, to string)
	MessageRefused(ctx context.Context, msgID id.MessageID, to, reason string)
}

type noopHooks struct{}

func (noopHooks) MessageSent(context.Context, id.MessageID, string, string, string) {}
func (noopHooks) MessageDelivered(context.Context, id.MessageID, string)            {}
func (noopHooks) MessageRefused(context.Context, id.MessageID, string, string)      {}
```

Add the fake clock to `a2a/memstore_test.go`:

```go
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -run 'TestOptions|TestFakeClock' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/options.go a2a/seams.go a2a/options_test.go a2a/memstore_test.go
git commit -m "feat(a2a): add options, the injected clock and the remaining seams"
```

---

### Task 6: The bus, and sending an informative

The first end-to-end path: validate, open a conversation, persist the envelope, queue one delivery per receiver, fire `MessageSent`.

**Files:**
- Create: `a2a/bus.go`
- Test: `a2a/bus_send_test.go`, `a2a/fakes_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2 to 5.
- Produces: `a2a.BusConfig`, `a2a.NewBus`, `a2a.SendParams`, `a2a.SendResult`, `a2a.DeliveryOutcome`, `(*Bus).Send`, and the internal `(*Bus).submit`. Also the test doubles `fakeRunner`, `fakeResumer`, `recordingHooks`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"testing"

	"github.com/xraph/cortex/id"
)

func newTestBus(t *testing.T) (*Bus, *memStore, *fakeRunner, *fakeResumer, *recordingHooks, *fakeClock) {
	t.Helper()
	st, runner, res := newMemStore(), newFakeRunner(), newFakeResumer()
	hooks, clk := &recordingHooks{}, &fakeClock{now: testNow}
	b, err := NewBus(BusConfig{
		Store:       st,
		Runner:      runner,
		Resumer:     res,
		Hooks:       hooks,
		Clock:       clk,
		Synchronous: true,
		Options:     Options{HopCeiling: 3, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	return b, st, runner, res, hooks, clk
}

func TestNewBusRequiresStoreAndRunner(t *testing.T) {
	if _, err := NewBus(BusConfig{Runner: newFakeRunner()}); err == nil {
		t.Fatal("a bus with no store must not build")
	}
	if _, err := NewBus(BusConfig{Store: newMemStore()}); err == nil {
		t.Fatal("a bus with no runner must not build")
	}
}

func TestSendInformativeQueuesOneDeliveryPerReceiver(t *testing.T) {
	b, st, _, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender:       Address{Agent: "planner"},
		Receivers:    []Address{{Agent: "w1"}, {Agent: "w2"}},
		Performative: Inform,
		Content:      "the build is green",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID.IsNil() || res.ConversationID.IsNil() {
		t.Fatalf("Send returned empty ids: %+v", res)
	}
	if len(res.Deliveries) != 2 {
		t.Fatalf("got %d delivery outcomes, want 2", len(res.Deliveries))
	}
	for _, d := range res.Deliveries {
		if d.Status != DeliveryQueued {
			t.Errorf("%s: status = %s, want queued", d.Receiver.Agent, d.Status)
		}
	}

	queued, err := st.ListQueuedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueuedDeliveries: %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("store holds %d queued deliveries, want 2", len(queued))
	}
	if got := hooks.sent(); got != 1 {
		t.Fatalf("MessageSent fired %d times, want 1", got)
	}
}

func TestSendPersistsTheEnvelopeAndOpensAConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender:       Address{Agent: "planner"},
		Receivers:    []Address{{Agent: "w1"}},
		Performative: Inform,
		Content:      "hello",
		Protocol:     "fipa-request",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg, err := st.GetMessage(ctx, res.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Content != "hello" || msg.Performative != Inform {
		t.Fatalf("stored envelope is wrong: %+v", msg)
	}
	if msg.Hops != 1 {
		t.Fatalf("Hops = %d, want 1 for the first message in a conversation", msg.Hops)
	}

	conv, err := st.GetConversation(ctx, res.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !conv.IsOpen() || conv.Protocol != "fipa-request" || conv.HopCeiling != 3 {
		t.Fatalf("conversation is wrong: %+v", conv)
	}
	if !conv.HasParticipant(Address{Agent: "planner"}) || !conv.HasParticipant(Address{Agent: "w1"}) {
		t.Fatalf("both ends must be recorded as participants: %+v", conv.Participants)
	}
}

func TestSendJoinsAnExistingConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	first, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one",
	})
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	second, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "two", ConversationID: first.ConversationID,
	})
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if second.ConversationID != first.ConversationID {
		t.Fatal("an explicit conversation id must be joined, not replaced")
	}

	msg, err := st.GetMessage(ctx, second.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Hops != 2 {
		t.Fatalf("Hops = %d, want 2 for the second message", msg.Hops)
	}
}

func TestSendRejectsAnInvalidEnvelopeBeforeWritingAnything(t *testing.T) {
	b, st, _, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "talking to myself",
	})
	if !errorsIs(err, ErrSelfAddressed) {
		t.Fatalf("err = %v, want ErrSelfAddressed", err)
	}
	msgs, listErr := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	if listErr != nil {
		t.Fatalf("ListMessages: %v", listErr)
	}
	if len(msgs) != 0 {
		t.Fatalf("a refused send wrote %d messages, want 0", len(msgs))
	}
	if hooks.sent() != 0 {
		t.Fatal("a refused send must not fire MessageSent")
	}
}
```

`a2a/fakes_test.go` holds the doubles:

```go
package a2a

import (
	"context"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// fakeRunner answers RunAgent from a per-agent canned output, defaulting to
// an echo. respond overrides both when set.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	outputs map[string]string
	err     error
	respond func(agentName, input string) string
}

type fakeCall struct{ AgentName, Input string }

func newFakeRunner() *fakeRunner { return &fakeRunner{outputs: map[string]string{}} }

func (f *fakeRunner) RunAgent(_ context.Context, name, input string, _ *cortex.RunOpts) (*cortex.AgentResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{name, input})
	err, respond := f.err, f.respond
	out, ok := f.outputs[name]
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	switch {
	case respond != nil:
		out = respond(name, input)
	case !ok:
		out = input
	}
	return &cortex.AgentResult{AgentName: name, Output: out, RunID: id.NewAgentRunID()}, nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRunner) lastInput() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1].Input
}

// fakeResumer records every resume so a test can assert exactly-once.
type fakeResumer struct {
	mu      sync.Mutex
	resumes []resumeCall
	err     error
}

type resumeCall struct {
	RunID  id.AgentRunID
	CallID string
	Result string
}

func newFakeResumer() *fakeResumer { return &fakeResumer{} }

func (f *fakeResumer) ResumeAgentReply(_ context.Context, runID id.AgentRunID, callID, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.resumes = append(f.resumes, resumeCall{runID, callID, result})
	return nil
}

func (f *fakeResumer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resumes)
}

func (f *fakeResumer) last() resumeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resumes) == 0 {
		return resumeCall{}
	}
	return f.resumes[len(f.resumes)-1]
}

type recordingHooks struct {
	mu                                  sync.Mutex
	sentN, deliveredN, refusedN         int
	lastRefusal                         string
}

func (h *recordingHooks) MessageSent(context.Context, id.MessageID, string, string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sentN++
}

func (h *recordingHooks) MessageDelivered(context.Context, id.MessageID, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliveredN++
}

func (h *recordingHooks) MessageRefused(_ context.Context, _ id.MessageID, _, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refusedN++
	h.lastRefusal = reason
}

func (h *recordingHooks) sent() int      { h.mu.Lock(); defer h.mu.Unlock(); return h.sentN }
func (h *recordingHooks) delivered() int { h.mu.Lock(); defer h.mu.Unlock(); return h.deliveredN }
func (h *recordingHooks) refused() int   { h.mu.Lock(); defer h.mu.Unlock(); return h.refusedN }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestSend`
Expected: FAIL, `undefined: NewBus`.

- [ ] **Step 3: Write minimal implementation**

`a2a/bus.go`. The write ordering below is load-bearing: validate everything, then envelope, then deliveries. A half-written send must never become a suspension nothing can resume.

```go
package a2a

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Bus errors.
var (
	// ErrNoStore means NewBus was called without persistence.
	ErrNoStore = errors.New("cortex: a2a: no store configured")
	// ErrNoRunner means NewBus was called without an agent runner.
	ErrNoRunner = errors.New("cortex: a2a: no agent runner configured")
	// ErrConversationClosed means the conversation no longer accepts messages.
	ErrConversationClosed = errors.New("cortex: a2a: conversation is closed")
	// ErrHopCeiling means the conversation used up its containment budget.
	ErrHopCeiling = errors.New("cortex: a2a: conversation hop ceiling exceeded")
	// ErrUnroutable means no transport handles the receiver's address.
	ErrUnroutable = errors.New("cortex: a2a: no transport handles that address")
	// ErrScopeMismatch means the receiver does not resolve in the sender's scope.
	ErrScopeMismatch = errors.New("cortex: a2a: receiver is outside the sender's scope")
)

// BusConfig builds a Bus. Store and Runner are required; everything else
// has a working default.
type BusConfig struct {
	Store      Store
	Runner     cortex.AgentRunner
	Resumer    Resumer
	Hooks      HookEmitter
	Clock      Clock
	Transports []Transport
	Options    Options

	// Synchronous makes the dispatcher deliver only when Drain is called.
	// Tests set it so an assertion can never observe a run mid-flight.
	Synchronous bool
}

// Bus routes envelopes between agents.
type Bus struct {
	store      Store
	runner     cortex.AgentRunner
	resumer    Resumer
	hooks      HookEmitter
	clock      Clock
	transports []Transport
	opts       Options
	dispatch   *dispatcher
}

// NewBus builds a Bus from cfg.
func NewBus(cfg BusConfig) (*Bus, error) {
	if cfg.Store == nil {
		return nil, ErrNoStore
	}
	if cfg.Runner == nil {
		return nil, ErrNoRunner
	}
	if cfg.Hooks == nil {
		cfg.Hooks = noopHooks{}
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	b := &Bus{
		store:      cfg.Store,
		runner:     cfg.Runner,
		resumer:    cfg.Resumer,
		hooks:      cfg.Hooks,
		clock:      cfg.Clock,
		transports: cfg.Transports,
		opts:       cfg.Options.withDefaults(),
	}
	if len(b.transports) == 0 {
		b.transports = []Transport{inProcess{}}
	}
	b.dispatch = newDispatcher(b, cfg.Synchronous)
	return b, nil
}

// SendParams is one outbound message.
type SendParams struct {
	Sender         Address
	Receivers      []Address
	Performative   Performative
	Content        string
	ConversationID id.ConversationID
	ReplyTo        []Address
	Language       string
	Encoding       string
	Ontology       string
	Protocol       string
	ReplyWith      string
	InReplyTo      string
	ReplyBy        *time.Time
	OriginRunID    id.AgentRunID
	Metadata       map[string]any
}

// DeliveryOutcome is one receiver's result from a send. A broadcast reports
// per receiver, because failing the whole send over one bad name throws
// away the deliveries that were fine.
type DeliveryOutcome struct {
	Receiver Address `json:"receiver"`
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
}

// SendResult is what a send produced.
type SendResult struct {
	MessageID      id.MessageID      `json:"message_id"`
	ConversationID id.ConversationID `json:"conversation_id"`
	Deliveries     []DeliveryOutcome `json:"deliveries"`
}

// Send validates, persists and queues one message. It returns as soon as
// the deliveries are queued; nothing waits for the recipients.
func (b *Bus) Send(ctx context.Context, p SendParams) (*SendResult, error) {
	e, conv, err := b.prepare(ctx, p)
	if err != nil {
		return nil, err
	}
	return b.submit(ctx, e, conv)
}

// prepare builds and validates the envelope and resolves its conversation,
// touching the store only for reads plus the conversation row itself.
func (b *Bus) prepare(ctx context.Context, p SendParams) (*Envelope, *Conversation, error) {
	scope, err := cortex.ScopeFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	e := &Envelope{
		Entity:       cortex.NewEntity(),
		ID:           id.NewMessageID(),
		Scope:        scope,
		Performative: p.Performative,
		Sender:       p.Sender,
		Receivers:    p.Receivers,
		ReplyTo:      p.ReplyTo,
		Content:      p.Content,
		Language:     p.Language,
		Encoding:     p.Encoding,
		Ontology:     p.Ontology,
		Protocol:     p.Protocol,
		ReplyWith:    p.ReplyWith,
		InReplyTo:    p.InReplyTo,
		ReplyBy:      p.ReplyBy,
		OriginRunID:  p.OriginRunID,
		Metadata:     p.Metadata,
	}
	if err := e.Validate(); err != nil {
		return nil, nil, err
	}

	conv, err := b.resolveConversation(ctx, p, scope)
	if err != nil {
		return nil, nil, err
	}
	if !conv.IsOpen() {
		return nil, nil, ErrConversationClosed
	}
	if conv.HopsUsed+1 > conv.HopCeiling {
		return nil, nil, fmt.Errorf("%w: used %d of %d", ErrHopCeiling, conv.HopsUsed, conv.HopCeiling)
	}
	for _, r := range e.Receivers {
		if !b.routable(r) {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnroutable, r)
		}
	}

	e.ConversationID = conv.ID
	e.Hops = conv.HopsUsed + 1
	return e, conv, nil
}

func (b *Bus) resolveConversation(ctx context.Context, p SendParams, scope cortex.Scope) (*Conversation, error) {
	if !p.ConversationID.IsNil() {
		return b.store.GetConversation(ctx, p.ConversationID)
	}
	conv := &Conversation{
		Entity:     cortex.NewEntity(),
		ID:         id.NewConversationID(),
		Scope:      scope,
		Protocol:   p.Protocol,
		Initiator:  p.Sender,
		Status:     StatusOpen,
		HopCeiling: b.opts.HopCeiling,
	}
	if err := b.store.CreateConversation(ctx, conv); err != nil {
		return nil, err
	}
	return conv, nil
}

// submit writes the envelope, bumps the conversation, queues one delivery
// per receiver and fires MessageSent. Ordering matters: the envelope lands
// first, so a delivery can never point at a message that is not there.
func (b *Bus) submit(ctx context.Context, e *Envelope, conv *Conversation) (*SendResult, error) {
	if err := b.store.CreateMessage(ctx, e); err != nil {
		return nil, err
	}

	conv.HopsUsed = e.Hops
	conv.AddParticipant(e.Sender)
	for _, r := range e.Receivers {
		conv.AddParticipant(r)
	}
	if err := b.store.UpdateConversation(ctx, conv); err != nil {
		return nil, err
	}

	res := &SendResult{MessageID: e.ID, ConversationID: e.ConversationID}
	for _, r := range e.Receivers {
		d := &Delivery{
			Entity:    cortex.NewEntity(),
			ID:        id.NewDeliveryID(),
			Scope:     e.Scope,
			MessageID: e.ID,
			Receiver:  r,
			State:     DeliveryQueued,
		}
		if err := b.store.CreateDelivery(ctx, d); err != nil {
			res.Deliveries = append(res.Deliveries, DeliveryOutcome{Receiver: r, Status: DeliveryFailed, Error: err.Error()})
			continue
		}
		res.Deliveries = append(res.Deliveries, DeliveryOutcome{Receiver: r, Status: DeliveryQueued})
		b.dispatch.enqueue(d.ID)
	}

	b.hooks.MessageSent(ctx, e.ID, e.Sender.String(), addressList(e.Receivers), string(e.Performative))
	return res, nil
}

func (b *Bus) routable(addr Address) bool {
	for _, t := range b.transports {
		if t.Handles(addr) {
			return true
		}
	}
	return false
}

func addressList(addrs []Address) string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return strings.Join(out, ",")
}
```

Add the missing imports (`strings`, `time`) as the compiler asks for them.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS. Tasks 7 onward will need `dispatcher`, `inProcess` and `Drain`, so write the smallest stubs that compile now (`newDispatcher` returning a struct whose `enqueue` appends to a slice, `inProcess.Handles` returning `addr.IsLocal()`, `inProcess.Deliver` returning nil) and replace them in Tasks 9 and 13.

- [ ] **Step 5: Commit**

```bash
git add a2a/bus.go a2a/bus_send_test.go a2a/fakes_test.go
git commit -m "feat(a2a): add the bus and the send path"
```

---

### Task 7: Refusals before anything suspends

**Files:**
- Modify: `a2a/bus.go` (no new code expected; this task proves the guarantees)
- Test: `a2a/bus_refusal_test.go`

**Interfaces:**
- Consumes: `Send`, `prepare` from Task 6.
- Produces: no new API. It pins behaviour later tasks must not regress.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestSendRefusesAClosedConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	first, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	conv, err := st.GetConversation(ctx, first.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	conv.Status = StatusClosed
	if err := st.UpdateConversation(ctx, conv); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}

	_, err = b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "two", ConversationID: first.ConversationID,
	})
	if !errorsIs(err, ErrConversationClosed) {
		t.Fatalf("err = %v, want ErrConversationClosed", err)
	}
}

func TestSendRefusesPastTheHopCeiling(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t) // ceiling is 3
	ctx := testCtx()

	var convID id.ConversationID
	for i := 0; i < 3; i++ {
		res, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Inform, Content: "msg", ConversationID: convID,
		})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		convID = res.ConversationID
	}

	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one too many", ConversationID: convID,
	})
	if !errorsIs(err, ErrHopCeiling) {
		t.Fatalf("err = %v, want ErrHopCeiling", err)
	}
}

func TestSendRefusesAnUnroutableAddress(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	// Only the in-process transport is configured, and it handles local
	// addresses only. A Node names a peer nothing here can reach.
	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1", Node: "peer.example"}},
		Performative: Inform, Content: "hello over there",
	})
	if !errorsIs(err, ErrUnroutable) {
		t.Fatalf("err = %v, want ErrUnroutable", err)
	}
}

func TestSendRequiresAScope(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)

	_, err := b.Send(context.Background(), SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "no scope here",
	})
	if !errorsIs(err, cortex.ErrNoScope) {
		t.Fatalf("err = %v, want cortex.ErrNoScope", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestSendRefuses`
Expected: some pass already from Task 6's implementation; the hop-ceiling and unroutable cases fail if `prepare` skipped them. Fix whatever fails.

- [ ] **Step 3: Write minimal implementation**

Only if a case failed. The checks belong in `prepare`, before `submit` writes anything.

- [ ] **Step 4: Verify no writes happened on a refusal**

Extend each subtest with a `ListMessages` assertion of zero rows, the same shape Task 6 used. Run: `go test ./a2a/ -race -v`.

- [ ] **Step 5: Commit**

```bash
git add a2a/bus_refusal_test.go a2a/bus.go
git commit -m "test(a2a): pin the refusals that must happen before a send writes"
```

---

### Task 8: Ask, and the pending-ask ledger

**Files:**
- Modify: `a2a/bus.go`
- Test: `a2a/bus_ask_test.go`

**Interfaces:**
- Consumes: `prepare`, `submit`.
- Produces: `a2a.AskParams`, `a2a.AskResult`, `(*Bus).Ask`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"testing"
	"time"

	"github.com/xraph/cortex/id"
)

func TestAskWritesAPendingAskKeyedByReplyWith(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runID := id.NewAgentRunID()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Content: "what is the status?",
		},
		AskerRunID: runID,
		ToolCallID: "call-7",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.ReplyWith == "" {
		t.Fatal("Ask must mint a reply-with token")
	}

	ask, err := st.ClaimPendingAsk(ctx, res.ReplyWith)
	if err != nil {
		t.Fatalf("ClaimPendingAsk: %v", err)
	}
	if ask.AskerRunID != runID || ask.ToolCallID != "call-7" {
		t.Fatalf("pending ask lost its correlation: %+v", ask)
	}
	if ask.Expected.Agent != "w1" {
		t.Fatalf("Expected = %s, want w1", ask.Expected)
	}
	if ask.Deadline == nil {
		t.Fatal("an ask with no explicit ReplyBy must still get the default deadline")
	}
	if want := testNow.Add(DefaultReplyBy); !ask.Deadline.Equal(want) {
		t.Fatalf("Deadline = %s, want %s", ask.Deadline, want)
	}
}

func TestAskDefaultsToRequest(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	msg, err := st.GetMessage(ctx, res.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Performative != Request {
		t.Fatalf("Performative = %s, want request", msg.Performative)
	}
	if msg.ReplyWith != res.ReplyWith {
		t.Fatal("the envelope must carry the same reply-with as the ledger row")
	}
}

func TestAskRefusesMoreThanOneReceiver(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"},
			Receivers: []Address{{Agent: "w1"}, {Agent: "w2"}},
			Content: "?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrAskNeedsOneReceiver) {
		t.Fatalf("err = %v, want ErrAskNeedsOneReceiver", err)
	}
}

func TestAskRefusesAnInformativePerformative(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Inform, Content: "this answers nothing",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrAskNeedsDirective) {
		t.Fatalf("err = %v, want ErrAskNeedsDirective", err)
	}
}

func TestAskWritesNoLedgerRowWhenTheSendIsRefused(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "planner"}},
			Content: "?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrSelfAddressed) {
		t.Fatalf("err = %v, want ErrSelfAddressed", err)
	}
	if _, err := st.ListExpiredAsks(ctx, testNow.Add(time.Hour), 10); err != nil {
		t.Fatalf("ListExpiredAsks: %v", err)
	} else if asks, _ := st.ListExpiredAsks(ctx, testNow.Add(time.Hour), 10); len(asks) != 0 {
		t.Fatalf("a refused ask left %d ledger rows, want 0", len(asks))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestAsk`
Expected: FAIL, `undefined: AskParams`.

- [ ] **Step 3: Write minimal implementation**

```go
// Ask errors.
var (
	// ErrAskNeedsOneReceiver means Ask was given zero or several receivers.
	// A durable ask correlates one reply to one waiting run.
	ErrAskNeedsOneReceiver = errors.New("cortex: a2a: ask needs exactly one receiver")
	// ErrAskNeedsDirective means the performative does not demand an answer,
	// so nothing would ever resume the asker.
	ErrAskNeedsDirective = errors.New("cortex: a2a: ask needs a directive performative")
)

// AskParams is one message whose sender suspends until the answer arrives.
type AskParams struct {
	SendParams
	AskerRunID id.AgentRunID
	ToolCallID string
}

// AskResult identifies the message and the token a reply must carry.
type AskResult struct {
	MessageID      id.MessageID      `json:"message_id"`
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with"`
}

// Ask sends a directive and records the sender's run as waiting on the
// answer. The caller suspends its run once this returns.
//
// The ledger row is written AFTER the message, and the whole thing is
// refused before either write when the send could not go out. A pending
// ask with no message behind it is a run nothing can ever resume.
func (b *Bus) Ask(ctx context.Context, p AskParams) (*AskResult, error) {
	if p.Performative == "" {
		p.Performative = Request
	}
	if len(p.Receivers) != 1 {
		return nil, ErrAskNeedsOneReceiver
	}
	if c, ok := p.Performative.Class(); !ok || c != ClassDirective {
		return nil, ErrAskNeedsDirective
	}
	if p.ReplyWith == "" {
		p.ReplyWith = id.NewMessageID().String()
	}
	if p.ReplyBy == nil {
		by := b.clock.Now().Add(b.opts.DefaultReplyBy)
		p.ReplyBy = &by
	}

	e, conv, err := b.prepare(ctx, p.SendParams)
	if err != nil {
		return nil, err
	}
	sent, err := b.submit(ctx, e, conv)
	if err != nil {
		return nil, err
	}

	ask := &PendingAsk{
		Entity:         cortex.NewEntity(),
		Scope:          e.Scope,
		ReplyWith:      e.ReplyWith,
		ConversationID: e.ConversationID,
		MessageID:      e.ID,
		AskerRunID:     p.AskerRunID,
		AskerAgent:     e.Sender.Agent,
		ToolCallID:     p.ToolCallID,
		Expected:       e.Receivers[0],
		Deadline:       e.ReplyBy,
	}
	if err := b.store.CreatePendingAsk(ctx, ask); err != nil {
		return nil, err
	}
	return &AskResult{MessageID: sent.MessageID, ConversationID: sent.ConversationID, ReplyWith: e.ReplyWith}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/bus.go a2a/bus_ask_test.go
git commit -m "feat(a2a): add the durable ask and its correlation ledger"
```

---

### Task 9: Delivering one message

The dispatcher hands the bus a delivery row. What happens next is entirely decided by the performative's routing class.

**Files:**
- Create: `a2a/deliver.go`, `a2a/render.go`, `a2a/transport.go`
- Test: `a2a/deliver_test.go`, `a2a/render_test.go`

**Interfaces:**
- Consumes: `Delivery`, `Envelope`, `Class`, `cortex.AgentRunner`.
- Produces: `(*Bus).deliverOne(ctx context.Context, deliveryID id.DeliveryID) error`, `func RenderInput(e *Envelope) string`, `a2a.inProcess` implementing `Transport`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestDeliverInformativeLandsInTheInboxAndStartsNoRun(t *testing.T) {
	b, st, runner, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "the build is green",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if runner.callCount() != 0 {
		t.Fatal("an informative must not start a run")
	}
	inbox, err := st.ListInbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].MessageID != res.MessageID {
		t.Fatalf("inbox = %+v, want the sent message", inbox)
	}
	if hooks.delivered() != 1 {
		t.Fatalf("MessageDelivered fired %d times, want 1", hooks.delivered())
	}
}

func TestDeliverDirectiveStartsARunAndRepliesWithItsOutput(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.outputs["w1"] = "status: green"

	res, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if runner.callCount() != 1 {
		t.Fatalf("runner called %d times, want 1", runner.callCount())
	}
	if !strings.Contains(runner.lastInput(), "report status") {
		t.Fatalf("the rendered input lost the content: %q", runner.lastInput())
	}

	msgs, err := st.ListMessages(ctx, &MessageListFilter{ConversationID: res.ConversationID, Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("conversation holds %d messages, want the request and its reply", len(msgs))
	}
	reply := msgs[1]
	if reply.Performative != Inform || reply.InReplyTo != "rw-1" {
		t.Fatalf("reply is wrong: %+v", reply)
	}
	if reply.Content != "status: green" || reply.Sender.Agent != "w1" {
		t.Fatalf("reply lost the run output: %+v", reply)
	}
}

func TestDeliverDirectiveWhoseRunFailsRepliesWithFailure(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.err = errors.New("model exploded")

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne must not surface the peer's failure as its own error: %v", err)
	}

	msgs, _ := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	reply := msgs[len(msgs)-1]
	if reply.Performative != Failure {
		t.Fatalf("Performative = %s, want failure", reply.Performative)
	}
	if !strings.Contains(reply.Content, "model exploded") {
		t.Fatalf("the failure must carry the error text, got %q", reply.Content)
	}
}

func TestDeliverMarksTheRowDelivered(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "x",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatalf("%d rows still queued after delivery, want 0", len(left))
	}
}

func TestRenderInputCarriesSenderPerformativeAndContent(t *testing.T) {
	e := &Envelope{
		Performative: Request,
		Sender:       Address{Agent: "planner"},
		Content:      "summarise the incident",
		Ontology:     "ops",
	}
	got := RenderInput(e)
	for _, want := range []string{"planner", "request", "summarise the incident", "ops"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered input is missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run 'TestDeliver|TestRender'`
Expected: FAIL, `undefined: deliverOne`.

- [ ] **Step 3: Write minimal implementation**

`a2a/render.go`:

```go
package a2a

import (
	"fmt"
	"strings"
)

// RenderInput turns an envelope into the text a recipient's run receives.
// It names the sender and the speech act, because an agent that cannot
// tell a request from a proposal cannot answer either one properly.
func RenderInput(e *Envelope) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Message from %s (%s)", e.Sender, e.Performative)
	if e.Ontology != "" {
		fmt.Fprintf(&sb, " [ontology: %s]", e.Ontology)
	}
	if e.Protocol != "" {
		fmt.Fprintf(&sb, " [protocol: %s]", e.Protocol)
	}
	sb.WriteString("\n\n")
	sb.WriteString(e.Content)
	if e.ReplyBy != nil {
		fmt.Fprintf(&sb, "\n\nReply by %s.", e.ReplyBy.Format(time.RFC3339))
	}
	return sb.String()
}
```

`a2a/transport.go`:

```go
package a2a

import "context"

// inProcess is the default transport: it handles agents in this engine and
// leaves the actual work to the bus, which already holds the runner and the
// store. Remote transports do real I/O here.
type inProcess struct{}

func (inProcess) Handles(addr Address) bool { return addr.IsLocal() }

func (inProcess) Deliver(context.Context, *Envelope, Address) error { return nil }
```

`a2a/deliver.go`:

```go
package a2a

import (
	"context"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// deliverOne carries one queued delivery to its receiver. What that means
// depends on the routing class, and the three cases are the whole of the
// delivery contract.
//
// A recipient's failure is never returned here. It is turned into a failure
// message the sender can read, because a peer that broke is information for
// the asker and not a reason to fail the delivery loop.
func (b *Bus) deliverOne(ctx context.Context, deliveryID id.DeliveryID) error {
	d, err := b.store.ClaimDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	e, err := b.store.GetMessage(ctx, d.MessageID)
	if err != nil {
		return b.failDelivery(ctx, d, err)
	}

	class, ok := e.Performative.Class()
	if !ok {
		return b.failDelivery(ctx, d, ErrInvalidPerformative)
	}

	switch class {
	case ClassInformative:
		return b.finishDelivery(ctx, d, e, id.AgentRunID{})
	case ClassControl:
		if err := b.handleControl(ctx, e); err != nil {
			return b.failDelivery(ctx, d, err)
		}
		return b.finishDelivery(ctx, d, e, id.AgentRunID{})
	case ClassDirective:
		return b.runDirective(ctx, d, e)
	default:
		return b.failDelivery(ctx, d, ErrInvalidPerformative)
	}
}

// runDirective starts the recipient's run and turns its outcome into a
// reply on the same conversation.
func (b *Bus) runDirective(ctx context.Context, d *Delivery, e *Envelope) error {
	out, runErr := b.runner.RunAgent(ctx, d.Receiver.Agent, RenderInput(e), nil)

	reply := SendParams{
		Sender:         d.Receiver,
		Receivers:      []Address{e.Sender},
		ConversationID: e.ConversationID,
		InReplyTo:      e.ReplyWith,
		Protocol:       e.Protocol,
		Ontology:       e.Ontology,
	}
	var runID id.AgentRunID
	if runErr != nil {
		reply.Performative = Failure
		reply.Content = fmt.Sprintf("%s could not answer: %v", d.Receiver.Agent, runErr)
	} else {
		reply.Performative = Inform
		reply.Content = out.Output
		runID = out.RunID
	}

	if err := b.finishDelivery(ctx, d, e, runID); err != nil {
		return err
	}
	// A reply that cannot be sent (a closed conversation, an exhausted hop
	// budget) still has to reach a waiting asker, so route it directly.
	if _, err := b.Send(ctx, reply); err != nil {
		return b.resolveAskWithFailure(ctx, e.ReplyWith, err.Error())
	}
	return nil
}

func (b *Bus) finishDelivery(ctx context.Context, d *Delivery, e *Envelope, runID id.AgentRunID) error {
	now := b.clock.Now()
	d.State = DeliveryDelivered
	d.DeliveredAt = &now
	d.RunID = runID
	if err := b.store.UpdateDelivery(ctx, d); err != nil {
		return err
	}
	b.hooks.MessageDelivered(ctx, e.ID, d.Receiver.String())
	return nil
}

func (b *Bus) failDelivery(ctx context.Context, d *Delivery, cause error) error {
	d.State = DeliveryFailed
	d.Error = cause.Error()
	if err := b.store.UpdateDelivery(ctx, d); err != nil {
		return err
	}
	b.hooks.MessageRefused(ctx, d.MessageID, d.Receiver.String(), cause.Error())
	return nil
}
```

Add `ClaimDelivery(ctx context.Context, deliveryID id.DeliveryID) (*Delivery, error)` and the `DeliveryDelivering` state to `a2a/store.go`, `a2a/delivery.go` and `memStore`. It returns `ErrDeliveryAlreadyClaimed` when the row is not queued, which is what stops two workers running the same directive twice. `handleControl` and `resolveAskWithFailure` are stubs returning nil for now; Tasks 10 and 12 fill them in.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/deliver.go a2a/render.go a2a/transport.go a2a/store.go a2a/delivery.go a2a/deliver_test.go a2a/render_test.go a2a/memstore_test.go
git commit -m "feat(a2a): deliver by routing class, and reply with the run's output"
```

---

### Task 10: Correlation, the claim, and exactly-once resume

**Files:**
- Modify: `a2a/bus.go`, `a2a/deliver.go`
- Test: `a2a/bus_reply_test.go`

**Interfaces:**
- Consumes: `ClaimPendingAsk`, `Resumer`, `Performative.ResolvesAsk`.
- Produces: `(*Bus).resolveAsk(ctx, e *Envelope) (bool, error)`, `(*Bus).resolveAskWithFailure(ctx, replyWith, reason string) error`, and the `AskReply` JSON shape a resumed tool call receives.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/id"
)

// The full loop: A asks, B answers, A's run resumes with B's words.
func TestReplyResumesTheWaitingRun(t *testing.T) {
	b, st, runner, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.outputs["w1"] = "all clear"
	runID := id.NewAgentRunID()

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: runID, ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want exactly 1", resumer.count())
	}
	got := resumer.last()
	if got.RunID != runID || got.CallID != "call-1" {
		t.Fatalf("resumed the wrong call: %+v", got)
	}

	var payload AskReply
	if err := json.Unmarshal([]byte(got.Result), &payload); err != nil {
		t.Fatalf("the resume result must be JSON the tool can return: %v", err)
	}
	if payload.Content != "all clear" || payload.Performative != string(Inform) || payload.Sender != "w1" {
		t.Fatalf("reply payload is wrong: %+v", payload)
	}
}

// A second reply carrying the same in-reply-to must be stored and must not
// resume anything. The claim is what makes that true.
func TestSecondReplyDoesNotResumeTwice(t *testing.T) {
	b, st, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
			Performative: Inform, Content: "answer", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
		}); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want exactly 1", resumer.count())
	}
	msgs, _ := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	if len(msgs) != 3 {
		t.Fatalf("stored %d messages, want the ask plus both replies", len(msgs))
	}
}

// agree means "working on it". It must be delivered without resuming.
func TestAgreeDoesNotResume(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Agree, Content: "on it", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resumer.count() != 0 {
		t.Fatal("agree must not un-pause the asker")
	}
}

// A reply whose in-reply-to matches nothing is ordinary mail.
func TestUnmatchedReplyIsJustAMessage(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "unsolicited", InReplyTo: "nobody-asked",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resumer.count() != 0 {
		t.Fatal("an unmatched in-reply-to must not resume anything")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run 'TestReply|TestSecondReply|TestAgree|TestUnmatched'`
Expected: FAIL, `undefined: AskReply`.

- [ ] **Step 3: Write minimal implementation**

In `a2a/bus.go`, add the payload type and hook correlation into `submit`, right after the envelope is written:

```go
// AskReply is what a resumed agent_ask tool call returns to the model.
type AskReply struct {
	Performative   string `json:"performative"`
	Sender         string `json:"sender"`
	Content        string `json:"content"`
	ConversationID string `json:"conversation_id"`
}

// resolveAsk matches an inbound reply to a waiting ask and resumes it.
// Reports whether a run was resumed.
//
// The claim happens before the resume, and that ordering is the design
// rather than a precaution: a late reply, the deadline sweep and a cancel
// are three writers racing for one row, and only the winner may resume.
func (b *Bus) resolveAsk(ctx context.Context, e *Envelope) (bool, error) {
	if e.InReplyTo == "" || !e.Performative.ResolvesAsk() {
		return false, nil
	}
	ask, err := b.store.ClaimPendingAsk(ctx, e.InReplyTo)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return false, nil
	case err != nil:
		return false, err
	}
	if b.resumer == nil {
		return false, nil
	}

	payload, err := json.Marshal(AskReply{
		Performative:   string(e.Performative),
		Sender:         e.Sender.String(),
		Content:        e.Content,
		ConversationID: e.ConversationID.String(),
	})
	if err != nil {
		return false, err
	}
	if err := b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

// resolveAskWithFailure un-pauses a waiting ask with a failure the asking
// agent can read: a timeout, a cancelled conversation, an undeliverable
// reply. Used by the sweep, by cancel handling, and by a reply that could
// not be sent.
func (b *Bus) resolveAskWithFailure(ctx context.Context, replyWith, reason string) error {
	if replyWith == "" {
		return nil
	}
	ask, err := b.store.ClaimPendingAsk(ctx, replyWith)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return nil
	case err != nil:
		return err
	}
	if b.resumer == nil {
		return nil
	}
	payload, err := json.Marshal(AskReply{
		Performative:   string(Failure),
		Sender:         ask.Expected.String(),
		Content:        reason,
		ConversationID: ask.ConversationID.String(),
	})
	if err != nil {
		return err
	}
	return b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload))
}
```

Call `resolveAsk` from `submit`, after `CreateMessage` and before queueing deliveries. When it resumes a run, the reply still gets stored (it already is) but no delivery is queued for it: the asker got the content through the resume, so an inbox copy would be a duplicate. Return the `SendResult` with an empty `Deliveries` slice in that case.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/bus.go a2a/deliver.go a2a/bus_reply_test.go
git commit -m "feat(a2a): correlate replies to waiting asks and resume exactly once"
```

---

### Task 11: Cancel, and closing a conversation

**Files:**
- Modify: `a2a/deliver.go`, `a2a/store.go`, `a2a/memstore_test.go`
- Test: `a2a/bus_cancel_test.go`

**Interfaces:**
- Consumes: `resolveAskWithFailure`.
- Produces: `(*Bus).handleControl`, and `Store.ListPendingAsksByConversation(ctx, convID) ([]*PendingAsk, error)`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestCancelClosesTheConversationAndFailsWaitingAsks(t *testing.T) {
	b, st, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Cancel, Content: "never mind", ConversationID: ask.ConversationID,
	}); err != nil {
		t.Fatalf("cancel Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	for _, d := range queued {
		if err := b.deliverOne(ctx, d.ID); err != nil {
			t.Fatalf("deliverOne: %v", err)
		}
	}

	conv, err := st.GetConversation(ctx, ask.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.Status != StatusClosed {
		t.Fatalf("Status = %s, want closed", conv.Status)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1 (the cancelled ask)", resumer.count())
	}
	var payload AskReply
	if err := json.Unmarshal([]byte(resumer.last().Result), &payload); err != nil {
		t.Fatalf("unmarshal resume payload: %v", err)
	}
	if payload.Performative != string(Failure) {
		t.Fatalf("a cancelled ask must resume with a failure, got %s", payload.Performative)
	}
}

func TestCancelStartsNoRun(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Cancel, Content: "stop",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatal("handing an LLM a bookkeeping message is a wasted call")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestCancel`
Expected: FAIL, the conversation stays open because `handleControl` is still a stub.

- [ ] **Step 3: Write minimal implementation**

```go
// handleControl interprets a control message. cancel closes the
// conversation and un-pauses everyone waiting on it, because a run paused
// behind a cancelled conversation would wait for its deadline and learn
// nothing.
func (b *Bus) handleControl(ctx context.Context, e *Envelope) error {
	if e.Performative != Cancel {
		return nil
	}
	asks, err := b.store.ListPendingAsksByConversation(ctx, e.ConversationID)
	if err != nil {
		return err
	}
	for _, a := range asks {
		reason := fmt.Sprintf("conversation cancelled by %s: %s", e.Sender, e.Content)
		if err := b.resolveAskWithFailure(ctx, a.ReplyWith, reason); err != nil {
			return err
		}
	}

	conv, err := b.store.GetConversation(ctx, e.ConversationID)
	if err != nil {
		return err
	}
	conv.Status = StatusClosed
	return b.store.UpdateConversation(ctx, conv)
}
```

Add `ListPendingAsksByConversation` to `Store` and `memStore`, returning only unclaimed rows.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/deliver.go a2a/store.go a2a/bus_cancel_test.go a2a/memstore_test.go
git commit -m "feat(a2a): handle cancel by closing the conversation and failing its asks"
```

---

### Task 12: The deadline sweep

**Files:**
- Create: `a2a/sweep.go`
- Test: `a2a/sweep_test.go`

**Interfaces:**
- Consumes: `ListExpiredAsks`, `resolveAskWithFailure`, `Clock`.
- Produces: `(*Bus).SweepExpiredAsks(ctx context.Context) (int, error)`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
)

func TestSweepResolvesAnOverdueAskIntoAFailure(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	deadline := testNow.Add(time.Minute)
	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Content: "status?", ReplyBy: &deadline,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	n, err := b.SweepExpiredAsks(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}
	if n != 0 {
		t.Fatalf("swept %d asks before the deadline, want 0", n)
	}

	clk.advance(2 * time.Minute)
	n, err = b.SweepExpiredAsks(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d asks after the deadline, want 1", n)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}

	var payload AskReply
	if err := json.Unmarshal([]byte(resumer.last().Result), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Performative != string(Failure) || !strings.Contains(payload.Content, "deadline") {
		t.Fatalf("a swept ask must resume with a timeout failure, got %+v", payload)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	clk.advance(DefaultReplyBy + time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := b.SweepExpiredAsks(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times across three sweeps, want 1", resumer.count())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestSweep`
Expected: FAIL, `undefined: SweepExpiredAsks`.

- [ ] **Step 3: Write minimal implementation**

```go
package a2a

import (
	"context"
	"fmt"
)

// sweepBatch caps one pass so a large backlog cannot hold a worker forever.
const sweepBatch = 100

// SweepExpiredAsks resolves every ask whose deadline has passed into a
// timeout failure and resumes the run waiting on it. It returns how many
// asks it resolved.
//
// The engine's own suspension sweep FAILS a run nobody answered in time.
// For an agent-reply pause that is the wrong verb, so this runs first: a
// peer that did not answer is something the asking agent can react to, and
// killing the run throws that away. The engine sweep stays as the outer
// backstop for anything this missed.
func (b *Bus) SweepExpiredAsks(ctx context.Context) (int, error) {
	asks, err := b.store.ListExpiredAsks(ctx, b.clock.Now(), sweepBatch)
	if err != nil {
		return 0, err
	}
	var n int
	for _, a := range asks {
		reason := fmt.Sprintf("no reply from %s before the deadline", a.Expected)
		if err := b.resolveAskWithFailure(ctx, a.ReplyWith, reason); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
```

`memStore.ListExpiredAsks` must exclude claimed rows, which is what makes the sweep idempotent.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/sweep.go a2a/sweep_test.go a2a/memstore_test.go
git commit -m "feat(a2a): resolve overdue asks into failures instead of failing the run"
```

---

### Task 13: The dispatcher

**Files:**
- Create: `a2a/dispatcher.go`
- Test: `a2a/dispatcher_test.go`

**Interfaces:**
- Consumes: `deliverOne`, `ListQueuedDeliveries`, `Options.Workers`.
- Produces: `newDispatcher`, `(*dispatcher).enqueue`, `(*Bus).Drain(ctx) (int, error)`, `(*Bus).Start(ctx) error`, `(*Bus).Stop()`, `(*Bus).Redrive(ctx) (int, error)`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestDrainDeliversEverythingQueued(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	for i := 0; i < 3; i++ {
		if _, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Request, Content: "go",
		}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	n, err := b.Drain(ctx)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d, want 3", n)
	}
	if runner.callCount() != 3 {
		t.Fatalf("runner called %d times, want 3", runner.callCount())
	}
	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatalf("%d rows still queued after a drain", len(left))
	}
}

// Redrive is the restart story: rows queued by a process that died are
// picked up by the next one.
func TestRedrivePicksUpOrphanedDeliveries(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	ctx := testCtx()

	first, err := NewBus(BusConfig{Store: st, Runner: runner, Clock: &fakeClock{now: testNow}, Synchronous: true})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	if _, err := first.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "survive this",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// first "crashes" here: nothing drained it.

	second, err := NewBus(BusConfig{Store: st, Runner: runner, Clock: &fakeClock{now: testNow}, Synchronous: true})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	n, err := second.Redrive(ctx)
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if n != 1 {
		t.Fatalf("redrove %d deliveries, want 1", n)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner called %d times after redrive, want 1", runner.callCount())
	}
}

// Two workers must never run the same directive twice. The delivery claim
// is what guarantees it.
func TestConcurrentDeliveryOfOneRowHappensOnce(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "once please",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	target := queued[0].ID

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.deliverOne(ctx, target)
		}()
	}
	wg.Wait()

	if runner.callCount() != 1 {
		t.Fatalf("the directive ran %d times, want exactly 1", runner.callCount())
	}
}

func TestStartAndStopAreSafeToCallTwice(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	b, err := NewBus(BusConfig{Store: st, Runner: runner, Options: Options{Workers: 2}})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("second Start must be a no-op, got %v", err)
	}
	b.Stop()
	b.Stop()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run 'TestDrain|TestRedrive|TestConcurrent|TestStartAndStop'`
Expected: FAIL, `undefined: Drain`.

- [ ] **Step 3: Write minimal implementation**

```go
package a2a

import (
	"context"
	"errors"
	"sync"
)

// drainBatch caps one pass over the queue.
const drainBatch = 100

// dispatcher carries queued deliveries to the bus. In synchronous mode it
// delivers nothing on its own and waits for Drain, which is what lets a
// test assert without ever observing a run mid-flight.
type dispatcher struct {
	bus         *Bus
	synchronous bool

	mu      sync.Mutex
	started bool
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
}

func newDispatcher(b *Bus, synchronous bool) *dispatcher {
	return &dispatcher{bus: b, synchronous: synchronous, wake: make(chan struct{}, 1)}
}

// enqueue nudges the workers. The queue itself is the store, so a nudge
// that is dropped costs a delay and never a delivery: the next wake, the
// next sweep, or a redrive finds the row.
func (d *dispatcher) enqueue(id.DeliveryID) {
	if d.synchronous {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Start launches the delivery workers. Calling it twice is a no-op.
func (b *Bus) Start(ctx context.Context) error {
	d := b.dispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.synchronous {
		d.started = true
		return nil
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.cancel, d.done, d.started = cancel, make(chan struct{}), true

	var wg sync.WaitGroup
	for i := 0; i < b.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.work(runCtx)
		}()
	}
	go func() {
		wg.Wait()
		close(d.done)
	}()
	return nil
}

// Stop cancels the workers and WAITS for them. Signalling without waiting
// would let Stop return while a delivery was still writing.
func (b *Bus) Stop() {
	d := b.dispatch
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.started, d.cancel, d.done = false, nil, nil
	d.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (d *dispatcher) work(ctx context.Context) {
	for {
		if _, err := d.bus.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// A drain error is per-batch; the loop keeps going so one bad
			// row cannot stop delivery for everyone.
			_ = err
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(d.bus.opts.SweepInterval):
		}
	}
}

// Drain delivers everything currently queued and reports how many rows it
// carried. Tests call it directly; workers call it in a loop.
func (b *Bus) Drain(ctx context.Context) (int, error) {
	var n int
	for {
		rows, err := b.store.ListQueuedDeliveries(ctx, drainBatch)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		for _, row := range rows {
			err := b.deliverOne(ctx, row.ID)
			switch {
			case errors.Is(err, ErrDeliveryAlreadyClaimed):
				continue
			case err != nil:
				return n, err
			}
			n++
		}
	}
}

// Redrive picks up deliveries a previous process queued and never carried.
// It is the same work Drain does; the separate name is for the caller that
// runs it once at startup.
func (b *Bus) Redrive(ctx context.Context) (int, error) { return b.Drain(ctx) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v -count=4`
Expected: PASS every time. `-count=4` is here because a concurrency bug that shows up one run in three is still a bug.

- [ ] **Step 5: Commit**

```bash
git add a2a/dispatcher.go a2a/dispatcher_test.go
git commit -m "feat(a2a): add the dispatcher, a synchronous drain and restart redrive"
```

---

### Task 14: The inbox

**Files:**
- Modify: `a2a/bus.go`
- Test: `a2a/bus_inbox_test.go`

**Interfaces:**
- Consumes: `ListInbox`, `MarkDeliveryRead`, `GetMessage`.
- Produces: `a2a.InboxItem`, `(*Bus).Inbox(ctx, agentName string, f InboxFilter) ([]InboxItem, error)`.

- [ ] **Step 1: Write the failing test**

```go
package a2a

import "testing"

func TestInboxReturnsEnvelopesAndMarksThemRead(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "note one",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	items, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Content != "note one" || items[0].Sender != "planner" {
		t.Fatalf("item is wrong: %+v", items[0])
	}

	// Reading is what marks it read, so a second call comes back empty.
	again, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("second Inbox: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("got %d items on the second read, want 0", len(again))
	}
	_ = st
}

func TestInboxLeavesUndeliveredRowsAlone(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "still queued",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	items, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(items) != 0 {
		t.Fatal("a queued delivery has not arrived yet and must not show in an inbox")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./a2a/ -run TestInbox`
Expected: FAIL, `undefined: InboxItem`.

- [ ] **Step 3: Write minimal implementation**

```go
// InboxItem is one delivered message as an agent sees it.
type InboxItem struct {
	DeliveryID     string `json:"delivery_id"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	Sender         string `json:"sender"`
	Performative   string `json:"performative"`
	Content        string `json:"content"`
	ReceivedAt     string `json:"received_at"`
}

// Inbox returns delivered messages for an agent and marks what it returns
// as read. Reading is the acknowledgement: an agent that saw a message in
// a tool result must not be handed it again on the next turn.
func (b *Bus) Inbox(ctx context.Context, agentName string, f InboxFilter) ([]InboxItem, error) {
	rows, err := b.store.ListInbox(ctx, agentName, f)
	if err != nil {
		return nil, err
	}
	items := make([]InboxItem, 0, len(rows))
	for _, d := range rows {
		e, err := b.store.GetMessage(ctx, d.MessageID)
		if err != nil {
			return nil, err
		}
		item := InboxItem{
			DeliveryID:     d.ID.String(),
			MessageID:      e.ID.String(),
			ConversationID: e.ConversationID.String(),
			Sender:         e.Sender.String(),
			Performative:   string(e.Performative),
			Content:        e.Content,
		}
		if d.DeliveredAt != nil {
			item.ReceivedAt = d.DeliveredAt.Format(time.RFC3339)
		}
		items = append(items, item)
		if err := b.store.MarkDeliveryRead(ctx, d.ID); err != nil {
			return nil, err
		}
	}
	return items, nil
}
```

`memStore.ListInbox` must return only rows in state `DeliveryDelivered`, which is what makes the second test pass.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./a2a/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add a2a/bus.go a2a/bus_inbox_test.go a2a/memstore_test.go
git commit -m "feat(a2a): add the inbox read path"
```

---

### Task 15: The leaf-package guarantee and the package doc

The whole architecture rests on `a2a` importing nothing from this module but `cortex` and `id`. That is worth a test, not a comment.

**Files:**
- Create: `a2a/imports_test.go`, `a2a/doc.go` (if the package doc is not already on `performative.go`)
- Test: `a2a/imports_test.go`

- [ ] **Step 1: Write the failing test**

```go
package a2a

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// a2a reaches the host through injected seams. An import of engine, plugin,
// store or orchestration is an import cycle waiting to happen, and it means
// the seams stopped being the boundary.
func TestPackageImportsNothingButCortexAndID(t *testing.T) {
	const module = "github.com/xraph/cortex"
	allowed := map[string]bool{
		module:      true,
		module + "/id": true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if !strings.HasPrefix(path, module) {
					continue // standard library and third party are fine
				}
				if !allowed[path] {
					t.Errorf("%s imports %s, which breaks the leaf-package rule", name, path)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./a2a/ -run TestPackageImports -v`
Expected: PASS if the package was built as specified. If it fails, the fix is to move whatever leaked behind a seam, not to widen the allowlist.

- [ ] **Step 3: Run everything, and lint**

Run: `go test ./... -race && make lint`
Expected: PASS and clean. Fix any lint finding in place; do not add nolint directives without a reason written next to them.

- [ ] **Step 4: Commit**

```bash
git add a2a/imports_test.go a2a/doc.go
git commit -m "test(a2a): pin the leaf-package import boundary"
```

---

## Self-review

**Spec coverage.** Every section of the spec maps to a task: §4.1 and §4.2 to Tasks 1 and 5, §5.1 to Task 3, §5.2 and §5.3 to Task 2, §5.4 to Task 10, §6 to Task 4, §7.2 to Tasks 6, 8 and 14, §7.3 to Tasks 9 and 10, §7.4 to Task 13, §8 to Tasks 7 and 12, §9.1 to Task 7, §9.2 to Tasks 9, 11 and 12, §9.3 to Task 10, §9.4 to Task 6, §12 to every task plus Task 15.

Two spec items are deliberately absent from this plan and belong to Plan 2, because they are engine changes: §7.5's `ToolAuthorizer` gating (the tools do not exist yet) and §10's builtin outcome contract, `ReasonAgentReply`, Resume gating, `WithA2A`, lifecycle and plugin hooks. §11's API surface is Plan 3.

**Known gap, carried forward on purpose.** A delivery claimed by a process that then dies is left in `delivering` and no redrive picks it up. The ask deadline resolves the asker either way, so nothing wedges, but the delivery itself is lost. Plan 2 adds a claim age to the store so a redrive can reclaim stale rows; it needs a real database to test properly.
