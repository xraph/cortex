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

// CleanupReport counts observations, not unique lifetime jobs. Cleaned means
// service fencing and the selected cleanup operation succeeded, never a result.
type CleanupReport struct{ Examined, Managed, Cleaned, Unresolved int }

// CleanupPage advances by canonical job ID. Restart with an empty cursor after
// Done to discover entries inserted before the cursor during the previous pass.
type CleanupPage struct {
	CleanupReport
	Next string
	Done bool
}

type cleanupScanner struct {
	store             *workerstore.Store
	reaper            *Reaper
	maximum, pageSize int
	discover          func(context.Context, int) ([]id.ID, error)
	cleanup           func(context.Context, id.ID) error
}

func newCleanupScanner(cfg OrphanConfig, discover func(context.Context, int) ([]id.ID, error), cleanup func(context.Context, id.ID) error) (*cleanupScanner, error) {
	if cfg.Store == nil || cfg.Reaper == nil || discover == nil || cleanup == nil || cfg.MaxUnits < 1 || cfg.MaxUnits > 10000 || cfg.PageSize < 1 || cfg.PageSize > 256 {
		return nil, fmt.Errorf("cortex: invalid detached cleanup configuration")
	}
	return &cleanupScanner{store: cfg.Store, reaper: cfg.Reaper, maximum: cfg.MaxUnits, pageSize: cfg.PageSize, discover: discover, cleanup: cleanup}, nil
}

// Recover visits a complete bounded discovery pass before admission or after
// shutdown. It preserves the first failure while attempting remaining pages.
func (s *cleanupScanner) Recover(ctx context.Context) (CleanupReport, error) {
	var report CleanupReport
	var first error
	cursor := ""
	for {
		page, err := s.Sweep(ctx, cursor)
		report.Examined += page.Examined
		report.Managed += page.Managed
		report.Cleaned += page.Cleaned
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

// Sweep processes one page from a fresh discovery snapshot. Known nonterminal
// jobs remain with normal execution recovery. Each page has a 30-second budget.
func (s *cleanupScanner) Sweep(ctx context.Context, after string) (CleanupPage, error) {
	page := CleanupPage{Next: after}
	if after != "" {
		key, err := id.ParseWithPrefix(after, sandbox.PrefixJob)
		if err != nil || key.String() != after {
			return page, fmt.Errorf("cortex: invalid detached cleanup cursor")
		}
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keys, err := s.discover(operation, s.maximum)
	if err != nil {
		return page, err
	}
	keys, err = validateDiscoveredWorkers(operation, keys, s.maximum)
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
		if checkErr := s.check(operation, key, &page.CleanupReport); checkErr != nil {
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

func (s *cleanupScanner) check(ctx context.Context, key id.ID, report *CleanupReport) error {
	record, readErr := s.store.Load(ctx, key)
	if readErr == nil && record.Phase != workerstore.Terminal {
		report.Managed++
		return nil
	}
	stopErr := s.cleanup(ctx, key)
	if stopErr == nil {
		report.Cleaned++
	}
	if readErr == broker.ErrNotFound { //nolint:errorlint // Only bare absence proves there is no uncertain journal failure to retain.
		readErr = nil
	}
	return errors.Join(readErr, stopErr, ctx.Err())
}

func (s *cleanupScanner) boundTo(store *workerstore.Store, reaper *Reaper) bool {
	return s != nil && s.store == store && s.reaper == reaper && s.discover != nil && s.cleanup != nil
}
