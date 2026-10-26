package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func (e *Engine) releasePendingQuestions(ctx context.Context, paused *suspension.Suspension, bus *a2a.Bus) error {
	if bus == nil || !bus.SupportsHeldAskRelease() {
		return nil
	}
	if err := checkResumeRecoveryAuthority(ctx, paused.RunID); err != nil {
		return err
	}
	current, err := e.store.GetRun(ctx, paused.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.ID != paused.RunID || current.State != run.StatePaused || current.Scope.Canonical() != paused.Scope.Canonical() || current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return cortex.ErrNotSuspended
	}
	for _, call := range paused.Pending {
		if err := checkResumeRecoveryAuthority(ctx, current.ID); err != nil {
			return err
		}
		pending, pendingErr := bus.HeldAskPending(ctx, call.ReplyWith)
		if pendingErr != nil {
			return pendingErr
		}
		if !pending {
			continue
		}
		if err := checkResumeRecoveryAuthority(ctx, current.ID); err != nil {
			return err
		}
		_, releaseErr := bus.ReleasePausedAsk(ctx, a2a.HeldAskRequest{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision, ReplyWith: call.ReplyWith})
		if releaseErr != nil {
			return releaseErr
		}
	}
	return nil
}
