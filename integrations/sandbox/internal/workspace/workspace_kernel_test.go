//go:build linux && sandbox_mount_kernel

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func exerciseWorkspace(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "workspaces")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	job := testJob(t)
	ctx := cortex.WithScope(t.Context(), job.Scope)
	calls := 0
	cfg := Config{Root: directory, Policy: testPolicy(), Job: job, ServiceMemory: 512 << 20, Deadline: time.Now().Add(time.Minute), Authorize: func(ctx context.Context, candidate sandbox.Job) error {
		calls++
		if candidate.Digest != job.Digest || cortex.ScopeFromContext(ctx).Canonical() != job.Scope.Canonical() {
			t.Fatal("workspace authorization lost identity")
		}
		candidate.Argv[0] = "changed"
		return ctx.Err()
	}}
	boundary := func(ctx context.Context, key id.ID) error {
		if key != job.ID {
			t.Fatal("containment lost job")
		}
		return ctx.Err()
	}
	denied := cfg
	denied.Authorize = func(context.Context, sandbox.Job) error { return errors.New("revoked") }
	if w, err := create(ctx, denied, boundary); err == nil || w != nil {
		t.Fatal("denied workspace created")
	}
	if _, err := os.Stat(filepath.Join(directory, job.ID.String())); !os.IsNotExist(err) {
		t.Fatal("denial wrote workspace")
	}
	if w, err := Create(ctx, cfg); err == nil || w != nil {
		t.Fatal("ambient container acquired worker workspace")
	}
	if w, err := create(t.Context(), cfg, boundary); err == nil || w != nil {
		t.Fatal("missing scope acquired workspace")
	}
	w, err := create(ctx, cfg, boundary)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := w.Paths()
	if err != nil || calls != 2 {
		t.Fatal("workspace ready without two authority checks", calls, err)
	}
	if err = w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(paths.Bundle); err != nil {
		t.Fatal(err)
	}
	if other, createErr := create(ctx, cfg, boundary); createErr == nil || other != nil {
		t.Fatal("existing workspace reused")
	}
	held, err := os.Open(paths.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(t.Context()); err == nil {
		t.Fatal("busy workspace cleanup reported success")
	}
	if _, err = w.Paths(); err == nil {
		t.Fatal("closing workspace remained usable")
	}
	held.Close()
	stagePath := filepath.Join(paths.Directory, "stage")
	retained := filepath.Join(paths.Directory, "stage-owned")
	if err = os.Rename(stagePath, retained); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(t.Context()); err == nil {
		t.Fatal("replaced mountpoint was cleaned")
	}
	if _, err = os.Stat(stagePath); err != nil {
		t.Fatal("foreign mountpoint was removed", err)
	}
	if err = os.Remove(stagePath); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(retained, stagePath); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(paths.Directory); !os.IsNotExist(err) {
		t.Fatal("owned empty workspace retained", err)
	}
	if err = w.Close(t.Context()); err != nil {
		t.Fatal("close not idempotent", err)
	}
	calls = 0
	cfg.Authorize = func(context.Context, sandbox.Job) error {
		calls++
		if calls == 2 {
			return errors.New("revoked during setup")
		}
		return nil
	}
	if w, err = create(ctx, cfg, boundary); err == nil || w != nil {
		t.Fatal("revoked setup retained usable workspace", err)
	}
	if _, err = os.Stat(paths.Directory); !os.IsNotExist(err) {
		t.Fatal("failed setup was not cleaned", err)
	}
	calls = 0
	cancelled, cancel := context.WithCancel(ctx)
	cfg.Authorize = func(context.Context, sandbox.Job) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return nil
	}
	if w, err = create(cancelled, cfg, boundary); err == nil || w != nil {
		t.Fatal("cancelled setup survived cleanup", err)
	}
	if _, err = os.Stat(paths.Directory); !os.IsNotExist(err) {
		t.Fatal("cancelled setup left storage", err)
	}
	calls = 0
	var pinned *os.File
	cfg.Authorize = func(context.Context, sandbox.Job) error {
		calls++
		if calls == 2 {
			pinned, err = os.Open(paths.Scratch)
			if err != nil {
				t.Fatal(err)
			}
			return errors.New("revoked with open file")
		}
		return nil
	}
	w, err = create(ctx, cfg, boundary)
	if err == nil || w == nil {
		t.Fatal("failed cleanup lost its handle", err)
	}
	if _, err = w.Paths(); err == nil {
		t.Fatal("failed setup exposed paths")
	}
	pinned.Close()
	if err = w.Close(t.Context()); err != nil {
		t.Fatal("failed setup cleanup could not retry", err)
	}
	cfg.Authorize = func(ctx context.Context, _ sandbox.Job) error { return ctx.Err() }
	w, err = create(ctx, cfg, boundary)
	if err != nil {
		t.Fatal(err)
	}
	retainedJob := paths.Directory + "-owned"
	if err = os.Rename(paths.Directory, retainedJob); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(paths.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = w.Check(ctx); err == nil {
		t.Fatal("replaced job directory accepted")
	}
	if err = w.Close(t.Context()); err == nil {
		t.Fatal("replaced job directory removed")
	}
	if err = os.Remove(paths.Directory); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(retainedJob, paths.Directory); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err = create(ctx, cfg, boundary)
	if err != nil {
		t.Fatal(err)
	}
	var checks sync.WaitGroup
	for range 4 {
		checks.Go(func() {
			for range 10 {
				_ = w.Check(ctx)
				_, _ = w.Paths()
			}
		})
	}
	if err = w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	checks.Wait()
}
