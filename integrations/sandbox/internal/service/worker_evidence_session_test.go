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
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func evidenceSessionFixture(t *testing.T, change func(*workerBoundary)) (*WorkerSession, sandbox.Job, evidence.Ref, evidence.Artifact) {
	t.Helper()
	var input evidence.Ref
	name, directory, credential, boundary := workerBootstrapFixture(t, func(job *sandbox.Job) {
		declareCapture(job)
		input = wireCaptureRef(t)
		input.Scope = job.Scope
		input.Version = "source:1"
		job.Inputs = []sandbox.Input{{Path: "/inputs/data", Ref: input}}
	})
	boundary.capture = func(ctx context.Context, _ string, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
		session := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
		return session.Capture(ctx, ref, confirm)
	}
	boundary.readEvidence = func(ctx context.Context, _ string, _ WorkerClaim, _ evidence.Ref, _ bool, w io.Writer) error {
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(acceptanceDeadline(credential.Record.Receipt)) {
			t.Error("transfer extended signed deadline")
		}
		_, err := w.Write([]byte("candidate"))
		return errors.Join(err, ctx.Err())
	}
	boundary.putArtifact = func(ctx context.Context, _ string, _ WorkerClaim, a evidence.Artifact) (evidence.Ref, error) {
		return a.Ref, ctx.Err()
	}
	if change != nil {
		change(&boundary)
	}
	w, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil {
		t.Fatal(err)
	}
	job := credential.Record.Receipt.Job
	return w, job, input, evidence.Artifact{Ref: captureRefForJob(t, job), Data: []byte("candidate")}
}

func TestWorkerEvidenceAdaptersRequireExecutionAndCaptureIntent(t *testing.T) {
	w, job, input, output := evidenceSessionFixture(t, nil)
	ctx := cortex.WithScope(t.Context(), job.Scope)
	if _, err := w.InputSource().Open(ctx, input); err == nil {
		t.Fatal("input opened before execution attempt")
	}
	if err := w.BeginExecution(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := w.InputSource().Open(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(got) != "candidate" {
		t.Fatal("input transfer", err, closeErr)
	}
	repository := w.ArtifactRepository()
	if _, err = repository.Put(ctx, output); err == nil {
		t.Fatal("upload without capture intent")
	}
	if err = w.CaptureJournal().Record(ctx, job, output.Ref); err != nil {
		t.Fatal(err)
	}
	if err = w.CaptureJournal().Confirm(ctx, job, output.Ref); err == nil {
		t.Fatal("confirmed before verified readback")
	}
	if _, err = repository.Put(ctx, output); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.Put(ctx, output); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("repeated upload", err)
	}
	reader, err = repository.Open(ctx, output.Ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(reader)
	closeErr = reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, output.Data) {
		t.Fatal("output readback", err, closeErr)
	}
	if err = w.CaptureJournal().Confirm(ctx, job, output.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Report(ctx, sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err = w.InputSource().Open(ctx, input); err == nil {
		t.Fatal("input opened after report")
	}
}

func TestWorkerEvidenceReaderCloseCancelsAndJoinsTransfer(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	w, job, input, _ := evidenceSessionFixture(t, func(b *workerBoundary) {
		b.readEvidence = func(ctx context.Context, _ string, _ WorkerClaim, _ evidence.Ref, _ bool, destination io.Writer) error {
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
	reader, err := w.InputSource().Open(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("transfer did not start")
	}
	if err = w.Authorize(ctx); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("authorization overlapped transfer", err)
	}
	if err = reader.Close(); err == nil {
		t.Fatal("early close acknowledged transfer")
	}
	select {
	case <-finished:
	default:
		t.Fatal("close returned before transfer joined")
	}
	if err = w.Authorize(ctx); err != nil {
		t.Fatal("closed reader retained session", err)
	}
}

func TestWorkerEvidenceUploadConsumesUncertainAttempt(t *testing.T) {
	var calls atomic.Int32
	w, job, _, output := evidenceSessionFixture(t, func(b *workerBoundary) {
		b.putArtifact = func(context.Context, string, WorkerClaim, evidence.Artifact) (evidence.Ref, error) {
			calls.Add(1)
			return evidence.Ref{}, broker.ErrPending
		}
	})
	ctx := cortex.WithScope(t.Context(), job.Scope)
	if err := w.BeginExecution(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.CaptureJournal().Record(ctx, job, output.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ArtifactRepository().Put(ctx, output); !errors.Is(err, broker.ErrPending) {
		t.Fatal(err)
	}
	if _, err := w.ArtifactRepository().Put(ctx, output); !errors.Is(err, broker.ErrConflict) || calls.Load() != 1 {
		t.Fatal("uncertain upload retried", err)
	}
}

func TestWorkerEvidenceReaderPreservesLateFailureAndRevocation(t *testing.T) {
	for _, mode := range []string{"lost-receipt", "revoked", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			var revoked atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w, job, input, _ := evidenceSessionFixture(t, func(b *workerBoundary) {
				authorize := b.authorize
				b.authorize = func(ctx context.Context, path string, claim WorkerClaim) error {
					if revoked.Load() {
						return broker.ErrDenied
					}
					return authorize(ctx, path, claim)
				}
				b.readEvidence = func(_ context.Context, _ string, _ WorkerClaim, _ evidence.Ref, _ bool, destination io.Writer) error {
					if _, err := destination.Write([]byte("candidate")); err != nil {
						return err
					}
					switch mode {
					case "lost-receipt":
						return broker.ErrDenied
					case "revoked":
						revoked.Store(true)
					case "cancelled":
						cancel()
					}
					return nil
				}
			})
			ctx = cortex.WithScope(ctx, job.Scope)
			if err := w.BeginExecution(ctx); err != nil {
				t.Fatal(err)
			}
			reader, err := w.InputSource().Open(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(reader)
			if err == nil {
				t.Fatal("provisional bytes became successful EOF")
			}
			if closeErr := reader.Close(); closeErr == nil {
				t.Fatal("close discarded transfer failure")
			}
		})
	}
}

func TestWorkerEvidenceAdaptersRejectForeignBindingsBeforeTransport(t *testing.T) {
	for _, mode := range []string{"scope", "input-binding", "output-binding", "before-upload", "confirmed", "missing-capability"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			w, job, input, output := evidenceSessionFixture(t, func(b *workerBoundary) {
				next := b.readEvidence
				b.readEvidence = func(ctx context.Context, path string, claim WorkerClaim, ref evidence.Ref, artifact bool, destination io.Writer) error {
					calls.Add(1)
					return next(ctx, path, claim, ref, artifact, destination)
				}
			})
			ctx := cortex.WithScope(t.Context(), job.Scope)
			if err := w.BeginExecution(ctx); err != nil {
				t.Fatal(err)
			}
			source, ref := w.InputSource(), input
			switch mode {
			case "scope":
				ctx = t.Context()
			case "input-binding":
				ref.Version = "changed"
			case "output-binding", "before-upload", "confirmed":
				source, ref = w.ArtifactRepository(), output.Ref
				if err := w.CaptureJournal().Record(ctx, job, ref); err != nil {
					t.Fatal(err)
				}
				if mode == "output-binding" {
					ref.RetainUntil = ref.RetainUntil.Add(time.Minute)
				}
				if mode == "confirmed" {
					if _, err := w.ArtifactRepository().Put(ctx, output); err != nil {
						t.Fatal(err)
					}
					reader, err := source.Open(ctx, ref)
					if err != nil {
						t.Fatal(err)
					}
					_, err = io.Copy(io.Discard, reader)
					closeErr := reader.Close()
					if err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
					calls.Store(0)
					if err := w.CaptureJournal().Confirm(ctx, job, ref); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-capability":
				w.boundary.readEvidence = nil
			}
			if reader, err := source.Open(ctx, ref); err == nil || reader != nil || calls.Load() != 0 {
				t.Fatal("unbound read reached transport", err)
			}
		})
	}
	var empty *WorkerSession
	if empty.InputSource() != nil || empty.ArtifactRepository() != nil {
		t.Fatal("nil session advertised evidence")
	}
}

func TestWorkerEvidenceUploadRechecksBindingAndAuthority(t *testing.T) {
	for _, mode := range []string{"binding", "revoked", "cancelled", "mutation"} {
		t.Run(mode, func(t *testing.T) {
			var revoked atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w, job, _, output := evidenceSessionFixture(t, func(b *workerBoundary) {
				authorize := b.authorize
				b.authorize = func(ctx context.Context, path string, claim WorkerClaim) error {
					if revoked.Load() {
						return broker.ErrDenied
					}
					return authorize(ctx, path, claim)
				}
				b.putArtifact = func(_ context.Context, _ string, _ WorkerClaim, a evidence.Artifact) (evidence.Ref, error) {
					switch mode {
					case "binding":
						a.Ref.Version = "changed"
					case "revoked":
						revoked.Store(true)
					case "cancelled":
						cancel()
					case "mutation":
						a.Ref.Scope.Levels[0].Value = "changed"
						a.Data[0] = 'x'
					}
					return a.Ref, nil
				}
			})
			ctx = cortex.WithScope(ctx, job.Scope)
			if err := w.BeginExecution(ctx); err != nil {
				t.Fatal(err)
			}
			if err := w.CaptureJournal().Record(ctx, job, output.Ref); err != nil {
				t.Fatal(err)
			}
			if ref, err := w.ArtifactRepository().Put(ctx, output); err == nil || !ref.ID.IsNil() {
				t.Fatal("unverified upload acknowledged", ref, err)
			}
			if string(output.Data) != "candidate" || output.Ref.Scope.Canonical() != job.Scope.Canonical() {
				t.Fatal("provider mutated caller artifact")
			}
			if _, err := w.ArtifactRepository().Open(ctx, output.Ref); err == nil {
				t.Fatal("readback allowed after failed upload")
			}
		})
	}
}
