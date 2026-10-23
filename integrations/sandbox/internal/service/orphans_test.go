package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

type orphanManager struct {
	fakeManager
	mu           sync.Mutex
	units        []id.ID
	listErr      error
	stoppedUnits []string
}

func (m *orphanManager) Workers(ctx context.Context, _ int) ([]id.ID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.units), errors.Join(m.listErr, ctx.Err())
}

func (m *orphanManager) Stop(ctx context.Context, unit string) error {
	m.stoppedUnits = append(m.stoppedUnits, unit)
	return m.fakeManager.Stop(ctx, unit)
}

func orphanFixture(t *testing.T) (*OrphanScanner, *RecoveryScanner, []sandbox.Receipt, *orphanManager) {
	t.Helper()
	scanner, receipts, _ := scannerFixture(t, 2)
	manager := &orphanManager{}
	scanner.reaper.manager = manager
	orphans, err := NewOrphanScanner(OrphanConfig{Store: scanner.store, Reaper: scanner.reaper, MaxUnits: 8, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	return orphans, scanner, receipts, manager
}

func TestOrphansFenceMissingAndTerminalJobsWithoutClaiming(t *testing.T) {
	orphans, scanner, receipts, manager := orphanFixture(t)
	for _, receipt := range receipts {
		if _, err := scanner.store.Claim(t.Context(), receipt.Job.ID); err != nil {
			t.Fatal(err)
		}
	}
	stopped, err := scanner.store.Cancel(t.Context(), receipts[1].Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scanner.store.Reaped(t.Context(), receipts[1].Job.ID, stopped.Revision); err != nil {
		t.Fatal(err)
	}
	missing := id.New(sandbox.PrefixJob)
	manager.units = []id.ID{missing, receipts[1].Job.ID, receipts[0].Job.ID}
	report, err := orphans.Recover(t.Context())
	if err != nil || report.Examined != 3 || report.Managed != 1 || report.Stopped != 2 || report.Unresolved != 0 || len(manager.stoppedUnits) != 2 {
		t.Fatal("incorrect independent recovery", report, manager.stoppedUnits, err)
	}
	if _, err = scanner.store.Load(t.Context(), missing); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal("orphan cleanup created a claim or terminal result", err)
	}
	if current, loadErr := scanner.store.Load(t.Context(), receipts[0].Job.ID); loadErr != nil || current.Phase != workerstore.Claimed {
		t.Fatal("live managed worker disturbed", loadErr)
	}
}

func TestOrphansRetainFailedCleanupAndUncertainJournal(t *testing.T) {
	for _, mode := range []string{"cleanup", "journal"} {
		t.Run(mode, func(t *testing.T) {
			orphans, scanner, _, manager := orphanFixture(t)
			manager.units = []id.ID{id.New(sandbox.PrefixJob)}
			if mode == "cleanup" {
				manager.fail = "stop"
			} else if err := scanner.store.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := orphans.Recover(t.Context())
			if err == nil || report.Unresolved != 1 || len(manager.stoppedUnits) != 1 {
				t.Fatal("unresolved orphan hidden", report, err)
			}
			if mode == "cleanup" {
				if report.Stopped != 0 {
					t.Fatal("failed stop acknowledged")
				}
				manager.fail = ""
				report, err = orphans.Recover(t.Context())
				if err != nil || report.Stopped != 1 {
					t.Fatal("orphan retry failed", report, err)
				}
			} else if report.Stopped != 1 {
				t.Fatal("journal failure prevented independent fence")
			}
		})
	}
}

func TestOrphansRefuseInvalidSnapshotsAndCursorsBeforeMutation(t *testing.T) {
	for _, mode := range []string{"foreign", "duplicate", "oversized", "discovery", "cursor", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			orphans, _, _, manager := orphanFixture(t)
			manager.units = []id.ID{id.New(sandbox.PrefixJob)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cursor := ""
			switch mode {
			case "foreign":
				manager.units = []id.ID{id.New(id.PrefixAgent)}
			case "duplicate":
				manager.units = append(manager.units, manager.units[0])
			case "oversized":
				for range 8 {
					manager.units = append(manager.units, id.New(sandbox.PrefixJob))
				}
			case "discovery":
				manager.listErr = errors.New("listing failed")
			case "cursor":
				cursor = "invalid"
			case "cancel":
				cancel()
			}
			if _, err := orphans.Sweep(ctx, cursor); err == nil || len(manager.calls) != 0 {
				t.Fatal("ambiguous discovery mutated services", err)
			}
		})
	}
}

func TestControllerRunsIndependentDiscoveryBeforeAdmissionAndPeriodically(t *testing.T) {
	c, _, _ := controllerFixture(t)
	manager := &orphanManager{}
	c.launcher.reaper.manager = manager
	orphans, err := NewOrphanScanner(OrphanConfig{Store: c.launcher.store, Reaper: c.launcher.reaper, MaxUnits: 8, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.scanner.orphans = orphans
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	awaitControllerReady(t, c)
	manager.mu.Lock()
	manager.listErr = errors.New("periodic discovery failed")
	manager.mu.Unlock()
	select {
	case runErr := <-done:
		if runErr == nil || c.Status().Ready {
			t.Fatal("discovery failure retained admission")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("periodic discovery was not run")
	}
}

func TestControllerWithholdsAdmissionAfterFailedOrphanRecovery(t *testing.T) {
	c, _, _ := controllerFixture(t)
	manager := &orphanManager{units: []id.ID{id.New(sandbox.PrefixJob)}}
	manager.fail = "stop"
	c.launcher.reaper.manager = manager
	orphans, err := NewOrphanScanner(OrphanConfig{Store: c.launcher.store, Reaper: c.launcher.reaper, MaxUnits: 8, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.scanner.orphans = orphans
	if err = c.Run(t.Context()); err == nil || c.Status().Ready || c.Status().Running || len(manager.stoppedUnits) != 1 {
		t.Fatal("failed orphan recovery admitted work", err)
	}
}

func TestOrphanPagesRevisitInsertionsBeforeCursorOnNextPass(t *testing.T) {
	orphans, _, _, manager := orphanFixture(t)
	keys := []id.ID{id.New(sandbox.PrefixJob), id.New(sandbox.PrefixJob), id.New(sandbox.PrefixJob)}
	slices.SortFunc(keys, func(a, b id.ID) int { return strings.Compare(a.String(), b.String()) })
	manager.units = keys[1:]
	first, err := orphans.Sweep(t.Context(), "")
	if err != nil || first.Done || first.Stopped != 1 || first.Next != keys[1].String() {
		t.Fatal(first, err)
	}
	manager.units = keys
	second, err := orphans.Sweep(t.Context(), first.Next)
	if err != nil || !second.Done || second.Stopped != 1 || second.Next != keys[2].String() {
		t.Fatal(second, err)
	}
	next, err := orphans.Sweep(t.Context(), "")
	if err != nil || next.Next != keys[0].String() || next.Stopped != 1 {
		t.Fatal("new earlier service lost", next, err)
	}
}

func TestOrphanRecoveryRejectsMissingDiscoveryAndMismatchedOwnership(t *testing.T) {
	orphans, scanner, _, _ := orphanFixture(t)
	for _, mode := range []string{"store", "reaper", "discovery", "maximum", "page"} {
		cfg := OrphanConfig{Store: scanner.store, Reaper: scanner.reaper, MaxUnits: 8, PageSize: 1}
		switch mode {
		case "store":
			cfg.Store = nil
		case "reaper":
			cfg.Reaper = nil
		case "discovery":
			cfg.Reaper = &Reaper{manager: &fakeManager{}}
		case "maximum":
			cfg.MaxUnits = 10001
		case "page":
			cfg.PageSize = 257
		}
		if value, err := NewOrphanScanner(cfg); err == nil || value != nil {
			t.Fatal("incomplete recovery accepted", mode)
		}
	}
	other, _, _ := scannerFixture(t, 0)
	for _, wrongReaper := range []bool{false, true} {
		cfg := RecoveryConfig{Store: scanner.store, Reaper: scanner.reaper, Orphans: orphans, PageSize: 1, Revalidate: scanner.revalidate}
		if wrongReaper {
			cfg.Reaper = other.reaper
		} else {
			cfg.Store = other.store
		}
		if value, err := NewRecoveryScanner(cfg); err == nil || value != nil {
			t.Fatal("orphan recovery crossed journal ownership")
		}
	}
}
