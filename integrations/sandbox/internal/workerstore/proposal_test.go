package workerstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/sandbox"
)

func proposalFixture(t *testing.T) (*Store, Config, sandbox.Receipt, Claim, sandbox.Result) {
	t.Helper()
	cfg, receipt := fixture(t)
	store := openStore(t, cfg)
	if _, err := store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("complete output")}
	return store, cfg, receipt, claim, result
}

func TestProposalSurvivesRestartWithoutBecomingTerminal(t *testing.T) {
	store, cfg, receipt, claim, result := proposalFixture(t)
	proposed, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil || proposed.Phase != Finalizing || proposed.Result != nil {
		t.Fatalf("proposal not retained: %+v %v", proposed, err)
	}
	result.Stdout[0] = 'X'
	if _, err = store.VerifyClaim(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("proposed result retained execution authority")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cfg)
	pending, err := reopened.Pending(t.Context(), "", 10)
	if err != nil || len(pending) != 1 || pending[0].Phase != Finalizing {
		t.Fatalf("proposal omitted from recovery: %+v %v", pending, err)
	}
	if _, err = reopened.Finalized(t.Context(), receipt.Job.ID, proposed.Revision-1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale cleanup revision accepted")
	}
	final, err := reopened.Finalized(t.Context(), receipt.Job.ID, proposed.Revision)
	if err != nil || final.Result == nil || string(final.Result.Stdout) != "complete output" || final.Phase != Terminal {
		t.Fatalf("finalized: %+v %v", final, err)
	}
	if _, err = reopened.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("finalized identity reused")
	}
}

func TestProposalCancellationWinsOverStaleCleanup(t *testing.T) {
	store, _, receipt, claim, result := proposalFixture(t)
	proposed, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.Cancel(t.Context(), receipt.Job.ID)
	if err != nil || cancelled.Phase != Reaping || cancelled.Result != nil {
		t.Fatalf("cancellation bypassed cleanup: %+v %v", cancelled, err)
	}
	if _, err = store.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("stale finalizer overwrote cancellation")
	}
	if _, err = store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); !errors.Is(err, ErrConflict) {
		t.Fatal("canceled proposal replay accepted")
	}
	final, err := store.Reaped(t.Context(), receipt.Job.ID, cancelled.Revision)
	if err != nil || final.Result.State != sandbox.StateCancelled {
		t.Fatal("cancellation lost", err)
	}
}

func TestProposalRejectsChangedOrExpiredOwnership(t *testing.T) {
	for _, mode := range []string{"token", "revision", "expired", "changed result", "cleanup expired"} {
		t.Run(mode, func(t *testing.T) {
			store, _, receipt, claim, result := proposalFixture(t)
			now := receipt.AcceptedAt.Add(time.Second)
			store.cfg.Now = func() time.Time { return now }
			switch mode {
			case "token":
				claim.Token = strings.Repeat("0", 64)
			case "revision":
				claim.Record.Revision++
			case "expired":
				now = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
			case "changed result", "cleanup expired":
				proposed, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "cleanup expired" {
					now = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
					if _, err = store.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("expired cleanup finalized", err)
					}
					return
				}
				result.Stdout = []byte("different")
			}
			if _, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err == nil {
				t.Fatal("invalid proposal accepted")
			}
		})
	}
}

func TestProposalConcurrentReplayHasOneDurableRevision(t *testing.T) {
	store, _, receipt, claim, result := proposalFixture(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			record, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
			if err != nil || record.Revision != claim.Record.Revision+1 {
				t.Errorf("duplicate proposal transition: %v", err)
			}
		})
	}
	wg.Wait()
	record, err := store.Load(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Proposal.Result.Stdout[0] = 'X'
	saved, err := store.Load(t.Context(), receipt.Job.ID)
	if err != nil || string(saved.Proposal.Result.Stdout) != "complete output" {
		t.Fatal("proposal returned aliased state")
	}
}

func TestProposalFailedWritesRemainRecoverable(t *testing.T) {
	store, cfg, receipt, claim, result := proposalFixture(t)
	trigger := "CREATE TRIGGER fail_worker_update BEFORE UPDATE ON worker_jobs BEGIN SELECT RAISE(ABORT,'injected write failure'); END"
	if _, err := store.journal.Conn.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	if record, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err == nil || !record.Receipt.Job.ID.IsNil() {
		t.Fatal("failed proposal write acknowledged")
	}
	if _, err := store.journal.Conn.ExecContext(t.Context(), "DROP TRIGGER fail_worker_update"); err != nil {
		t.Fatal(err)
	}
	proposed, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.journal.Conn.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	if record, writeErr := store.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); writeErr == nil || !record.Receipt.Job.ID.IsNil() {
		t.Fatal("failed finalization write acknowledged")
	}
	if _, err = store.journal.Conn.ExecContext(t.Context(), "DROP TRIGGER fail_worker_update"); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cfg)
	saved, err := reopened.Load(t.Context(), receipt.Job.ID)
	if err != nil || saved.Phase != Finalizing || saved.Result != nil {
		t.Fatal("failed finalization lost pending report", err)
	}
	if _, err = reopened.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestProposalJournalMigratesLegacyClaimsWithoutNewAuthority(t *testing.T) {
	store, cfg, receipt, claim, result := proposalFixture(t)
	old := claim.Record
	old.SchemaVersion = 1
	body, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.journal.Conn.ExecContext(t.Context(), "UPDATE worker_jobs SET body=? WHERE id=?", body, receipt.Job.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.journal.Conn.ExecContext(t.Context(), "PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cfg)
	var version int
	if err = reopened.journal.Conn.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != JournalVersion {
		t.Fatal("journal migration failed", err)
	}
	if _, err = reopened.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("migration reissued claim")
	}
	saved, err := reopened.Load(t.Context(), receipt.Job.ID)
	if err != nil || saved.SchemaVersion != 1 || saved.Revision != claim.Record.Revision {
		t.Fatal("migration rewrote existing claim", err)
	}
	proposed, err := reopened.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil || proposed.SchemaVersion != RecordVersion {
		t.Fatal("legacy claim could not transition", err)
	}
}

func TestProposalRejectsCorruptReportBindings(t *testing.T) {
	for _, mode := range []string{"digest", "time", "revision", "discarded", "legacy", "terminal failed output"} {
		t.Run(mode, func(t *testing.T) {
			store, _, receipt, claim, result := proposalFixture(t)
			if mode == "terminal failed output" {
				result.State = sandbox.StateFailed
				result.ExitCode = -1
			}
			record, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "digest":
				record.Proposal.Digest = strings.Repeat("0", 64)
			case "time":
				record.Proposal.ReceivedAt = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
			case "revision":
				record.Proposal.ClaimRevision++
			case "discarded":
				record.Proposal.Discarded = true
			case "legacy":
				record.SchemaVersion = 1
			case "terminal failed output":
				record, err = store.Finalized(t.Context(), receipt.Job.ID, record.Revision)
				if err != nil {
					t.Fatal(err)
				}
				record.Result.Stdout = []byte("changed")
			}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.journal.Conn.ExecContext(t.Context(), "UPDATE worker_jobs SET body=? WHERE id=?", body, receipt.Job.ID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err = store.Load(t.Context(), receipt.Job.ID); !errors.Is(err, ErrCorrupt) {
				t.Fatal("corrupt proposal accepted", err)
			}
		})
	}
}

func TestProposalUsesTrustedClockForQualificationExpiry(t *testing.T) {
	cfg, receipt, key := fixtureWithKey(t)
	receipt.Capabilities.QualifiedUntil = receipt.AcceptedAt.Add(30 * time.Second)
	receipt = sign(t, receipt, key)
	now := receipt.AcceptedAt
	cfg.Now = func() time.Time { return now }
	store := openStore(t, cfg)
	if _, err := store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = receipt.Capabilities.QualifiedUntil
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}
	if _, err = store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("trusted clock did not enforce qualification expiry", err)
	}
}

func TestProposalFinalizationUsesAbsoluteDeadlineWhenClockStalls(t *testing.T) {
	cfg, receipt, key := fixtureWithKey(t)
	receipt.Job.Limits.WallTime = time.Second
	var err error
	receipt.Job, err = sandbox.Seal(receipt.Job)
	if err != nil {
		t.Fatal(err)
	}
	receipt = sign(t, receipt, key)
	cfg.Now = func() time.Time { return receipt.AcceptedAt }
	store := openStore(t, cfg)
	if _, err = store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}
	proposed, err := store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	deadline := receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
	timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if _, err = store.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stalled clock allowed a late final write", err)
	}
	saved, err := store.Load(t.Context(), receipt.Job.ID)
	if err != nil || saved.Phase != Finalizing || saved.Result != nil {
		t.Fatal("deadline failure committed result", err)
	}
}
