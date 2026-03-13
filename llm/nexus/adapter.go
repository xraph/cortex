// Package nexus adapts the Nexus LLM gateway to the cortex llm.Client interface.
package nexus

import (
	"context"

	"github.com/xraph/nexus"
	"github.com/xraph/nexus/provider"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
)

// Adapter implements llm.Client by delegating to a nexus.Engine.
type Adapter struct {
	engine *nexus.Engine
}

// New creates a new Nexus adapter from a nexus.Gateway.
func New(gw *nexus.Gateway) *Adapter {
	return &Adapter{engine: gw.Engine()}
}

// EngineOption returns an engine.Option that auto-discovers a Nexus gateway
// from the DI container and configures the engine's LLM client.
// If no gateway is found, returns a no-op option (safe to always include).
func EngineOption(c vessel.Vessel) engine.Option {
	gw, err := vessel.Inject[*nexus.Gateway](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithLLM(New(gw))
}

// Complete sends a synchronous completion request via the Nexus pipeline.
func (a *Adapter) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	nReq := toNexusRequest(req)
	resp, err := a.engine.Complete(ctx, nReq)
	if err != nil {
		return nil, err
	}
	return fromNexusResponse(resp), nil
}

// CompleteStream sends a streaming completion request via the Nexus pipeline.
func (a *Adapter) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	nReq := toNexusRequest(req)
	nReq.Stream = true
	stream, err := a.engine.CompleteStream(ctx, nReq)
	if err != nil {
		return nil, err
	}
	return &streamAdapter{stream: stream}, nil
}

// ──────────────────────────────────────────────────
// Request conversion: llm.Request → provider.CompletionRequest
// ──────────────────────────────────────────────────

func toNexusRequest(req *llm.Request) *provider.CompletionRequest {
	nReq := &provider.CompletionRequest{
		Model:       req.Model,
		System:      req.System,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Messages:    toNexusMessages(req.Messages),
		Tools:       toNexusTools(req.Tools),
	}
	return nReq
}

func toNexusMessages(msgs []llm.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{
			Role:       m.Role,
			Content:    m.Content, // string → any (provider.Message.Content is any)
			ToolCalls:  toNexusToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		}
	}
	return out
}

func toNexusTools(tools []llm.Tool) []provider.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]provider.Tool, len(tools))
	for i, t := range tools {
		out[i] = provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}

func toNexusToolCalls(tcs []llm.ToolCall) []provider.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(tcs))
	for i, tc := range tcs {
		out[i] = provider.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: provider.ToolCallFunc{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		}
	}
	return out
}

// ──────────────────────────────────────────────────
// Response conversion: provider.CompletionResponse → llm.Response
// ──────────────────────────────────────────────────

func fromNexusResponse(resp *provider.CompletionResponse) *llm.Response {
	r := &llm.Response{
		Model: resp.Model,
		Usage: llm.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		r.Content = messageContentString(choice.Message.Content)
		r.ToolCalls = fromNexusToolCalls(choice.Message.ToolCalls)
		r.FinishReason = choice.FinishReason
	}
	return r
}

func fromNexusToolCalls(tcs []provider.ToolCall) []llm.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, len(tcs))
	for i, tc := range tcs {
		out[i] = llm.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
	}
	return out
}

// messageContentString extracts a string from provider.Message.Content,
// which can be a string or []ContentPart.
func messageContentString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		// Try to extract text parts.
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			result := parts[0]
			for i := 1; i < len(parts); i++ {
				result += parts[i]
			}
			return result
		}
	}
	return ""
}

// ──────────────────────────────────────────────────
// Stream adapter: provider.Stream → llm.Stream
// ──────────────────────────────────────────────────

type streamAdapter struct {
	stream provider.Stream
}

func (s *streamAdapter) Next(ctx context.Context) (*llm.Chunk, error) {
	chunk, err := s.stream.Next(ctx)
	if err != nil {
		return nil, err // includes io.EOF
	}
	return &llm.Chunk{
		Content:      chunk.Delta.Content,
		ToolCalls:    fromNexusToolCalls(chunk.Delta.ToolCalls),
		FinishReason: chunk.FinishReason,
	}, nil
}

func (s *streamAdapter) Close() error {
	return s.stream.Close()
}

func (s *streamAdapter) Usage() *llm.Usage {
	u := s.stream.Usage()
	if u == nil {
		return nil
	}
	return &llm.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}
