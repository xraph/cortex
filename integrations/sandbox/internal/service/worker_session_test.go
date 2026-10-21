package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestWorkerSessionReportOnlyRetriesIdenticalBackpressure(t *testing.T) {
	name, directory, credential, boundary := workerBootstrapFixture(t)
	var calls atomic.Int32
	report := boundary.report
	boundary.report = func(ctx context.Context, socket string, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(acceptanceDeadline(credential.Record.Receipt)) {
			t.Fatal("report extended the original deadline")
		}
		if calls.Add(1) == 1 {
			return WorkerReportReceipt{}, broker.ErrPending
		}
		return report(ctx, socket, claim, result)
	}
	worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited, Stdout: []byte("verified transport, ungraded output")}
	if _, err = worker.Report(t.Context(), result); !errors.Is(err, broker.ErrPending) || calls.Load() != 1 {
		t.Fatal("report capacity refusal was not preserved", err)
	}
	if err = worker.Authorize(t.Context()); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("report backpressure allowed further execution authorization", err)
	}
	changed := result
	changed.Stdout = []byte("changed")
	if _, err = worker.Report(t.Context(), changed); !errors.Is(err, broker.ErrConflict) || calls.Load() != 1 {
		t.Fatal("report retry changed the payload", err)
	}
	receipt, err := worker.Report(t.Context(), result)
	body, _ := json.Marshal(result)
	if err != nil || receipt.ProposalDigest != evidence.Digest(body) || calls.Load() != 2 {
		t.Fatal("identical report retry failed", err)
	}
	if _, err = worker.Report(t.Context(), result); !errors.Is(err, broker.ErrConflict) || calls.Load() != 2 {
		t.Fatal("accepted report sent twice", err)
	}
}

func TestWorkerSessionRefusesUncertainReportRetries(t *testing.T) {
	for _, mode := range []string{"lost reply", "bad receipt", "cancelled after receipt", "joined pending"} {
		t.Run(mode, func(t *testing.T) {
			name, directory, credential, boundary := workerBootstrapFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			report := boundary.report
			boundary.report = func(operation context.Context, socket string, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
				calls.Add(1)
				switch mode {
				case "lost reply":
					return WorkerReportReceipt{}, broker.ErrDenied
				case "joined pending":
					return WorkerReportReceipt{}, errors.Join(broker.ErrPending, broker.ErrDenied)
				case "bad receipt":
					return WorkerReportReceipt{SchemaVersion: 1}, nil
				default:
					got, err := report(operation, socket, claim, result)
					cancel()
					return got, err
				}
			}
			worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
			if err != nil {
				t.Fatal(err)
			}
			result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited}
			if got, reportErr := worker.Report(ctx, result); reportErr == nil || !got.JobID.IsNil() || calls.Load() != 1 {
				t.Fatal("uncertain report acknowledged", reportErr)
			}
			if _, err = worker.Report(t.Context(), result); !errors.Is(err, broker.ErrConflict) || calls.Load() != 1 {
				t.Fatal("uncertain report was retried", err)
			}
		})
	}
}

func TestWorkerSessionSerializesAuthorityAndReport(t *testing.T) {
	name, directory, credential, boundary := workerBootstrapFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	report := boundary.report
	boundary.report = func(ctx context.Context, socket string, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
		close(entered)
		select {
		case <-release:
			return report(ctx, socket, claim, result)
		case <-ctx.Done():
			return WorkerReportReceipt{}, ctx.Err()
		}
	}
	worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited}
	done := make(chan error, 1)
	go func() { _, reportErr := worker.Report(t.Context(), result); done <- reportErr }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("report never reached transport")
	}
	if _, err = worker.Report(t.Context(), result); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("concurrent report reached transport", err)
	}
	if err = worker.Authorize(t.Context()); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("execution authorization overlapped a report", err)
	}
	close(release)
	if err = awaitControllerStop(t, done); err != nil {
		t.Fatal("original report did not finish", err)
	}
}

func TestWorkerSessionRechecksDeploymentAndMembership(t *testing.T) {
	for _, mode := range []string{"configuration", "runtime", "membership", "expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			name, directory, credential, boundary := workerBootstrapFixture(t)
			worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
			if err != nil {
				t.Fatal(err)
			}
			var reports atomic.Int32
			worker.boundary.report = func(context.Context, string, WorkerClaim, sandbox.Result) (WorkerReportReceipt, error) {
				reports.Add(1)
				return WorkerReportReceipt{}, broker.ErrDenied
			}
			switch mode {
			case "configuration":
				err = os.WriteFile(name, []byte("changed"), 0o600)
			case "runtime":
				err = os.WriteFile(filepath.Join(worker.configuration.Settings.Runtime.Directory, "runsc"), []byte("changed"), 0o755)
			case "membership":
				worker.boundary.verifyCurrent = func(context.Context, id.ID) error { return broker.ErrDenied }
			case "expired":
				worker.credential.Record.Receipt.AcceptedAt = time.Now().Add(-time.Hour)
			case "revoked":
				worker.boundary.authorize = func(context.Context, string, WorkerClaim) error { return broker.ErrDenied }
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.Authorize(t.Context()); err == nil {
				t.Fatal("changed authority survived reauthorization")
			}
			result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited}
			if got, reportErr := worker.Report(t.Context(), result); reportErr == nil || !got.JobID.IsNil() {
				t.Fatal("changed authority produced a receipt")
			}
			// ReportAt checks current grants itself. All local failures must
			// stop before that transport is called.
			if mode != "revoked" && reports.Load() != 0 {
				t.Fatal("local failure reached report transport")
			}
		})
	}
}
