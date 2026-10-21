package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func controllerFixture(t *testing.T) (*Controller, sandbox.Receipt, *atomic.Int32) {
	t.Helper()
	launcher, _, starts := launcherFixture(t)
	_, receipt, cfg := recoveryStore(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Database = filepath.Join(directory, "controller.db")
	store, err := workerstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher.store = store
	scanner, err := NewRecoveryScanner(RecoveryConfig{Store: store, Reaper: launcher.reaper, PageSize: 2, Revalidate: func(context.Context, sandbox.Job) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(launcher, scanner, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return controller, receipt, starts
}

func runController(t *testing.T, c *Controller) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func awaitControllerReady(t *testing.T, c *Controller) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !c.Status().Ready {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("controller not ready: %+v", c.Status())
		}
	}
}

func awaitControllerStop(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not join shutdown")
		return nil
	}
}

func TestControllerGatesLaunchAndRecoversOnShutdown(t *testing.T) {
	c, receipt, starts := controllerFixture(t)
	if err := c.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 0 {
		t.Fatalf("launch before recovery: %v", err)
	}
	cancel, done := runController(t, c)
	awaitControllerReady(t, c)
	if err := c.Launch(t.Context(), receipt); err != nil || starts.Load() != 1 {
		t.Fatalf("ready launch: %v", err)
	}
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	status := c.Status()
	if status.Ready || status.Running || !errors.Is(status.Err, context.Canceled) {
		t.Fatalf("stopped admission open: %+v", status)
	}
	record, err := c.launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateFailed {
		t.Fatalf("shutdown lost claim: %+v %v", record, err)
	}
	if err = c.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 1 {
		t.Fatalf("stopped controller relaunched: %v", err)
	}
	if err = c.Run(t.Context()); !errors.Is(err, workerstore.ErrConflict) {
		t.Fatalf("controller reused lifecycle: %v", err)
	}
}

func TestControllerNeverAdmitsAfterFailedStartupRecovery(t *testing.T) {
	c, receipt, starts := controllerFixture(t)
	if _, err := c.launcher.store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := c.launcher.store.Claim(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	c.launcher.reaper.manager.(*fakeManager).fail = "stop"
	_, done := runController(t, c)
	if err := awaitControllerStop(t, done); err == nil {
		t.Fatal("failed startup reported success")
	}
	if c.Status().Ready || c.Status().Running {
		t.Fatalf("failed startup opened admission: %+v", c.Status())
	}
	if err := c.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 0 {
		t.Fatal("failed recovery admitted launch")
	}
}

func TestControllerWaitsForStartupFenceBeforeOpeningAdmission(t *testing.T) {
	c, receipt, starts := controllerFixture(t)
	if _, err := c.launcher.store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := c.launcher.store.Claim(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	c.launcher.reaper.check = func(ctx context.Context, _ id.ID) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cancel, done := runController(t, c)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("startup did not reach service fence")
	}
	if !c.Status().Running || c.Status().Ready {
		t.Fatalf("startup opened admission before cleanup: %+v", c.Status())
	}
	if err := c.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 0 {
		t.Fatal("startup permitted service dispatch")
	}
	close(release)
	awaitControllerReady(t, c)
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestControllerCancelsAndJoinsInFlightLaunchBeforeShutdownRecovery(t *testing.T) {
	c, receipt, starts := controllerFixture(t)
	entered := make(chan struct{})
	c.launcher.prepare = func(ctx context.Context, _ LaunchProfile, _ workerstore.Claim) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	cancel, done := runController(t, c)
	awaitControllerReady(t, c)
	launched := make(chan error, 1)
	go func() { launched <- c.Launch(t.Context(), receipt) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("launch did not reach credential publication")
	}
	cancel()
	if err := awaitControllerStop(t, launched); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if starts.Load() != 0 || c.Status().Ready {
		t.Fatal("shutdown raced a new service start")
	}
	record, err := c.launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateCancelled {
		t.Fatalf("cancelled launch not joined: %+v %v", record, err)
	}
}

func TestControllerScanFailureClosesAdmissionAndRemainsVisible(t *testing.T) {
	c, receipt, starts := controllerFixture(t)
	var fail atomic.Bool
	outage := errors.New("grant service unavailable")
	c.scanner.revalidate = func(context.Context, sandbox.Job) error {
		if fail.Load() {
			return outage
		}
		return nil
	}
	_, done := runController(t, c)
	awaitControllerReady(t, c)
	if err := c.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := awaitControllerStop(t, done); !errors.Is(err, outage) {
		t.Fatalf("scan failure hidden: %v", err)
	}
	if !errors.Is(c.Status().Err, outage) || c.Status().Ready {
		t.Fatalf("unhealthy controller admitted work: %+v", c.Status())
	}
	if err := c.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 1 {
		t.Fatal("scan failure permitted relaunch")
	}
}

func TestControllerRetainsUnresolvedShutdownAndValidatesOwnership(t *testing.T) {
	c, receipt, _ := controllerFixture(t)
	c.launcher.reaper.manager.(*fakeManager).fail = "stop"
	for _, interval := range []time.Duration{0, time.Millisecond, 2 * time.Second} {
		if _, err := NewController(c.launcher, c.scanner, interval); err == nil {
			t.Fatal("unbounded sweep interval accepted")
		}
	}
	other, _, _ := controllerFixture(t)
	if _, err := NewController(c.launcher, other.scanner, time.Second); err == nil {
		t.Fatal("foreign journal admitted")
	}
	cancel, done := runController(t, c)
	awaitControllerReady(t, c)
	if err := c.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) || err.Error() == context.Canceled.Error() {
		t.Fatalf("cleanup failure hidden: %v", err)
	}
	record, err := c.launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Reaping {
		t.Fatalf("failed cleanup became terminal: %+v %v", record, err)
	}
}
