package scopespy

import (
	"context"
	"errors"

	"github.com/xraph/cortex/llm"
)

type staticLLM struct{ reply string }

// StaticLLM returns an llm.Client that always answers with reply and
// never requests a tool, so a run terminates after one step.
func StaticLLM(reply string) llm.Client { return &staticLLM{reply: reply} }

func (s *staticLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: s.reply}, nil
}

// CompleteStream is required by llm.Client. The scope regression test only
// exercises the synchronous path, so this returns an error rather than a
// half-working stream that would mask a real failure.
func (s *staticLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("scopespy: CompleteStream not supported")
}
