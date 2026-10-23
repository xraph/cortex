//go:build linux

package hostfs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// MountIdentity returns inode and unique mount identity, failing on unsupported kernels.
func MountIdentity(fd int) (unix.Statx_t, error) {
	var stat unix.Statx_t
	mask := unix.STATX_BASIC_STATS | unix.STATX_MNT_ID_UNIQUE
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, mask, &stat); err != nil {
		return stat, err
	}
	required := uint32(unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_UID | unix.STATX_GID | unix.STATX_INO | unix.STATX_MNT_ID_UNIQUE)
	if stat.Mask&required != required || stat.Mnt_id == 0 {
		return stat, fmt.Errorf("cortex: unique workspace mount identity unavailable")
	}
	return stat, nil
}
