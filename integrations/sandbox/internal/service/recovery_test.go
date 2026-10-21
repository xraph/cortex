package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func recoveryStore(t *testing.T) (*workerstore.Store, sandbox.Receipt, workerstore.Config) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	limits := sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024}
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "recovery", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt := sandbox.Receipt{SchemaVersion: 1, Job: job, Broker: "trusted", AcceptedAt: now, Capabilities: sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: strings.Repeat("b", 64), Profile: sandbox.ProfileDenyAll, Qualification: "fixture-not-qualified", QualifiedUntil: now.Add(time.Hour), Limits: limits}}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(private, body)
	cfg := workerstore.Config{Database: filepath.Join(directory, "worker.db"), Broker: "trusted", PublicKey: public, MaxJobs: 10}
	store, err := workerstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err = store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	return store, receipt, cfg
}

func TestRecoveryCommitsOnlyAfterDurableFenceAndCleanup(t *testing.T) {
	store, receipt, _ := recoveryStore(t)
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeManager{fail: "stop"}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}}
	if _, err = r.Reap(t.Context(), store, receipt.Job.ID); err == nil {
		t.Fatal("failed stop marked reaped")
	}
	record, err := store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Reaping || !m.masked {
		t.Fatalf("lost unresolved claim: %+v %v", record, err)
	}
	if _, err = store.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0}); !errors.Is(err, workerstore.ErrConflict) {
		t.Fatalf("late worker overwrote reaping: %v", err)
	}
	m.fail = ""
	record, err = r.Reap(t.Context(), store, receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateFailed {
		t.Fatalf("reap retry: %+v %v", record, err)
	}
	calls := len(m.calls)
	if _, err = r.Reap(t.Context(), store, receipt.Job.ID); err != nil || len(m.calls) != calls {
		t.Fatal("terminal retry repeated service operation")
	}
}

func TestConcurrentCancellationFencesStaleReaperRevision(t *testing.T) {
	store, receipt, _ := recoveryStore(t)
	if _, err := store.Claim(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	m := &fakeManager{}
	changed := false
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(ctx context.Context, key id.ID) (containment.Observation, error) {
		if !changed {
			changed = true
			if _, err := store.Cancel(ctx, key); err != nil {
				t.Fatal(err)
			}
		}
		return containment.Observation{Empty: true}, nil
	}}
	if _, err := r.Reap(t.Context(), store, receipt.Job.ID); !errors.Is(err, workerstore.ErrConflict) {
		t.Fatalf("stale cleanup revision committed: %v", err)
	}
	record, err := r.Reap(t.Context(), store, receipt.Job.ID)
	if err != nil || record.Result == nil || record.Result.State != sandbox.StateCancelled {
		t.Fatalf("cancellation lost: %+v %v", record, err)
	}
}

func TestUnclaimedRecoveryNeverTouchesServiceManager(t *testing.T) {
	store, receipt, _ := recoveryStore(t)
	m := &fakeManager{}
	r := &Reaper{manager: m}
	if _, err := r.Reap(t.Context(), store, receipt.Job.ID); !errors.Is(err, workerstore.ErrConflict) || len(m.calls) != 0 {
		t.Fatal("unclaimed work reached manager")
	}
}

func TestLostTerminalWriteRecoversFromDurableReaping(t *testing.T) {
	store, receipt, cfg := recoveryStore(t)
	if _, err := store.Claim(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	m := &fakeManager{}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return containment.Observation{Empty: true}, nil
	}}
	if _, err := r.Reap(t.Context(), store, receipt.Job.ID); err == nil {
		t.Fatal("lost final database write accepted")
	}
	reopened, err := workerstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, err := reopened.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Reaping || !m.masked {
		t.Fatalf("unresolved job lost after reopen: %+v %v", record, err)
	}
	r.kill = func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}
	record, err = r.Reap(t.Context(), reopened, receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal {
		t.Fatalf("recovery after lost terminal write: %+v %v", record, err)
	}
}
