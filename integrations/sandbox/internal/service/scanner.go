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

// RecoveryConfig supplies trusted host services, independent of a user request.
// Revalidate must check current grants for the signed job scope and principal.
// It must honor context cancellation and must not require the original caller
// to remain connected. A non-nil error stops the job, including provider outages.
type RecoveryConfig struct {
	Store      *workerstore.Store
	Reaper     *Reaper
	Revalidate func(context.Context, sandbox.Job) error
	Now        func() time.Time
	PageSize   int
}

// RecoveryReport counts observations in one scan, not unique lifetime events.
// Unresolved counts entries whose checks or cleanup returned an error.
type RecoveryReport struct {
	Examined, Cancelled, Reaped, Unresolved int
}

// RecoveryPage advances even when an individual entry fails. Continue with Next
// until Done, then start the next pass with an empty cursor to retry failures.
// Done means the index page ended; only a nil error establishes a healthy page.
type RecoveryPage struct {
	RecoveryReport
	Next string
	Done bool
}

// RecoveryScanner reconciles bounded pages without starting or reclaiming jobs.
// The daemon must stop admission while Recover runs, keep admission closed on
// any recovery error, and call Sweep continuously during operation. Sweeps are a
// recovery fallback, not a substitute for worker and external deadline limits.
type RecoveryScanner struct {
	store      *workerstore.Store
	reaper     *Reaper
	revalidate func(context.Context, sandbox.Job) error
	now        func() time.Time
	pageSize   int
}

// NewRecoveryScanner requires explicit grant revalidation and a bounded page.
func NewRecoveryScanner(cfg RecoveryConfig) (*RecoveryScanner, error) {
	if cfg.Store == nil || cfg.Reaper == nil || cfg.Revalidate == nil || cfg.PageSize < 1 || cfg.PageSize > 256 {
		return nil, fmt.Errorf("cortex: invalid sandbox recovery scanner")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RecoveryScanner{store: cfg.Store, reaper: cfg.Reaper, revalidate: cfg.Revalidate, now: cfg.Now, pageSize: cfg.PageSize}, nil
}

// Recover abandons every outstanding acceptance before the daemon admits work.
// Unclaimed jobs are cancelled. Claims are fenced and reaped as failed unless
// cancellation was already requested. One failure does not stop later pages;
// the report retains counts and the first error without unbounded accumulation.
func (s *RecoveryScanner) Recover(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport
	var first error
	cursor := ""
	for {
		page, err := s.scan(ctx, cursor, true)
		report.Examined += page.Examined
		report.Cancelled += page.Cancelled
		report.Reaped += page.Reaped
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

// Sweep checks one index page using fresh records. It retries reaping, cancels
// expired or unauthorized work and leaves currently authorized live work alone.
// Jobs inserted before the cursor are visited on the next complete pass.
func (s *RecoveryScanner) Sweep(ctx context.Context, after string) (RecoveryPage, error) {
	return s.scan(ctx, after, false)
}

func (s *RecoveryScanner) scan(ctx context.Context, after string, startup bool) (RecoveryPage, error) {
	page := RecoveryPage{Next: after}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	entries, err := s.store.Pending(ctx, after, s.pageSize)
	if err != nil {
		return page, err
	}
	var failures []error
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return page, errors.Join(append(failures, err)...)
		}
		if err = s.check(ctx, entry.ID, startup, &page.RecoveryReport); err != nil {
			page.Unresolved++
			failures = append(failures, fmt.Errorf("cortex: recover sandbox job %s: %w", entry.ID, err))
		}
		page.Examined++
		page.Next = entry.ID.String()
	}
	page.Done = ctx.Err() == nil && len(entries) < s.pageSize
	return page, errors.Join(append(failures, ctx.Err())...)
}

func (s *RecoveryScanner) check(ctx context.Context, key id.ID, startup bool, report *RecoveryReport) error {
	record, err := s.store.Load(ctx, key)
	if err != nil {
		// The index supplies only a validated service identity. A damaged record
		// cannot justify a terminal write, but must not leave its service running.
		return errors.Join(err, s.reaper.FenceAndStop(ctx, key))
	}
	if record.Phase == workerstore.Terminal {
		return nil
	}
	if record.Phase == workerstore.Reaping || (startup && record.Phase == workerstore.Claimed) {
		return s.reap(ctx, key, report)
	}
	var checkErr error
	if !startup {
		checkErr = s.eligible(ctx, record.Receipt)
		if checkErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	cancelled, err := s.store.Cancel(ctx, key)
	if err != nil {
		// Failure to persist cancellation still requires a service fence. Never
		// publish terminal status from this fallback; the journal must recover.
		return errors.Join(checkErr, err, s.reaper.FenceAndStop(ctx, key))
	}
	if cancelled.CancelRequested {
		report.Cancelled++
	}
	if cancelled.Phase != workerstore.Terminal {
		err = s.reap(ctx, key, report)
	}
	if errors.Is(checkErr, broker.ErrDenied) || errors.Is(checkErr, errAcceptanceExpired) {
		checkErr = nil
	}
	return errors.Join(checkErr, err)
}

func (s *RecoveryScanner) reap(ctx context.Context, key id.ID, report *RecoveryReport) error {
	_, err := s.reaper.Reap(ctx, s.store, key)
	if err != nil {
		// A failed journal write may have prevented Reap from reaching the
		// manager. Still fence the known identity, retaining the original error
		// and unresolved record for a later pass even if this stop succeeds.
		return errors.Join(err, s.reaper.FenceAndStop(ctx, key))
	}
	report.Reaped++
	return nil
}

func (s *RecoveryScanner) eligible(ctx context.Context, receipt sandbox.Receipt) error {
	if err := checkAcceptanceTime(receipt, s.now()); err != nil {
		return err
	}
	if err := receipt.Capabilities.Supports(receipt.Job); err != nil {
		return err
	}
	job, err := sandbox.Seal(receipt.Job)
	if err != nil {
		return err
	}
	check, cancel := context.WithDeadline(ctx, acceptanceDeadline(receipt))
	defer cancel()
	check, bound := context.WithTimeout(check, 30*time.Second)
	defer bound()
	if err = s.revalidate(check, job); err != nil {
		return errors.Join(err, checkAcceptanceTime(receipt, s.now()))
	}
	return errors.Join(check.Err(), checkAcceptanceTime(receipt, s.now()))
}
