package checkpoint

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for checkpoints.
type Store interface {
	CreateCheckpoint(ctx context.Context, cp *Checkpoint) error
	GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*Checkpoint, error)
	Resolve(ctx context.Context, cpID id.CheckpointID, decision Decision) error
	ListPending(ctx context.Context, filter *ListFilter) ([]*Checkpoint, error)
}

// ListFilter controls pagination for checkpoint listing.
type ListFilter struct {
	RunID    string
	TenantID string
	Limit    int
	Offset   int
}
