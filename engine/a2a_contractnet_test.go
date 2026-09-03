package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// tenderLLM plays every part in a contract net: the initiator that calls
// for proposals and then awards, and three participants that bid,
// decline, or bid worse.
type tenderLLM struct {
	mu      sync.Mutex
	stage   int
	awarded chan struct{}
	once    sync.Once
}

func (l *tenderLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	system := req.System
	switch {
	case strings.Contains(system, "fast-reviewer"):
		return &llm.Response{Content: "I can review it by 3pm"}, nil
	case strings.Contains(system, "slow-reviewer"):
		return &llm.Response{Content: "I can review it by 9pm"}, nil
	case strings.Contains(system, "busy-reviewer"):
		return &llm.Response{Content: "not today, I am full"}, nil
	}

	// The initiator's turns, in order.
	l.stage++
	switch l.stage {
	case 1:
		return &llm.Response{ToolCalls: []llm.ToolCall{{
			ID:   "cfp-1",
			Name: toolAgentAsk,
			Arguments: `{"to":["fast-reviewer","slow-reviewer","busy-reviewer"],` +
				`"performative":"cfp","protocol":"fipa-contract-net",` +
				`"content":"who can review the migration today?"}`,
		}}}, nil
	case 2:
		// Resumed with the field's answers. Award to the fastest.
		return &llm.Response{ToolCalls: []llm.ToolCall{{
			ID:   "award-1",
			Name: toolAgentSend,
			Arguments: `{"to":["fast-reviewer"],"performative":"accept-proposal",` +
				`"protocol":"fipa-contract-net","content":"yours, by 3pm please"}`,
		}}}, nil
	default:
		l.once.Do(func() { close(l.awarded) })
		return &llm.Response{Content: "awarded to fast-reviewer"}, nil
	}
}

func (l *tenderLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("not supported")
}

// TestContractNetEndToEnd runs a whole tender: a call for proposals to
// three agents, two bids and a refusal, and an award to the one the
// initiator picked.
//
// The protocol is the primitives in the order FIPA describes, so this
// test is really asking whether those primitives compose the way the
// design claims they do.
func TestContractNetEndToEnd(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	st := newApprovalStore(ctx, t)
	model := &tenderLLM{awarded: make(chan struct{})}

	e, err := New(
		WithStore(st),
		WithLLM(model),
		WithA2A(a2a.Options{HopCeiling: 12, Workers: 1}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"initiator", "fast-reviewer", "slow-reviewer", "busy-reviewer"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{
			ID: id.NewAgentID(), Name: name, SystemPrompt: "you are the " + name,
			Model: "test-model", MaxSteps: 6,
		}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", name, createErr)
		}
	}

	// 1. The call for proposals goes out, and the initiator stops.
	paused, err := e.RunAgent(ctx, "initiator", "get the migration reviewed", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("state = %s, want paused while the field answers", paused.State)
	}

	// 2. Everyone answers, and the initiator resumes once they all have.
	if _, drainErr := e.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}

	final, err := st.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.State != run.StateCompleted {
		t.Fatalf("state = %s, want completed (error: %q)", final.State, final.Error)
	}

	// The tender came back as one result carrying every answer.
	payload := askPayload(ctx, t, st, paused.ID)
	if !payload.Complete {
		t.Errorf("all three answered, so the tender is complete: %+v", payload)
	}
	if len(payload.Replies) != 3 {
		t.Fatalf("got %d answers, want three: %+v", len(payload.Replies), payload.Replies)
	}

	var bids, declines int
	for _, r := range payload.Replies {
		switch {
		case strings.Contains(r.Content, "I can review"):
			bids++
		case strings.Contains(r.Content, "not today"):
			declines++
		}
	}
	if bids != 2 || declines != 1 {
		t.Fatalf("got %d bids and %d declines, want 2 and 1: %+v", bids, declines, payload.Replies)
	}

	// 3. The award reached the winner and nobody else.
	msgs, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var awards int
	for _, m := range msgs {
		if m.Performative != a2a.AcceptProposal {
			continue
		}
		awards++
		if len(m.Receivers) != 1 || m.Receivers[0].Agent != "fast-reviewer" {
			t.Errorf("the award went to %+v, want the agent the initiator picked", m.Receivers)
		}
		if m.Protocol != a2a.ProtocolContractNet {
			t.Errorf("the award is not stamped as part of the tender: %q", m.Protocol)
		}
	}
	if awards != 1 {
		t.Fatalf("%d awards were sent, want exactly 1", awards)
	}
}

// askPayload reads the tender's result out of the tool call it came back
// on, which is where the initiator's model actually saw it.
func askPayload(ctx context.Context, t *testing.T, st interface {
	ListSteps(context.Context, id.AgentRunID) ([]*run.Step, error)
	ListToolCalls(context.Context, id.StepID) ([]*run.ToolCall, error)
}, runID id.AgentRunID,
) a2a.AskReply {
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
			if c.ToolName != toolAgentAsk {
				continue
			}
			var payload a2a.AskReply
			if err := json.Unmarshal([]byte(c.Result), &payload); err != nil {
				t.Fatalf("the ask result is not an AskReply: %v (%q)", err, c.Result)
			}
			return payload
		}
	}
	t.Fatal("the tender never came back on a tool call")
	return a2a.AskReply{}
}
