//go:build linux || darwin

package service

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func inspectControlSocket(path string, owner uint32) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 100 || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("cortex: invalid worker control socket path")
	}
	root, err := hostfs.OpenDirectory(filepath.Dir(path), owner)
	if err != nil {
		return err
	}
	defer root.Close()
	var directory unix.Stat_t
	if statErr := unix.Fstat(int(root.Fd()), &directory); statErr != nil {
		return statErr
	}
	if directory.Mode&0o7777 != 0o700 {
		return fmt.Errorf("cortex: worker control directory is not private")
	}
	var socket unix.Stat_t
	if statErr := unix.Fstatat(int(root.Fd()), filepath.Base(path), &socket, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
		return statErr
	}
	if socket.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.Uid != owner || socket.Mode&0o7777 != 0o600 || socket.Nlink != 1 {
		return fmt.Errorf("cortex: unsafe worker control socket")
	}
	return nil
}
