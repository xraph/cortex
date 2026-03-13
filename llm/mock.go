package llm

import (
	"context"
	"io"
	"time"
)

// MockClient is an echo-based LLM client for testing and fallback.
// It echoes the user input back as the assistant response with simulated delays.
type MockClient struct {
	// TokenDelay is the delay between each streamed character. Default: 20ms.
	TokenDelay time.Duration
}

// NewMockClient creates a new echo-based mock LLM client.
func NewMockClient() *MockClient {
	return &MockClient{
		TokenDelay: 20 * time.Millisecond,
	}
}

// Complete returns an echo response synchronously.
func (m *MockClient) Complete(_ context.Context, req *Request) (*Response, error) {
	output := "Echo: " + lastUserMessage(req.Messages)
	return &Response{
		Content:      output,
		FinishReason: "stop",
		Model:        req.Model,
		Usage: Usage{
			PromptTokens:     len(lastUserMessage(req.Messages)),
			CompletionTokens: len(output),
			TotalTokens:      len(lastUserMessage(req.Messages)) + len(output),
		},
	}, nil
}

// CompleteStream returns a stream that yields the echo response token by token.
func (m *MockClient) CompleteStream(_ context.Context, req *Request) (Stream, error) {
	output := "Echo: " + lastUserMessage(req.Messages)
	return &mockStream{
		content:    output,
		pos:        0,
		tokenDelay: m.TokenDelay,
		usage: &Usage{
			PromptTokens:     len(lastUserMessage(req.Messages)),
			CompletionTokens: len(output),
			TotalTokens:      len(lastUserMessage(req.Messages)) + len(output),
		},
	}, nil
}

// lastUserMessage returns the content of the last user message in the conversation.
func lastUserMessage(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// mockStream streams content character by character.
type mockStream struct {
	content    string
	pos        int
	tokenDelay time.Duration
	usage      *Usage
}

func (s *mockStream) Next(ctx context.Context) (*Chunk, error) {
	if s.pos >= len(s.content) {
		return nil, io.EOF
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.tokenDelay > 0 {
		time.Sleep(s.tokenDelay)
	}

	ch := string(s.content[s.pos])
	s.pos++

	chunk := &Chunk{Content: ch}
	if s.pos >= len(s.content) {
		chunk.FinishReason = "stop"
	}
	return chunk, nil
}

func (s *mockStream) Close() error {
	return nil
}

func (s *mockStream) Usage() *Usage {
	return s.usage
}
