package workerpeer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

type linuxProcess struct {
	number int32
	file   *os.File
}

func openProcess(ctx context.Context, connection *net.UnixConn) (peerProcess, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cortex: worker control requires root")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, err
	}
	descriptor := -1
	var credentials *unix.Ucred
	var captureErr error
	controlErr := raw.Control(func(fd uintptr) {
		kind, socketErr := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		if socketErr != nil || kind != unix.SOCK_STREAM {
			captureErr = errors.Join(socketErr, fmt.Errorf("cortex: worker control requires a Unix stream"))
			return
		}
		credentials, captureErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if captureErr != nil {
			return
		}
		if credentials == nil || credentials.Pid <= 1 || credentials.Uid != 0 || credentials.Gid != 0 {
			captureErr = fmt.Errorf("cortex: worker socket peer is not a root process")
			return
		}
		// This handle refers to the original connecting process, even if its
		// numeric PID has since been reused. Never substitute pidfd_open here.
		descriptor, captureErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		if captureErr != nil {
			return
		}
		if descriptor < 0 {
			captureErr = fmt.Errorf("cortex: invalid worker pidfd")
			return
		}
		flags, flagErr := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0)
		if flagErr != nil || flags&unix.FD_CLOEXEC == 0 {
			captureErr = errors.Join(flagErr, fmt.Errorf("cortex: worker pidfd is not close-on-exec"))
		}
	})
	if err = errors.Join(controlErr, captureErr); err != nil {
		if descriptor >= 0 {
			err = errors.Join(err, unix.Close(descriptor))
		}
		return nil, err
	}
	process := &linuxProcess{number: credentials.Pid, file: os.NewFile(uintptr(descriptor), "worker-peer")}
	if err = process.alive(ctx); err != nil {
		return nil, errors.Join(err, process.close())
	}
	return process, nil
}

func (p *linuxProcess) pid() int32   { return p.number }
func (p *linuxProcess) close() error { return p.file.Close() }

func (p *linuxProcess) alive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd := p.file.Fd()
	if fd > math.MaxInt32 {
		return os.ErrClosed
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	count, err := unix.Poll(poll, 0)
	if err != nil {
		return err
	}
	if count != 0 || poll[0].Revents != 0 {
		return fmt.Errorf("cortex: worker peer exited")
	}
	return errors.Join(unix.PidfdSendSignal(int(fd), 0, nil, 0), ctx.Err())
}

func (p *linuxProcess) snapshot(ctx context.Context) (status, membership []byte, err error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, nil, contextErr
	}
	root, err := hostfs.OpenDirectory("/proc", 0)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	if checkErr := procFile(root, true); checkErr != nil {
		return nil, nil, checkErr
	}
	var self [64]byte
	count, err := unix.Readlinkat(int(root.Fd()), "self", self[:])
	if err != nil {
		return nil, nil, err
	}
	if string(self[:count]) != strconv.Itoa(os.Getpid()) {
		return nil, nil, fmt.Errorf("cortex: procfs PID namespace differs from worker control")
	}
	fd, err := unix.Openat(int(root.Fd()), strconv.FormatInt(int64(p.number), 10), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	directory := os.NewFile(uintptr(fd), "worker-process")
	defer directory.Close()
	if checkErr := procFile(directory, true); checkErr != nil {
		return nil, nil, checkErr
	}
	status, err = readProc(directory, "status", 65536)
	if err != nil {
		return nil, nil, err
	}
	membership, err = readProc(directory, "cgroup", 8192)
	if checkErr := errors.Join(err, ctx.Err()); checkErr != nil {
		return nil, nil, checkErr
	}
	return status, membership, nil
}

func procFile(file *os.File, directory bool) error {
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &filesystem); err != nil {
		return err
	}
	if filesystem.Type != unix.PROC_SUPER_MAGIC {
		return fmt.Errorf("cortex: worker identity requires procfs")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	want := uint32(unix.S_IFREG)
	if directory {
		want = unix.S_IFDIR
	}
	if stat.Mode&unix.S_IFMT != want || stat.Uid != 0 || stat.Gid != 0 || stat.Mode&0o022 != 0 {
		return fmt.Errorf("cortex: unsafe worker procfs identity file")
	}
	return nil
}

func readProc(directory *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err = procFile(file, false); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	body, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	if checkErr := errors.Join(readErr, file.Close()); checkErr != nil {
		return nil, checkErr
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("cortex: worker procfs identity exceeds bound")
	}
	return body, nil
}
