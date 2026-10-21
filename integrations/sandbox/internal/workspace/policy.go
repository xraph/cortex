// Package workspace bounds temporary worker storage independently of job output.
package workspace

import (
	"fmt"

	"github.com/xraph/cortex/sandbox"
)

// Policy is host configuration, pinned by the worker launch profile. Staging
// covers image, input, bundle and runtime files; scratch is a separate filesystem
// bounded by the signed job. ReserveMemoryBytes leaves room for worker overhead.
type Policy struct {
	StagingBytes       int64 `json:"staging_bytes"`
	StagingInodes      int64 `json:"staging_inodes"`
	ScratchInodes      int64 `json:"scratch_inodes"`
	ReserveMemoryBytes int64 `json:"reserve_memory_bytes"`
}

// Validate rejects unlimited storage, inode counts and unbounded reservations.
func (p Policy) Validate() error {
	if p.StagingBytes < 1<<20 || p.StagingBytes > 16<<30 || p.StagingBytes%(64<<10) != 0 || p.StagingInodes < 64 || p.StagingInodes > 1_000_000 || p.ScratchInodes < 64 || p.ScratchInodes > 1_000_000 || p.ReserveMemoryBytes < 64<<20 || p.ReserveMemoryBytes > 1<<30 {
		return fmt.Errorf("cortex: invalid workspace policy")
	}
	return nil
}

// Admit requires storage, guest memory and the overhead reservation to fit the
// outer service ceiling. This is a limit check, not a promise of runtime capacity.
func (p Policy) Admit(job sandbox.Job, serviceMemory uint64) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := job.Validate(); err != nil {
		return err
	}
	if serviceMemory == 0 || serviceMemory > 1<<40 || job.Limits.ScratchBytes < 64<<10 || job.Limits.ScratchBytes > 16<<30 {
		return fmt.Errorf("cortex: unsupported workspace memory or scratch budget")
	}
	remaining := serviceMemory
	for _, value := range []int64{p.StagingBytes, job.Limits.ScratchBytes, job.Limits.MemoryBytes, p.ReserveMemoryBytes} {
		if value <= 0 || uint64(value) > remaining {
			return fmt.Errorf("cortex: workspace and guest exceed service memory budget")
		}
		remaining -= uint64(value)
	}
	return nil
}
