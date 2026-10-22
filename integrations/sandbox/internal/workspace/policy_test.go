package workspace

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func testPolicy() Policy {
	return Policy{StagingBytes: 32 << 20, StagingInodes: 4096, ScratchInodes: 1024, ReserveMemoryBytes: 64 << 20}
}

func testJob(t *testing.T) sandbox.Job {
	t.Helper()
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "workspace", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestWorkspacePolicyAccountsForStorageGuestAndWorkerMemory(t *testing.T) {
	policy, job := testPolicy(), testJob(t)
	budget := uint64(policy.StagingBytes + policy.ReserveMemoryBytes + job.Limits.ScratchBytes + job.Limits.MemoryBytes)
	if err := policy.Admit(job, budget); err != nil {
		t.Fatal("exact service budget refused", err)
	}
	if err := policy.Admit(job, budget-1); err == nil {
		t.Fatal("workspace exceeded service memory budget")
	}
	job.Limits.ScratchBytes = math.MaxInt64
	job, err := sandbox.Seal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.Admit(job, 1<<40); err == nil {
		t.Fatal("overflowing scratch budget admitted")
	}
}

func TestWorkspacePolicyRejectsUnboundedOrEmptyLimits(t *testing.T) {
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.StagingBytes = 0 }, func(p *Policy) { p.StagingBytes = (16 << 30) + 1 },
		func(p *Policy) { p.StagingInodes = 0 }, func(p *Policy) { p.StagingInodes = 1_000_001 },
		func(p *Policy) { p.ScratchInodes = 0 }, func(p *Policy) { p.ScratchInodes = 1_000_001 },
		func(p *Policy) { p.ReserveMemoryBytes = 0 }, func(p *Policy) { p.ReserveMemoryBytes = (1 << 30) + 1 },
	} {
		policy := testPolicy()
		mutate(&policy)
		if err := policy.Validate(); err == nil {
			t.Fatal("invalid workspace policy accepted")
		}
	}
}
