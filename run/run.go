// Package run defines the Run, Step, and ToolCall entities for agent execution.
package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// RunState represents the lifecycle state of an agent run.
type RunState string

const (
	StateCreated   RunState = "created"
	StateRunning   RunState = "running"
	StateCompleted RunState = "completed"
	StateFailed    RunState = "failed"
	StateCancelled RunState = "cancelled"
	StatePaused    RunState = "paused"
)

// Run represents a single execution of an agent.
type Run struct {
	cortex.Entity
	ID          id.AgentRunID  `json:"id"`
	AgentID     id.AgentID     `json:"agent_id"`
	TenantID    string         `json:"tenant_id,omitempty"`
	State       RunState       `json:"state"`
	Input       string         `json:"input"`
	Output      string         `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
	StepCount   int            `json:"step_count"`
	TokensUsed  int            `json:"tokens_used"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	PersonaRef  string         `json:"persona_ref,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}
