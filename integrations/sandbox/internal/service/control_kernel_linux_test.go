//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestControlKernelWorkerBootstrapUsesPrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker bootstrap transport fixture requires root")
	}
	name, directory, credential, boundary := workerBootstrapFixture(t)
	listener, socket := controlListener(t)
	loaded, err := readWorkerConfig(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Settings.ControlSocket = socket
	writeWorkerConfig(t, name, loaded.Settings)
	loaded, err = readWorkerConfig(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	credential.ProfileDigest = loaded.ProfileDigest
	writeBootstrapCredential(t, directory, credential)
	var authorizations atomic.Int32
	server := &WorkerControl{limits: ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second}, authorize: func(_ context.Context, _ *net.UnixConn, claim WorkerClaim) error {
		if claim.JobID != credential.Record.Receipt.Job.ID || claim.Token != credential.Token || claim.ProfileDigest != loaded.ProfileDigest {
			return broker.ErrDenied
		}
		if authorizations.Add(1) == 1 {
			return errWorkerActivating
		}
		return nil
	}, beginReport: func(_ context.Context, _ *net.UnixConn, claim WorkerClaim) (reportSession, error) {
		return &fixtureReportSession{claim: claim}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener) }()
	// Keep the service-membership seam explicit: this checks real daemon socket
	// authentication and the startup/report exchange, not a live systemd worker.
	boundary.authorize, boundary.report = AuthorizeAt, ReportAt
	worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil || authorizations.Load() != 2 {
		t.Fatal("authenticated activation handshake failed", err)
	}
	result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited}
	if _, err = worker.Report(t.Context(), result); err != nil {
		t.Fatal("bootstrapped worker report failed", err)
	}
	if ambient, ambientErr := BootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID); ambientErr == nil || ambient != nil {
		t.Fatal("ambient root process bootstrapped outside its worker service")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlKernelAuthenticatesPrivateDaemonSocket(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error { return nil }, ControlLimits{Connections: 1, Timeout: time.Second})
	if err := AuthorizeAt(t.Context(), path, controlClaim()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeAt(t.Context(), path, controlClaim()); err == nil {
		t.Fatal("public endpoint accepted")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlKernelPublicServerRejectsAmbientRootAndJoinsController(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	controller, _, _ := controllerFixture(t)
	cancelController, controllerDone := runController(t, controller)
	awaitControllerReady(t, controller)
	server, err := NewWorkerControl(controller, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, path := controlListener(t)
	served := make(chan error, 1)
	go func() { served <- server.Serve(t.Context(), listener) }()
	if err = AuthorizeAt(t.Context(), path, controlClaim()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("ambient root result: %v", err)
	}
	if _, err = ReportAt(t.Context(), path, controlClaim(), testWireResult()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("ambient root report: %v", err)
	}
	cancelController()
	if err = awaitControllerStop(t, controllerDone); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = awaitControllerStop(t, served); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("server removed host-owned socket path")
	}
}

func TestControlKernelReportClientAuthenticatesPrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	claim := controlClaim()
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		return &fixtureReportSession{claim: claim}, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	if receipt, err := ReportAt(t.Context(), path, claim, testWireResult()); err != nil || receipt.JobID != claim.JobID {
		t.Fatal("private report client failed", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportAt(t.Context(), path, claim, testWireResult()); err == nil {
		t.Fatal("report sent through unsafe endpoint")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}
