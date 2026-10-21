package workspace

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/xraph/cortex/sandbox"
)

const maxMountInfo = 4 << 20

type mountPlan struct {
	bytes, inodes int64
	mode          uint32
	noexec        bool
}

func (p Policy) mountPlans(job sandbox.Job, serviceMemory uint64, page int64) (staging, scratch mountPlan, planErr error) {
	if err := p.Admit(job, serviceMemory); err != nil {
		return mountPlan{}, mountPlan{}, err
	}
	if page < 4096 || page > 65536 || page&(page-1) != 0 {
		return mountPlan{}, mountPlan{}, fmt.Errorf("cortex: unsupported workspace page size")
	}
	return mountPlan{bytes: p.StagingBytes, inodes: p.StagingInodes, mode: 0o700}, mountPlan{bytes: job.Limits.ScratchBytes / page * page, inodes: p.ScratchInodes, mode: 0o1777, noexec: true}, nil
}

// checkMountInfo accepts only one complete row for the mount. Propagation into
// or out of the worker namespace and swap backing are both forbidden.
func checkMountInfo(body []byte, mountID uint64) error {
	if mountID == 0 || len(body) == 0 || len(body) > maxMountInfo || body[len(body)-1] != '\n' {
		return fmt.Errorf("cortex: incomplete workspace mount inventory")
	}
	want := strconv.FormatUint(mountID, 10)
	found := false
	for line := range bytes.SplitSeq(body, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) == 0 || fields[0] != want {
			continue
		}
		if found || len(fields) < 10 {
			return fmt.Errorf("cortex: ambiguous workspace mount")
		}
		found = true
		separator := 6
		for separator < len(fields) && fields[separator] != "-" {
			// Refuse unknown propagation metadata too, rather than infer isolation.
			if fields[separator] != "unbindable" {
				return fmt.Errorf("cortex: workspace mount is not private")
			}
			separator++
		}
		if separator+4 != len(fields) || fields[separator+1] != "tmpfs" || !hasOption(fields[separator+3], "noswap") {
			return fmt.Errorf("cortex: workspace requires nonswapping tmpfs")
		}
	}
	if !found {
		return fmt.Errorf("cortex: workspace mount missing from inventory")
	}
	return nil
}

func hasOption(options, want string) bool {
	for option := range strings.SplitSeq(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}
