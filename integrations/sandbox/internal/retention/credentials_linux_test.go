//go:build linux

package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
)

func credentialFixture(t *testing.T, name string) (*Recovery, string, id.ID) {
	t.Helper()
	root, key := retainedFixture(t)
	directory := filepath.Join(root, key.String())
	for _, child := range []string{"stage", "scratch"} {
		if err := os.Remove(filepath.Join(directory, child)); err != nil {
			t.Fatal(err)
		}
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	recovery, err := newRecovery(root, func(ctx context.Context, _ id.ID) error { return ctx.Err() }, uint32(os.Geteuid()), credentialLayout)
	if err != nil {
		t.Fatal(err)
	}
	return recovery, directory, key
}

func TestRetainedCredentialCleanupAcceptsPartialWritesWithoutReadingSecrets(t *testing.T) {
	for _, name := range []string{"", "claim.tmp", "claim.json"} {
		t.Run(name, func(t *testing.T) {
			recovery, directory, key := credentialFixture(t, name)
			if name == "claim.json" {
				if err := os.Chmod(filepath.Join(directory, name), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, name), []byte("invalid or expired secret"), 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(directory, name), 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if err := recovery.Reclaim(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(directory); !os.IsNotExist(err) {
				t.Fatal("credential directory retained", err)
			}
			if err := recovery.Reclaim(t.Context(), key); err != nil {
				t.Fatal("cleanup retry failed", err)
			}
		})
	}
}

func TestRetainedCredentialCleanupRejectsUnsafeFilesBeforeDeletion(t *testing.T) {
	for _, mode := range []string{"permissions", "hardlink", "symlink", "fifo", "directory", "oversized", "unexpected", "both"} {
		t.Run(mode, func(t *testing.T) {
			recovery, directory, key := credentialFixture(t, "claim.json")
			name := filepath.Join(directory, "claim.json")
			var err error
			switch mode {
			case "permissions":
				err = os.Chmod(name, 0o600)
			case "hardlink":
				err = os.Link(name, filepath.Join(t.TempDir(), "keep"))
			case "symlink", "fifo", "directory":
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "symlink":
					err = os.Symlink(t.TempDir(), name)
				case "fifo":
					err = unix.Mkfifo(name, 0o400)
				default:
					err = os.Mkdir(name, 0o400)
				}
			case "oversized":
				if err = os.Chmod(name, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.Truncate(name, (1<<20)+1); err != nil {
					t.Fatal(err)
				}
				err = os.Chmod(name, 0o400)
			case "unexpected":
				err = os.WriteFile(filepath.Join(directory, "other"), nil, 0o400)
			case "both":
				err = os.WriteFile(filepath.Join(directory, "claim.tmp"), nil, 0o400)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = recovery.Reclaim(t.Context(), key); err == nil {
				t.Fatal("unsafe credential deleted")
			}
			if _, err = os.Lstat(name); err != nil {
				t.Fatal("preflight removed credential", err)
			}
		})
	}
}

func TestRetainedCredentialCleanupRequiresFence(t *testing.T) {
	recovery, directory, key := credentialFixture(t, "claim.tmp")
	failure := errors.New("service unresolved")
	recovery.fence = func(context.Context, id.ID) error { return failure }
	if err := recovery.Reclaim(t.Context(), key); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "claim.tmp")); err != nil {
		t.Fatal("failed fence removed temporary claim", err)
	}
}
