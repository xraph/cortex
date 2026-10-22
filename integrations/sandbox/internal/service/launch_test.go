package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func launcherFixture(t *testing.T) (*Launcher, sandbox.Receipt, *atomic.Int32) {
	t.Helper()
	store, receipt, _ := recoveryStore(t)
	profile := profileFixture(t)
	reaper := &Reaper{manager: &fakeManager{}, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}}
	starts := &atomic.Int32{}
	launcher := &Launcher{store: store, reaper: reaper, profile: profile, now: time.Now, authorize: func(context.Context, sandbox.Job) error { return nil }, preflight: func(context.Context, LaunchProfile, id.ID) error { return nil }, prepare: func(context.Context, LaunchProfile, workerstore.Claim) error { return nil }, start: func(context.Context, id.ID) error { starts.Add(1); return nil }}
	return launcher, receipt, starts
}

func TestConcurrentLaunchConsumesOneClaimAndStartsOnce(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			if err := launcher.Launch(t.Context(), receipt); err != nil {
				t.Errorf("launch: %v", err)
			}
		})
	}
	workers.Wait()
	if starts.Load() != 1 {
		t.Fatalf("duplicate execution: %d", starts.Load())
	}
	if err := launcher.Launch(t.Context(), receipt); err != nil || starts.Load() != 1 {
		t.Fatalf("retry relaunched: %v", err)
	}
}

func TestLostStartAcknowledgementReapsWithoutAnotherAttempt(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	lost := errors.New("start reply lost")
	launcher.start = func(context.Context, id.ID) error { starts.Add(1); return lost }
	if err := launcher.Launch(t.Context(), receipt); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	record, err := launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateFailed {
		t.Fatalf("ambiguous start unresolved: %+v %v", record, err)
	}
	if err = launcher.Launch(t.Context(), receipt); err != nil || starts.Load() != 1 {
		t.Fatal("ambiguous start executed twice")
	}
}

func TestLateCancellationOrDeadlinePreventsDispatch(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "revoked", "prepare"} {
		t.Run(mode, func(t *testing.T) {
			launcher, receipt, starts := launcherFixture(t)
			calls := 0
			launcher.authorize = func(context.Context, sandbox.Job) error {
				calls++
				if mode == "revoked" && calls > 1 {
					return broker.ErrDenied
				}
				return nil
			}
			launcher.prepare = func(ctx context.Context, _ LaunchProfile, _ workerstore.Claim) error {
				switch mode {
				case "cancel":
					_, err := launcher.store.Cancel(ctx, receipt.Job.ID)
					return err
				case "deadline":
					launcher.now = func() time.Time { return receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime) }
				case "prepare":
					return errors.New("credential write failed")
				}
				return nil
			}
			if err := launcher.Launch(t.Context(), receipt); err == nil || starts.Load() != 0 {
				t.Fatal("revoked or expired claim dispatched")
			}
			record, err := launcher.store.Load(t.Context(), receipt.Job.ID)
			if err != nil || record.Phase != workerstore.Terminal {
				t.Fatalf("failed launch not recovered: %+v %v", record, err)
			}
		})
	}
}

func TestPreflightFailureDoesNotConsumeClaim(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	launcher.preflight = func(context.Context, LaunchProfile, id.ID) error { return errors.New("unsafe installation") }
	if err := launcher.Launch(t.Context(), receipt); err == nil {
		t.Fatal("unsafe deployment admitted")
	}
	record, err := launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Accepted || starts.Load() != 0 {
		t.Fatal("preflight failure consumed execution")
	}
}

func TestAuthorizationCannotMutateSignedLaunch(t *testing.T) {
	launcher, receipt, _ := launcherFixture(t)
	launcher.authorize = func(_ context.Context, job sandbox.Job) error { job.Argv[0] = "/evil"; return nil }
	launcher.prepare = func(_ context.Context, _ LaunchProfile, claim workerstore.Claim) error {
		if claim.Record.Receipt.Job.Argv[0] != "/check" {
			t.Fatal("authorization callback mutated launch")
		}
		return nil
	}
	if err := launcher.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchContextCannotOutliveRuntimeQualification(t *testing.T) {
	launcher, receipt, _ := launcherFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Capabilities.QualifiedUntil = time.Now().Add(5 * time.Second)
	receipt.Signature = nil
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(private, body)
	root := t.TempDir()
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := workerstore.Open(t.Context(), workerstore.Config{Database: filepath.Join(root, "worker.db"), Broker: receipt.Broker, PublicKey: public, MaxJobs: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher.store = store
	launcher.start = func(ctx context.Context, _ id.ID) error {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(receipt.Capabilities.QualifiedUntil) {
			t.Fatal("dispatch outlives qualified runtime")
		}
		return nil
	}
	if err = launcher.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReapKeepsLaunchPendingWithoutRestart(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	manager := launcher.reaper.manager.(*fakeManager)
	manager.fail = "stop"
	launcher.start = func(context.Context, id.ID) error { starts.Add(1); return errors.New("lost reply") }
	if err := launcher.Launch(t.Context(), receipt); err == nil {
		t.Fatal("failed reap accepted")
	}
	record, err := launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Reaping {
		t.Fatal("uncertain launch forgotten", err)
	}
	if err = launcher.Launch(t.Context(), receipt); !errors.Is(err, broker.ErrPending) || starts.Load() != 1 {
		t.Fatal("pending cleanup relaunched worker", err)
	}
}

func TestSecondPreflightFencesDeploymentChangesAfterClaim(t *testing.T) {
	launcher, receipt, starts := launcherFixture(t)
	calls := 0
	launcher.preflight = func(context.Context, LaunchProfile, id.ID) error {
		calls++
		if calls == 2 {
			return errors.New("deployment changed")
		}
		return nil
	}
	if err := launcher.Launch(t.Context(), receipt); err == nil || starts.Load() != 0 {
		t.Fatal("changed deployment dispatched")
	}
	record, err := launcher.store.Load(t.Context(), receipt.Job.ID)
	if err != nil || record.Phase != workerstore.Terminal {
		t.Fatal("claimed failure not reaped", err)
	}
}
