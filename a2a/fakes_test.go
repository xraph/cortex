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

func (f *fakeRunner) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// what stops the next test having to change the double.
//
//nolint:unparam // every current caller answers as w1; the parameter is
func (f *fakeRunner) setOutput(agent, out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[agent] = out
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
	mu                          sync.Mutex
	sentN, deliveredN, refusedN int
	lastRefusal                 string
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
