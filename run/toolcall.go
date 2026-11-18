package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// MetadataCallID is the tool call metadata key that holds the model's id
// for the call (llm.ToolCall.ID, suspension.PendingCall.ID): the id its
// tool result and an approval decision name it by. It is not unique
// across a run: a later model response may reuse it.
const MetadataCallID = "call_id"

// ToolCall represents a single tool invocation within a step.
type ToolCall struct {
	cortex.Entity
	ID          id.ToolCallID  `json:"id"`
	StepID      id.StepID      `json:"step_id"`
	RunID       id.AgentRunID  `json:"run_id"`
	Scope       cortex.Scope   `json:"scope"`
	ToolName    string         `json:"tool_name"`
	Arguments   string         `json:"arguments,omitempty"`
	Result      string         `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}
