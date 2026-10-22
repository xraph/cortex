package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func captureSessionFixture(t *testing.T, change func(*workerBoundary)) (*WorkerSession, sandbox.Job, evidence.Ref) {
	t.Helper()
	name, directory, credential, boundary := workerBootstrapFixture(t, declareCapture)
	job := credential.Record.Receipt.Job
	boundary.capture = func(ctx context.Context, _ string, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(acceptanceDeadline(credential.Record.Receipt)) {
			t.Fatal("capture extended signed deadline")
		}
		header, _, err := prepareCapture(claim, ref, confirm)
		state, revision := workerstore.CaptureIntent, uint64(1)
		if confirm {
			state, revision = workerstore.CaptureVerified, 2
		}
		return WorkerCaptureReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: claim.Revision, ArtifactID: ref.ID, BindingDigest: header.Digest, State: state, Revision: revision}, err
	}
	if change != nil {
		change(&boundary)
	}
	w, err := bootstrapWorker(t.Context(), name, directory, job.ID, boundary)
	if err != nil {
		t.Fatal(err)
	}
	return w, job, captureRefForJob(t, job)
}

func TestWorkerCaptureSessionRequiresExecutionAndExactIntent(t *testing.T) {
	w, job, ref := captureSessionFixture(t, nil)
	j := w.CaptureJournal()
	if err := j.Record(t.Context(), job, ref); err == nil {
		t.Fatal("captured before execution")
	}
	if err := w.BeginExecution(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := j.Confirm(t.Context(), job, ref); err == nil {
		t.Fatal("confirmed without intent")
	}
	if err := j.Record(t.Context(), job, ref); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(t.Context(), job, ref); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("repeated intent", err)
	}
	changed := ref
	changed.RetainUntil = ref.RetainUntil.Add(time.Minute)
	if err := j.Confirm(t.Context(), job, changed); err == nil {
		t.Fatal("confirmed substituted reference")
	}
	if err := j.Confirm(t.Context(), job, ref); err != nil {
		t.Fatal(err)
	}
	if err := j.Confirm(t.Context(), job, ref); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("repeated confirmation", err)
	}
}

func TestWorkerCaptureSessionDoesNotRetryUncertainExchange(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		for _, mode := range []string{"lost", "busy", "receipt", "cancel", "revoked"} {
			t.Run(mode+map[bool]string{false: "-intent", true: "-confirm"}[confirm], func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var calls atomic.Int32
				w, job, ref := captureSessionFixture(t, func(b *workerBoundary) {
					next := b.capture
					b.capture = func(ctx context.Context, path string, claim WorkerClaim, ref evidence.Ref, isConfirm bool) (WorkerCaptureReceipt, error) {
						if isConfirm != confirm {
							return next(ctx, path, claim, ref, isConfirm)
						}
						calls.Add(1)
						switch mode {
						case "lost":
							return WorkerCaptureReceipt{}, broker.ErrDenied
						case "busy":
							return WorkerCaptureReceipt{}, broker.ErrPending
						case "receipt":
							return WorkerCaptureReceipt{}, nil
						case "cancel":
							cancel()
						}
						return next(ctx, path, claim, ref, isConfirm)
					}
				})
				if err := w.BeginExecution(t.Context()); err != nil {
					t.Fatal(err)
				}
				j := w.CaptureJournal()
				operation := j.Record
				if confirm {
					if err := j.Record(t.Context(), job, ref); err != nil {
						t.Fatal(err)
					}
					operation = j.Confirm
				}
				if mode == "revoked" {
					w.boundary.authorize = func(context.Context, string, WorkerClaim) error { return broker.ErrDenied }
				}
				if err := operation(ctx, job, ref); err == nil {
					t.Fatal("uncertain capture succeeded")
				}
				before := calls.Load()
				if err := operation(t.Context(), job, ref); !errors.Is(err, broker.ErrConflict) || calls.Load() != before {
					t.Fatal("capture retried", err)
				}
				if _, err := w.Report(t.Context(), sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed}); err != nil {
					t.Fatal("capture prevented failure report", err)
				}
			})
		}
	}
}

func TestWorkerCaptureSessionRejectsForeignJobAndFinishedReport(t *testing.T) {
	w, job, ref := captureSessionFixture(t, nil)
	if err := w.BeginExecution(t.Context()); err != nil {
		t.Fatal(err)
	}
	other := job
	other.Principal = "someone-else"
	other, err := sandbox.Seal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.CaptureJournal().Record(t.Context(), other, ref); err == nil {
		t.Fatal("captured foreign job")
	}
	if _, err := w.Report(t.Context(), sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed}); err != nil {
		t.Fatal(err)
	}
	if err := w.CaptureJournal().Record(t.Context(), job, ref); err == nil {
		t.Fatal("capture after report")
	}
	w.boundary.capture = nil
	if w.CaptureJournal() != nil {
		t.Fatal("advertised unavailable transport")
	}
}

func TestWorkerCaptureSerializesReportAndRechecksAuthorityAfterReceipt(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var revoked atomic.Bool
	w, job, ref := captureSessionFixture(t, func(b *workerBoundary) {
		next, authorize := b.capture, b.authorize
		b.authorize = func(ctx context.Context, path string, claim WorkerClaim) error {
			if revoked.Load() {
				return broker.ErrDenied
			}
			return authorize(ctx, path, claim)
		}
		b.capture = func(ctx context.Context, path string, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return WorkerCaptureReceipt{}, ctx.Err()
			}
			revoked.Store(true)
			return next(ctx, path, claim, ref, confirm)
		}
	})
	if err := w.BeginExecution(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.CaptureJournal().Record(t.Context(), job, ref) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("capture never entered transport")
	}
	if err := w.Authorize(t.Context()); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("overlapping authorization", err)
	}
	if _, err := w.Report(t.Context(), sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed}); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("report overlapped capture", err)
	}
	if err := w.CaptureJournal().Confirm(t.Context(), job, ref); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("overlapping confirmation", err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, broker.ErrDenied) {
			t.Fatal("post-receipt revocation ignored", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not finish")
	}
	if err := w.CaptureJournal().Record(t.Context(), job, ref); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("revoked capture retried", err)
	}
}
