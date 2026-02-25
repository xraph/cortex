// Package run defines the Run, Step, and ToolCall entities for agent execution.
package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// State represents the lifecycle state of an agent run.
type State string

const (
	StateCreated   State = "created"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StatePaused    State = "paused"
)

// Run represents a single execution of an agent.
type Run struct {
	cortex.Entity
	ID          id.AgentRunID  `json:"id"`
	AgentID     id.AgentID     `json:"agent_id"`
	TenantID    string         `json:"tenant_id,omitempty"`
	State       State          `json:"state"`
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
