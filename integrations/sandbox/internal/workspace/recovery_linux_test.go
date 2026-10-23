//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func retainedFixture(t *testing.T) (string, id.ID) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	key := id.New(sandbox.PrefixJob)
	base := filepath.Join(root, key.String())
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stage", "scratch"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, key
}

func recoveryFixture(t *testing.T, root string, fence func(context.Context, id.ID) error) *Recovery {
	t.Helper()
	recovery, err := newRecovery(root, fence, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	return recovery
}

func TestRetainedWorkspaceReclaimsOnlyAfterFenceAndSupportsRetry(t *testing.T) {
	root, key := retainedFixture(t)
	failure := errors.New("service still live")
	calls := 0
	recovery := recoveryFixture(t, root, func(ctx context.Context, got id.ID) error {
		calls++
		if got != key {
			t.Fatal("wrong service fenced")
		}
		return errors.Join(failure, ctx.Err())
	})
	keys, err := recovery.Discover(t.Context(), 3)
	if err != nil || len(keys) != 1 || keys[0] != key || calls != 0 {
		t.Fatal(keys, err, calls)
	}
	if err = recovery.Reclaim(t.Context(), key); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, key.String(), "stage")); err != nil {
		t.Fatal("failed fence deleted storage", err)
	}
	failure = nil
	if err = recovery.Reclaim(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, key.String())); !os.IsNotExist(err) {
		t.Fatal("workspace retained", err)
	}
	if err = recovery.Reclaim(t.Context(), key); err != nil || calls != 3 {
		t.Fatal("retry failed", calls, err)
	}
}

func TestRetainedWorkspaceRefusesUnexpectedContentWithoutDeletion(t *testing.T) {
	for _, mode := range []string{"file", "directory", "symlink", "nonempty", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			root, key := retainedFixture(t)
			base := filepath.Join(root, key.String())
			recovery := recoveryFixture(t, root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
			var err error
			switch mode {
			case "file":
				err = os.WriteFile(filepath.Join(base, "unexpected"), []byte("keep"), 0o600)
			case "directory":
				err = os.Mkdir(filepath.Join(base, "unexpected"), 0o700)
			case "symlink":
				if err = os.Remove(filepath.Join(base, "scratch")); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(t.TempDir(), filepath.Join(base, "scratch"))
			case "nonempty":
				err = os.WriteFile(filepath.Join(base, "scratch", "keep"), []byte("keep"), 0o600)
			case "permissions":
				err = os.Chmod(filepath.Join(base, "scratch"), 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = recovery.Reclaim(t.Context(), key); err == nil {
				t.Fatal("unsafe workspace reclaimed")
			}
			if _, err = os.Stat(filepath.Join(base, "stage")); err != nil {
				t.Fatal("preflight partially deleted workspace", err)
			}
		})
	}
}

func TestRetainedWorkspaceRejectsRootReplacementAndInvalidAuthority(t *testing.T) {
	root, key := retainedFixture(t)
	calls := 0
	recovery := recoveryFixture(t, root, func(ctx context.Context, _ id.ID) error { calls++; return ctx.Err() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := recovery.Reclaim(ctx, key); err == nil {
		t.Fatal("cancelled cleanup accepted")
	}
	if err := recovery.Reclaim(t.Context(), id.New(id.PrefixAgent)); err == nil || calls != 0 {
		t.Fatal("invalid cleanup fenced service", calls, err)
	}
	if _, err := newRecovery(root, nil, uint32(os.Geteuid())); err == nil {
		t.Fatal("missing fence accepted")
	}
	previous := root + "-retained"
	if err := os.Rename(root, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(previous) })
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Discover(t.Context(), 3); err == nil {
		t.Fatal("replaced root accepted")
	}
	if err := recovery.Reclaim(t.Context(), key); err == nil {
		t.Fatal("replaced root claimed as cleaned")
	}
}

func TestRetainedWorkspaceDiscoveryRejectsAmbiguityAndBounds(t *testing.T) {
	for _, mode := range []string{"limit", "oversized", "foreign", "symlink", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := retainedFixture(t)
			recovery := recoveryFixture(t, root, func(context.Context, id.ID) error { t.Fatal("discovery fenced service"); return nil })
			limit := 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "limit":
				limit = 0
			case "oversized":
				if err := os.Mkdir(filepath.Join(root, id.New(sandbox.PrefixJob).String()), 0o700); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				limit = 3
				if err := os.Mkdir(filepath.Join(root, "foreign"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				limit = 3
				if err := os.Symlink(t.TempDir(), filepath.Join(root, id.New(sandbox.PrefixJob).String())); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			if keys, err := recovery.Discover(ctx, limit); err == nil || len(keys) != 0 {
				t.Fatal("ambiguous discovery accepted", keys, err)
			}
		})
	}
}

func TestRetainedWorkspaceAcceptsPartialSetup(t *testing.T) {
	for _, names := range [][]string{nil, {"scratch"}, {"stage"}} {
		root, key := retainedFixture(t)
		for _, name := range []string{"stage", "scratch"} {
			if err := os.Remove(filepath.Join(root, key.String(), name)); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range names {
			if err := os.Mkdir(filepath.Join(root, key.String(), name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		recovery := recoveryFixture(t, root, func(ctx context.Context, _ id.ID) error { return ctx.Err() })
		if err := recovery.Reclaim(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedWorkspaceRechecksRootAfterFence(t *testing.T) {
	root, key := retainedFixture(t)
	retained := root + "-retained"
	recovery := recoveryFixture(t, root, func(context.Context, id.ID) error {
		if err := os.Rename(root, retained); err != nil {
			return err
		}
		return os.Mkdir(root, 0o700)
	})
	t.Cleanup(func() { os.RemoveAll(retained) })
	if err := recovery.Reclaim(t.Context(), key); err == nil {
		t.Fatal("fence-time root replacement accepted")
	}
	if _, err := os.Stat(filepath.Join(retained, key.String(), "stage")); err != nil {
		t.Fatal("original root was modified", err)
	}
}
