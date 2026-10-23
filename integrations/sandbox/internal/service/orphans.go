package service

import (
	"fmt"

	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

// OrphanConfig binds bounded independent discovery to one worker journal.
// The host must reserve the complete discovered namespace for this journal.
type OrphanConfig struct {
	Store              *workerstore.Store
	Reaper             *Reaper
	MaxUnits, PageSize int
}

// OrphanScanner fences services absent from the journal or already terminal.
// It never creates claims, reconstructs receipts, or changes recorded results.
// Journal read errors still trigger cleanup and remain unresolved afterward.
type OrphanScanner struct{ *cleanupScanner }

// NewOrphanScanner requires independent enumeration and explicit size bounds.
func NewOrphanScanner(cfg OrphanConfig) (*OrphanScanner, error) {
	if cfg.Reaper == nil {
		return nil, fmt.Errorf("cortex: missing orphan service reaper")
	}
	if _, ok := cfg.Reaper.manager.(workerDiscovery); !ok {
		return nil, fmt.Errorf("cortex: service manager cannot discover orphan workers")
	}
	scanner, err := newCleanupScanner(cfg, cfg.Reaper.Workers, cfg.Reaper.FenceAndStop)
	if err != nil {
		return nil, err
	}
	return &OrphanScanner{cleanupScanner: scanner}, nil
}
