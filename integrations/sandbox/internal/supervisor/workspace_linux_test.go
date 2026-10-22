//go:build linux

package supervisor

import (
	"context"
	"testing"

	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
)

func TestWorkspaceRuntimeRejectsForeignPathsAndJob(t *testing.T) {
	j := job(t)
	paths := workspace.Paths{JobID: j.ID, JobDigest: j.Digest, Directory: "/work/job", Images: "/work/job/stage/images", Inputs: "/work/job/stage/inputs", Bundle: "/work/job/stage/bundle", Runtime: "/work/job/stage/runtime", Scratch: "/work/job/scratch"}
	cfg := RunscConfig{JobDigest: j.Digest, Bundle: paths.Bundle, StateRoot: paths.Runtime, Rootfs: paths.Images + "/verified/rootfs", Inputs: map[string]string{"/inputs/example": paths.Inputs + "/verified/file"}}
	if err := validateWorkspacePaths(cfg, paths); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"job", "bundle", "runtime", "image", "image-root", "image-prefix", "input", "input-parent"} {
		t.Run(mode, func(t *testing.T) {
			changed := cfg
			switch mode {
			case "job":
				changed.JobDigest = "other"
			case "bundle":
				changed.Bundle = "/other/bundle"
			case "runtime":
				changed.StateRoot = "/other/runtime"
			case "image":
				changed.Rootfs = "/other/rootfs"
			case "image-root":
				changed.Rootfs = paths.Images
			case "image-prefix":
				changed.Rootfs = paths.Images + "-other/rootfs"
			case "input":
				changed.Inputs = map[string]string{"/inputs/example": "/elsewhere/file"}
			case "input-parent":
				changed.Inputs = map[string]string{"/inputs/example": paths.Inputs + "/../file"}
			}
			if err := validateWorkspacePaths(changed, paths); err == nil {
				t.Fatal("foreign runtime binding accepted")
			}
		})
	}
	if driver, err := NewWorkspaceRunsc(context.Background(), cfg, nil); err == nil || driver != nil {
		t.Fatal("runtime accepted missing workspace")
	}
}
