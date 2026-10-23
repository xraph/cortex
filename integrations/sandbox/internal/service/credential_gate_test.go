package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func TestCredentialGateJoinsWriterBeforeCleanupAndRejectsDelayedWriter(t *testing.T) {
	launcher, receipt, _ := launcherFixture(t)
	claim, err := launcher.store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := newCredentialGate()
	writing, release := make(chan struct{}), make(chan struct{})
	prepared := gate.prepare(launcher.store, func(ctx context.Context, _ LaunchProfile, _ workerstore.Claim) error {
		close(writing)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	wrote := make(chan error, 1)
	go func() { wrote <- prepared(t.Context(), launcher.profile, claim) }()
	<-writing
	cleaned := make(chan struct{})
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- gate.cleanup(func(ctx context.Context, key id.ID) error {
			_, cancelErr := launcher.store.Cancel(ctx, key)
			close(cleaned)
			return cancelErr
		})(t.Context(), receipt.Job.ID)
	}()
	select {
	case <-cleaned:
		t.Fatal("cleanup overlapped credential writer")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err = <-wrote; err != nil {
		t.Fatal(err)
	}
	if err = <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	called := false
	delayed := gate.prepare(launcher.store, func(context.Context, LaunchProfile, workerstore.Claim) error { called = true; return nil })
	if err = delayed(t.Context(), launcher.profile, claim); err == nil || called {
		t.Fatal("delayed writer recreated revoked credential", err)
	}
}

func TestCredentialGateHonorsCancellationAndReleasesAfterFailure(t *testing.T) {
	gate := newCredentialGate()
	key := id.New(sandbox.PrefixJob)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	sentinel := errors.New("cleanup failed")
	cleanup := gate.cleanup(func(ctx context.Context, _ id.ID) error {
		close(entered)
		<-release
		return errors.Join(sentinel, ctx.Err())
	})
	go func() { done <- cleanup(t.Context(), key) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	next := gate.cleanup(func(context.Context, id.ID) error { called = true; return nil })
	if err := next(ctx, key); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancelled waiter entered", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := next(t.Context(), key); err != nil || !called {
		t.Fatal("failed cleanup stranded gate", err)
	}
}
