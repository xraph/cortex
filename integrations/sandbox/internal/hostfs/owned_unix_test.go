//go:build linux || darwin

package hostfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadOwnedBindsModeOwnershipAndBoundedBytes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "worker.json")
	data := []byte(`{"schema_version":1}`)
	if err = os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	owner := uint32(os.Geteuid())
	body, err := ReadOwned(t.Context(), name, owner, 0o600, int64(len(data)))
	if err != nil || !bytes.Equal(body, data) {
		t.Fatal("owned configuration read failed", err)
	}
	for _, limit := range []int64{-1, 0, int64(len(data) - 1), (1 << 20) + 1} {
		if body, err = ReadOwned(t.Context(), name, owner, 0o600, limit); err == nil || body != nil {
			t.Fatal("invalid bound returned content")
		}
	}
	if body, err = ReadOwned(t.Context(), name, owner+1, 0o600, 1<<20); err == nil || body != nil {
		t.Fatal("foreign owner returned content")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if body, err = ReadOwned(ctx, name, owner, 0o600, 1<<20); !errors.Is(err, context.Canceled) || body != nil {
		t.Fatal("cancelled read returned content", err)
	}
}

func TestReadOwnedRefusesAliasesSpecialFilesAndPermissions(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "fifo", "directory", "permissions", "empty", "parent alias"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(root, "config")
			if err = os.WriteFile(name, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "symlink":
				err = os.Symlink(name, name+"-alias")
				name += "-alias"
			case "hardlink":
				err = os.Link(name, name+"-alias")
			case "fifo":
				name += "-fifo"
				err = unix.Mkfifo(name, 0o600)
			case "directory":
				name = root
			case "permissions":
				err = os.Chmod(name, 0o644)
			case "empty":
				err = os.Truncate(name, 0)
			case "parent alias":
				err = os.Symlink(root, filepath.Join(root, "alias"))
				name = filepath.Join(root, "alias", "config")
			}
			if err != nil {
				t.Fatal(err)
			}
			if body, readErr := ReadOwned(t.Context(), name, uint32(os.Geteuid()), 0o600, 1<<20); readErr == nil || body != nil {
				t.Fatal("unsafe configuration returned content")
			}
		})
	}
}
