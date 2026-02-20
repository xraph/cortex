package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ToolCall represents a single tool invocation within a step.
type ToolCall struct {
	cortex.Entity
	ID          id.ToolCallID  `json:"id"`
	StepID      id.StepID      `json:"step_id"`
	RunID       id.AgentRunID  `json:"run_id"`
	ToolName    string         `json:"tool_name"`
	Arguments   string         `json:"arguments,omitempty"`
	Result      string         `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}
