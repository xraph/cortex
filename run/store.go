package run

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for runs, steps, and tool calls.
type Store interface {
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, runID id.AgentRunID) (*Run, error)
	UpdateRun(ctx context.Context, run *Run) error
	ListRuns(ctx context.Context, filter *ListFilter) ([]*Run, error)
	CountRuns(ctx context.Context, filter *ListFilter) (int64, error)

	CreateStep(ctx context.Context, step *Step) error
	ListSteps(ctx context.Context, runID id.AgentRunID) ([]*Step, error)

	CreateToolCall(ctx context.Context, tc *ToolCall) error
	ListToolCalls(ctx context.Context, stepID id.StepID) ([]*ToolCall, error)
}

// ListFilter controls pagination and matching for run listing. Scope
// arrives on the context; Exact narrows the read to rows stored at
// precisely that depth instead of everything beneath it.
type ListFilter struct {
	AgentID  string
	TenantID string
	State    State
	Exact    bool
	Limit    int
	Offset   int
}
