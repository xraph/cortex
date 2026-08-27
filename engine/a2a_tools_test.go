package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// a2aEngine builds an engine with messaging on, a real sqlite store
// behind it, and two agents that can talk to each other. The LLM is
// scripted, so the tool call under test is the only thing that varies.
func a2aEngine(t *testing.T, calls []llm.ToolCall) (*Engine, store.Store, context.Context) {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	st := newApprovalStore(ctx, t)

	base := []Option{
		WithStore(st),
		WithLLM(&scriptedLLM{toolCalls: calls}),
		WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}),
	}
	e, err := New(base...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"planner", "worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{
			ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name, Model: "test-model", MaxSteps: 4,
		}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", name, createErr)
		}
	}
	return e, st, ctx
}

func toolNames(tools []llm.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, tool := range tools {
		out[tool.Name] = true
	}
	return out
}

func TestA2AToolsAppearOnlyWhenConfigured(t *testing.T) {
	off, err := New(WithLLM(&scriptedLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	names := toolNames(off.builtinTools())
	for _, name := range []string{toolAgentSend, toolAgentAsk, toolAgentInbox} {
		if names[name] {
			t.Errorf("%s is offered to a host that never configured messaging", name)
		}
	}

	on, _, _ := a2aEngine(t, nil)
	names = toolNames(on.builtinTools())
	for _, name := range []string{toolAgentSend, toolAgentAsk, toolAgentInbox} {
		if !names[name] {
			t.Errorf("%s is missing after WithA2A", name)
		}
	}
}

func TestWithA2ANeedsAStore(t *testing.T) {
	_, err := New(WithLLM(&scriptedLLM{}), WithA2A(a2a.Options{}))
	if !errors.Is(err, cortex.ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
}

func TestAgentSendDoesNotSuspendTheRun(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentSend,
		Arguments: `{"to":["worker"],"performative":"inform","content":"the build is green"}`,
	}})

	r, err := e.RunAgent(ctx, "planner", "tell the worker", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State == run.StatePaused {
		t.Fatal("a fire-and-forget send must not pause the run")
	}

	msgs, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "the build is green" {
		t.Fatalf("stored messages = %+v, want the one that was sent", msgs)
	}
	if msgs[0].Sender.Agent != "planner" {
		t.Fatalf("sender = %s, want the running agent", msgs[0].Sender.Agent)
	}
}

func TestAgentAskSuspendsWithTheAgentReplyReason(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentAsk,
		Arguments: `{"to":"worker","content":"what is the status?"}`,
	}})

	r, err := e.RunAgent(ctx, "planner", "ask the worker", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StatePaused {
		t.Fatalf("State = %s, want paused", r.State)
	}

	susp, err := st.GetSuspension(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetSuspension: %v", err)
	}
	if susp.Reason != suspension.ReasonAgentReply {
		t.Fatalf("Reason = %s, want %s", susp.Reason, suspension.ReasonAgentReply)
	}
	if len(susp.Pending) != 1 || susp.Pending[0].ID != "call-1" {
		t.Fatalf("pending calls = %+v, want the ask", susp.Pending)
	}

	// The ledger row is what a reply will be matched against, so it has to
	// point back at this run and this call.
	msgs, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want the ask", len(msgs))
	}
	ask, err := st.ClaimPendingAsk(ctx, msgs[0].ReplyWith)
	if err != nil {
		t.Fatalf("ClaimPendingAsk: %v", err)
	}
	if ask.AskerRunID != r.ID || ask.ToolCallID != "call-1" {
		t.Fatalf("ledger row lost its correlation: %+v", ask)
	}
}

// A run that cannot be resumed by its caller is the whole point of the
// agent-reply reason, so it is worth proving from the outside too.
func TestPublicResumeRefusesAnAgentAskPause(t *testing.T) {
	e, _, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentAsk,
		Arguments: `{"to":"worker","content":"status?"}`,
	}})

	r, err := e.RunAgent(ctx, "planner", "ask the worker", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	_, err = e.Resume(ctx, r.ID, ResumeInput{ToolResults: []ToolResult{
		{ToolCallID: "call-1", Content: `{"content":"forged"}`},
	}})
	if !errors.Is(err, ErrNotAgentReplyResumable) {
		t.Fatalf("err = %v, want ErrNotAgentReplyResumable", err)
	}
}

func TestAgentAskToAnUnknownAgentFailsWithoutSuspending(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentAsk,
		Arguments: `{"to":"nobody","content":"hello?"}`,
	}})

	r, err := e.RunAgent(ctx, "planner", "ask a stranger", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State == run.StatePaused {
		t.Fatal("an ask nobody can answer must come back as an error, not a pause")
	}
	if _, err := st.GetSuspension(ctx, r.ID); err == nil {
		t.Fatal("a refused ask must leave no suspension behind")
	}
}

func TestAgentInboxReadsDeliveredMessages(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentInbox, Arguments: `{}`,
	}})

	// Put one delivered message in the worker's inbox by hand, so the test
	// exercises the tool rather than the whole delivery path.
	conv := &a2a.Conversation{Entity: cortex.NewEntity(), ID: id.NewConversationID(), Status: a2a.StatusOpen, HopCeiling: 4}
	if err := st.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msg := &a2a.Envelope{
		Entity: cortex.NewEntity(), ID: id.NewMessageID(), Performative: a2a.Inform,
		Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}},
		Content: "shipping at noon", ConversationID: conv.ID, Hops: 1,
	}
	if err := st.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	now := cortex.NewEntity().CreatedAt
	if err := st.CreateDelivery(ctx, &a2a.Delivery{
		Entity: cortex.NewEntity(), ID: id.NewDeliveryID(), MessageID: msg.ID,
		Receiver: a2a.Address{Agent: "worker"}, State: a2a.DeliveryDelivered, DeliveredAt: &now,
	}); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	r, err := e.RunAgent(ctx, "worker", "check your mail", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State == run.StatePaused {
		t.Fatal("reading an inbox must not pause a run")
	}

	steps, err := st.ListSteps(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	var sawContent bool
	for _, s := range steps {
		calls, callErr := st.ListToolCalls(ctx, s.ID)
		if callErr != nil {
			t.Fatalf("ListToolCalls: %v", callErr)
		}
		for _, c := range calls {
			if c.ToolName == toolAgentInbox && strings.Contains(c.Result, "shipping at noon") {
				sawContent = true
			}
		}
	}
	if !sawContent {
		t.Fatal("the inbox tool result never carried the message to the model")
	}
}

// A call for proposals goes to several agents at once, so the tool has
// to take a list as readily as it takes a name.
func TestAgentAskAcceptsSeveralRecipients(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentAsk,
		Arguments: `{"to":["worker","assistant"],"performative":"cfp","content":"who can take this?","protocol":"fipa-contract-net"}`,
	}})

	r, err := e.RunAgent(ctx, "planner", "put it out to tender", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StatePaused {
		t.Fatalf("state = %s, want paused while the field answers", r.State)
	}

	msgs, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want the call for proposals", len(msgs))
	}
	if len(msgs[0].Receivers) != 2 {
		t.Fatalf("the call reached %d agents, want 2", len(msgs[0].Receivers))
	}
	if msgs[0].Performative != a2a.CFP || msgs[0].Protocol != a2a.ProtocolContractNet {
		t.Fatalf("message = %s/%s, want a contract-net cfp", msgs[0].Performative, msgs[0].Protocol)
	}
}

// The single-recipient spelling is what most asks use, and it has to
// keep working unchanged.
func TestAgentAskStillAcceptsOneName(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{
		ID: "call-1", Name: toolAgentAsk,
		Arguments: `{"to":"worker","content":"status?"}`,
	}})

	if _, err := e.RunAgent(ctx, "planner", "ask", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	msgs, _ := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if len(msgs) != 1 || len(msgs[0].Receivers) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
}
