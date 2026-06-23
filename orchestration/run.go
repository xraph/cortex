package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Orchestration run status values.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Run is the persisted execution record of one orchestration.
type Run struct {
	cortex.Entity
	ID          id.OrchestrationID       `json:"id"`
	ConfigID    id.OrchestrationConfigID `json:"config_id,omitempty"` // empty for programmatic runs
	AppID       string                   `json:"app_id"`
	TenantID    string                   `json:"tenant_id,omitempty"`
	Strategy    string                   `json:"strategy"`
	Status      string                   `json:"status"`
	Input       string                   `json:"input"`
	Output      string                   `json:"output,omitempty"`
	Error       string                   `json:"error,omitempty"`
	AgentRunIDs []id.AgentRunID          `json:"agent_run_ids,omitempty"`
	StartedAt   time.Time                `json:"started_at"`
	CompletedAt *time.Time               `json:"completed_at,omitempty"`
}

// RunStore defines persistence for orchestration run records.
type RunStore interface {
	CreateOrchestrationRun(ctx context.Context, r *Run) error
	GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*Run, error)
	UpdateOrchestrationRun(ctx context.Context, r *Run) error
	ListOrchestrationRuns(ctx context.Context, filter *RunListFilter) ([]*Run, error)
	CountOrchestrationRuns(ctx context.Context, filter *RunListFilter) (int64, error)
}

// RunListFilter controls pagination and filtering for orchestration run listing.
type RunListFilter struct {
	AppID  string
	Status string
	Limit  int
	Offset int
}
