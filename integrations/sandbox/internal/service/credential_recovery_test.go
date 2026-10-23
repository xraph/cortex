package service

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

func TestCredentialRecoverySharesControllerPublicationGate(t *testing.T) {
	c, receipt, _ := controllerFixture(t)
	retained := &retainedWorkspaces{}
	scanner, err := newCredentialScanner(OrphanConfig{Store: c.scanner.store, Reaper: c.scanner.reaper, MaxUnits: 10, PageSize: 2}, retained)
	if err != nil {
		t.Fatal(err)
	}
	c.scanner.credentials = scanner
	written := false
	c.launcher.prepare = func(context.Context, LaunchProfile, workerstore.Claim) error { written = true; return nil }
	for range 2 {
		if _, err = NewController(c.launcher, c.scanner, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = c.launcher.store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := c.launcher.store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.launcher.store.Cancel(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.launcher.dispatch(t.Context(), receipt, claim); err == nil || written {
		t.Fatal("controller left delayed credential publication unguarded", err)
	}
	retained.keys = append(retained.keys, receipt.Job.ID)
	report, err := c.scanner.Recover(t.Context())
	if err != nil || report.Credentials.Cleaned != 1 {
		t.Fatal("credential recovery not wired", report, err)
	}
	other, err := newCredentialScanner(OrphanConfig{Store: c.scanner.store, Reaper: c.scanner.reaper, MaxUnits: 10, PageSize: 2}, retained)
	if err != nil {
		t.Fatal(err)
	}
	c.scanner.credentials = other
	if _, err = NewController(c.launcher, c.scanner, 10*time.Millisecond); err == nil {
		t.Fatal("replacement credential gate accepted")
	}
	c.scanner.credentials = nil
	if _, err = NewController(c.launcher, c.scanner, 10*time.Millisecond); err == nil {
		t.Fatal("credential gate silently discarded")
	}
}

func TestRecoveryScannerRejectsEmptyCleanupServices(t *testing.T) {
	c, _, _ := controllerFixture(t)
	for _, kind := range []string{"service", "workspace", "credential"} {
		t.Run(kind, func(t *testing.T) {
			cfg := RecoveryConfig{Store: c.scanner.store, Reaper: c.scanner.reaper, PageSize: 1, Revalidate: c.scanner.revalidate}
			switch kind {
			case "service":
				cfg.Orphans = &OrphanScanner{}
			case "workspace":
				cfg.Workspaces = &WorkspaceScanner{}
			case "credential":
				cfg.Credentials = &CredentialScanner{}
			}
			if value, err := NewRecoveryScanner(cfg); err == nil || value != nil {
				t.Fatal("empty cleanup service accepted")
			}
		})
	}
}
