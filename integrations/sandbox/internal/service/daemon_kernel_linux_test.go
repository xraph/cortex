//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

func TestDaemonKernelPrivateEndpointLifecycle(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("daemon transport fixture requires root")
	}
	fixture, job, caller := daemonFixture(t)
	cfg := fixture.cfg
	cfg.Broker.Runtime = nil
	control, err := NewWorkerControl(cfg.Controller, ControlLimits{Connections: 2, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Control = control
	daemon, err := NewDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, func(lifetime context.Context, instance *broker.Broker) error {
			if _, submitErr := instance.Submit(caller, job); submitErr != nil {
				return submitErr
			}
			close(started)
			<-lifetime.Done()
			return lifetime.Err()
		})
	}()
	select {
	case <-started:
	case runErr := <-done:
		t.Fatal("daemon startup failed", runErr)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon startup stalled")
	}
	if err = AuthorizeAt(t.Context(), cfg.Socket, controlClaim()); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("ambient root gained worker authority", err)
	}
	cancel()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Lstat(cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private socket retained", err)
	}
	endpoint, err := openControlEndpoint(t.Context(), cfg.Socket, 0)
	if err != nil {
		t.Fatal("daemon retained endpoint ownership", err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
}
