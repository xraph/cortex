package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func (e *Engine) preparedMessageWorkReady(ctx context.Context, parent *orchestration.Run) (bool, error) {
	if e.preparedMessaging == nil {
		return false, nil
	}
	runner, err := e.restorePreparedTeam(ctx, parent.ID)
	if err != nil {
		return false, err
	}
	bus, err := runner.messagingBus()
	if err != nil || bus == nil {
		return false, err
	}
	if authErr := e.authorizePreparedMessaging(ctx, parent.ID, id.Nil); authErr != nil {
		return false, authErr
	}
	// Reclaim changes only stale queue ownership, never child execution authority.
	if _, reclaimErr := bus.ReclaimStaleDeliveries(ctx); reclaimErr != nil {
		return false, reclaimErr
	}
	if ready, readErr := bus.PendingDispatch(ctx); readErr != nil || ready {
		return ready, readErr
	}
	private, err := a2a.NewRoutingStore(e.store, parent.ID)
	if err != nil {
		return false, err
	}
	source, ok := private.(a2a.RecipientDiscoveryStore)
	if !ok {
		return false, a2a.ErrRecipientDiscoveryUnavailable
	}
	var after id.DeliveryID
	for seen := 0; seen < 4096; {
		rows, readErr := source.ListDeferredRecipients(ctx, after, 256)
		if readErr != nil {
			return false, readErr
		}
		for _, key := range rows {
			delivery, deliveryErr := private.GetDelivery(ctx, key)
			if deliveryErr != nil {
				return false, deliveryErr
			}
			if delivery.Scope.Canonical() != parent.Scope.Canonical() || delivery.OrchestrationID != parent.ID.String() {
				return false, cortex.ErrInvalidPrepared
			}
			outcome, observeErr := runner.ObserveRecipient(ctx, a2a.RecipientRequest{OrchestrationID: parent.ID, RunID: delivery.RunID, AgentName: delivery.Receiver.Agent})
			if observeErr != nil {
				return false, observeErr
			}
			if outcome.State != a2a.RecipientWaiting {
				return true, e.authorizePreparedMessaging(ctx, parent.ID, delivery.RunID)
			}
		}
		seen += len(rows)
		if len(rows) < 256 {
			break
		}
		after = rows[len(rows)-1]
	}
	durable, available := e.store.(suspension.ResumeAdmissionStore)
	if !available {
		return false, suspension.ErrResumeAdmissionUnavailable
	}
	reader, err := runner.resolutionReader()
	if err != nil {
		return false, err
	}
	resumer := a2aResumer{eng: e, team: parent.ID, reader: reader}
	for _, key := range parent.AgentRunIDs {
		if authErr := e.authorizePreparedMessaging(ctx, parent.ID, key); authErr != nil {
			return false, authErr
		}
		child, readErr := e.store.GetRun(ctx, key)
		if readErr != nil {
			return false, readErr
		}
		if child.State != run.StatePaused && child.State != run.StateRunning {
			continue
		}
		paused, readErr := e.store.GetSuspension(ctx, key)
		if errors.Is(readErr, cortex.ErrNotSuspended) {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		if paused.Reason != suspension.ReasonAgentReply {
			continue
		}
		if child.State == run.StateRunning {
			admitted, admitErr := durable.GetResumeAdmission(ctx, key, paused.ID)
			if errors.Is(admitErr, suspension.ErrResumeAdmissionNotFound) {
				continue
			}
			if admitErr != nil {
				return false, admitErr
			}
			_, startErr := durable.GetResumeStart(ctx, key, admitted.Request.Version.SuspensionID)
			if errors.Is(startErr, suspension.ErrResumeAdmissionNotFound) {
				return true, nil
			}
			if startErr != nil {
				return false, startErr
			}
			continue
		}
		if len(paused.Pending) > maxPendingQuestions {
			return false, a2a.ErrAskResolutionConflict
		}
		for _, call := range paused.Pending {
			retained, replyErr := reader.GetAskResolution(ctx, call.ReplyWith)
			if errors.Is(replyErr, a2a.ErrAskResolutionNotFound) {
				continue
			}
			if replyErr != nil {
				return false, replyErr
			}
			_, input, assembleErr := resumer.assembleResolvedQuestions(ctx, retained, paused.ID)
			if assembleErr != nil {
				return false, assembleErr
			}
			if input != nil {
				return true, e.authorizePreparedMessaging(ctx, parent.ID, key)
			}
			break
		}
	}
	return false, nil
}
