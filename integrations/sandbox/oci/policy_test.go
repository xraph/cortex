package oci_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

func TestPolicyEncodesRequiredControls(t *testing.T) {
	j := policyJob(t)
	spec, err := oci.Spec(j, "/private/bundle/rootfs", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Root.Readonly || !spec.Process.NoNewPrivileges || spec.Process.User.UID == 0 || len(spec.Process.Capabilities.Bounding) != 0 {
		t.Fatal("unsafe process policy")
	}
	if *spec.Linux.Resources.Memory.Limit != j.Limits.MemoryBytes || *spec.Linux.Resources.Pids.Limit != j.Limits.Processes || *spec.Linux.Resources.CPU.Quota != 50000 {
		t.Fatal("limits lost")
	}
	network := false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" {
			network = true
			if ns.Path != "" {
				t.Fatal("host namespace inherited")
			}
		}
	}
	if !network {
		t.Fatal("network namespace absent")
	}
	if !reflect.DeepEqual(spec.Hooks, &specs.Hooks{}) {
		t.Fatal("host hooks injected")
	}
	contained, err := oci.ContainedSpec(j, "/private/bundle/rootfs", nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := containment.ForJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if contained.Linux.CgroupsPath != layout.Payload {
		t.Fatalf("payload escaped worker service: %q", contained.Linux.CgroupsPath)
	}
	contained.Linux.CgroupsPath = spec.Linux.CgroupsPath
	if !reflect.DeepEqual(contained, spec) {
		t.Fatal("contained profile widened other sandbox controls")
	}
}

func TestGuestProcessLimitIsIndependentOfHostThreads(t *testing.T) {
	j := policyJob(t)
	spec, err := oci.Spec(j, "/private/bundle/rootfs", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range spec.Process.Rlimits {
		if limit.Type == "RLIMIT_NPROC" && limit.Hard == 16 && limit.Soft == 16 {
			return
		}
	}
	t.Fatal("guest process limit missing; a host-thread cgroup limit alone cannot bound guest processes")
}

func policyJob(t *testing.T) sandbox.Job {
	t.Helper()
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "test", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 8 << 20, WallTime: time.Minute, OutputBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestWorkspaceSpecBindsScratchWithoutWideningControls(t *testing.T) {
	j := policyJob(t)
	regular, err := oci.ContainedSpec(j, "/workspace/stage/images/rootfs", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := oci.WorkspaceSpec(j, "/workspace/stage/images/rootfs", nil, "/workspace/scratch")
	if err != nil {
		t.Fatal(err)
	}
	want := specs.Mount{Destination: "/tmp", Type: "bind", Source: "/workspace/scratch", Options: []string{"bind", "rw", "nosuid", "nodev", "noexec", "rprivate"}}
	if !reflect.DeepEqual(spec.Mounts[1], want) {
		t.Fatal("scratch binding lost confinement", spec.Mounts[1])
	}
	spec.Mounts[1] = regular.Mounts[1]
	if !reflect.DeepEqual(spec, regular) {
		t.Fatal("workspace spec changed other controls")
	}
	for _, scratch := range []string{"", "/", "relative", "/workspace/scratch/..", "/workspace/stage", "/workspace/stage/images/rootfs", "/workspace/stage/images/rootfs/tmp"} {
		if _, pathErr := oci.WorkspaceSpec(j, "/workspace/stage/images/rootfs", nil, scratch); pathErr == nil {
			t.Fatalf("unsafe scratch accepted: %q", scratch)
		}
	}
}
