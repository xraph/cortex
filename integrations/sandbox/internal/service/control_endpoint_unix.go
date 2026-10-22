//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

type controlEndpoint struct {
	listener        *net.UnixListener
	directory, lock *os.File
	name            string
	socket          unix.Stat_t
	once            sync.Once
	err             error
}

// ServeAt creates and owns the private worker socket for this daemon lifetime.
// The host must precreate its root-owned 0700 directory and prevent owner mutation.
// A stale socket is reclaimed only with matching durable identity and an explicit
// kernel connection refusal. Shutdown joins handlers before removing its inode.
// The persistent lock file must not be removed while any daemon may be running.
func (s *WorkerControl) ServeAt(ctx context.Context, path string) (err error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errUnsupported
	}
	endpoint, err := openControlEndpoint(ctx, path, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, endpoint.Close()) }()
	return s.Serve(ctx, endpoint.listener)
}

func openControlEndpoint(ctx context.Context, path string, owner uint32) (endpoint *controlEndpoint, err error) {
	if checkErr := ctx.Err(); checkErr != nil {
		return nil, checkErr
	}
	if !profilePath(path) || len(path) > 100 {
		return nil, fmt.Errorf("cortex: invalid worker control endpoint")
	}
	directory, err := hostfs.OpenDirectory(filepath.Dir(path), owner)
	if err != nil {
		return nil, err
	}
	endpoint = &controlEndpoint{directory: directory, name: filepath.Base(path)}
	defer func() {
		if err != nil {
			err = errors.Join(err, endpoint.Close())
			endpoint = nil
		}
	}()
	var parent unix.Stat_t
	if checkErr := unix.Fstat(int(directory.Fd()), &parent); checkErr != nil {
		return endpoint, checkErr
	}
	if parent.Mode&0o7777 != 0o700 {
		return endpoint, fmt.Errorf("cortex: worker control directory is not private")
	}
	fd, err := unix.Openat(int(directory.Fd()), endpoint.name+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return endpoint, err
	}
	endpoint.lock = os.NewFile(uintptr(fd), "worker-control-lock")
	var lock unix.Stat_t
	if checkErr := unix.Fstat(fd, &lock); checkErr != nil {
		return endpoint, checkErr
	}
	if lock.Mode&unix.S_IFMT != unix.S_IFREG || lock.Mode&0o7777 != 0o600 || lock.Uid != owner || lock.Nlink != 1 {
		return endpoint, fmt.Errorf("cortex: unsafe worker control lock")
	}
	if checkErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); checkErr != nil {
		return endpoint, checkErr
	}
	var bound unix.Stat_t
	if checkErr := unix.Fstatat(int(directory.Fd()), endpoint.name+".lock", &bound, unix.AT_SYMLINK_NOFOLLOW); checkErr != nil {
		return endpoint, checkErr
	}
	if !sameControlInode(lock, bound) {
		return endpoint, fmt.Errorf("cortex: worker control lock changed")
	}
	if recoveryErr := endpoint.recoverSocket(ctx, path, owner); recoveryErr != nil {
		return endpoint, recoveryErr
	}
	endpoint.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return endpoint, err
	}
	endpoint.listener.SetUnlinkOnClose(false)
	if checkErr := unix.Fstatat(int(directory.Fd()), endpoint.name, &endpoint.socket, unix.AT_SYMLINK_NOFOLLOW); checkErr != nil {
		return endpoint, checkErr
	}
	if endpoint.socket.Mode&unix.S_IFMT != unix.S_IFSOCK || endpoint.socket.Uid != owner || endpoint.socket.Nlink != 1 {
		endpoint.socket = unix.Stat_t{}
		return endpoint, fmt.Errorf("cortex: worker control socket changed")
	}
	if checkErr := unix.Fchmodat(int(directory.Fd()), endpoint.name, 0o600, unix.AT_SYMLINK_NOFOLLOW); checkErr != nil {
		return endpoint, checkErr
	}
	if checkErr := inspectControlSocket(path, owner); checkErr != nil {
		return endpoint, checkErr
	}
	if recordErr := endpoint.recordSocket(path); recordErr != nil {
		return endpoint, recordErr
	}
	return endpoint, ctx.Err()
}

func sameControlInode(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&unix.S_IFMT == b.Mode&unix.S_IFMT && a.Uid == b.Uid
}

func (e *controlEndpoint) Close() error {
	e.once.Do(func() {
		if e.listener != nil {
			if err := e.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				e.err = errors.Join(e.err, err)
			}
		}
		if e.socket.Ino != 0 {
			var current unix.Stat_t
			err := unix.Fstatat(int(e.directory.Fd()), e.name, &current, unix.AT_SYMLINK_NOFOLLOW)
			switch {
			case errors.Is(err, unix.ENOENT):
			case err != nil:
				e.err = errors.Join(e.err, err)
			case !sameControlInode(e.socket, current):
				e.err = errors.Join(e.err, fmt.Errorf("cortex: worker control socket replaced; cleanup refused"))
			default:
				e.err = errors.Join(e.err, unix.Unlinkat(int(e.directory.Fd()), e.name, 0))
			}
		}
		if e.lock != nil {
			e.err = errors.Join(e.err, e.lock.Close())
		}
		if e.directory != nil {
			e.err = errors.Join(e.err, e.directory.Close())
		}
	})
	return e.err
}
