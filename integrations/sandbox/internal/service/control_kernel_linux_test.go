//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

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
	server, err := NewWorkerControl(controller, ControlLimits{Connections: 2, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, path := controlListener(t)
	served := make(chan error, 1)
	go func() { served <- server.Serve(t.Context(), listener) }()
	if err = AuthorizeAt(t.Context(), path, controlClaim()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("ambient root result: %v", err)
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
