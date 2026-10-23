//go:build linux && sandbox_mount_kernel

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/retention"
	"github.com/xraph/cortex/sandbox"
)

func exerciseRetainedMount(t *testing.T) {
	t.Helper()
	root, key := retainedMountFixture(t)
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
	recovery, err := retention.NewWorkspaces(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
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
	root, key = retainedMountFixture(t)
	directory = filepath.Join(root, key.String())
	recovery, err = retention.NewWorkspaces(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
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

func retainedMountFixture(t *testing.T) (string, id.ID) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	key := id.New(sandbox.PrefixJob)
	if err := os.Mkdir(filepath.Join(root, key.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stage", "scratch"} {
		if err := os.Mkdir(filepath.Join(root, key.String(), name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, key
}

func exerciseRetainedCredentialMount(t *testing.T) {
	t.Helper()
	root, key := retainedMountFixture(t)
	directory := filepath.Join(root, key.String())
	for _, name := range []string{"stage", "scratch"} {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	name := filepath.Join(directory, "claim.json")
	if err := os.WriteFile(name, []byte("credential"), 0o400); err != nil {
		t.Fatal(err)
	}
	recovery, err := retention.NewCredentials(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Mount(name, name, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(name, unix.UMOUNT_NOFOLLOW)
	if err = recovery.Reclaim(t.Context(), key); err == nil {
		t.Fatal("mounted credential deleted")
	}
	if err = unix.Unmount(name, unix.UMOUNT_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Reclaim(t.Context(), key); err != nil {
		t.Fatal(err)
	}
}
