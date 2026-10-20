package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func testJob() sandbox.Job {
	return sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "request-1", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, Principal: "actor-a", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/task"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 32, ScratchBytes: 8 << 20, WallTime: time.Minute, OutputBytes: 1 << 20}}
}

func TestJobPolicyRejectsPrivilegedMount(t *testing.T) {
	j := testJob()
	if _, err := sandbox.Seal(j); err != nil {
		t.Fatal(err)
	}
	j.Inputs = []sandbox.Input{{Path: "/var/run/docker.sock"}}
	if _, err := sandbox.Seal(j); err == nil {
		t.Fatal("host socket accepted as evidence input")
	}
}

func TestResourceLimitsRequired(t *testing.T) {
	for _, change := range []func(*sandbox.Job){func(j *sandbox.Job) { j.Limits.Processes = 0 }, func(j *sandbox.Job) { j.Limits.ScratchBytes = -1 }, func(j *sandbox.Job) { j.Limits.WallTime = 0 }, func(j *sandbox.Job) { j.Network = "host" }, func(j *sandbox.Job) { j.Image = "alpine:latest" }, func(j *sandbox.Job) { j.Argv = []string{""} }, func(j *sandbox.Job) { j.Env = map[string]string{"LD_PRELOAD": "/evil"} }} {
		j := testJob()
		change(&j)
		if _, err := sandbox.Seal(j); err == nil {
			t.Fatal("unsafe job accepted")
		}
	}
}

func TestSealedJobDetachedAndBound(t *testing.T) {
	j := testJob()
	sealed, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	j.Argv[0] = "changed"
	j.Scope.Levels[0].Value = "other"
	if err = sealed.Validate(); err != nil {
		t.Fatal(err)
	}
	sealed.Principal = "other"
	if err = sealed.Validate(); err == nil {
		t.Fatal("tampered principal accepted")
	}
}

func TestCapabilitiesRequireQualification(t *testing.T) {
	j, err := sandbox.Seal(testJob())
	if err != nil {
		t.Fatal(err)
	}
	c := sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: strings.Repeat("b", 64), Profile: sandbox.ProfileDenyAll, Limits: j.Limits}
	if err = c.Supports(j); err == nil {
		t.Fatal("unqualified runtime accepted")
	}
	c.Qualification = "fixture-only"
	c.QualifiedUntil = time.Now().Add(time.Hour)
	if err = c.Supports(j); err != nil {
		t.Fatal(err)
	}
	c.Limits.Processes--
	if err = c.Supports(j); err == nil {
		t.Fatal("unsupported process limit accepted")
	}
}
