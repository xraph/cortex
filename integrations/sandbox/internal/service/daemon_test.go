package service

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func daemonFixture(t *testing.T) (*Daemon, sandbox.Job, context.Context) {
	t.Helper()
	instance, config, controller, job, caller := runtimeBrokerFixture(t)
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	attest := config.Runtime.(*Runtime).attest
	config.Runtime = nil
	controller.status = ControllerStatus{}
	controller.lifetime = nil
	control := &WorkerControl{controller: controller, limits: ControlLimits{Connections: 2, Timeout: time.Second}, authorize: func(context.Context, *net.UnixConn, WorkerClaim) error { return broker.ErrDenied }}
	directory, err := os.MkdirTemp("/tmp", "cx-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDaemon(DaemonConfig{Controller: controller, Control: control, Broker: config, Attest: attest, Socket: filepath.Join(directory, "control.sock"), Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	d.openEndpoint = func(ctx context.Context, path string) (*controlEndpoint, error) {
		return openControlEndpoint(ctx, path, uint32(os.Geteuid()))
	}
	d.serveControl = func(ctx context.Context, listener *net.UnixListener, ready func()) error {
		operation, finish, beginErr := controller.begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		defer finish()
		return control.serveWithReady(operation, listener, ready)
	}
	return d, job, caller
}

func TestDaemonRecoversBeforePublicAdmissionAndJoinsShutdown(t *testing.T) {
	d, job, caller := daemonFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serving := make(chan struct{})
	done := make(chan error, 1)
	var publicJoined atomic.Bool
	go func() {
		done <- d.Run(ctx, func(lifetime context.Context, b *broker.Broker) error {
			if !d.cfg.Controller.Status().Ready {
				return errors.New("public admission before recovery")
			}
			d.cfg.Control.mu.Lock()
			started := d.cfg.Control.started
			d.cfg.Control.mu.Unlock()
			if !started {
				return errors.New("public admission before private transport")
			}
			if _, err := b.Submit(caller, job); err != nil {
				return err
			}
			close(serving)
			<-lifetime.Done()
			publicJoined.Store(true)
			return lifetime.Err()
		})
	}()
	select {
	case <-serving:
	case err := <-done:
		t.Fatal("startup failed", err)
	case <-time.After(3 * time.Second):
		t.Fatal("public broker not started")
	}
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !publicJoined.Load() || d.cfg.Controller.Status().Running {
		t.Fatal("daemon did not join children")
	}
	if _, err := os.Lstat(d.cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket retained", err)
	}
	record, err := d.cfg.Controller.launcher.store.Load(t.Context(), job.ID)
	if err != nil || record.Phase != workerstore.Terminal {
		t.Fatal("shutdown did not recover claim", err)
	}
	reopened, err := broker.Open(d.cfg.Broker)
	if err != nil {
		t.Fatal("broker journal ownership retained", err)
	}
	_ = reopened.Close()
	if err = d.Run(t.Context(), func(context.Context, *broker.Broker) error { return nil }); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("daemon lifecycle reused", err)
	}
}

func TestDaemonStartupFailuresNeverExposeBroker(t *testing.T) {
	for _, stage := range []string{"endpoint", "recovery", "transport", "cancelled"} {
		t.Run(stage, func(t *testing.T) {
			d, _, _ := daemonFixture(t)
			failure := errors.New("startup failure")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch stage {
			case "endpoint":
				d.openEndpoint = func(context.Context, string) (*controlEndpoint, error) { return nil, failure }
			case "recovery":
				_ = d.cfg.Controller.launcher.store.Close()
			case "transport":
				d.serveControl = func(context.Context, *net.UnixListener, func()) error { return failure }
			case "cancelled":
				cancel()
			}
			exposed := false
			if err := d.Run(ctx, func(context.Context, *broker.Broker) error { exposed = true; return nil }); err == nil {
				t.Fatal("startup failure hidden")
			}
			if exposed || d.cfg.Controller.Status().Running {
				t.Fatal("failed startup exposed broker or leaked controller")
			}
			if _, err := os.Lstat(d.cfg.Socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed startup retained socket", err)
			}
		})
	}
}

func TestDaemonUnexpectedPublicExitClosesPrivateTransport(t *testing.T) {
	for _, failure := range []error{nil, errors.New("public listener failed")} {
		d, _, _ := daemonFixture(t)
		err := d.Run(t.Context(), func(context.Context, *broker.Broker) error { return failure })
		if err == nil || (failure != nil && !errors.Is(err, failure)) {
			t.Fatal("public exit hidden", err)
		}
		if d.cfg.Controller.Status().Running {
			t.Fatal("controller leaked")
		}
		if _, statErr := os.Lstat(d.cfg.Socket); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("private transport leaked", statErr)
		}
	}
}

func TestDaemonCancellationDuringPrivateStartupJoinsController(t *testing.T) {
	d, _, _ := daemonFixture(t)
	entered := make(chan struct{})
	d.serveControl = func(ctx context.Context, _ *net.UnixListener, _ func()) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx, func(context.Context, *broker.Broker) error { return errors.New("public broker started early") })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("private transport not started")
	}
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d.cfg.Controller.Status().Running {
		t.Fatal("controller leaked")
	}
}

func TestDaemonReconcilesOldClaimBeforePublicServer(t *testing.T) {
	d, job, caller := daemonFixture(t)
	c := d.cfg.Controller
	c.status = ControllerStatus{Running: true, Ready: true}
	c.lifetime = t.Context()
	old, err := broker.Open(d.cfg.Broker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Submit(caller, job); err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	c.status = ControllerStatus{}
	c.lifetime = nil
	observed := false
	stopped := errors.New("test public server finished")
	err = d.Run(t.Context(), func(_ context.Context, instance *broker.Broker) error {
		result, collectErr := instance.Collect(caller, job.ID)
		if collectErr != nil || result.State != sandbox.StateFailed {
			return errors.New("public admission preceded old claim recovery")
		}
		if verifyErr := broker.VerifyResult(result, job, instance.PublicKey()); verifyErr != nil {
			return verifyErr
		}
		observed = true
		return stopped
	})
	if !observed || !errors.Is(err, stopped) {
		t.Fatal("startup reconciliation failed", err)
	}
}

func TestDaemonPreservesShutdownCleanupFailure(t *testing.T) {
	d, job, caller := daemonFixture(t)
	d.cfg.Controller.launcher.reaper.manager.(*fakeManager).fail = "stop"
	original := errors.New("public service failed")
	err := d.Run(t.Context(), func(_ context.Context, instance *broker.Broker) error {
		if _, submitErr := instance.Submit(caller, job); submitErr != nil {
			return submitErr
		}
		return original
	})
	if !errors.Is(err, original) || !strings.Contains(err.Error(), "stop") {
		t.Fatal("cleanup failure lost", err)
	}
	record, loadErr := d.cfg.Controller.launcher.store.Load(t.Context(), job.ID)
	if loadErr != nil || record.Phase != workerstore.Reaping {
		t.Fatal("unresolved cleanup became terminal", loadErr)
	}
}

func TestDaemonPrivateTransportFailureCancelsPublicServer(t *testing.T) {
	d, _, _ := daemonFixture(t)
	publicStarted := make(chan struct{})
	failure := errors.New("private transport failed")
	d.serveControl = func(ctx context.Context, _ *net.UnixListener, ready func()) error {
		ready()
		select {
		case <-publicStarted:
			return failure
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	joined := false
	err := d.Run(t.Context(), func(ctx context.Context, _ *broker.Broker) error {
		close(publicStarted)
		<-ctx.Done()
		joined = true
		return ctx.Err()
	})
	if !errors.Is(err, failure) || !joined || d.cfg.Controller.Status().Running {
		t.Fatal("private failure did not join public service", err)
	}
}

func TestDaemonStartupReconciliationFailureWithholdsPublicServer(t *testing.T) {
	d, _, _ := daemonFixture(t)
	d.cfg.Broker.Now = func() time.Time { return time.Time{} }
	exposed := false
	err := d.Run(t.Context(), func(context.Context, *broker.Broker) error { exposed = true; return nil })
	if err == nil || exposed || d.cfg.Controller.Status().Running {
		t.Fatal("failed reconciliation admitted public service", err)
	}
}

func TestDaemonRequiresMatchingComponentsAndOwnedRuntime(t *testing.T) {
	for _, change := range []string{"controller", "control", "foreign", "runtime", "attest", "socket", "interval", "used-controller", "used-control", "broker-name", "broker-key", "invalid-key", "short-key"} {
		t.Run(change, func(t *testing.T) {
			d, _, _ := daemonFixture(t)
			cfg := d.cfg
			adapter := cfg.Broker.Runtime
			cfg.Broker.Runtime = nil
			switch change {
			case "controller":
				cfg.Controller = nil
			case "control":
				cfg.Control = nil
			case "foreign":
				other, _, _ := controllerFixture(t)
				cfg.Controller = other
			case "runtime":
				cfg.Broker.Runtime = adapter
			case "attest":
				cfg.Attest = nil
			case "socket":
				cfg.Socket = "relative.sock"
			case "interval":
				cfg.Interval = time.Minute
			case "used-controller":
				cfg.Controller.started = true
			case "used-control":
				cfg.Control.started = true
			case "broker-name":
				cfg.Broker.Name = "foreign-broker"
			case "broker-key":
				cfg.Broker.SigningKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			case "invalid-key":
				cfg.Broker.SigningKey = make([]byte, ed25519.PrivateKeySize)
			case "short-key":
				cfg.Broker.SigningKey = nil
			}
			if _, err := NewDaemon(cfg); err == nil {
				t.Fatal("invalid daemon accepted")
			}
		})
	}
}
