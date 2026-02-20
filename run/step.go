package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Step represents a single reasoning step within a run.
type Step struct {
	cortex.Entity
	ID          id.StepID      `json:"id"`
	RunID       id.AgentRunID  `json:"run_id"`
	Index       int            `json:"index"`
	Type        string         `json:"type,omitempty"`
	Input       string         `json:"input,omitempty"`
	Output      string         `json:"output,omitempty"`
	TokensUsed  int            `json:"tokens_used"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}
