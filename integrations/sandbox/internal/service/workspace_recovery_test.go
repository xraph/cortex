package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type retainedWorkspaces struct {
	keys    []id.ID
	removed []id.ID
	failure error
}

func (w *retainedWorkspaces) Discover(ctx context.Context, _ int) ([]id.ID, error) {
	return slices.Clone(w.keys), ctx.Err()
}
func (w *retainedWorkspaces) Reclaim(ctx context.Context, key id.ID) error {
	w.removed = append(w.removed, key)
	return errors.Join(w.failure, ctx.Err())
}

func TestWorkspaceRecoveryKeepsManagedJobsAndRetriesCleanup(t *testing.T) {
	_, scanner, receipts, _ := orphanFixture(t)
	missing := id.New(sandbox.PrefixJob)
	retained := &retainedWorkspaces{keys: []id.ID{missing, receipts[0].Job.ID}}
	recovery, err := newWorkspaceScanner(OrphanConfig{Store: scanner.store, Reaper: scanner.reaper, MaxUnits: 8, PageSize: 1}, retained)
	if err != nil {
		t.Fatal(err)
	}
	report, err := recovery.Recover(t.Context())
	if err != nil || report.Managed != 1 || report.Cleaned != 1 || !slices.Equal(retained.removed, []id.ID{missing}) {
		t.Fatal(report, retained.removed, err)
	}
	retained.failure = errors.New("storage uncertain")
	report, err = recovery.Recover(t.Context())
	if err == nil || report.Unresolved != 1 || report.Cleaned != 0 {
		t.Fatal(report, err)
	}
	retained.failure = nil
	if _, err = scanner.store.Cancel(t.Context(), receipts[0].Job.ID); err != nil {
		t.Fatal(err)
	}
	scanner.workspaces = recovery
	reportAll, err := scanner.Recover(t.Context())
	if err != nil || reportAll.Workspaces.Cleaned != 2 {
		t.Fatal("startup did not reclaim after execution recovery", reportAll, err)
	}
}

func TestWorkspaceRecoveryFailureClosesAdmission(t *testing.T) {
	c, _, _ := controllerFixture(t)
	retained := &retainedWorkspaces{}
	scanner, err := newWorkspaceScanner(OrphanConfig{Store: c.scanner.store, Reaper: c.scanner.reaper, MaxUnits: 8, PageSize: 1}, retained)
	if err != nil {
		t.Fatal(err)
	}
	c.scanner.workspaces = scanner
	// Startup success is followed by a new orphan whose storage cannot be removed.
	// Discover synchronizes the transition inside the controller goroutine.
	source := &changingWorkspaces{retainedWorkspaces: retained}
	scanner.discover = source.Discover
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case runErr := <-done:
		if runErr == nil || c.Status().Ready {
			t.Fatal("cleanup failure retained admission", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workspace recovery did not run periodically")
	}
}

type changingWorkspaces struct {
	*retainedWorkspaces
	calls int
}

func (w *changingWorkspaces) Discover(ctx context.Context, maximum int) ([]id.ID, error) {
	w.calls++
	if w.calls > 1 {
		w.keys = []id.ID{id.New(sandbox.PrefixJob)}
		w.failure = errors.New("cleanup failed")
	}
	return w.retainedWorkspaces.Discover(ctx, maximum)
}
