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

// OpenPrivateState opens a mode-0600, single-link regular file in a private
// mode-0700 directory. Set create to permit creation without truncation. Paths
// must be canonical; the trusted owner must prevent replacement during use.
// The caller owns the descriptor and any required lifetime lock or sync.
func OpenPrivateState(name string, owner uint32, create bool) (*os.File, error) {
	flags := unix.O_RDWR
	if create {
		flags |= unix.O_CREAT
	}
	file, _, err := openOwnedFile(name, owner, 0o600, flags, true)
	return file, err
}

func openOwnedRegular(name string, owner, mode uint32) (*os.File, int64, error) {
	return openOwnedFile(name, owner, mode, unix.O_RDONLY, false)
}

// StatPrivateState checks metadata without opening the file. This preserves
// process-owned SQLite locks, which closing another database descriptor can drop.
func StatPrivateState(name string, owner uint32) (unix.Stat_t, error) {
	var stat unix.Stat_t
	directory, err := ownedParent(name, owner, 0o600, true)
	if err != nil {
		return stat, err
	}
	defer directory.Close()
	if statErr := unix.Fstatat(int(directory.Fd()), filepath.Base(name), &stat, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
		return stat, statErr
	}
	return stat, validateOwnedStat(stat, owner, 0o600)
}

func ownedParent(name string, owner, mode uint32, privateParent bool) (*os.File, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || mode&^0o777 != 0 || mode&0o022 != 0 {
		return nil, fmt.Errorf("cortex: invalid owned file path or permissions")
	}
	directory, err := OpenDirectory(filepath.Dir(name), owner)
	if err != nil {
		return nil, err
	}
	if privateParent {
		info, statErr := directory.Stat()
		if statErr != nil {
			return nil, errors.Join(statErr, directory.Close())
		}
		if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSticky|os.ModeSetgid|os.ModeSetuid) != 0 {
			return nil, errors.Join(fmt.Errorf("cortex: state directory must be private 0700"), directory.Close())
		}
	}
	return directory, nil
}

func validateOwnedStat(stat unix.Stat_t, owner, mode uint32) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != owner || uint64(stat.Mode)&0o7777 != uint64(mode) {
		return fmt.Errorf("cortex: unsafe owned file metadata")
	}
	return nil
}

func openOwnedFile(name string, owner, mode uint32, flags int, privateParent bool) (*os.File, int64, error) {
	directory, err := ownedParent(name, owner, mode, privateParent)
	if err != nil {
		return nil, 0, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), filepath.Base(name), flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	if err = validateOwnedStat(stat, owner, mode); err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	return file, stat.Size, nil
}
