package service

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

// Reap fences worker completion durably before stopping the service. It commits
// the terminal failure or cancellation only at the revision that was fenced.
// Concurrent cancellation, journal failures and uncertain service observations
// leave work unresolved for retry. Cancel the journal first for a user-requested
// cancellation; Reap alone records recovery of a lost worker as failed.
func (r *Reaper) Reap(ctx context.Context, store *workerstore.Store, key id.ID) (workerstore.Record, error) {
	if store == nil {
		return workerstore.Record{}, fmt.Errorf("cortex: missing worker journal")
	}
	record, err := store.RequireReap(ctx, key)
	if err != nil {
		return workerstore.Record{}, err
	}
	if record.Phase == workerstore.Terminal {
		return record, nil
	}
	if operationErr := r.FenceAndStop(ctx, key); operationErr != nil {
		return workerstore.Record{}, operationErr
	}
	return store.Reaped(ctx, key, record.Revision)
}
