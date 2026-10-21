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

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
)

// Pin identifies an owner-controlled regular file. Expected values come from
// approved deployment configuration, never from the file being checked.
type Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

// VerifyPin checks every path component, ownership, exact mode, length and hash.
// The trusted owner must keep verified bytes unchanged until their final use.
func VerifyPin(ctx context.Context, pin Pin, owner uint32) error {
	if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !evidence.ValidDigest(pin.SHA256) || pin.Bytes <= 0 || pin.Bytes > 2<<30 || pin.Mode&^0o777 != 0 || pin.Mode&0o022 != 0 {
		return fmt.Errorf("cortex: invalid host file pin")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := OpenDirectory(filepath.Dir(pin.Path), owner)
	if err != nil {
		return err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), filepath.Base(pin.Path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), pin.Path)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return errors.Join(err, file.Close())
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != owner || uint64(stat.Mode)&0o7777 != uint64(pin.Mode) || stat.Size != pin.Bytes {
		return errors.Join(fmt.Errorf("cortex: unsafe or mismatched pinned host file"), file.Close())
	}
	return verifiedio.Read(ctx, file, io.Discard, pin.Bytes, pin.Bytes, pin.SHA256)
}
