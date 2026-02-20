// Package checkpoint defines the Checkpoint entity for human-in-the-loop control.
package checkpoint

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Decision represents the resolution of a checkpoint.
type Decision struct {
	Approved  bool      `json:"approved"`
	DecidedBy string    `json:"decided_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Checkpoint represents a point where a run pauses for approval.
type Checkpoint struct {
	cortex.Entity
	ID        id.CheckpointID `json:"id"`
	RunID     id.AgentRunID   `json:"run_id"`
	AgentID   id.AgentID      `json:"agent_id"`
	TenantID  string          `json:"tenant_id,omitempty"`
	Reason    string          `json:"reason"`
	StepIndex int             `json:"step_index"`
	State     string          `json:"state"`
	Decision  *Decision       `json:"decision,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}
