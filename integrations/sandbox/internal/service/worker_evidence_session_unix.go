//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
)

type sessionEvidenceSource struct {
	worker   *WorkerSession
	artifact bool
}
type sessionArtifactRepository struct{ sessionEvidenceSource }

// InputSource returns the signed-input read capability of this session. Readers
// hold the session until transfer completes or Close cancels and joins it. EOF
// means the broker receipt and final current-authority check both succeeded.
func (w *WorkerSession) InputSource() staging.Source {
	if !w.valid() || w.boundary.readEvidence == nil {
		return nil
	}
	return sessionEvidenceSource{worker: w}
}

// ArtifactRepository returns this session's capture upload/readback capability.
// Each artifact gets one upload attempt after intent acknowledgement. An uncertain
// upload, including capacity refusal, requires broker recovery instead of replay.
func (w *WorkerSession) ArtifactRepository() capture.Repository {
	if !w.valid() || w.boundary.readEvidence == nil || w.boundary.putArtifact == nil || w.boundary.capture == nil {
		return nil
	}
	return sessionArtifactRepository{sessionEvidenceSource{worker: w, artifact: true}}
}

func (w *WorkerSession) evidenceBinding(ctx context.Context, ref evidence.Ref, artifact bool) error {
	if cortex.ScopeFromContext(ctx).Canonical() != w.credential.Record.Receipt.Job.Scope.Canonical() {
		return evidence.ErrDenied
	}
	header, _, err := prepareEvidenceRead(w.claim, ref, artifact)
	if err != nil {
		return err
	}
	if artifact {
		attempt, ok := w.captures[ref.ID]
		if !ok || !attempt.recorded || attempt.confirmationStarted || attempt.digest != header.Digest {
			return broker.ErrConflict
		}
		return nil
	}
	for _, input := range w.credential.Record.Receipt.Job.Inputs {
		digest, digestErr := input.Ref.BindingDigest()
		if digestErr == nil && digest == header.Digest {
			return nil
		}
	}
	return evidence.ErrDenied
}

func (s sessionEvidenceSource) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	w := s.worker
	if !w.valid() || w.boundary.readEvidence == nil {
		return nil, broker.ErrDenied
	}
	if !w.reportMu.TryLock() {
		return nil, broker.ErrConflict
	}
	locked := true
	defer func() {
		if locked {
			w.reportMu.Unlock()
		}
	}()
	if !w.executionStarted || w.reportDigest != "" {
		return nil, broker.ErrConflict
	}
	if err := w.evidenceBinding(ctx, ref, s.artifact); err != nil {
		return nil, err
	}
	if s.artifact && !w.captures[ref.ID].uploaded {
		return nil, broker.ErrConflict
	}
	operation, cancel := context.WithDeadline(ctx, w.Deadline())
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	stop := func() { bound(); cancel() }
	if err := w.authorizeConfigured(operation); err != nil {
		stop()
		return nil, err
	}
	ref = detachedCaptureRef(ref)
	reader, writer := io.Pipe()
	transfer := &sessionEvidenceReader{PipeReader: reader, cancel: stop, done: make(chan struct{})}
	interrupt := context.AfterFunc(operation, func() { _ = reader.CloseWithError(operation.Err()) })
	locked = false
	go func() {
		err := w.boundary.readEvidence(operation, w.configuration.Settings.ControlSocket, w.claim, ref, s.artifact, writer)
		if err == nil {
			err = w.authorizeConfigured(operation)
		}
		err = errors.Join(err, operation.Err())
		if err == nil && s.artifact {
			attempt := w.captures[ref.ID]
			attempt.readbackVerified = true
			w.captures[ref.ID] = attempt
		}
		transfer.err = err
		interrupt()
		stop()
		w.reportMu.Unlock()
		// Set the result before EOF. Close may cancel after observing EOF, but
		// it still joins this completed exchange and preserves its final result.
		_ = writer.CloseWithError(err)
		close(transfer.done)
	}()
	return transfer, nil
}

type sessionEvidenceReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

func (r *sessionEvidenceReader) Close() error {
	r.once.Do(func() {
		select {
		case <-r.done:
		default:
			r.cancel()
			_ = r.CloseWithError(context.Canceled)
			<-r.done
		}
		_ = r.PipeReader.Close()
	})
	return r.err
}

func (s sessionArtifactRepository) Put(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	w := s.worker
	if !w.valid() || w.boundary.putArtifact == nil {
		return evidence.Ref{}, broker.ErrDenied
	}
	if !w.reportMu.TryLock() {
		return evidence.Ref{}, broker.ErrConflict
	}
	defer w.reportMu.Unlock()
	if !w.executionStarted || w.reportDigest != "" {
		return evidence.Ref{}, broker.ErrConflict
	}
	if _, _, err := prepareArtifact(w.claim, a); err != nil {
		return evidence.Ref{}, err
	}
	if err := w.evidenceBinding(ctx, a.Ref, true); err != nil {
		return evidence.Ref{}, err
	}
	attempt := w.captures[a.Ref.ID]
	if attempt.uploadStarted {
		return evidence.Ref{}, broker.ErrConflict
	}
	attempt.uploadStarted = true
	w.captures[a.Ref.ID] = attempt
	operation, cancel := context.WithDeadline(ctx, w.Deadline())
	defer cancel()
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	defer bound()
	if err := w.authorizeConfigured(operation); err != nil {
		return evidence.Ref{}, err
	}
	expected := detachedCaptureRef(a.Ref)
	detached, err := evidence.Seal(a)
	if err != nil {
		return evidence.Ref{}, err
	}
	stored, err := w.boundary.putArtifact(operation, w.configuration.Settings.ControlSocket, w.claim, detached)
	if err != nil {
		return evidence.Ref{}, err
	}
	digest, err := stored.BindingDigest()
	if err != nil || digest != attempt.digest {
		return evidence.Ref{}, broker.ErrDenied
	}
	if authorizationErr := w.authorizeConfigured(operation); authorizationErr != nil {
		return evidence.Ref{}, authorizationErr
	}
	if operationErr := operation.Err(); operationErr != nil {
		return evidence.Ref{}, operationErr
	}
	attempt.uploaded = true
	w.captures[expected.ID] = attempt
	return expected, nil
}
