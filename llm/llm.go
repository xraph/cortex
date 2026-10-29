// Package llm defines a provider-agnostic LLM client interface for the Cortex engine.
//
// This abstraction allows the engine to call any LLM backend (Nexus, direct providers,
// mock) without importing provider-specific packages. Adapters convert between these
// types and the concrete provider types.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrContentBlocked reports that a guard in front of the provider refused
// the request or the response. A client wraps the guard's own error with
// it, so errors.As still finds that error. The engine ends the run with
// stop reason safety_block and dispatches no tool call from the response.
var ErrContentBlocked = errors.New("llm: provider guard blocked the response")

// Keys the engine sets in Request.Metadata.
const (
	// MetadataRun is the ID of the run that made the request.
	MetadataRun = "run"
	// MetadataAgent is the ID of the agent the run belongs to.
	MetadataAgent = "agent"
)

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

	// ToolChoice constrains whether and which tool the model calls. Nil
	// leaves it to the model. The engine never sets it.
	ToolChoice *ToolChoice `json:"ToolChoice,omitempty"`

	// Metadata labels the request for the client's usage records, for
	// example with MetadataRun and MetadataAgent. The engine sets it. It is
	// kept out of the request's JSON, so request digests do not depend on
	// it.
	Metadata map[string]string `json:"-"`
}

// ToolChoice constrains the model's use of tools.
type ToolChoice struct {
	// Mode is "auto", "none", "required" or "function".
	Mode string
	// Function names the tool to call when Mode is "function".
	Function string
}

// NativeContent is an assistant reply in the provider's own format, such
// as Anthropic content blocks with their signed thinking. A client sets it
// on a response, and the engine keeps it on the assistant message it
// appends and sends it back unchanged, so a thinking model sees its own
// earlier turns. It never goes into conversation memory and is never
// published.
type NativeContent struct {
	// Provider names the provider whose format Content is in.
	Provider string `json:"provider"`
	// Model is the model that produced the reply.
	Model string `json:"model"`
	// Content is the reply's raw content blocks, thinking signatures
	// included.
	Content json.RawMessage `json:"content"`
}

// Clone returns a copy of n that shares no memory with it, or nil for nil.
func (n *NativeContent) Clone() *NativeContent {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = append(json.RawMessage(nil), n.Content...)
	return &out
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

	// Native is the provider's own form of an assistant message, carried
	// back to the model that produced it. Nil for every other message.
	Native *NativeContent `json:"Native,omitempty"`
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

	// Native is the reply in the provider's own format, when the client
	// has it.
	Native *NativeContent `json:"Native,omitempty"`
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

	// Native is the whole reply in the provider's own format. A client sets
	// it on the final chunk only.
	Native *NativeContent `json:"Native,omitempty"`
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
