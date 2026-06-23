package orchestration

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestParallelRunsAllAndConcatenates(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "c": "CC"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}, {AgentName: "c"}}
	o := newParallel(runner, "app1", parts, Settings{MaxConcurrency: 2})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := runner.callNames()
	sort.Strings(got)
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("called = %v, want a,b,c", got)
	}
	if len(res.AgentResults) != 3 {
		t.Fatalf("results = %d, want 3", len(res.AgentResults))
	}
	for _, want := range []string{"AA", "BB", "CC"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output %q missing %q", res.Output, want)
		}
	}
}

func TestParallelAggregatorSynthesizes(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "boss": "FINAL"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newParallel(runner, "app1", parts, Settings{Aggregator: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "FINAL" {
		t.Fatalf("output = %q, want FINAL (aggregator)", res.Output)
	}
}
