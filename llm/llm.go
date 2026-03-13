// Package llm defines a provider-agnostic LLM client interface for the Cortex engine.
//
// This abstraction allows the engine to call any LLM backend (Nexus, direct providers,
// mock) without importing provider-specific packages. Adapters convert between these
// types and the concrete provider types.
package llm

import "context"

// Client is the interface for sending completion requests to an LLM.
type Client interface {
	// Complete sends a synchronous completion request and returns the full response.
	Complete(ctx context.Context, req *Request) (*Response, error)

	// CompleteStream sends a streaming completion request and returns a stream
	// that yields incremental chunks.
	CompleteStream(ctx context.Context, req *Request) (Stream, error)
}

// Request is a completion request sent to an LLM.
type Request struct {
	// Model is the model identifier (e.g. "gpt-4o", "claude-sonnet-4-20250514", "smart").
	Model string

	// System is the system prompt injected before the conversation.
	System string

	// Messages is the conversation history.
	Messages []Message

	// Tools is the set of tools available for the LLM to call.
	Tools []Tool

	// MaxTokens limits the maximum output tokens.
	MaxTokens int

	// Temperature controls sampling randomness. Nil means provider default.
	Temperature *float64
}

// Message is a single message in a conversation.
type Message struct {
	// Role is the message author: "user", "assistant", or "tool".
	Role string

	// Content is the text content of the message.
	Content string

	// ToolCalls contains tool invocations requested by the assistant.
	ToolCalls []ToolCall

	// ToolCallID identifies which tool call this message is responding to (role=tool).
	ToolCallID string
}

// Response is the result of a synchronous completion request.
type Response struct {
	// Content is the assistant's text response.
	Content string

	// ToolCalls contains any tool invocations requested by the assistant.
	ToolCalls []ToolCall

	// Usage tracks token consumption.
	Usage Usage

	// Model is the actual model used (may differ from request if aliased).
	Model string

	// FinishReason indicates why generation stopped: "stop", "tool_calls", "length", etc.
	FinishReason string
}

// Stream yields incremental chunks from a streaming completion.
type Stream interface {
	// Next returns the next chunk. Returns io.EOF when the stream is complete.
	Next(ctx context.Context) (*Chunk, error)

	// Close releases stream resources.
	Close() error

	// Usage returns the final token usage after the stream completes.
	// May return nil if usage is not yet available.
	Usage() *Usage
}

// Chunk is a single incremental piece of a streaming response.
type Chunk struct {
	// Content is the incremental text delta.
	Content string

	// ToolCalls contains incremental tool call deltas.
	ToolCalls []ToolCall

	// FinishReason is set on the final chunk.
	FinishReason string
}

// Tool describes a function tool available for the LLM to call.
type Tool struct {
	// Name is the tool's unique identifier.
	Name string

	// Description explains what the tool does.
	Description string

	// Parameters is a JSON Schema describing the tool's input parameters.
	Parameters any
}

// ToolCall represents a tool invocation from the assistant.
type ToolCall struct {
	// ID uniquely identifies this tool call (used to match tool results).
	ID string

	// Name is the name of the tool to invoke.
	Name string

	// Arguments is the JSON-encoded arguments for the tool.
	Arguments string
}

// Usage tracks token consumption for a completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
