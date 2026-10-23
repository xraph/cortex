//go:build linux && sandbox_mount_kernel

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
)

func exerciseRetainedMount(t *testing.T) {
	t.Helper()
	root, key := retainedFixture(t)
	directory := filepath.Join(root, key.String())
	parent, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	mounted, err := mountTmpfs(t.Context(), int(parent.Fd()), "stage", mountPlan{bytes: 64 << 10, inodes: 64, mode: 0o700})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewRecovery(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	if err = recovery.Reclaim(t.Context(), key); err == nil {
		t.Fatal("mounted workspace reclaimed")
	}
	if _, err = os.Stat(filepath.Join(directory, "scratch")); err != nil {
		t.Fatal("mount preflight partially removed storage", err)
	}
	if err = mounted.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Reclaim(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	root, key = retainedFixture(t)
	directory = filepath.Join(root, key.String())
	recovery, err = NewRecovery(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Mount(directory, directory, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(directory, unix.UMOUNT_NOFOLLOW)
	if err = recovery.Reclaim(t.Context(), key); err == nil {
		t.Fatal("same-filesystem bind mount reclaimed")
	}
	if _, err = recovery.Discover(t.Context(), 10); err == nil {
		t.Fatal("mounted job identity accepted")
	}
	if err = unix.Unmount(directory, unix.UMOUNT_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Reclaim(t.Context(), key); err != nil {
		t.Fatal(err)
	}
}
