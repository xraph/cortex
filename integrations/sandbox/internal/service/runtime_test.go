package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func runtimeFixture(t *testing.T) (*Runtime, *Controller, sandbox.Receipt) {
	t.Helper()
	c, receipt, _ := controllerFixture(t)
	// State-mapping tests control finalization explicitly, without a scanner race.
	lifetime, cancel := context.WithCancel(t.Context())
	c.lifetime = lifetime
	c.status = ControllerStatus{Running: true, Ready: true}
	t.Cleanup(cancel)
	r, err := NewRuntime(c, func(context.Context) (sandbox.Capabilities, error) { return receipt.Capabilities, nil })
	if err != nil {
		t.Fatal(err)
	}
	return r, c, receipt
}

func TestRuntimeHidesProposalUntilCleanup(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	store := c.launcher.store
	if _, err := store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inspect(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrPending) {
		t.Fatalf("unclaimed inspection: %v", err)
	}
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposed := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, Stdout: []byte("candidate output")}
	if _, err = store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, proposed); err != nil {
		t.Fatal(err)
	}
	result, err := r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateRunning || len(result.Stdout) != 0 || len(result.Artifacts) != 0 {
		t.Fatalf("proposal leaked: %+v %v", result, err)
	}
	if _, err = c.scanner.Sweep(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	result, err = r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateExited || string(result.Stdout) != "candidate output" {
		t.Fatalf("final result: %+v %v", result, err)
	}
	result.Stdout[0] = 'X'
	again, err := r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || string(again.Stdout) != "candidate output" {
		t.Fatal("inspection changed durable output", err)
	}
}

func TestRuntimeCancelRetainsUnresolvedCleanup(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	if err := r.Start(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	manager := c.launcher.reaper.manager.(*fakeManager)
	manager.fail = "stop"
	if err := r.Cancel(t.Context(), receipt.Job.ID); err == nil {
		t.Fatal("failed cleanup acknowledged")
	}
	record, err := c.launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Reaping || !record.CancelRequested {
		t.Fatalf("lost cancellation: %+v %v", record, err)
	}
	result, err := r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateRunning {
		t.Fatalf("premature terminal result: %+v %v", result, err)
	}
	c.launcher.authorize = func(context.Context, sandbox.Job) error { return broker.ErrDenied }
	manager.fail = ""
	if err = r.Cancel(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	result, err = r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("cancel result: %+v %v", result, err)
	}
	calls := len(manager.calls)
	if err = r.Cancel(t.Context(), receipt.Job.ID); err != nil || len(manager.calls) != calls {
		t.Fatal("terminal cancellation repeated cleanup", err)
	}
}

func TestRuntimeCancelMissingIdentityFencesDelayedLaunch(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	manager := c.launcher.reaper.manager.(*fakeManager)
	manager.fail = "stop"
	if err := r.Cancel(t.Context(), receipt.Job.ID); err == nil || errors.Is(err, broker.ErrNotFound) {
		t.Fatalf("uncertain cleanup reported absent: %v", err)
	}
	manager.fail = ""
	if err := r.Cancel(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrNotFound) || !manager.masked || !manager.stopped {
		t.Fatalf("missing identity not fenced: %v", err)
	}
	c.launcher.preflight = func(context.Context, LaunchProfile, id.ID) error {
		if manager.masked {
			return broker.ErrDenied
		}
		return nil
	}
	if err := r.Start(t.Context(), receipt); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("delayed start crossed mask: %v", err)
	}
}

func TestRuntimeUncertainStartNeverExecutesAgain(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	calls := 0
	lost := errors.New("start reply lost")
	c.launcher.start = func(context.Context, id.ID) error { calls++; return lost }
	if err := r.Start(t.Context(), receipt); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), receipt); err != nil || calls != 1 {
		t.Fatalf("uncertain start replayed: %d %v", calls, err)
	}
	result, err := r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateFailed {
		t.Fatalf("uncertain result: %+v %v", result, err)
	}
}

func TestRuntimeRechecksDeploymentBeforeEnrollment(t *testing.T) {
	for _, change := range []string{"digest", "qualification", "profile", "expired", "shortened", "limits", "unqualified", "error", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			_, c, receipt := runtimeFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, err := NewRuntime(c, func(context.Context) (sandbox.Capabilities, error) {
				caps := receipt.Capabilities
				switch change {
				case "digest":
					caps.RuntimeDigest = strings.Repeat("c", 64)
				case "qualification":
					caps.Qualification = "different-worker"
				case "profile":
					caps.Profile = "other"
				case "expired":
					caps.QualifiedUntil = time.Now().Add(-time.Second)
				case "shortened":
					caps.QualifiedUntil = time.Now().Add(time.Second)
				case "limits":
					caps.Limits.Processes--
				case "unqualified":
					caps.Qualification = ""
				case "error":
					return sandbox.Capabilities{}, broker.ErrDenied
				case "cancelled":
					cancel()
				}
				return caps, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Start(ctx, receipt); err == nil {
				t.Fatal("changed deployment admitted")
			}
			if _, err = c.launcher.store.Load(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrNotFound) {
				t.Fatal("rejected receipt enrolled", err)
			}
		})
	}
}

func TestRuntimeOperationsJoinControllerShutdown(t *testing.T) {
	c, receipt, _ := controllerFixture(t)
	entered := make(chan struct{})
	r, err := NewRuntime(c, func(ctx context.Context) (sandbox.Capabilities, error) {
		close(entered)
		<-ctx.Done()
		return receipt.Capabilities, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Capabilities(t.Context()); !errors.Is(err, broker.ErrPending) {
		t.Fatal("unready deployment exposed", err)
	}
	cancel, done := runController(t, c)
	awaitControllerReady(t, c)
	response := make(chan error, 1)
	go func() { _, callErr := r.Capabilities(t.Context()); response <- callErr }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("attestation not called")
	}
	cancel()
	if err = awaitControllerStop(t, response); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled callback returned capabilities", err)
	}
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = r.Start(t.Context(), receipt); !errors.Is(err, broker.ErrPending) {
		t.Fatal(err)
	}
	if _, err = r.Inspect(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrPending) {
		t.Fatal(err)
	}
	if err = r.Cancel(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrPending) {
		t.Fatal(err)
	}
}

func TestRuntimeRequiresControllerAndAttestation(t *testing.T) {
	_, c, receipt := runtimeFixture(t)
	if _, err := NewRuntime(nil, func(context.Context) (sandbox.Capabilities, error) { return receipt.Capabilities, nil }); err == nil {
		t.Fatal("nil controller accepted")
	}
	if _, err := NewRuntime(c, nil); err == nil {
		t.Fatal("missing attestation accepted")
	}
}

func TestRuntimeRejectsForgedReceiptBeforeClaim(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	receipt.Signature[0] ^= 1
	if err := r.Start(t.Context(), receipt); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("forged receipt admitted", err)
	}
	if _, err := c.launcher.store.Load(t.Context(), receipt.Job.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal("forged receipt enrolled", err)
	}
}

func TestRuntimeCancelUnclaimedAndInvalidIdentities(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	if _, err := c.launcher.store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	result, err := r.Inspect(t.Context(), receipt.Job.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatal("unclaimed cancellation lost", err)
	}
	manager := c.launcher.reaper.manager.(*fakeManager)
	if len(manager.calls) != 0 {
		t.Fatal("unclaimed cancellation contacted manager")
	}
	if err = r.Cancel(t.Context(), id.New("agt")); !errors.Is(err, broker.ErrDenied) || len(manager.calls) != 0 {
		t.Fatal("invalid identity reached reaper", err)
	}
}

func TestRuntimeFencesKnownIdentityOnJournalFailure(t *testing.T) {
	r, c, receipt := runtimeFixture(t)
	if err := c.launcher.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel(t.Context(), receipt.Job.ID); err == nil || errors.Is(err, broker.ErrNotFound) {
		t.Fatal("journal failure acknowledged", err)
	}
	manager := c.launcher.reaper.manager.(*fakeManager)
	if !manager.masked || !manager.stopped {
		t.Fatal("journal failure skipped containment fence")
	}
}
