//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

type namespaceID struct{ device, inode uint64 }

// mountedTmpfs owns only a mount and a duplicate of its parent descriptor. The
// workspace lifecycle must authorize and verify worker containment
// before calling this private primitive. It does not create or remove paths.
type mountedTmpfs struct {
	mu        sync.Mutex
	parent    *os.File
	name      string
	plan      mountPlan
	namespace namespaceID
	identity  unix.Statx_t
	closed    bool
}

// mountTmpfs refuses the manager namespace and unsupported mount features. A
// nonnil handle with an error means attachment occurred and needs cleanup.
func mountTmpfs(ctx context.Context, parent int, name string, plan mountPlan) (*mountedTmpfs, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (name != "stage" && name != "scratch") || plan.bytes < 64<<10 || plan.bytes > 16<<30 || plan.bytes%int64(os.Getpagesize()) != 0 || plan.inodes < 64 || plan.inodes > 1_000_000 || (plan.mode != 0o700 && plan.mode != 0o1777) || (name == "scratch" && (plan.mode != 0o1777 || !plan.noexec)) || (name == "stage" && (plan.mode != 0o700 || plan.noexec)) {
		return nil, fmt.Errorf("cortex: invalid bounded mount plan")
	}
	namespace, err := privateNamespace()
	if err != nil {
		return nil, err
	}
	// This happens only in an existing, non-manager namespace. There is no
	// process-wide setns/unshare in the running Go worker.
	if checkErr := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); checkErr != nil {
		return nil, checkErr
	}
	parentFD, err := unix.FcntlInt(uintptr(parent), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parentFile := os.NewFile(uintptr(parentFD), "workspace parent")
	owned := true
	defer func() {
		if owned {
			parentFile.Close()
		}
	}()
	parentStat, err := mountIdentity(parentFD)
	if err != nil || parentStat.Uid != 0 || parentStat.Mode&unix.S_IFMT != unix.S_IFDIR || parentStat.Mode&0o7777 != 0o700 {
		return nil, errors.Join(err, fmt.Errorf("cortex: workspace parent must be private and root owned"))
	}
	targetFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	target := os.NewFile(uintptr(targetFD), name)
	defer target.Close()
	targetStat, err := mountIdentity(targetFD)
	if err != nil || targetStat.Uid != 0 || targetStat.Mode&0o7777 != 0o700 || targetStat.Mnt_id != parentStat.Mnt_id {
		return nil, errors.Join(err, fmt.Errorf("cortex: unsafe or already mounted workspace target"))
	}
	entries, err := target.Readdirnames(1)
	if len(entries) != 0 || !errors.Is(err, io.EOF) {
		return nil, errors.Join(fmt.Errorf("cortex: workspace mount target is not empty"), err)
	}
	filesystem, err := unix.Fsopen("tmpfs", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	fs := os.NewFile(uintptr(filesystem), "tmpfs context")
	defer fs.Close()
	for _, option := range [][2]string{{"size", strconv.FormatInt(plan.bytes, 10)}, {"nr_inodes", strconv.FormatInt(plan.inodes, 10)}, {"mode", strconv.FormatUint(uint64(plan.mode), 8)}, {"uid", "0"}, {"gid", "0"}} {
		if checkErr := unix.FsconfigSetString(filesystem, option[0], option[1]); checkErr != nil {
			return nil, checkErr
		}
	}
	if checkErr := unix.FsconfigSetFlag(filesystem, "noswap"); checkErr != nil {
		return nil, checkErr
	}
	if checkErr := unix.FsconfigCreate(filesystem); checkErr != nil {
		return nil, checkErr
	}
	flags := unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV
	if plan.noexec {
		flags |= unix.MOUNT_ATTR_NOEXEC
	}
	mountFD, err := unix.Fsmount(filesystem, unix.FSMOUNT_CLOEXEC, flags)
	if err != nil {
		return nil, err
	}
	detached := os.NewFile(uintptr(mountFD), "tmpfs mount")
	defer detached.Close()
	identity, err := mountIdentity(mountFD)
	if err != nil {
		return nil, err
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return nil, checkErr
	}
	if checkErr := unix.MoveMount(mountFD, "", targetFD, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); checkErr != nil {
		return nil, checkErr
	}
	mounted := &mountedTmpfs{parent: parentFile, name: name, plan: plan, namespace: namespace, identity: identity}
	owned = false
	checkErr := mounted.checkLocked(ctx)
	return mounted, checkErr
}

func (m *mountedTmpfs) check(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return m.checkLocked(ctx)
}

func (m *mountedTmpfs) checkLocked(ctx context.Context) error {
	root, err := m.openCurrent(ctx)
	if err != nil {
		return err
	}
	defer root.Close()
	var fs unix.Statfs_t
	if checkErr := unix.Fstatfs(int(root.Fd()), &fs); checkErr != nil {
		return checkErr
	}
	flags := int64(unix.ST_NOSUID | unix.ST_NODEV)
	if m.plan.noexec {
		flags |= unix.ST_NOEXEC
	}
	capacity, inodes := m.plan.bytes, m.plan.inodes
	if fs.Bsize <= 0 || fs.Bsize > 65536 || capacity <= 0 || capacity > 16<<30 || inodes <= 0 || inodes > 1_000_000 {
		return fmt.Errorf("cortex: invalid workspace filesystem accounting")
	}
	if fs.Type != unix.TMPFS_MAGIC || fs.Blocks != uint64(capacity)/uint64(fs.Bsize) || uint64(capacity)%uint64(fs.Bsize) != 0 || fs.Files != uint64(inodes) || fs.Flags&flags != flags || fs.Flags&unix.ST_RDONLY != 0 || (fs.Flags&unix.ST_NOEXEC != 0) != m.plan.noexec {
		return fmt.Errorf("cortex: workspace filesystem limits or flags changed")
	}
	var stat unix.Stat_t
	if checkErr := unix.Fstat(int(root.Fd()), &stat); checkErr != nil {
		return checkErr
	}
	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&0o7777 != m.plan.mode {
		return fmt.Errorf("cortex: workspace filesystem owner or mode changed")
	}
	var mount unix.Statx_t
	if checkErr := unix.Statx(int(root.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &mount); checkErr != nil {
		return checkErr
	}
	if mount.Mask&unix.STATX_MNT_ID == 0 {
		return fmt.Errorf("cortex: workspace mount inventory identity unavailable")
	}
	body, err := readMountInfo()
	if err != nil {
		return err
	}
	if checkErr := checkMountInfo(body, mount.Mnt_id); checkErr != nil {
		return checkErr
	}
	return ctx.Err()
}

func (m *mountedTmpfs) openCurrent(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closed {
		return nil, fmt.Errorf("cortex: workspace mount is closed")
	}
	namespace, err := privateNamespace()
	if err != nil {
		return nil, err
	}
	if namespace != m.namespace {
		return nil, fmt.Errorf("cortex: workspace mount namespace changed")
	}
	fd, err := unix.Openat(int(m.parent.Fd()), m.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), m.name)
	stat, err := mountIdentity(fd)
	if err != nil || stat.Mnt_id != m.identity.Mnt_id || stat.Ino != m.identity.Ino || stat.Dev_major != m.identity.Dev_major || stat.Dev_minor != m.identity.Dev_minor {
		file.Close()
		return nil, errors.Join(err, fmt.Errorf("cortex: workspace mount was replaced"))
	}
	return file, nil
}

// close uses normal unmount only. A busy or replaced mount remains unresolved;
// callers retain the handle and arrange independent worker cleanup if needed.
func (m *mountedTmpfs) close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if m.closed {
		return nil
	}
	root, err := m.openCurrent(ctx)
	if err != nil {
		return err
	}
	if checkErr := root.Close(); checkErr != nil {
		return checkErr
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return checkErr
	}
	// The parent is outside the mounted filesystem. Holding a descriptor inside
	// it here would itself make a normal unmount busy.
	if checkErr := unix.Unmount(fmt.Sprintf("/proc/self/fd/%d/%s", m.parent.Fd(), m.name), unix.UMOUNT_NOFOLLOW); checkErr != nil {
		return checkErr
	}
	m.closed = true
	return m.parent.Close()
}

func mountIdentity(fd int) (unix.Statx_t, error) {
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

func openProc() (*os.File, error) {
	fd, err := unix.Open("/proc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "proc")
	var fs unix.Statfs_t
	var stat unix.Stat_t
	if err = unix.Fstatfs(fd, &fs); err == nil {
		err = unix.Fstat(fd, &stat)
	}
	if err != nil || fs.Type != unix.PROC_SUPER_MAGIC || stat.Uid != 0 {
		file.Close()
		return nil, errors.Join(err, fmt.Errorf("cortex: workspace requires trusted procfs"))
	}
	return file, nil
}

func privateNamespace() (namespaceID, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Geteuid() != 0 {
		return namespaceID{}, fmt.Errorf("cortex: workspace mount requires root")
	}
	proc, err := openProc()
	if err != nil {
		return namespaceID{}, err
	}
	defer proc.Close()
	current, err := readNamespace(proc, "thread-self/ns/mnt")
	if err != nil {
		return namespaceID{}, err
	}
	manager, err := readNamespace(proc, "1/ns/mnt")
	if err != nil {
		return namespaceID{}, err
	}
	if current == manager {
		return namespaceID{}, fmt.Errorf("cortex: workspace refuses the manager mount namespace")
	}
	return current, nil
}

func readNamespace(proc *os.File, name string) (namespaceID, error) {
	// These two fixed procfs magic links are intentional kernel namespace handles.
	fd, err := unix.Openat(int(proc.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return namespaceID{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var fs unix.Statfs_t
	var stat unix.Stat_t
	if err = unix.Fstatfs(fd, &fs); err == nil {
		err = unix.Fstat(fd, &stat)
	}
	if err != nil || fs.Type != unix.NSFS_MAGIC {
		return namespaceID{}, errors.Join(err, fmt.Errorf("cortex: invalid mount namespace handle"))
	}
	return namespaceID{device: stat.Dev, inode: stat.Ino}, nil
}

func readMountInfo() ([]byte, error) {
	proc, err := openProc()
	if err != nil {
		return nil, err
	}
	defer proc.Close()
	fd, err := unix.Openat(int(proc.Fd()), "thread-self/mountinfo", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "mountinfo")
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxMountInfo+1))
}
