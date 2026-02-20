// Package memory defines the Message entity and memory store interface.
package memory

import "time"

// Message represents a single message in a conversation.
type Message struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []any          `json:"tool_calls,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}
