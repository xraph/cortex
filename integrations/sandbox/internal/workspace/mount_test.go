package workspace

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/sandbox"
)

func TestMountPlansNeverExpandSignedScratch(t *testing.T) {
	p, job := testPolicy(), testJob(t)
	job.Limits.ScratchBytes += 4095
	job, err := sandbox.Seal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []int64{4096, 16384, 65536} {
		stage, scratch, planErr := p.mountPlans(job, 512<<20, page)
		if planErr != nil {
			t.Fatal(planErr)
		}
		if stage.bytes != p.StagingBytes || stage.inodes != p.StagingInodes || stage.mode != 0o700 || stage.noexec {
			t.Fatal("staging quota or mode changed")
		}
		if scratch.bytes > job.Limits.ScratchBytes || scratch.bytes%page != 0 || scratch.bytes != job.Limits.ScratchBytes/page*page || scratch.inodes != p.ScratchInodes || scratch.mode != 0o1777 || !scratch.noexec {
			t.Fatal("scratch quota expanded or protections missing")
		}
	}
	for _, page := range []int64{0, -1, 3, 131072} {
		if _, _, planErr := p.mountPlans(job, 512<<20, page); planErr == nil {
			t.Fatal("invalid native page size accepted")
		}
	}
}

func TestMountInfoRequiresExactPrivateNoswapFilesystem(t *testing.T) {
	valid := "42 17 0:9 / /work/stage rw,nosuid,nodev - tmpfs tmpfs rw,size=32768k,nr_inodes=4096,noswap\n"
	if err := checkMountInfo([]byte(valid), 42); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		strings.ReplaceAll(valid, "noswap", "noswap=false"),
		strings.ReplaceAll(valid, ",noswap", ""),
		strings.ReplaceAll(valid, " - ", " shared:5 - "),
		strings.ReplaceAll(valid, " - ", " master:5 - "),
		strings.ReplaceAll(valid, " - ", " propagate_from:5 - "),
		strings.ReplaceAll(valid, " - tmpfs ", " - ext4 "),
		strings.ReplaceAll(valid, "42 ", "420 "),
		valid + valid, "42 corrupt\n", strings.TrimSuffix(valid, "\n"),
	} {
		if err := checkMountInfo([]byte(body), 42); err == nil {
			t.Fatalf("unsafe mountinfo accepted: %q", body)
		}
	}
}
