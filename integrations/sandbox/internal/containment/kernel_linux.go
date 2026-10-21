package containment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

type kernelGroup struct{}

// VerifyCurrent requires a root worker in the exact delegated service subgroup.
// The procfs and cgroup-v2 mounts are checked before any runtime invocation.
func VerifyCurrent(ctx context.Context, key id.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("cortex: containment requires a root-run Linux worker")
	}
	body, err := readMembership()
	if err != nil {
		return err
	}
	if membershipErr := CheckMembership(key, body); membershipErr != nil {
		return membershipErr
	}
	directory, err := openGroup(ctx, key)
	if err != nil {
		return err
	}
	return errors.Join(ctx.Err(), directory.Close())
}

func readMembership() ([]byte, error) {
	file, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var fs unix.Statfs_t
	if statErr := unix.Fstatfs(int(file.Fd()), &fs); statErr != nil {
		return nil, statErr
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		return nil, fmt.Errorf("cortex: membership is not from procfs")
	}
	return io.ReadAll(io.LimitReader(file, 8193))
}

func openGroup(ctx context.Context, key id.ID) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cortex: containment requires root")
	}
	layout, err := ForJob(key)
	if err != nil {
		return nil, err
	}
	root, err := hostfs.OpenDirectory("/sys/fs/cgroup", 0)
	if err != nil {
		return nil, err
	}
	if err = checkFilesystem(root); err != nil {
		root.Close()
		return nil, err
	}
	current := root
	for _, name := range []string{"system.slice", layout.Unit} {
		fd, openErr := unix.Openat(int(current.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		closeErr := current.Close()
		if openErr != nil || closeErr != nil {
			if openErr == nil {
				_ = unix.Close(fd)
			}
			if errors.Is(openErr, unix.ENOENT) && closeErr == nil {
				return nil, ErrAbsent
			}
			return nil, errors.Join(openErr, closeErr)
		}
		current = os.NewFile(uintptr(fd), name)
		if err = checkFilesystem(current); err != nil {
			current.Close()
			return nil, err
		}
	}
	return current, nil
}

func checkFilesystem(file *os.File) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return err
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("cortex: cgroup-v2 filesystem required")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != 0 || stat.Mode&0o022 != 0 {
		return fmt.Errorf("cortex: unsafe cgroup control ownership")
	}
	return nil
}

func controlFile(directory *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, fmt.Errorf("cortex: invalid cgroup control file")
	}
	if err = checkFilesystem(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (kernelGroup) Events(ctx context.Context, key id.ID) ([]byte, error) {
	directory, err := openGroup(ctx, key)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	file, err := controlFile(directory, "cgroup.events", unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 4097))
	return body, errors.Join(readErr, file.Close(), ctx.Err())
}

func (kernelGroup) Kill(ctx context.Context, key id.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	layout, err := ForJob(key)
	if err != nil {
		return err
	}
	body, err := readMembership()
	if err != nil {
		return err
	}
	current, err := membership(body)
	if err != nil {
		return err
	}
	if current == layout.Parent || strings.HasPrefix(current, layout.Parent+"/") {
		return fmt.Errorf("cortex: reaper cannot kill its own containment")
	}
	directory, err := openGroup(ctx, key)
	if err != nil {
		return err
	}
	defer directory.Close()
	file, err := controlFile(directory, "cgroup.kill", unix.O_WRONLY)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.Join(err, file.Close())
	}
	written, writeErr := file.WriteString("1")
	if writeErr == nil && written != 1 {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Close(), ctx.Err())
}
