package orchestration

import (
	"context"
	"sync"
)

// fakeRunner records calls and returns canned outputs keyed by agent name.
// A nil/missing entry echoes the input. respond may be set for dynamic replies.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	outputs map[string]string
	respond func(agentName, input string) string
}

type fakeCall struct {
	AgentName string
	Input     string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]string{}}
}

func (f *fakeRunner) RunAgent(_ context.Context, _ /*appID*/ string, agentName, input string, _ *RunOpts) (*AgentResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{AgentName: agentName, Input: input})
	f.mu.Unlock()

	out := input // default: echo
	if f.respond != nil {
		out = f.respond(agentName, input)
	} else if v, ok := f.outputs[agentName]; ok {
		out = v
	}
	return &AgentResult{AgentName: agentName, Output: out}, nil
}

func (f *fakeRunner) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.AgentName
	}
	return names
}
