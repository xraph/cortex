//go:build linux || darwin

package deployment_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/deployment"
)

func installedFixture(t *testing.T) (string, deployment.Manifest) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(directory, "gvisor-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := fixtureManifest()
	m.Architecture = runtime.GOARCH
	for name, body := range map[string]string{"runsc": "runtime", "gvisor-bin/sentry": "sidecar"} {
		if err = os.WriteFile(filepath.Join(directory, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return directory, m
}

func TestVerifyDeploymentInventory(t *testing.T) {
	dir, m := installedFixture(t)
	want, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid()))
	if err != nil || got != want {
		t.Fatalf("verify=%q %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = deployment.Verify(ctx, dir, m, uint32(os.Geteuid())); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification: %v", err)
	}
	if _, err = deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid()+1)); err == nil {
		t.Fatal("wrong installation owner accepted")
	}
	if m.Architecture == "amd64" {
		m.Architecture = "arm64"
	} else {
		m.Architecture = "amd64"
	}
	if _, err = deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid())); err == nil {
		t.Fatal("wrong worker architecture accepted")
	}
}

func TestVerifyDeploymentRejectsUnsafeInstallations(t *testing.T) {
	cases := map[string]func(string) error{
		"altered sidecar": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "gvisor-bin/sentry"), []byte("changed"), 0o755)
		},
		"missing sidecar": func(dir string) error { return os.Remove(filepath.Join(dir, "gvisor-bin/sentry")) },
		"extra sidecar": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "gvisor-bin/extra"), []byte("surprise"), 0o755)
		},
		"extra directory":    func(dir string) error { return os.Mkdir(filepath.Join(dir, "gvisor-bin/empty"), 0o755) },
		"writable directory": func(dir string) error { return os.Chmod(filepath.Join(dir, "gvisor-bin"), 0o777) },
		"writable root":      func(dir string) error { return os.Chmod(dir, 0o777) },
		"writable binary":    func(dir string) error { return os.Chmod(filepath.Join(dir, "runsc"), 0o777) },
		"mode differs":       func(dir string) error { return os.Chmod(filepath.Join(dir, "runsc"), 0o555) },
		"hardlink": func(dir string) error {
			return os.Link(filepath.Join(dir, "runsc"), filepath.Join(filepath.Dir(dir), "hardlink"))
		},
		"file symlink": func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "gvisor-bin/sentry")); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(dir, "runsc"), filepath.Join(dir, "gvisor-bin/sentry"))
		},
		"directory symlink": func(dir string) error {
			if err := os.Rename(filepath.Join(dir, "gvisor-bin"), filepath.Join(dir, "moved")); err != nil {
				return err
			}
			return os.Symlink("moved", filepath.Join(dir, "gvisor-bin"))
		},
		"fifo": func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "runsc")); err != nil {
				return err
			}
			return unix.Mkfifo(filepath.Join(dir, "runsc"), 0o755)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, m := installedFixture(t)
			if err := mutate(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid())); err == nil {
				t.Fatal("unsafe installation accepted")
			}
		})
	}
}

func TestVerifyRejectsSymlinkedInstallationAncestor(t *testing.T) {
	dir, m := installedFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(dir), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := deployment.Verify(t.Context(), filepath.Join(alias, filepath.Base(dir)), m, uint32(os.Geteuid())); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
}

func TestVerifyLegacyDeploymentHasNoUnlistedSidecars(t *testing.T) {
	dir, m := installedFixture(t)
	m.Files = m.Files[:1]
	if _, err := deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid())); err == nil {
		t.Fatal("single-binary manifest accepted installed sidecars")
	}
	if err := os.Remove(filepath.Join(dir, "gvisor-bin/sentry")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gvisor-bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := deployment.Verify(t.Context(), dir, m, uint32(os.Geteuid())); err != nil {
		t.Fatalf("legacy single-binary installation rejected: %v", err)
	}
}
