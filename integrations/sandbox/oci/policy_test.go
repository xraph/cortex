package oci_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

func TestPolicyEncodesRequiredControls(t *testing.T) {
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "test", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 8 << 20, WallTime: time.Minute, OutputBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
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
}
