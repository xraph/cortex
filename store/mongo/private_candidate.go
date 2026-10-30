package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
)

func (d assuranceDriver) SetRunCandidate(ctx context.Context, r *run.Run, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	revision, err := runstate.NextRevision(r.Revision)
	if err != nil {
		return err
	}
	m := runToModel(r)
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"], filter["revision"] = r.ID.String(), scope.Canonical(), r.Revision
	set := bson.M{"revision": revision, "state": m.State, "stop_reason": m.StopReason, "finish_reason": m.FinishReason, "output": m.Output, "verification_status": m.VerificationStatus, "result_ref": m.ResultRef, "step_count": m.StepCount, "tokens_used": m.TokensUsed, "usage": m.Usage, "completed_at": m.CompletedAt, "updated_at": at}
	if r.State != run.StateRunning {
		set["lease_owner"], set["lease_expires_at"] = "", nil
	}
	result, err := d.store.mdb.NewUpdate((*runModel)(nil)).Filter(filter).SetUpdate(bson.M{"$set": set}).Exec(ctx)
	if err != nil {
		return err
	}
	if result.MatchedCount() != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}
