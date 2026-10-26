package learning

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ActionDelete requires a host grant separate from ordinary publication ownership.
const ActionDelete Action = "delete"

// DeletionRequest removes local lesson payloads, including historical outboxes.
// Reason is a bounded policy code, never deleted source content.
type DeletionRequest struct {
	ID               id.ID
	ExpectedRevision int64
	Actor            string
	Reason           string
}

// Tombstone reserves an erased lesson identity without retaining its content.
type Tombstone struct {
	ID          id.ID        `json:"id"`
	Scope       cortex.Scope `json:"scope"`
	Revision    int64        `json:"revision"`
	Digest      string       `json:"digest"`
	AggregateID string       `json:"aggregate_id"`
	Actor       string       `json:"actor"`
	Reason      string       `json:"reason"`
	DeletedAt   time.Time    `json:"deleted_at"`
}

// TombstonePage is a bounded scan for restartable destination cleanup.
type TombstonePage struct {
	Items []Tombstone
	After string
}

// RetentionStore erases local payloads and retains minimal exact-scope audit data.
// Remote event history has its own destination retention policy.
type RetentionStore interface {
	DeleteLesson(context.Context, DeletionRequest) (Tombstone, error)
	GetLessonTombstone(context.Context, id.ID) (Tombstone, error)
	ListLessonTombstones(context.Context, string, int) (TombstonePage, error)
}

// Delete authorizes retention workers independently of the lesson's old owner.
func (g *Guard) Delete(ctx context.Context, request DeletionRequest) (Tombstone, error) {
	if err := g.Authorize(ctx, ActionDelete, request.ID, ""); err != nil {
		return Tombstone{}, err
	}
	repository, ok := g.config.Repository.(RetentionStore)
	if !ok {
		return Tombstone{}, ErrUnavailable
	}
	return repository.DeleteLesson(ctx, request)
}
