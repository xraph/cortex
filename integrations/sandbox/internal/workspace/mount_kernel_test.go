//go:build linux && sandbox_mount_kernel

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// This fixture owns a fresh child mount namespace. It does not qualify systemd,
// worker containment, runsc or the eventual workspace lifecycle.
func TestBoundedTmpfsKernel(t *testing.T) {
	if os.Getenv("CORTEX_MOUNT_CHILD") == "1" {
		runMountChild(t, os.Getenv("CORTEX_MOUNT_ROOT"))
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("mount fixture requires root in a disposable Linux container")
	}
	if _, namespaceErr := privateNamespace(); namespaceErr == nil {
		t.Fatal("ambient manager namespace accepted")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestBoundedTmpfsKernel$", "-test.v")
	cmd.Env = []string{"CORTEX_MOUNT_CHILD=1", "CORTEX_MOUNT_ROOT=" + root}
	cmd.SysProcAttr = &unix.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mount child: %v\n%s", err, output)
	}
	t.Log(string(output))
	for _, name := range []string{"stage", "scratch"} {
		entries, readErr := os.ReadDir(filepath.Join(root, name))
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("child mount propagated or survived exit: %s %v", name, readErr)
		}
	}
}

func runMountChild(t *testing.T, root string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	exerciseWorkspace(t, root)
	exerciseRetainedMount(t)
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"stage", "scratch"} {
		if err = os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plan := mountPlan{bytes: 64 << 10, inodes: 64, mode: 0o1777, noexec: true}
	scratch, err := mountTmpfs(t.Context(), int(parent.Fd()), "scratch", plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = scratch.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if other, mountErr := mountTmpfs(t.Context(), int(parent.Fd()), "scratch", plan); mountErr == nil || other != nil {
		t.Fatal("existing mount was covered")
	}
	file, err := os.Create(filepath.Join(root, "scratch", "large"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write(make([]byte, 128<<10))
	if !errors.Is(err, unix.ENOSPC) {
		t.Fatal("byte quota not enforced", err)
	}
	file.Close()
	os.Remove(filepath.Join(root, "scratch", "large"))
	exhausted := false
	for i := range 100 {
		f, createErr := os.Create(filepath.Join(root, "scratch", fmt.Sprintf("inode-%d", i)))
		if errors.Is(createErr, unix.ENOSPC) {
			exhausted = true
			break
		}
		if createErr != nil {
			t.Fatal(createErr)
		}
		f.Close()
	}
	if !exhausted {
		t.Fatal("inode quota not enforced")
	}
	held, err := os.Open(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if err = scratch.close(t.Context()); !errors.Is(err, unix.EBUSY) {
		t.Fatal("busy mount was removed", err)
	}
	held.Close()
	if err = scratch.close(t.Context()); err != nil {
		t.Fatal("cleanup retry failed", err)
	}
	if err = scratch.close(t.Context()); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if mounted, cancelErr := mountTmpfs(cancelled, int(parent.Fd()), "scratch", plan); cancelErr == nil || mounted != nil {
		t.Fatal("cancelled operation mounted storage")
	}
	first, err := mountTmpfs(t.Context(), int(parent.Fd()), "scratch", plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Unmount(filepath.Join(root, "scratch"), unix.UMOUNT_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	replacement, err := mountTmpfs(t.Context(), int(parent.Fd()), "scratch", plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.check(t.Context()); err == nil {
		t.Fatal("replacement mount accepted")
	}
	if err = first.close(t.Context()); err == nil {
		t.Fatal("replacement mount removed")
	}
	if err = replacement.check(t.Context()); err != nil {
		t.Fatal("replacement was affected", err)
	}
	if err = replacement.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	first.parent.Close()
	stage, err := mountTmpfs(t.Context(), int(parent.Fd()), "stage", mountPlan{bytes: 1 << 20, inodes: 128, mode: 0o700})
	if err != nil {
		t.Fatal(err)
	}
	if err = stage.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "stage", "hidden"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately leave a mount behind. Child exit must drop it without exposing
	// the contents or changing the parent's namespace.
}
