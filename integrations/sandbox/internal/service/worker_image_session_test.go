package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	broker "github.com/xraph/cortex/integrations/sandbox"
)

func TestWorkerImageSourceBindsSessionAndExecution(t *testing.T) {
	var calls atomic.Int32
	w, job, _, _ := evidenceSessionFixture(t, func(b *workerBoundary) {
		b.readImage = func(ctx context.Context, _ string, claim WorkerClaim, pin string, maximum int64, destination io.Writer) error {
			calls.Add(1)
			if claim.JobID.IsNil() || maximum != 64<<20 || pin != imagePin([]byte("candidate")) {
				t.Error("image request lost binding")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Error("unbounded image transfer")
			}
			_, err := destination.Write([]byte("candidate"))
			return err
		}
	})
	source := w.ImageSource()
	if source == nil {
		t.Fatal("image capability missing")
	}
	ctx := cortex.WithScope(t.Context(), job.Scope)
	pin := imagePin([]byte("candidate"))
	if _, err := source.Open(ctx, pin); err == nil {
		t.Fatal("image opened before execution")
	}
	if err := w.BeginExecution(ctx); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		ctx context.Context
		pin string
	}{{t.Context(), pin}, {ctx, "https://external.invalid/image"}} {
		if _, err := source.Open(invalid.ctx, invalid.pin); err == nil {
			t.Fatal("invalid image read admitted")
		}
	}
	reader, err := source.Open(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(got) != "candidate" || calls.Load() != 1 {
		t.Fatal("image read failed", readErr, closeErr)
	}
	w.reportDigest = "reported"
	if _, err = source.Open(ctx, pin); err == nil {
		t.Fatal("image opened after report")
	}
	w.boundary.readImage = nil
	if w.ImageSource() != nil {
		t.Fatal("unconfigured image capability advertised")
	}
}

func TestWorkerImageSourcePreservesLateFailureAndRevocation(t *testing.T) {
	for _, mode := range []string{"receipt", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			var revoked atomic.Bool
			w, job, _, _ := evidenceSessionFixture(t, func(b *workerBoundary) {
				original := b.authorize
				b.authorize = func(ctx context.Context, path string, claim WorkerClaim) error {
					if revoked.Load() {
						return broker.ErrDenied
					}
					return original(ctx, path, claim)
				}
				b.readImage = func(_ context.Context, _ string, _ WorkerClaim, _ string, _ int64, destination io.Writer) error {
					if _, err := destination.Write([]byte("candidate")); err != nil {
						return err
					}
					if mode == "receipt" {
						return broker.ErrDenied
					}
					revoked.Store(true)
					return nil
				}
			})
			ctx := cortex.WithScope(t.Context(), job.Scope)
			if err := w.BeginExecution(ctx); err != nil {
				t.Fatal(err)
			}
			reader, err := w.ImageSource().Open(ctx, imagePin([]byte("candidate")))
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if string(got) != "candidate" || !errors.Is(readErr, broker.ErrDenied) || !errors.Is(closeErr, broker.ErrDenied) {
				t.Fatal("late failure became clean EOF", readErr, closeErr)
			}
		})
	}
}

func TestWorkerImageReaderCloseCancelsAndJoins(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	w, job, _, _ := evidenceSessionFixture(t, func(b *workerBoundary) {
		b.readImage = func(ctx context.Context, _ string, _ WorkerClaim, _ string, _ int64, destination io.Writer) error {
			close(entered)
			defer close(finished)
			_, err := destination.Write(bytes.Repeat([]byte("x"), 64<<10))
			return errors.Join(err, ctx.Err())
		}
	})
	ctx := cortex.WithScope(t.Context(), job.Scope)
	if err := w.BeginExecution(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := w.ImageSource().Open(ctx, imagePin([]byte("image")))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = w.InputSource().Open(ctx, job.Inputs[0].Ref); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("concurrent session access admitted", err)
	}
	if err = reader.Close(); err == nil {
		t.Fatal("cancelled transfer acknowledged")
	}
	select {
	case <-finished:
	default:
		t.Fatal("image transfer not joined")
	}
	if err = w.Authorize(ctx); err != nil {
		t.Fatal("session lock leaked", err)
	}
}
