package a2aremote

import (
	"context"
	"net/http"
	"time"

	"github.com/xraph/cortex/safety"
)

func (s *Service) prepareResultHTTP(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func(), error) {
	active, err := ResultContentRequested(credentialsOf(r))
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		return w, r, nil, err
	}
	if !active {
		return w, r, func() {}, nil
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.contentDeliveries.Add(1) > 64 {
		s.contentDeliveries.Add(-1)
		return w, r, nil, resultContentError(safety.ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(r.Context(), MaxResultDeliveryDuration)
	release := func() { cancel(); s.contentDeliveries.Add(-1) }
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(deadline); err != nil {
		release()
		return w, r, nil, resultContentError(safety.ErrUnavailable)
	}
	return &resultResponseWriter{ResponseWriter: w, controller: controller, ctx: ctx}, r.WithContext(ctx), release, nil
}

// resultResponseWriter bounds delivery after the synchronous guarded read.
// A failed write never causes another response body to be appended.
type resultResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	ctx        context.Context
	started    bool
	err        error
}

func (w *resultResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	if w.err = w.ctx.Err(); w.err != nil {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	if caller, ok := w.ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if w.err = w.controller.SetWriteDeadline(deadline); w.err != nil {
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.ResponseWriter.WriteHeader(status)
}

func (w *resultResponseWriter) Write(b []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.ResponseWriter.Write(b)
	w.err = err
	return n, err
}
