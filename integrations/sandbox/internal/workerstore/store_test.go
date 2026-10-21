package workerstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func fixture(t *testing.T) (Config, sandbox.Receipt) {
	cfg, receipt, _ := fixtureWithKey(t)
	return cfg, receipt
}

func fixtureWithKey(t *testing.T) (Config, sandbox.Receipt, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	limits := sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024}
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "one", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	receipt := sandbox.Receipt{SchemaVersion: 1, Job: job, Broker: "trusted", AcceptedAt: now, Capabilities: sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: strings.Repeat("b", 64), Profile: sandbox.ProfileDenyAll, Qualification: "fixture-not-qualified", QualifiedUntil: now.Add(time.Hour), Limits: limits}}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(private, body)
	return Config{Database: filepath.Join(root, "worker.db"), Broker: "trusted", PublicKey: public, MaxJobs: 10}, receipt, private
}

func sign(t *testing.T, receipt sandbox.Receipt, key ed25519.PrivateKey) sandbox.Receipt {
	t.Helper()
	receipt.Signature = nil
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(key, body)
	return receipt
}

func openStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConcurrentClaimHasOneDurableWinner(t *testing.T) {
	cfg, receipt := fixture(t)
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	tokens := make(chan string, 12)
	for range 12 {
		wg.Go(func() {
			claim, err := s.Claim(t.Context(), receipt.Job.ID)
			if err == nil {
				tokens <- claim.Token
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("claim: %v", err)
			}
		})
	}
	wg.Wait()
	close(tokens)
	if len(tokens) != 1 {
		t.Fatalf("winners=%d", len(tokens))
	}
	token := <-tokens
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := openStore(t, cfg)
	if _, err := next.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("restart claimed twice: %v", err)
	}
	result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("result")}
	proposed, err := next.Propose(t.Context(), receipt.Job.ID, token, 2, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.Finalized(t.Context(), receipt.Job.ID, proposed.Revision); err != nil {
		t.Fatal(err)
	}
	first, err := next.Load(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := next.Propose(t.Context(), receipt.Job.ID, token, 2, result)
	if err != nil || second.Revision != first.Revision {
		t.Fatalf("completion replay: %+v %v", second, err)
	}
	result.Stdout = []byte("different")
	if _, err = next.Propose(t.Context(), receipt.Job.ID, token, 2, result); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed terminal result accepted: %v", err)
	}
	if _, err = next.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal identity reused: %v", err)
	}
}

func TestCancellationAndReapingFenceLateWorkers(t *testing.T) {
	for _, beforeClaim := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "after"}[beforeClaim], func(t *testing.T) {
			cfg, receipt := fixture(t)
			s := openStore(t, cfg)
			if _, err := s.Enroll(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
			var claim Claim
			if !beforeClaim {
				var err error
				claim, err = s.Claim(t.Context(), receipt.Job.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			record, err := s.Cancel(t.Context(), receipt.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if beforeClaim {
				if record.Phase != Terminal || record.Result.State != sandbox.StateCancelled {
					t.Fatalf("unstarted cancel: %+v", record)
				}
			} else {
				if record.Phase != Reaping || record.Result != nil {
					t.Fatalf("cleanup falsely complete: %+v", record)
				}
				result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited}
				if _, err = s.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); !errors.Is(err, ErrConflict) {
					t.Fatalf("late worker unfenced: %v", err)
				}
				if _, err = s.Reaped(t.Context(), receipt.Job.ID, record.Revision-1); !errors.Is(err, ErrConflict) {
					t.Fatalf("stale reap accepted: %v", err)
				}
				if record, err = s.Reaped(t.Context(), receipt.Job.ID, record.Revision); err != nil || record.Result.State != sandbox.StateCancelled {
					t.Fatalf("reap: %+v %v", record, err)
				}
			}
			if _, err = s.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("cancelled job claimed: %v", err)
			}
		})
	}
}

func TestWorkerRecoveryNeverRetriesAnAmbiguousClaim(t *testing.T) {
	cfg, receipt := fixture(t)
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.RequireReap(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited}); !errors.Is(err, ErrConflict) {
		t.Fatalf("old owner accepted: %v", err)
	}
	record, err = s.Reaped(t.Context(), receipt.Job.ID, record.Revision)
	if err != nil || record.Result.State != sandbox.StateFailed {
		t.Fatalf("uncertain worker became success: %+v %v", record, err)
	}
}

func TestWorkerStoreRejectsBindingAndDeadlineChanges(t *testing.T) {
	cfg, receipt := fixture(t)
	now := receipt.AcceptedAt
	cfg.Now = func() time.Time { return now }
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	changed := receipt
	changed.AcceptedAt = changed.AcceptedAt.Add(time.Second)
	if _, err := s.Enroll(t.Context(), changed); err == nil {
		t.Fatal("forged deadline accepted")
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired claim: %v", err)
	}
	record, err := s.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != Terminal || record.Result.State != sandbox.StateCancelled {
		t.Fatalf("expiry not retained: %+v %v", record, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Broker = "other"
	if reopened, openErr := Open(t.Context(), cfg); openErr == nil {
		_ = reopened.Close()
		t.Fatal("journal identity changed")
	}
}

func TestWorkerClaimSurvivesProcessExit(t *testing.T) {
	cfg, receipt := fixture(t)
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClaimProcessHelper$")
	cmd.Env = append(os.Environ(), "CORTEX_WORKER_CLAIM_HELPER=1", "CORTEX_WORKER_DB="+cfg.Database, "CORTEX_WORKER_PUBLIC_KEY="+hex.EncodeToString(cfg.PublicKey), "CORTEX_WORKER_JOB="+receipt.Job.ID.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %s %v", out, err)
	}
	next := openStore(t, cfg)
	if _, err := next.Claim(t.Context(), receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("process exit lost claim: %v", err)
	}
	pending, err := next.Pending(t.Context(), "", 10)
	if err != nil || len(pending) != 1 || pending[0].Phase != Claimed {
		t.Fatalf("missing recovery identity: %+v %v", pending, err)
	}
}

func TestClaimProcessHelper(t *testing.T) {
	if os.Getenv("CORTEX_WORKER_CLAIM_HELPER") != "1" {
		return
	}
	key, err := hex.DecodeString(os.Getenv("CORTEX_WORKER_PUBLIC_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := id.Parse(os.Getenv("CORTEX_WORKER_JOB"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), Config{Database: os.Getenv("CORTEX_WORKER_DB"), Broker: "trusted", PublicKey: key, MaxJobs: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	// Skip Close and all defers to exercise durable process-crash ownership.
	os.Exit(0)
}

func TestWorkerJournalCapacityAndResignedReceiptConflict(t *testing.T) {
	cfg, receipt, key := fixtureWithKey(t)
	cfg.MaxJobs = 1
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	changed := receipt
	changed.AcceptedAt = changed.AcceptedAt.Add(time.Nanosecond)
	changed = sign(t, changed, key)
	if _, err := s.Enroll(t.Context(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("replacement acceptance: %v", err)
	}
	if _, err := s.Cancel(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	changed = receipt
	changed.Job.ID = id.New(sandbox.PrefixJob)
	changed.Job.Key = "two"
	var err error
	changed.Job, err = sandbox.Seal(changed.Job)
	if err != nil {
		t.Fatal(err)
	}
	changed = sign(t, changed, key)
	if _, err = s.Enroll(t.Context(), changed); err == nil {
		t.Fatal("terminal tombstone silently evicted")
	}
	if again, loadErr := s.Enroll(t.Context(), receipt); loadErr != nil || again.Phase != Terminal {
		t.Fatalf("terminal retry lost: %+v %v", again, loadErr)
	}
}

func TestWorkerJournalFailedWritesGrantNoOwnership(t *testing.T) {
	cfg, receipt := fixture(t)
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Conn.ExecContext(t.Context(), "CREATE TRIGGER fail_worker_update BEFORE UPDATE ON worker_jobs BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(t.Context(), receipt.Job.ID)
	if err == nil || claim.Token != "" {
		t.Fatalf("failed write granted execution: %+v %v", claim, err)
	}
	record, err := s.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != Accepted || record.TokenHash != "" {
		t.Fatalf("failed write changed claim: %+v %v", record, err)
	}
	if _, err = s.journal.Conn.ExecContext(t.Context(), "DROP TRIGGER fail_worker_update"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerJournalRejectsCorruptionAndFutureSchema(t *testing.T) {
	cfg, receipt := fixture(t)
	s := openStore(t, cfg)
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Conn.ExecContext(t.Context(), "UPDATE worker_jobs SET phase='claimed' WHERE id=?", receipt.Job.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), receipt.Job.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("inconsistent record accepted: %v", err)
	}
	if _, err := s.journal.Conn.ExecContext(t.Context(), "PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(t.Context(), cfg); !errors.Is(err, ErrCorrupt) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("future schema accepted: %v", err)
	}
}

func TestWorkerJournalRejectsForeignTokensAndUnboundedResults(t *testing.T) {
	for _, test := range []string{"token", "digest", "exit", "output", "signature"} {
		t.Run(test, func(t *testing.T) {
			cfg, receipt := fixture(t)
			s := openStore(t, cfg)
			if _, err := s.Enroll(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
			claim, err := s.Claim(t.Context(), receipt.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			result := sandbox.Result{JobDigest: receipt.Job.Digest, State: sandbox.StateExited}
			switch test {
			case "token":
				claim.Token = strings.Repeat("x", 64)
			case "digest":
				result.JobDigest = strings.Repeat("a", 64)
			case "exit":
				result.ExitCode = 256
			case "output":
				result.Stdout = make([]byte, receipt.Job.Limits.OutputBytes+1)
			case "signature":
				result.Signature = []byte("untrusted")
			}
			if _, err = s.Propose(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, result); err == nil {
				t.Fatal("invalid worker result accepted")
			}
			record, err := s.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Phase != Claimed {
				t.Fatalf("invalid completion changed state: %+v %v", record, err)
			}
		})
	}
}
