package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func (e *Engine) expireAdmittedReply(ctx context.Context, admitted suspension.ResumeAdmission) (*run.Run, error) {
	key := admitted.Request.Version
	if err := checkResumeRecoveryAuthority(ctx, key.RunID); err != nil {
		return nil, err
	}
	durable, ok := e.store.(suspension.ResumeRecoveryStore)
	if !ok {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	receipt, err := durable.ExpireResumeAdmission(ctx, key.RunID, key.SuspensionID, admitted.RequestDigest)
	if err != nil {
		return nil, err
	}
	if receipt.Version != key || receipt.Scope.Canonical() != admitted.Request.Scope.Canonical() || receipt.RequestDigest != admitted.RequestDigest {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	if authErr := checkResumeRecoveryAuthority(ctx, key.RunID); authErr != nil {
		return nil, authErr
	}
	current, err := e.store.GetRun(ctx, key.RunID)
	if err != nil {
		return nil, err
	}
	if current == nil || current.ID != key.RunID || current.Scope.Canonical() != receipt.Scope.Canonical() || current.State != run.StateIncomplete || current.StopReason != run.StopDeadline || current.Revision != receipt.RunRevision {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	if receipt.Fresh {
		e.emitTerminalOutcome(ctx, current)
	}
	return current, nil
}

// replyRecoveryPause preserves discovery after rehydration removed a live pause.
// A retained admission only identifies work; RecoverAgentReply still refuses an
// existing start receipt. Callers authorize the run before this private read.
func (e *Engine) replyRecoveryPause(ctx context.Context, current *run.Run, authorize func(context.Context, id.AgentRunID) error) (id.SuspensionID, error) {
	if err := authorize(ctx, current.ID); err != nil {
		return id.Nil, err
	}
	paused, err := e.store.GetSuspension(ctx, current.ID)
	if err == nil {
		if paused == nil || paused.RunID != current.ID || paused.Scope.Canonical() != current.Scope.Canonical() {
			return id.Nil, suspension.ErrResumeAdmissionConflict
		}
		if paused.Reason != suspension.ReasonAgentReply {
			return id.Nil, nil
		}
		if current.State == run.StateRunning {
			// Initial suspension writes its row before changing the run state.
			// A running run is recoverable only with a durable resume admission.
			durable, ok := e.store.(suspension.ResumeAdmissionStore)
			if !ok {
				return id.Nil, nil
			}
			if authErr := authorize(ctx, current.ID); authErr != nil {
				return id.Nil, authErr
			}
			admitted, readErr := durable.GetResumeAdmission(ctx, current.ID, paused.ID)
			if errors.Is(readErr, suspension.ErrResumeAdmissionNotFound) {
				return id.Nil, nil
			}
			if readErr != nil {
				return id.Nil, readErr
			}
			if admitted.Request.Version.RunID != current.ID || admitted.Request.Version.SuspensionID != paused.ID || admitted.Request.Scope.Canonical() != current.Scope.Canonical() {
				return id.Nil, suspension.ErrResumeAdmissionConflict
			}
		}
		return paused.ID, nil
	}
	if !errors.Is(err, cortex.ErrNotSuspended) {
		return id.Nil, err
	}
	if current.State != run.StateRunning {
		return id.Nil, nil
	}
	durable, ok := e.store.(suspension.ResumeRecoveryStore)
	if !ok {
		return id.Nil, nil
	}
	if authErr := authorize(ctx, current.ID); authErr != nil {
		return id.Nil, authErr
	}
	admitted, err := durable.LatestResumeAdmission(ctx, current.ID)
	if errors.Is(err, suspension.ErrResumeAdmissionNotFound) {
		return id.Nil, nil
	}
	if err != nil {
		return id.Nil, err
	}
	if admitted.Request.Version.RunID != current.ID || admitted.Request.Scope.Canonical() != current.Scope.Canonical() || admitted.ClaimedRevision > current.Revision {
		return id.Nil, suspension.ErrResumeAdmissionConflict
	}
	return admitted.Request.Version.SuspensionID, nil
}
