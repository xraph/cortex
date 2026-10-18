// Package run defines the Run, Step, and ToolCall entities for agent execution.
package run

import (
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// State represents the lifecycle state of an agent run.
type State string

const (
	StateCreated    State = "created"
	StateRunning    State = "running"
	StateCompleted  State = "completed"
	StateIncomplete State = "incomplete"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
	StatePaused     State = "paused"
)

// Run represents a single execution of an agent.
type Run struct {
	Provenance ExecutionProvenance `json:"provenance,omitempty"`
	Budget     execution.Budget    `json:"budget,omitempty"`
	Usage      execution.Usage     `json:"usage,omitempty"`
	cortex.Entity
	ID                 id.AgentRunID       `json:"id"`
	AgentID            id.AgentID          `json:"agent_id"`
	SessionID          id.SessionID        `json:"session_id,omitempty"`
	Scope              cortex.Scope        `json:"scope"`
	State              State               `json:"state"`
	FinishReason       string              `json:"finish_reason,omitempty"`
	VerificationStatus verification.Status `json:"verification_status"`
	ResultRef          string              `json:"result_ref,omitempty"`
	StopReason         StopReason          `json:"stop_reason,omitempty"`
	Input              string              `json:"input"`
	Output             string              `json:"output,omitempty"`
	Error              string              `json:"error,omitempty"`
	StepCount          int                 `json:"step_count"`
	TokensUsed         int                 `json:"tokens_used"`
	StartedAt          *time.Time          `json:"started_at,omitempty"`
	CompletedAt        *time.Time          `json:"completed_at,omitempty"`
	PersonaRef         string              `json:"persona_ref,omitempty"`
	Metadata           map[string]any      `json:"metadata,omitempty"`
}

// MarshalJSON makes unchecked explicit for legacy runs and zero-value structs.
func (r Run) MarshalJSON() ([]byte, error) {
	type wire Run
	copyRun := wire(r)
	copyRun.VerificationStatus = r.VerificationStatus.Normalized()
	return json.Marshal(copyRun)
}

// UnmarshalJSON preserves unknown enum values and defaults missing assurance.
func (r *Run) UnmarshalJSON(data []byte) error {
	type wire Run
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	decoded.VerificationStatus = decoded.VerificationStatus.Normalized()
	*r = Run(decoded)
	return nil
}
