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
	StatusWaiting   = "waiting"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Run is the persisted execution record of one orchestration.
type Run struct {
	// ContinuationGeneration fences coordinator snapshots across waiting passes.
	// It is internal storage state and is not accepted through JSON APIs.
	ContinuationGeneration int64 `json:"-"`
	cortex.Entity
	ID          id.OrchestrationID       `json:"id"`
	ConfigID    id.OrchestrationConfigID `json:"config_id,omitempty"` // empty for programmatic runs
	Scope       cortex.Scope             `json:"scope"`
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

// ChildRunStore atomically links one reserved child while its parent is running.
// A link is single-use. Duplicate or uncertain acknowledgments never authorize
// another execution. Implementations preserve linked IDs through later updates.
type ChildRunStore interface {
	LinkOrchestrationChild(context.Context, id.OrchestrationID, id.AgentRunID) error
}

// RunListFilter controls pagination and filtering for orchestration run
// listing. Scope arrives on the context; Exact narrows to rows stored at
// precisely that depth instead of everything beneath it.
type RunListFilter struct {
	Exact  bool
	Status string
	Limit  int
	Offset int
}
