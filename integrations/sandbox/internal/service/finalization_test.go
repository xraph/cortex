package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func proposedScanner(t *testing.T) (*RecoveryScanner, sandbox.Receipt, *fakeManager) {
	t.Helper()
	scanner, receipts, manager := scannerFixture(t, 1)
	receipt := receipts[0]
	claim, err := scanner.store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("retained")}
	if _, err = scanner.store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err != nil {
		t.Fatal(err)
	}
	return scanner, receipt, manager
}

func TestProposalRecoveryFinalizesOnlyAfterWholeServiceCleanup(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(map[bool]string{false: "periodic", true: "startup"}[startup], func(t *testing.T) {
			scanner, receipt, manager := proposedScanner(t)
			manager.fail = "stop"
			if _, err := scanner.scan(t.Context(), "", startup); err == nil {
				t.Fatal("failed cleanup accepted")
			}
			record, err := scanner.store.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Phase != workerstore.Finalizing || record.Result != nil {
				t.Fatalf("failed cleanup lost proposal: %+v %v", record, err)
			}
			manager.fail = ""
			report, err := scanner.scan(t.Context(), "", startup)
			if err != nil || report.Reaped != 1 {
				t.Fatalf("proposal recovery failed: %+v %v", report, err)
			}
			record, err = scanner.store.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateExited || string(record.Result.Stdout) != "retained" {
				t.Fatalf("proposal changed: %+v %v", record, err)
			}
		})
	}
}

func TestProposalRecoveryRechecksGrantsAndCancellationAfterCleanup(t *testing.T) {
	for _, mode := range []string{"revoked", "provider failure", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			scanner, receipt, _ := proposedScanner(t)
			changed := false
			scanner.reaper.kill = func(ctx context.Context, key id.ID) (containment.Observation, error) {
				if !changed {
					changed = true
					switch mode {
					case "revoked":
						scanner.revalidate = func(context.Context, sandbox.Job) error { return broker.ErrDenied }
					case "provider failure":
						scanner.revalidate = func(context.Context, sandbox.Job) error { return errors.New("provider unavailable") }
					case "cancelled":
						if _, err := scanner.store.Cancel(ctx, key); err != nil {
							t.Fatal(err)
						}
					case "expired":
						scanner.now = func() time.Time { return receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime) }
					}
				}
				return containment.Observation{Empty: true}, nil
			}
			_, err := scanner.Sweep(t.Context(), "")
			if (mode == "provider failure" || mode == "cancelled") && err == nil {
				t.Fatal("unresolved change hidden")
			}
			if mode == "cancelled" {
				if _, err = scanner.Sweep(t.Context(), ""); err != nil {
					t.Fatal(err)
				}
			}
			record, err := scanner.store.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Result == nil || record.Result.State != sandbox.StateCancelled {
				t.Fatalf("changed authority published proposal: %+v %v", record, err)
			}
		})
	}
}

func TestProposalRecoveryRetriesLostFinalWriteAfterReopen(t *testing.T) {
	store, receipt, cfg := recoveryStore(t)
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("retained")}
	if _, err = store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err != nil {
		t.Fatal(err)
	}
	closed := false
	reaper := &Reaper{manager: &fakeManager{}, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		if !closed {
			closed = true
			if closeErr := store.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		return containment.Observation{Empty: true}, nil
	}}
	config := RecoveryConfig{Store: store, Reaper: reaper, PageSize: 2, Revalidate: func(context.Context, sandbox.Job) error { return nil }}
	scanner, err := NewRecoveryScanner(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scanner.Sweep(t.Context(), ""); err == nil {
		t.Fatal("lost final write reported success")
	}
	reopened, err := workerstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err := reopened.Load(t.Context(), receipt.Job.ID)
	if err != nil || pending.Phase != workerstore.Finalizing || pending.Result != nil {
		t.Fatal("reopen lost pending result", err)
	}
	config.Store = reopened
	scanner, err = NewRecoveryScanner(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scanner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	final, err := reopened.Load(t.Context(), receipt.Job.ID)
	if err != nil || final.Result == nil || final.Result.State != sandbox.StateExited || string(final.Result.Stdout) != "retained" {
		t.Fatal("restart did not finalize retained result", err)
	}
}

func TestProposalDoesNotTriggerAnotherLaunch(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	var claim workerstore.Claim
	launcher.prepare = func(_ context.Context, _ LaunchProfile, captured workerstore.Claim) error {
		claim = captured
		return nil
	}
	if err := launcher.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}
	if _, err := launcher.store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err != nil {
		t.Fatal(err)
	}
	if err := launcher.Launch(t.Context(), receipt); err != nil || starts.Load() != 1 {
		t.Fatal("pending finalization was relaunched", err)
	}
}
