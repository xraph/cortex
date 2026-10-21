package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func TestWorkerReportRetainsProposalBeforeControllerCleanup(t *testing.T) {
	controller, claim, peer, _ := authorizationFixture(t, func(c *Controller, _ *fixtureWorkerPeer) { c.interval = 10 * time.Millisecond })
	current, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: current.Receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("reported")}
	proposed, err := controller.proposeWorkerResult(t.Context(), peer, claim, result)
	if err != nil || proposed.JobID != claim.JobID || proposed.ClaimRevision != claim.Revision || proposed.ProposalDigest == "" {
		t.Fatalf("report bypassed pending cleanup: %+v %v", proposed, err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		record, loadErr := controller.launcher.store.Load(t.Context(), claim.JobID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if record.Phase == workerstore.Terminal {
			if record.Result.State != sandbox.StateExited || string(record.Result.Stdout) != "reported" {
				t.Fatal("report lost during controller finalization")
			}
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("controller did not finalize proposal")
		}
	}
}

func TestWorkerReportRejectsRevokedOrInvalidResults(t *testing.T) {
	for _, mode := range []string{"revoked", "invalid result", "peer exit", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			controller, claim, peer, _ := authorizationFixture(t)
			current, err := controller.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			result := sandbox.Result{JobDigest: current.Receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "revoked":
				controller.scanner.revalidate = func(context.Context, sandbox.Job) error { return broker.ErrDenied }
			case "invalid result":
				result.ExitCode = 256
			case "peer exit":
				peer.dieAfterFirst.Store(true)
			case "canceled":
				cancel()
			}
			if record, reportErr := controller.proposeWorkerResult(ctx, peer, claim, result); reportErr == nil || !record.JobID.IsNil() {
				t.Fatal("invalid report accepted")
			}
			saved, loadErr := controller.launcher.store.Load(t.Context(), claim.JobID)
			if loadErr != nil || saved.Phase != workerstore.Claimed || saved.Proposal != nil {
				t.Fatal("rejected report changed journal")
			}
		})
	}
}

func TestWorkerReportJoinsControllerShutdown(t *testing.T) {
	controller, claim, peer, stop := authorizationFixture(t)
	current, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: current.Receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}
	entered := make(chan struct{})
	controller.checkWorker = func(ctx context.Context, _ LaunchProfile, _ id.ID, _ uint32) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, reportErr := controller.proposeWorkerResult(t.Context(), peer, claim, result)
		done <- reportErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("report did not reach authority check")
	}
	stop()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("report outlived controller shutdown", err)
	}
}
