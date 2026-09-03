package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// routingLLM answers as whichever agent is calling, which is what lets
// one engine drive both sides of a conversation. It keys on the system
// prompt because that is the only thing in a request that says who is
// asking.
type routingLLM struct {
	mu       sync.Mutex
	asked    bool
	prompts  []string
	resumed  chan struct{}
	closeOne sync.Once
}

func (l *routingLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var system string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = m.Content
		}
	}
	if req.System != "" {
		system = req.System
	}
	l.prompts = append(l.prompts, system)

	if strings.Contains(system, "worker") {
		return &llm.Response{Content: "all clear, nothing burning"}, nil
	}
	if !l.asked {
		l.asked = true
		return &llm.Response{ToolCalls: []llm.ToolCall{{
			ID:        "call-1",
			Name:      toolAgentAsk,
			Arguments: `{"to":"worker","content":"what is the status?"}`,
		}}}, nil
	}
	// The second turn is the resumed one: whatever the model says here it
	// says having seen the worker's answer as the tool result.
	if l.resumed != nil {
		l.closeOne.Do(func() { close(l.resumed) })
	}
	return &llm.Response{Content: "the worker says it is all clear"}, nil
}

func (l *routingLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("routingLLM: CompleteStream not supported")
}

// TestAskRunReplyResume is the whole feature in one test: A asks, A's run
// is suspended on disk, B runs and answers, and A comes back with B's
// words as its tool result.
//
// Every piece under this is covered by a unit test somewhere. This is
// here for the wiring between them, which is what no unit test can see.
func TestAskRunReplyResume(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	st := newApprovalStore(ctx, t)
	model := &routingLLM{}

	e, err := New(
		WithStore(st),
		WithLLM(model),
		WithA2A(a2a.Options{HopCeiling: 6, Workers: 1}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"planner", "worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{
			ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name,
			Model: "test-model", MaxSteps: 4,
		}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", name, createErr)
		}
	}

	// 1. The planner asks and stops. Nothing is holding a goroutine open:
	// the run is a row, and the question is a queued delivery.
	paused, err := e.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("State = %s, want paused", paused.State)
	}

	// 2. The dispatcher carries the question, runs the worker, turns its
	// output into a reply, and resumes the planner off the reply. Drain is
	// synchronous so the test never has to guess when that finished.
	if _, drainErr := e.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}

	// 3. The planner finished, on the answer it was waiting for.
	final, err := st.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.State != run.StateCompleted {
		t.Fatalf("State = %s, want completed (error: %q)", final.State, final.Error)
	}
	if !strings.Contains(final.Output, "all clear") {
		t.Fatalf("Output = %q, want it to reflect the worker's answer", final.Output)
	}

	assertAskResultReachedTheModel(ctx, t, st, paused.ID)
	assertConversationTranscript(ctx, t, st)
}

// assertAskResultReachedTheModel checks the resumed tool call carries the
// worker's words, which is the difference between a run that continued
// and a run that continued knowing something.
func assertAskResultReachedTheModel(ctx context.Context, t *testing.T, st store.Store, runID id.AgentRunID) {
	t.Helper()
	steps, err := st.ListSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for _, s := range steps {
		calls, callErr := st.ListToolCalls(ctx, s.ID)
		if callErr != nil {
			t.Fatalf("ListToolCalls: %v", callErr)
		}
		for _, c := range calls {
			if c.ToolName == toolAgentAsk && strings.Contains(c.Result, "all clear") {
				return
			}
		}
	}
	t.Fatal("the ask never came back carrying the worker's answer")
}

// assertConversationTranscript checks both halves were persisted on one
// conversation, which is what a later reader (an operator, the API) sees.
func assertConversationTranscript(ctx context.Context, t *testing.T, st store.Store) {
	t.Helper()
	convs, err := st.ListConversations(ctx, &a2a.ConversationListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convs))
	}

	msgs, err := st.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: convs[0].ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the question and the answer", len(msgs))
	}
	if msgs[0].Performative != a2a.Request || msgs[0].Sender.Agent != "planner" {
		t.Fatalf("first message is wrong: %+v", msgs[0])
	}
	if msgs[1].Performative != a2a.Inform || msgs[1].Sender.Agent != "worker" {
		t.Fatalf("second message is wrong: %+v", msgs[1])
	}
	if msgs[1].InReplyTo != msgs[0].ReplyWith {
		t.Fatal("the reply does not point back at the question it answers")
	}
	if msgs[1].Hops != 2 {
		t.Fatalf("Hops = %d, want 2: the reply is one hop past the question", msgs[1].Hops)
	}
}

// TestEngineStartCarriesMessagesWithoutADrain proves the lifecycle wiring:
// with the engine started, the dispatcher does the carrying itself and
// nothing in the caller's code has to know delivery exists.
func TestEngineStartCarriesMessagesWithoutADrain(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	st := newApprovalStore(ctx, t)
	model := &routingLLM{resumed: make(chan struct{})}

	e, err := New(
		WithStore(st),
		WithLLM(model),
		WithA2A(a2a.Options{HopCeiling: 6, Workers: 2}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"planner", "worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{
			ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name,
			Model: "test-model", MaxSteps: 4,
		}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", name, createErr)
		}
	}

	if startErr := e.Start(ctx); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	defer func() {
		if stopErr := e.Stop(ctx); stopErr != nil {
			t.Fatalf("Stop: %v", stopErr)
		}
	}()

	paused, err := e.RunAgent(ctx, "planner", "ask the worker", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("State = %s, want paused", paused.State)
	}

	// The workers pick the delivery up on their own. Waiting on the
	// model's own signal keeps this deterministic: no sleep, no polling.
	//
	// The wait is bounded because an unbounded one turns a failure into a
	// hang: the suite sits until its own timeout and reports nothing
	// about which step never happened.
	select {
	case <-model.resumed:
	case <-time.After(30 * time.Second):
		final, err := st.GetRun(ctx, paused.ID)
		if err != nil {
			t.Fatalf("the planner never resumed, and its run could not be read: %v", err)
		}
		t.Fatalf("the planner never resumed; its run is %s (error: %q)", final.State, final.Error)
	}
}
