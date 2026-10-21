//go:build linux || darwin

// Package hostfs opens trusted worker directories without following aliases.
package hostfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenDirectory walks a canonical absolute path through no-follow descriptors.
// Ancestors must belong to root or owner and reject other writers, except for
// root-owned sticky directories. The final directory must belong to owner and
// reject group/world writes. Callers apply their own read/execute/privacy policy.
// The trusted owner must prevent changes during use; this is not a write lock.
func OpenDirectory(directory string, owner uint32) (*os.File, error) {
	if directory == "/" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, fmt.Errorf("cortex: host directory requires a canonical absolute path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	parts := strings.Split(strings.TrimPrefix(directory, "/"), "/")
	for _, part := range parts {
		next, openErr := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if closeErr := current.Close(); closeErr != nil {
			if openErr == nil {
				_ = unix.Close(next)
			}
			return nil, errors.Join(openErr, closeErr)
		}
		if openErr != nil {
			return nil, openErr
		}
		current = os.NewFile(uintptr(next), part)
		var stat unix.Stat_t
		if err = unix.Fstat(next, &stat); err != nil {
			current.Close()
			return nil, err
		}
		stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		if (stat.Uid != 0 && stat.Uid != owner) || (stat.Mode&0o022 != 0 && !stickyRoot) {
			current.Close()
			return nil, fmt.Errorf("cortex: unsafe host directory ancestor")
		}
	}
	var final unix.Stat_t
	if err = unix.Fstat(int(current.Fd()), &final); err != nil {
		current.Close()
		return nil, err
	}
	if final.Uid != owner || final.Mode&0o022 != 0 {
		current.Close()
		return nil, fmt.Errorf("cortex: unsafe host directory owner or permissions")
	}
	return current, nil
}
