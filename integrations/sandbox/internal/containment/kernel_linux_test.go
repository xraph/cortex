package containment

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func TestKernelRejectsOrdinaryFilesystemAndControlSymlinks(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err = checkFilesystem(directory); err == nil {
		t.Fatal("ordinary filesystem accepted as cgroup-v2")
	}
	if err = os.WriteFile(filepath.Join(root, "target"), []byte("populated 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("target", filepath.Join(root, "cgroup.events")); err != nil {
		t.Fatal(err)
	}
	if file, openErr := controlFile(directory, "cgroup.events", unix.O_RDONLY); openErr == nil {
		file.Close()
		t.Fatal("control symlink accepted")
	}
}

func TestKernelRequiresTheActualJobService(t *testing.T) {
	if err := VerifyCurrent(t.Context(), id.New(sandbox.PrefixJob)); err == nil {
		t.Fatal("unrelated test process admitted as job service")
	}
}
