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
			body, readErr := ReadPin(ctx, pin, uint32(os.Geteuid()))
			if scenario == "valid" {
				if readErr != nil || string(body) != "trusted" {
					t.Fatal("pinned snapshot mismatch", readErr)
				}
			} else if readErr == nil || body != nil {
				t.Fatal("unsafe pin returned a snapshot", readErr)
			}
		})
	}
}

func TestReadPinBoundsAllocation(t *testing.T) {
	for _, size := range []int64{-1, 0, (64 << 20) + 1, 1 << 62} {
		body, err := ReadPin(t.Context(), Pin{Bytes: size}, uint32(os.Geteuid()))
		if err == nil || body != nil {
			t.Fatal("invalid snapshot size accepted", size)
		}
	}
}

func TestFilePinAuthenticatesEmptyContent(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pin := Pin{Path: filepath.Join(root, "empty"), SHA256: evidence.Digest(nil), Mode: 0o400}
	if err = os.WriteFile(pin.Path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err = VerifyPin(t.Context(), pin, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	body, err := ReadPin(t.Context(), pin, uint32(os.Geteuid()))
	if err != nil || len(body) != 0 {
		t.Fatal("empty snapshot", err)
	}
	pin.SHA256 = evidence.Digest([]byte("other"))
	if body, err = ReadPin(t.Context(), pin, uint32(os.Geteuid())); err == nil || body != nil {
		t.Fatal("empty digest bypass", err)
	}
}
