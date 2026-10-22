//go:build linux

package supervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
)

// NewWorkspaceRunsc binds a contained runtime to one live, bounded workspace.
// Staging must have verified the image and inputs and written WorkspaceSpec to
// Bundle. The caller must keep the workspace open until runtime cleanup and
// authorized exports finish. This constructor never executes a workload.
func NewWorkspaceRunsc(ctx context.Context, cfg RunscConfig, work *workspace.Workspace) (Driver, error) {
	if work == nil {
		return nil, fmt.Errorf("cortex: runtime workspace is required")
	}
	if err := work.Check(ctx); err != nil {
		return nil, err
	}
	paths, err := work.Paths()
	if err != nil {
		return nil, err
	}
	if bindingErr := validateWorkspacePaths(cfg, paths); bindingErr != nil {
		return nil, bindingErr
	}
	if membershipErr := containment.VerifyCurrent(ctx, paths.JobID); membershipErr != nil {
		return nil, membershipErr
	}
	driver, err := newRunsc(ctx, cfg, paths.JobID)
	if err != nil {
		return nil, err
	}
	driver.scratch = paths.Scratch
	driver.checkStorage = work.VerifyStorage
	driver.checkExecution = work.Check
	if verificationErr := driver.verifyWorkspace(ctx, false); verificationErr != nil {
		return nil, verificationErr
	}
	return driver, nil
}

func validateWorkspacePaths(cfg RunscConfig, paths workspace.Paths) error {
	if _, err := containment.ForJob(paths.JobID); err != nil {
		return err
	}
	if cfg.JobDigest != paths.JobDigest || cfg.Bundle != paths.Bundle || cfg.StateRoot != paths.Runtime || !withinWorkspace(cfg.Rootfs, paths.Images) {
		return fmt.Errorf("cortex: runtime job or staging paths differ from workspace")
	}
	for _, source := range cfg.Inputs {
		if !withinWorkspace(source, paths.Inputs) {
			return fmt.Errorf("cortex: runtime input is outside workspace")
		}
	}
	return nil
}

func withinWorkspace(name, root string) bool {
	return filepath.IsAbs(name) && filepath.Clean(name) == name && strings.HasPrefix(name, root+string(filepath.Separator))
}
