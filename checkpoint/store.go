package checkpoint

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Store defines persistence for checkpoints.
type Store interface {
	CreateCheckpoint(ctx context.Context, cp *Checkpoint) error
	GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*Checkpoint, error)
	// Resolve records decision on a pending checkpoint at the caller's
	// exact scope. A checkpoint that is no longer pending keeps its first
	// decision and Resolve returns an error wrapping cortex.ErrInvalidState;
	// one that does not exist there is cortex.ErrCheckpointNotFound.
	Resolve(ctx context.Context, cpID id.CheckpointID, decision Decision) error
	ListPending(ctx context.Context, filter *ListFilter) ([]*Checkpoint, error)
	CountPending(ctx context.Context, filter *ListFilter) (int64, error)
}

// ListFilter controls pagination for checkpoint listing.
type ListFilter struct {
	RunID  string
	Limit  int
	Offset int
}
