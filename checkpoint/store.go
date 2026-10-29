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

// SweepStore is an optional capability of checkpoint stores, for the
// engine sweeper. It finds pending checkpoints that no decision will ever
// reach, such as one whose run has ended or moved on to another pause.
type SweepStore interface {
	// ListPendingAcrossScopes lists pending checkpoints whose id sorts
	// after the given one, in id order, at most limit. A nil id starts
	// from the first. It deliberately bypasses scope, for the sweeper
	// only, which acts on each checkpoint under that checkpoint's own
	// scope.
	ListPendingAcrossScopes(ctx context.Context, after id.CheckpointID, limit int) ([]*Checkpoint, error)
}
