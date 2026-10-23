package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) InsertTaskRun(ctx context.Context, r *run.Run) error {
	if !d.resultTransaction {
		return coordination.ErrAssignmentUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if r == nil || r.Scope.Canonical() != scope.Canonical() || r.Revision != 1 || r.CreatedAt.IsZero() {
		return coordination.ErrInvalidClaim
	}
	err = d.insertRun(ctx, r, r.CreatedAt)
	if mongo.IsDuplicateKeyError(err) {
		return assurance.ErrExists
	}
	return err
}
