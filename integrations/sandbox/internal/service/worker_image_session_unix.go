//go:build linux || darwin

package service

import (
	"context"
	"io"

	"github.com/xraph/cortex"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
)

type sessionImageSource struct{ worker *WorkerSession }

// ImageSource returns this session's private image read capability. The daemon
// permits only the signed manifest and its direct members. Readers retain the
// session until a verified receipt or Close cancels and joins the transfer.
func (w *WorkerSession) ImageSource() image.Source {
	if !w.valid() || w.boundary.readImage == nil {
		return nil
	}
	return sessionImageSource{worker: w}
}

func (s sessionImageSource) Open(ctx context.Context, pin string) (io.ReadCloser, error) {
	w := s.worker
	if !w.valid() || w.boundary.readImage == nil {
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
	if cortex.ScopeFromContext(ctx).Canonical() != w.credential.Record.Receipt.Job.Scope.Canonical() || !image.ValidDigest(pin) {
		return nil, broker.ErrDenied
	}
	maximum := w.configuration.Settings.ImageStaging.CompressedBytes
	if _, _, err := prepareImageRead(w.claim, pin, maximum); err != nil {
		return nil, err
	}
	reader, err := w.openContentLocked(ctx, func(operation context.Context, destination io.Writer) error {
		return w.boundary.readImage(operation, w.configuration.Settings.ControlSocket, w.claim, pin, maximum, destination)
	}, nil)
	if err == nil {
		locked = false
	}
	return reader, err
}
