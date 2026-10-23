//go:build linux

package retention

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func (r *Recovery) reclaimCredential(ctx context.Context, root, directory *os.File, key id.ID) error {
	names, err := boundedNames(directory, 1)
	if err != nil {
		return err
	}
	if len(names) == 1 {
		name := names[0]
		file, expected, openErr := r.openCredential(directory, name)
		if openErr != nil {
			return openErr
		}
		defer file.Close()
		if checkErr := ctx.Err(); checkErr != nil {
			return checkErr
		}
		current, observed, checkErr := r.openCredential(directory, name)
		if checkErr != nil {
			return checkErr
		}
		if closeErr := current.Close(); closeErr != nil {
			return closeErr
		}
		if expected != observed {
			return fmt.Errorf("cortex: retained credential identity changed")
		}
		if removeErr := unix.Unlinkat(int(directory.Fd()), name, 0); removeErr != nil {
			return removeErr
		}
	}
	return r.removeDirectory(ctx, root, directory, key)
}

func (r *Recovery) openCredential(directory *os.File, name string) (*os.File, recoveryIdentity, error) {
	if name != "claim.tmp" && name != "claim.json" {
		return nil, recoveryIdentity{}, fmt.Errorf("cortex: unexpected retained credential entry")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, recoveryIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	stat, err := hostfs.MountIdentity(fd)
	required := uint32(unix.STATX_NLINK | unix.STATX_SIZE)
	if err != nil || stat.Mask&required != required || stat.Uid != r.owner || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o400 || stat.Nlink != 1 || stat.Size > 1<<20 || stat.Mnt_id != r.identity.mount || stat.Dev_major != r.identity.deviceMajor || stat.Dev_minor != r.identity.deviceMinor {
		file.Close()
		return nil, recoveryIdentity{}, errors.Join(err, fmt.Errorf("cortex: unsafe retained credential metadata"))
	}
	return file, recoveryIdentity{deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor, inode: stat.Ino, mount: stat.Mnt_id}, nil
}
