//go:build linux || darwin

package service

import (
	"context"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/sandbox"
)

type captureAttempt struct {
	digest              string
	recorded            bool
	confirmationStarted bool
}

type sessionCaptureJournal struct{ worker *WorkerSession }

// CaptureJournal returns this session's authenticated capture capability, or nil
// when unavailable. Each declared output gets one intent and one confirmation
// attempt. An uncertain response requires daemon recovery, never worker replay.
func (w *WorkerSession) CaptureJournal() capture.Journal {
	if !w.valid() || w.boundary.capture == nil {
		return nil
	}
	return sessionCaptureJournal{worker: w}
}

func (j sessionCaptureJournal) Record(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	return j.worker.capture(ctx, job, ref, false)
}

func (j sessionCaptureJournal) Confirm(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	return j.worker.capture(ctx, job, ref, true)
}

func (w *WorkerSession) capture(ctx context.Context, job sandbox.Job, ref evidence.Ref, confirm bool) error {
	if !w.valid() || w.boundary.capture == nil || job.Validate() != nil || job.Digest != w.credential.Record.Receipt.Job.Digest {
		return broker.ErrDenied
	}
	if !w.reportMu.TryLock() {
		return broker.ErrConflict
	}
	defer w.reportMu.Unlock()
	if !w.executionStarted || w.reportDigest != "" {
		return broker.ErrConflict
	}
	header, _, err := prepareCapture(w.claim, ref, confirm)
	if err != nil {
		return err
	}
	if validationErr := w.credential.Record.Receipt.Job.ValidateArtifacts([]evidence.Ref{ref}, false); validationErr != nil {
		return validationErr
	}
	attempt, exists := w.captures[ref.ID]
	if confirm {
		if !exists || !attempt.recorded || attempt.confirmationStarted || attempt.digest != header.Digest {
			return broker.ErrConflict
		}
		attempt.confirmationStarted = true
	} else {
		if exists {
			return broker.ErrConflict
		}
		attempt = captureAttempt{digest: header.Digest}
	}
	if w.captures == nil {
		w.captures = make(map[id.ID]captureAttempt)
	}
	// Consume before external verification or transport. Even explicit busy is
	// final for capture: this path cannot cause a second repository upload.
	w.captures[ref.ID] = attempt
	operation, cancel := context.WithDeadline(ctx, w.Deadline())
	defer cancel()
	if authorizationErr := w.authorizeConfigured(operation); authorizationErr != nil {
		return authorizationErr
	}
	receipt, err := w.boundary.capture(operation, w.configuration.Settings.ControlSocket, w.claim, ref, confirm)
	if err != nil {
		return err
	}
	if !captureReceiptMatches(receipt, header, ref.ID) {
		return broker.ErrDenied
	}
	if authorizationErr := w.authorizeConfigured(operation); authorizationErr != nil {
		return authorizationErr
	}
	if err := operation.Err(); err != nil {
		return err
	}
	if !confirm {
		attempt.recorded = true
		w.captures[ref.ID] = attempt
	}
	return nil
}
