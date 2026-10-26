package service

import (
	"context"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func observeQualificationHost(ctx context.Context) (QualificationHost, error) {
	machine, err := hostfs.ReadOwned(ctx, "/etc/machine-id", 0, 0o644, 128)
	if err != nil {
		machine, err = hostfs.ReadOwned(ctx, "/etc/machine-id", 0, 0o444, 128)
	}
	if err != nil {
		return QualificationHost{}, err
	}
	identity := strings.TrimSpace(string(machine))
	if len(identity) != 32 {
		return QualificationHost{}, broker.ErrDenied
	}
	for _, char := range identity {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return QualificationHost{}, broker.ErrDenied
		}
	}
	file, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return QualificationHost{}, err
	}
	boot, readErr := io.ReadAll(io.LimitReader(file, 128))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(boot) >= 128 {
		return QualificationHost{}, broker.ErrDenied
	}
	var system unix.Utsname
	if unameErr := unix.Uname(&system); unameErr != nil {
		return QualificationHost{}, unameErr
	}
	host := QualificationHost{MachineID: evidence.Digest(machine), BootID: strings.TrimSpace(string(boot)), KernelRelease: unix.ByteSliceToString(system.Release[:]), KernelVersion: unix.ByteSliceToString(system.Version[:]), Architecture: unix.ByteSliceToString(system.Machine[:])}
	if !validQualificationHost(host) || unix.ByteSliceToString(system.Sysname[:]) != "Linux" {
		return QualificationHost{}, broker.ErrDenied
	}
	return host, ctx.Err()
}
