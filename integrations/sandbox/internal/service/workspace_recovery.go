package service

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/retention"
)

type retainedRecovery interface {
	Discover(context.Context, int) ([]id.ID, error)
	Reclaim(context.Context, id.ID) error
}

// WorkspaceScanner reclaims detached workspace mountpoints after fencing their
// services. Known nonterminal jobs remain with normal execution recovery.
type WorkspaceScanner struct{ *cleanupScanner }

// NewWorkspaceScanner pins the installed workspace directory. It shares the
// bounded journal eligibility rules with independent service orphan recovery.
func NewWorkspaceScanner(cfg OrphanConfig, root string) (*WorkspaceScanner, error) {
	if cfg.Reaper == nil {
		return nil, fmt.Errorf("cortex: workspace recovery requires a reaper")
	}
	recovery, err := retention.NewWorkspaces(root, cfg.Reaper.FenceAndStop)
	if err != nil {
		return nil, err
	}
	return newWorkspaceScanner(cfg, recovery)
}

func newWorkspaceScanner(cfg OrphanConfig, recovery retainedRecovery) (*WorkspaceScanner, error) {
	if recovery == nil {
		return nil, fmt.Errorf("cortex: workspace recovery is missing")
	}
	scanner, err := newCleanupScanner(cfg, recovery.Discover, recovery.Reclaim)
	if err != nil {
		return nil, err
	}
	return &WorkspaceScanner{cleanupScanner: scanner}, nil
}
