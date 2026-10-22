//go:build linux || darwin

package journal

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestJournalRejectsAliasesSpecialFilesAndConcurrentOwnership(t *testing.T) {
	for _, test := range []string{"symlink", "hardlink", "fifo", "permissions", "exclusive"} {
		t.Run(test, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "journal.db")
			switch test {
			case "symlink", "hardlink":
				target := filepath.Join(root, "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if test == "symlink" {
					err = os.Symlink(target, path)
				} else {
					err = os.Link(target, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case "exclusive":
				owner, err := Open(t.Context(), path, Broker)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
			}
			if next, err := Open(t.Context(), path, Broker); err == nil {
				_ = next.Close()
				t.Fatal("unsafe journal admitted")
			}
		})
	}
}

func TestJournalApplicationCannotChange(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "journal.db")
	owner, err := Open(t.Context(), path, Worker)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	if next, openErr := Open(t.Context(), path, Broker); openErr == nil {
		_ = next.Close()
		t.Fatal("journal application changed")
	}
}
