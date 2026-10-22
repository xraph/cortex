package service

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func endpointPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cx-endpoint-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "control.sock")
}

func TestControlEndpointOwnsPrivateSocketAndRestarts(t *testing.T) {
	path := endpointPath(t)
	endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	if err = inspectControlSocket(path, uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if second, openErr := openControlEndpoint(t.Context(), path, uint32(os.Getuid())); openErr == nil || second != nil {
		t.Fatal("second daemon acquired endpoint")
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal("repeat close", err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket retained", err)
	}
	next, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal("normal restart", err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlEndpointPreservesReplacementAndUnknownPaths(t *testing.T) {
	for _, mode := range []string{"replacement", "replacement-socket", "regular", "symlink", "stale"} {
		t.Run(mode, func(t *testing.T) {
			path := endpointPath(t)
			if mode == "replacement" || mode == "replacement-socket" {
				endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mode == "replacement-socket" {
					replacement, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
					if listenErr != nil {
						t.Fatal(listenErr)
					}
					replacement.SetUnlinkOnClose(false)
					defer replacement.Close()
				} else if err = os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err = endpoint.Close(); err == nil {
					t.Fatal("replacement went unreported")
				}
			} else {
				switch mode {
				case "regular":
					if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("missing", path); err != nil {
						t.Fatal(err)
					}
				case "stale":
					listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
					if err != nil {
						t.Fatal(err)
					}
					listener.SetUnlinkOnClose(false)
					listener.Close()
				}
				if endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid())); err == nil || endpoint != nil {
					t.Fatal("existing path adopted or replaced")
				}
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("foreign path removed", err)
			}
		})
	}
}

func TestControlEndpointRejectsUnsafeOwnershipAndCancellation(t *testing.T) {
	for _, mode := range []string{"parent", "lock-mode", "lock-link", "cancel", "relative"} {
		t.Run(mode, func(t *testing.T) {
			path := endpointPath(t)
			ctx := t.Context()
			switch mode {
			case "parent":
				os.Chmod(filepath.Dir(path), 0o750)
			case "lock-mode":
				os.WriteFile(path+".lock", []byte(""), 0o666)
				os.Chmod(path+".lock", 0o666)
			case "lock-link":
				os.Symlink("missing", path+".lock")
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "relative":
				path = "relative.sock"
			}
			if endpoint, err := openControlEndpoint(ctx, path, uint32(os.Getuid())); err == nil || endpoint != nil {
				t.Fatal("unsafe endpoint opened")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refusal created socket", err)
			}
		})
	}
}

func TestControlEndpointClosesLockDescriptor(t *testing.T) {
	path := endpointPath(t)
	endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	fd := int(endpoint.lock.Fd())
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatal("lock descriptor leaked", err)
	}
}
