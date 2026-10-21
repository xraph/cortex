//go:build linux || darwin

package hostfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
)

func TestFilePinRejectsSubstitutionAndUnsafeMetadata(t *testing.T) {
	for _, scenario := range []string{"valid", "symlink", "hardlink", "fifo", "mode", "length", "digest", "parent alias", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			pin := Pin{Path: filepath.Join(root, "worker"), SHA256: evidence.Digest([]byte("trusted")), Bytes: 7, Mode: 0o600}
			if err = os.WriteFile(pin.Path, []byte("trusted"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "symlink":
				if err = os.Rename(pin.Path, pin.Path+".real"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink("worker.real", pin.Path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err = os.Link(pin.Path, pin.Path+".link"); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err = os.Remove(pin.Path); err != nil {
					t.Fatal(err)
				}
				if err = unix.Mkfifo(pin.Path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err = os.Chmod(pin.Path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "length":
				pin.Bytes++
			case "digest":
				pin.SHA256 = evidence.Digest([]byte("changed"))
			case "parent alias":
				alias := root + "-alias"
				if err = os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(alias)
				pin.Path = filepath.Join(alias, "worker")
			case "cancelled":
				cancel()
			}
			err = VerifyPin(ctx, pin, uint32(os.Geteuid()))
			if (scenario == "valid") != (err == nil) {
				t.Fatalf("pin verification: %v", err)
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
