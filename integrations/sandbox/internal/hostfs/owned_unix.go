//go:build linux || darwin

package hostfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ReadOwned reads at most one MiB from a single-link, owner-controlled file with
// an exact mode. Authority comes from the protected host path, not its content.
func ReadOwned(ctx context.Context, name string, owner, mode uint32, maximum int64) ([]byte, error) {
	if maximum < 1 || maximum > 1<<20 {
		return nil, fmt.Errorf("cortex: invalid owned file size limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, size, err := openOwnedRegular(name, owner, mode)
	if err != nil {
		return nil, err
	}
	if size <= 0 || size > maximum {
		return nil, errors.Join(fmt.Errorf("cortex: owned file exceeds size limit"), file.Close())
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	if operationErr := errors.Join(readErr, file.Close(), ctx.Err()); operationErr != nil {
		return nil, operationErr
	}
	if int64(len(body)) != size {
		return nil, fmt.Errorf("cortex: owned file size changed during read")
	}
	return body, nil
}

func openOwnedRegular(name string, owner, mode uint32) (*os.File, int64, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || mode&^0o777 != 0 || mode&0o022 != 0 {
		return nil, 0, fmt.Errorf("cortex: invalid owned file path or permissions")
	}
	directory, err := OpenDirectory(filepath.Dir(name), owner)
	if err != nil {
		return nil, 0, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), filepath.Base(name), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != owner || uint64(stat.Mode)&0o7777 != uint64(mode) {
		return nil, 0, errors.Join(fmt.Errorf("cortex: unsafe owned file metadata"), file.Close())
	}
	return file, stat.Size, nil
}
