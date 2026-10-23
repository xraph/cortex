package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// OrphanConfig binds independent service discovery to the worker journal.
// The host must reserve the complete worker service namespace for this journal.
type OrphanConfig struct {
	Store              *workerstore.Store
	Reaper             *Reaper
	MaxUnits, PageSize int
}

// OrphanReport counts observations, not unique lifetime jobs. Stopped means the
// persistent fence and whole-service drain succeeded; it is not a job result.
type OrphanReport struct{ Examined, Managed, Stopped, Unresolved int }

// OrphanPage advances by canonical job ID. Restart with an empty cursor after
// Done to discover services inserted before the cursor during the previous pass.
type OrphanPage struct {
	OrphanReport
	Next string
	Done bool
}

// OrphanScanner fences services absent from the journal or already terminal.
// It never creates claims, reconstructs receipts, or changes recorded results.
// Journal read errors still trigger independent containment cleanup but remain
// unresolved errors, even when that cleanup succeeds.
type OrphanScanner struct {
	store             *workerstore.Store
	reaper            *Reaper
	maximum, pageSize int
}

// NewOrphanScanner requires independent enumeration and explicit size bounds.
func NewOrphanScanner(cfg OrphanConfig) (*OrphanScanner, error) {
	if cfg.Store == nil || cfg.Reaper == nil || cfg.MaxUnits < 1 || cfg.MaxUnits > 10000 || cfg.PageSize < 1 || cfg.PageSize > 256 {
		return nil, fmt.Errorf("cortex: invalid orphan recovery configuration")
	}
	if _, ok := cfg.Reaper.manager.(workerDiscovery); !ok {
		return nil, fmt.Errorf("cortex: service manager cannot discover orphan workers")
	}
	return &OrphanScanner{store: cfg.Store, reaper: cfg.Reaper, maximum: cfg.MaxUnits, pageSize: cfg.PageSize}, nil
}

// Recover visits a complete bounded discovery pass before admission or after
// shutdown. It preserves the first failure while attempting remaining pages.
func (s *OrphanScanner) Recover(ctx context.Context) (OrphanReport, error) {
	var report OrphanReport
	var first error
	cursor := ""
	for {
		page, err := s.Sweep(ctx, cursor)
		report.Examined += page.Examined
		report.Managed += page.Managed
		report.Stopped += page.Stopped
		report.Unresolved += page.Unresolved
		if first == nil {
			first = err
		}
		if page.Done || ctx.Err() != nil || page.Next == cursor {
			return report, errors.Join(first, ctx.Err())
		}
		cursor = page.Next
	}
}

// Sweep processes one page from a fresh manager snapshot. Known nonterminal
// jobs remain with normal execution recovery. Each page has a 30-second budget.
func (s *OrphanScanner) Sweep(ctx context.Context, after string) (OrphanPage, error) {
	page := OrphanPage{Next: after}
	if after != "" {
		key, err := id.ParseWithPrefix(after, sandbox.PrefixJob)
		if err != nil || key.String() != after {
			return page, fmt.Errorf("cortex: invalid orphan recovery cursor")
		}
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keys, err := s.reaper.Workers(operation, s.maximum)
	if err != nil {
		return page, err
	}
	var first error
	page.Done = true
	for _, key := range keys {
		if key.String() <= after {
			continue
		}
		if page.Examined == s.pageSize {
			page.Done = false
			break
		}
		if operationErr := operation.Err(); operationErr != nil {
			page.Done = false
			return page, errors.Join(first, operationErr)
		}
		if checkErr := s.check(operation, key, &page.OrphanReport); checkErr != nil {
			page.Unresolved++
			if first == nil {
				first = checkErr
			}
		}
		page.Examined++
		page.Next = key.String()
	}
	return page, errors.Join(first, operation.Err())
}

func (s *OrphanScanner) check(ctx context.Context, key id.ID, report *OrphanReport) error {
	record, readErr := s.store.Load(ctx, key)
	if readErr == nil && record.Phase != workerstore.Terminal {
		report.Managed++
		return nil
	}
	stopErr := s.reaper.FenceAndStop(ctx, key)
	if stopErr == nil {
		report.Stopped++
	}
	if readErr == broker.ErrNotFound { //nolint:errorlint // Only bare absence proves there is no uncertain journal failure to retain.
		readErr = nil
	}
	return errors.Join(readErr, stopErr, ctx.Err())
}
