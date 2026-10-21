//go:build linux || darwin

package journal

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func preparePath(filename string) (string, error) {
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || strings.Contains(filename, "?") {
		return "", fmt.Errorf("cortex: sandbox journal requires an absolute file path")
	}
	// Resolve the configured directory once (including macOS temporary-directory
	// aliases), then use only this checked canonical path for SQLite access.
	parent, err := filepath.EvalSymlinks(filepath.Dir(filename))
	if err != nil {
		return "", err
	}
	owner := os.Geteuid()
	if owner < 0 || int64(owner) > math.MaxUint32 {
		return "", fmt.Errorf("cortex: invalid journal owner")
	}
	directory, err := hostfs.OpenDirectory(parent, uint32(owner))
	if err != nil {
		return "", err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return "", err
	}
	if info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("cortex: sandbox journal directory must be private 0700")
	}
	name := filepath.Base(filename)
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return "", err
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(fd, &stat)
	if operationErr := errors.Join(statErr, unix.Close(fd)); operationErr != nil {
		return "", operationErr
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 {
		return "", fmt.Errorf("cortex: sandbox journal must be a private singly-linked regular file")
	}
	return filepath.Join(parent, name), nil
}
