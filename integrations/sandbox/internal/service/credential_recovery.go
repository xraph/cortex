package service

import (
	"fmt"

	"github.com/xraph/cortex/integrations/sandbox/internal/retention"
)

// CredentialScanner removes detached launch credentials after service fencing.
// Its gate also guards the controller's credential publisher, preventing a late
// writer from recreating a file after cleanup has returned.
type CredentialScanner struct {
	*cleanupScanner
	gate *credentialGate
}

// NewCredentialScanner pins the installed launch directory and shares the
// existing journal eligibility and bounded paging rules with other cleanup.
func NewCredentialScanner(cfg OrphanConfig, root string) (*CredentialScanner, error) {
	if cfg.Reaper == nil {
		return nil, fmt.Errorf("cortex: credential recovery requires a reaper")
	}
	recovery, err := retention.NewCredentials(root, cfg.Reaper.FenceAndStop)
	if err != nil {
		return nil, err
	}
	return newCredentialScanner(cfg, recovery)
}

func newCredentialScanner(cfg OrphanConfig, recovery retainedRecovery) (*CredentialScanner, error) {
	if recovery == nil {
		return nil, fmt.Errorf("cortex: credential recovery is missing")
	}
	gate := newCredentialGate()
	scanner, err := newCleanupScanner(cfg, recovery.Discover, gate.cleanup(recovery.Reclaim))
	if err != nil {
		return nil, err
	}
	return &CredentialScanner{cleanupScanner: scanner, gate: gate}, nil
}
