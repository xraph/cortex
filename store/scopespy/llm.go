package scopespy

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/xraph/cortex/llm"
)

type staticLLM struct{ reply string }

// StaticLLM returns an llm.Client that always answers with reply and
// never requests a tool, so a run terminates after one step.
func StaticLLM(reply string) llm.Client { return &staticLLM{reply: reply} }

func (s *staticLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: s.reply}, nil
}

// CompleteStream is required by llm.Client. StaticLLM backs the
// synchronous-path tests only; use StaticStreamLLM for streamReAct so a
// half-working stream never masks a real failure there.
func (s *staticLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("scopespy: CompleteStream not supported")
}

type streamOnlyLLM struct{ reply string }

// StaticStreamLLM returns an llm.Client whose CompleteStream answers with
// a single chunk containing reply and then ends, so streamReAct completes
// a run without needing a real provider. Complete is implemented for
// interface completeness but the streaming tests that use this double
// never call it.
func StaticStreamLLM(reply string) llm.Client { return &streamOnlyLLM{reply: reply} }

func (s *streamOnlyLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: s.reply}, nil
}

func (s *streamOnlyLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return &staticStream{reply: s.reply}, nil
}

// staticStream yields one content chunk, then io.EOF.
type staticStream struct {
	reply string
	sent  bool
}

func (s *staticStream) Next(_ context.Context) (*llm.Chunk, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return &llm.Chunk{Content: s.reply, FinishReason: "stop"}, nil
}

func (s *staticStream) Close() error { return nil }

func (s *staticStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 1} }

// toolCallingLLM answers its first Complete with a call to tool, then a
// plain "done" on every call after — enough for a single ReAct loop
// invocation to dispatch a tool and then finish.
type toolCallingLLM struct {
	mu       sync.Mutex
	tool     string
	answered bool
}

// ToolCallingLLM returns an llm.Client that requests toolName on its
// first Complete call and answers plainly on the next, so the react loop
// reaches CreateToolCall. Used with the synchronous RunAgent path only —
// CompleteStream is not implemented.
func ToolCallingLLM(toolName string) llm.Client { return &toolCallingLLM{tool: toolName} }

func (t *toolCallingLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.answered {
		t.answered = true
		return &llm.Response{
			ToolCalls: []llm.ToolCall{{ID: "spy-tool-call-1", Name: t.tool, Arguments: "{}"}},
		}, nil
	}
	return &llm.Response{Content: "done"}, nil
}

// CompleteStream is required by llm.Client. ToolCallingLLM backs the
// synchronous-path tool-call test only.
func (t *toolCallingLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("scopespy: CompleteStream not supported by ToolCallingLLM")
}
