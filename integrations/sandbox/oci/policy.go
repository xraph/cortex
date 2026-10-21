// Package oci builds the deny-all profile. Encoding a spec does not qualify a
// runtime; a deployed runsc adapter must enforce and probe every control.
package oci

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

// ContainedSpec places the payload beneath its fixed worker service cgroup.
// The service must delegate CPU, memory and PID controllers, place the worker
// in its supervisor subgroup, and enforce a kill policy for the complete tree.
// This encoding does not attest to those external controls being installed.
func ContainedSpec(j sandbox.Job, rootfs string, inputs map[string]string) (*specs.Spec, error) {
	spec, err := Spec(j, rootfs, inputs)
	if err != nil {
		return nil, err
	}
	layout, err := containment.ForJob(j.ID)
	if err != nil {
		return nil, err
	}
	spec.Linux.CgroupsPath = layout.Payload
	return spec, nil
}

// Spec encodes a sealed job with host-staged rootfs and input paths. Staging must
// verify the pinned image and each evidence digest before creating the bundle.
// No workload-supplied host paths, namespaces, capabilities or hooks are accepted.
// The adapter must additionally use runsc --network=none, enforce the wall/output
// limits, and tear down the complete sandbox. Inputs must be regular files.
func Spec(j sandbox.Job, rootfs string, inputs map[string]string) (*specs.Spec, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(rootfs) || filepath.Clean(rootfs) != rootfs || len(inputs) != len(j.Inputs) {
		return nil, fmt.Errorf("cortex: invalid sandbox staging paths")
	}
	quota := j.Limits.CPUMillis * 100
	period := uint64(100000)
	memory := j.Limits.MemoryBytes
	processes := j.Limits.Processes
	// Keep the signed-to-unsigned conversion guarded at this boundary.
	if processes < 0 {
		return nil, fmt.Errorf("cortex: negative sandbox process limit")
	}
	guestProcesses := uint64(processes)
	env := []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "TMPDIR=/tmp"}
	keys := make([]string, 0, len(j.Env))
	for key := range j.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		env = append(env, key+"="+j.Env[key])
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "noexec", "mode=1777", "size=" + strconv.FormatInt(j.Limits.ScratchBytes, 10)}},
	}
	for _, input := range j.Inputs {
		source, ok := inputs[input.Path]
		if !ok || !filepath.IsAbs(source) || filepath.Clean(source) != source {
			return nil, fmt.Errorf("cortex: missing verified sandbox input")
		}
		mounts = append(mounts, specs.Mount{Destination: input.Path, Type: "bind", Source: source, Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}})
	}
	return &specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: rootfs, Readonly: true},
		Process: &specs.Process{User: specs.User{UID: 65532, GID: 65532}, Args: append([]string(nil), j.Argv...), Env: env, Cwd: "/tmp", NoNewPrivileges: true, Capabilities: &specs.LinuxCapabilities{}, Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024}, {Type: "RLIMIT_NPROC", Hard: guestProcesses, Soft: guestProcesses}, {Type: "RLIMIT_CORE", Hard: 0, Soft: 0}}},
		Mounts:  mounts, Hooks: &specs.Hooks{},
		Linux: &specs.Linux{
			CgroupsPath: "/cortex-sandbox/" + j.ID.String(),
			Namespaces:  []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.MountNamespace}, {Type: specs.NetworkNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace}, {Type: specs.CgroupNamespace}},
			Resources:   &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &memory, Swap: &memory}, Pids: &specs.LinuxPids{Limit: &processes}, CPU: &specs.LinuxCPU{Quota: &quota, Period: &period}, Devices: []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}}},
			MaskedPaths: []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/sched_debug", "/sys/firmware"}, ReadonlyPaths: []string{"/proc/sys", "/proc/sysrq-trigger", "/proc/irq", "/proc/bus"},
		},
	}, nil
}
