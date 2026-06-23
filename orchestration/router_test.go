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
