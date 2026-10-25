package coordination

import (
	"context"

	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// settlementTermination reads metadata only after scan authorization. Storage
// checks it again under guards; this observation cannot authorize closure.
func settlementTermination(ctx context.Context, reader TaskSettlementRunReader, claim Claim) (*TaskUsageTermination, error) {
	if hostport.IsNil(reader) {
		return nil, ErrInvalidScheduler
	}
	current, err := reader.GetRun(ctx, claim.RunID)
	if err != nil {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if current == nil || current.ID != claim.RunID || current.Scope.Canonical() != claim.Scope.Canonical() || current.AgentID.String() != claim.Worker.Agent || current.Provenance.TaskClaimID != claim.ID.String() {
		return nil, ErrTaskUsageConflict
	}
	switch current.State {
	case run.StateCreated, run.StateRunning, run.StatePaused, run.StateCompleted:
		return nil, nil
	case run.StateFailed, run.StateCancelled, run.StateIncomplete:
		if current.Revision < 1 || current.CompletedAt == nil || current.CompletedAt.IsZero() || current.ResultRef != "" || current.VerificationStatus == verification.StatusPassed {
			return nil, ErrTaskUsageConflict
		}
		return &TaskUsageTermination{RunRevision: current.Revision, State: current.State, CompletedAt: *current.CompletedAt}, nil
	default:
		return nil, ErrTaskUsageConflict
	}
}
