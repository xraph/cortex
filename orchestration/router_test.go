package orchestration

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestRouterStaticRules(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"billing": "BILL", "support": "SUPP"}
	parts := []Participant{{AgentName: "billing"}, {AgentName: "support"}}
	o := newRouter(runner, "app1", parts, Settings{
		RouterRules: map[string]string{"refund": "billing", "broken": "support"},
	})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "I need a refund please", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "BILL" {
		t.Fatalf("output = %q, want BILL (routed to billing)", res.Output)
	}
	if got := runner.callNames(); len(got) != 1 || got[0] != "billing" {
		t.Fatalf("calls = %v, want [billing]", got)
	}
}

func TestRouterAgentDecides(t *testing.T) {
	runner := newFakeRunner()
	// router agent returns the chosen agent name; chosen agent returns its output.
	runner.outputs = map[string]string{"dispatcher": "support", "support": "SUPP"}
	parts := []Participant{{AgentName: "billing"}, {AgentName: "support"}}
	o := newRouter(runner, "app1", parts, Settings{RouterAgent: "dispatcher"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "my app is broken", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "SUPP" {
		t.Fatalf("output = %q, want SUPP", res.Output)
	}
	if len(res.Handoffs) != 1 || res.Handoffs[0].From != "dispatcher" || res.Handoffs[0].To != "support" {
		t.Fatalf("handoffs = %+v, want dispatcher→support", res.Handoffs)
	}
}

func TestRouterFallsBackToFirst(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newRouter(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "anything", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "AA" {
		t.Fatalf("output = %q, want AA (first participant)", res.Output)
	}
}

func TestRouterRuleUnknownAgentFallsBack(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	// RouterRules has "bug" keyword mapping to "unknown_agent" (not in participants)
	o := newRouter(runner, "app1", parts, Settings{
		RouterRules: map[string]string{"bug": "unknown_agent"},
	})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "there is a bug in the system", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Should fall back to first participant since the matched rule maps to unknown agent
	if res.Output != "AA" {
		t.Fatalf("output = %q, want AA (first participant fallback)", res.Output)
	}
	if got := runner.callNames(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("calls = %v, want [a]", got)
	}
}

func TestRouterRulesDeterministicOnMultiMatch(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"billing": "BILL", "support": "SUPP"}
	parts := []Participant{{AgentName: "billing"}, {AgentName: "support"}}
	// Rules: "refund" → billing, "payment" → support
	// Input "refund payment issue" matches both keywords.
	// Alphabetically, "payment" < "refund", so "payment" → support should win.
	o := newRouter(runner, "app1", parts, Settings{
		RouterRules: map[string]string{"refund": "billing", "payment": "support"},
	})

	// Run multiple times to ensure determinism
	for i := 0; i < 5; i++ {
		runner.calls = nil // clear calls for each iteration
		bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
		res, err := o.Run(context.Background(), "refund payment issue", bb)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		// "payment" comes before "refund" alphabetically, so it should match first
		if res.Output != "SUPP" {
			t.Fatalf("Run %d: output = %q, want SUPP (support matched 'payment' keyword)", i, res.Output)
		}
		if got := runner.callNames(); len(got) != 1 || got[0] != "support" {
			t.Fatalf("Run %d: calls = %v, want [support]", i, got)
		}
	}
}
