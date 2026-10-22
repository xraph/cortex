package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func TestRecoveryRevalidationBindsVerifiedScope(t *testing.T) {
	scanner, receipts, _ := scannerFixture(t, 1)
	scanner.revalidate = func(ctx context.Context, job sandbox.Job) error {
		scope := cortex.ScopeFromContext(ctx)
		if scope.Canonical() != job.Scope.Canonical() {
			t.Errorf("grant lookup inherited unverified scope: %s", scope.Canonical())
		}
		if len(scope.Levels) > 0 {
			scope.Levels[0].Value = "mutated"
		}
		return nil
	}
	ctx := cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "foreign"}}})
	if _, err := scanner.Sweep(ctx, ""); err != nil {
		t.Fatal(err)
	}
	record, err := scanner.store.Load(t.Context(), receipts[0].Job.ID)
	if err != nil || record.Receipt.Job.Scope.Canonical() != receipts[0].Job.Scope.Canonical() {
		t.Fatal("grant lookup mutated signed scope")
	}
}

func scannerFixture(t *testing.T, count int, configure ...func(*sandbox.Receipt)) (*RecoveryScanner, []sandbox.Receipt, *fakeManager) {
	t.Helper()
	_, template, _ := recoveryStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := workerstore.Open(t.Context(), workerstore.Config{Database: filepath.Join(directory, "scan.db"), Broker: template.Broker, PublicKey: public, MaxJobs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	receipts := make([]sandbox.Receipt, count)
	for i := range receipts {
		receipt := template
		receipt.Job.ID = id.New(sandbox.PrefixJob)
		receipt.Job, err = sandbox.Seal(receipt.Job)
		if err != nil {
			t.Fatal(err)
		}
		receipt.Signature = nil
		for _, apply := range configure {
			apply(&receipt)
		}
		body, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		receipt.Signature = ed25519.Sign(private, body)
		if _, err = store.Enroll(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
		receipts[i] = receipt
	}
	manager := &fakeManager{}
	reaper := &Reaper{manager: manager, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}}
	scanner, err := NewRecoveryScanner(RecoveryConfig{Store: store, Reaper: reaper, PageSize: 2, Revalidate: func(context.Context, sandbox.Job) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return scanner, receipts, manager
}

func TestStartupRecoveryCancelsUnclaimedAndFencesEveryOldClaim(t *testing.T) {
	scanner, receipts, _ := scannerFixture(t, 5)
	for _, receipt := range receipts[1:] {
		if _, err := scanner.store.Claim(t.Context(), receipt.Job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scanner.store.Cancel(t.Context(), receipts[2].Job.ID); err != nil {
		t.Fatal(err)
	}
	report, err := scanner.Recover(t.Context())
	if err != nil || report.Examined != 5 || report.Cancelled != 1 || report.Reaped != 4 || report.Unresolved != 0 {
		t.Fatalf("incomplete startup recovery: %+v %v", report, err)
	}
	for i, receipt := range receipts {
		record, loadErr := scanner.store.Load(t.Context(), receipt.Job.ID)
		want := sandbox.StateFailed
		if i == 0 || i == 2 {
			want = sandbox.StateCancelled
		}
		if loadErr != nil || record.Phase != workerstore.Terminal || record.Result.State != want {
			t.Fatalf("unsafe restart state: %+v %v", record, loadErr)
		}
		if _, claimErr := scanner.store.Claim(t.Context(), receipt.Job.ID); !errors.Is(claimErr, workerstore.ErrConflict) {
			t.Fatalf("restart reissued claim: %v", claimErr)
		}
	}
}

func TestRecoveryFailureDoesNotStarveLaterPages(t *testing.T) {
	scanner, receipts, _ := scannerFixture(t, 5)
	for _, receipt := range receipts {
		if _, err := scanner.store.Claim(t.Context(), receipt.Job.ID); err != nil {
			t.Fatal(err)
		}
	}
	lost := errors.New("containment unavailable")
	scanner.reaper.kill = func(_ context.Context, key id.ID) (containment.Observation, error) {
		if key == receipts[0].Job.ID {
			return containment.Observation{}, lost
		}
		return containment.Observation{Empty: true}, nil
	}
	report, err := scanner.Recover(t.Context())
	if !errors.Is(err, lost) || report.Examined != 5 || report.Reaped != 4 || report.Unresolved != 1 {
		t.Fatalf("cleanup failure starved scan: %+v %v", report, err)
	}
	record, err := scanner.store.Load(t.Context(), receipts[0].Job.ID)
	if err != nil || record.Phase != workerstore.Reaping {
		t.Fatalf("lost unresolved recovery: %+v %v", record, err)
	}
	scanner.reaper.kill = func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Empty: true}, nil
	}
	report, err = scanner.Recover(t.Context())
	if err != nil || report.Examined != 1 || report.Reaped != 1 {
		t.Fatalf("retry lost failed identity: %+v %v", report, err)
	}
}

func TestPeriodicRecoveryKeepsLiveWorkAndCancelsRevokedWork(t *testing.T) {
	scanner, receipts, manager := scannerFixture(t, 2)
	for _, receipt := range receipts {
		if _, err := scanner.store.Claim(t.Context(), receipt.Job.ID); err != nil {
			t.Fatal(err)
		}
	}
	scanner.revalidate = func(_ context.Context, job sandbox.Job) error {
		job.Argv[0] = "/mutated"
		if job.ID == receipts[1].Job.ID {
			return broker.ErrDenied
		}
		return nil
	}
	page, err := scanner.Sweep(t.Context(), "")
	if err != nil || page.Examined != 2 || page.Reaped != 1 || page.Cancelled != 1 || page.Next == "" {
		t.Fatalf("sweep: %+v %v", page, err)
	}
	live, err := scanner.store.Load(t.Context(), receipts[0].Job.ID)
	if err != nil || live.Phase != workerstore.Claimed || live.Receipt.Job.Argv[0] != "/check" {
		t.Fatalf("live work changed: %+v %v", live, err)
	}
	revoked, err := scanner.store.Load(t.Context(), receipts[1].Job.ID)
	if err != nil || revoked.Result == nil || revoked.Result.State != sandbox.StateCancelled || len(manager.calls) == 0 {
		t.Fatalf("revoked work survived: %+v %v", revoked, err)
	}
	last, err := scanner.Sweep(t.Context(), page.Next)
	if err != nil || !last.Done || last.Examined != 0 {
		t.Fatalf("scan did not finish: %+v %v", last, err)
	}
}

func TestPeriodicRecoveryUsesOriginalDeadlineAndRechecksAfterAuthorization(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "claimed"}[claimed], func(t *testing.T) {
			scanner, receipts, manager := scannerFixture(t, 1)
			receipt := receipts[0]
			if claimed {
				if _, err := scanner.store.Claim(t.Context(), receipt.Job.ID); err != nil {
					t.Fatal(err)
				}
			}
			now := receipt.AcceptedAt
			scanner.now = func() time.Time { return now }
			scanner.revalidate = func(ctx context.Context, _ sandbox.Job) error {
				deadline, ok := ctx.Deadline()
				if !ok || deadline.After(receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)) {
					t.Fatal("authorization outlives receipt")
				}
				now = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
				return nil
			}
			page, err := scanner.Sweep(t.Context(), "")
			if err != nil || page.Cancelled != 1 {
				t.Fatalf("expiry lost: %+v %v", page, err)
			}
			record, err := scanner.store.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateCancelled {
				t.Fatalf("expired work survived: %+v %v", record, err)
			}
			if !claimed && len(manager.calls) != 0 {
				t.Fatal("unclaimed cancellation touched service")
			}
		})
	}
}

func TestRecoveryCancellationAndInvalidCursorDoNotSkipWork(t *testing.T) {
	scanner, _, manager := scannerFixture(t, 2)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	page, err := scanner.Sweep(ctx, "")
	if !errors.Is(err, context.Canceled) || page.Done || page.Next != "" || len(manager.calls) != 0 {
		t.Fatalf("cancelled scan advanced: %+v %v", page, err)
	}
	page, err = scanner.Sweep(t.Context(), "invalid")
	if err == nil || page.Done || page.Examined != 0 {
		t.Fatalf("invalid cursor admitted: %+v %v", page, err)
	}
}

func TestRecoveryReportsAuthorizationOutagesAfterStoppingWork(t *testing.T) {
	for _, outage := range []error{errors.New("grants unavailable"), context.DeadlineExceeded} {
		t.Run(outage.Error(), func(t *testing.T) {
			scanner, receipts, _ := scannerFixture(t, 1)
			if _, err := scanner.store.Claim(t.Context(), receipts[0].Job.ID); err != nil {
				t.Fatal(err)
			}
			scanner.revalidate = func(context.Context, sandbox.Job) error { return outage }
			page, err := scanner.Sweep(t.Context(), "")
			if !errors.Is(err, outage) || page.Unresolved != 1 || page.Reaped != 1 || page.Cancelled != 1 {
				t.Fatalf("authorization outage hidden: %+v %v", page, err)
			}
			record, err := scanner.store.Load(t.Context(), receipts[0].Job.ID)
			if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateCancelled {
				t.Fatalf("outage left execution authorized: %+v %v", record, err)
			}
		})
	}
}

func TestRecoveryExpiryAndClockFailureDoNotDependOnGrantProvider(t *testing.T) {
	for _, mode := range []string{"qualification", "clock"} {
		t.Run(mode, func(t *testing.T) {
			scanner, receipts, _ := scannerFixture(t, 1, func(r *sandbox.Receipt) { r.Capabilities.QualifiedUntil = r.AcceptedAt.Add(10 * time.Second) })
			if _, err := scanner.store.Claim(t.Context(), receipts[0].Job.ID); err != nil {
				t.Fatal(err)
			}
			scanner.revalidate = func(context.Context, sandbox.Job) error {
				t.Fatal("invalid acceptance reached grant provider")
				return nil
			}
			scanner.now = func() time.Time {
				if mode == "clock" {
					return time.Time{}
				}
				return receipts[0].Capabilities.QualifiedUntil
			}
			page, err := scanner.Sweep(t.Context(), "")
			if page.Cancelled != 1 || page.Reaped != 1 || (mode == "qualification" && err != nil) || (mode == "clock" && err == nil) {
				t.Fatalf("invalid time did not stop work: %+v %v", page, err)
			}
		})
	}
}

func TestRecoveryFencesKnownServiceWhenRecordCannotBeRead(t *testing.T) {
	scanner, receipts, _ := scannerFixture(t, 2)
	scanner.revalidate = func(context.Context, sandbox.Job) error { return scanner.store.Close() }
	var fenced []id.ID
	scanner.reaper.check = func(_ context.Context, key id.ID) error { fenced = append(fenced, key); return nil }
	page, err := scanner.Sweep(t.Context(), "")
	if err == nil || page.Examined != 2 || page.Unresolved != 1 || page.Reaped != 0 || len(fenced) != 1 || fenced[0] != receipts[1].Job.ID {
		t.Fatalf("unreadable journal left known service alive: %+v %v fenced=%v", page, err, fenced)
	}
}

func TestRecoveryBoundsAndCancellationDuringAuthorization(t *testing.T) {
	scanner, receipts, manager := scannerFixture(t, 1)
	for _, size := range []int{0, -1, 257} {
		if _, err := NewRecoveryScanner(RecoveryConfig{Store: scanner.store, Reaper: scanner.reaper, PageSize: size, Revalidate: scanner.revalidate}); err == nil {
			t.Fatal("unbounded scan configuration accepted")
		}
	}
	if _, err := NewRecoveryScanner(RecoveryConfig{Store: scanner.store, Reaper: scanner.reaper, PageSize: 1}); err == nil {
		t.Fatal("missing grants provider accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scanner.revalidate = func(context.Context, sandbox.Job) error { cancel(); return nil }
	page, err := scanner.Sweep(ctx, "")
	if !errors.Is(err, context.Canceled) || page.Done || len(manager.calls) != 0 {
		t.Fatalf("shutdown reported a healthy sweep: %+v %v", page, err)
	}
	record, err := scanner.store.Load(t.Context(), receipts[0].Job.ID)
	if err != nil || record.Phase != workerstore.Accepted {
		t.Fatalf("request cancellation changed work: %+v %v", record, err)
	}
}

func TestRecoveryFencesAfterCancellationWriteFailure(t *testing.T) {
	scanner, receipts, _ := scannerFixture(t, 1)
	if _, err := scanner.store.Claim(t.Context(), receipts[0].Job.ID); err != nil {
		t.Fatal(err)
	}
	scanner.revalidate = func(context.Context, sandbox.Job) error {
		if err := scanner.store.Close(); err != nil {
			t.Fatal(err)
		}
		return broker.ErrDenied
	}
	var fenced []id.ID
	scanner.reaper.check = func(_ context.Context, key id.ID) error { fenced = append(fenced, key); return nil }
	page, err := scanner.Sweep(t.Context(), "")
	if err == nil || page.Unresolved != 1 || page.Reaped != 0 || page.Cancelled != 0 || len(fenced) != 1 || fenced[0] != receipts[0].Job.ID {
		t.Fatalf("journal failure bypassed service fence: %+v %v fenced=%v", page, err, fenced)
	}
}
