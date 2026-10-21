package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

type fixtureWorkerPeer struct {
	key           id.ID
	checks        atomic.Int32
	dieAfterFirst atomic.Bool
}

func (p *fixtureWorkerPeer) Check(ctx context.Context, key id.ID) (uint32, error) {
	checks := p.checks.Add(1)
	if key != p.key || (p.dieAfterFirst.Load() && checks > 1) {
		return 0, broker.ErrDenied
	}
	return 123, ctx.Err()
}

func authorizationFixture(t *testing.T, configure ...func(*Controller, *fixtureWorkerPeer)) (*Controller, WorkerClaim, *fixtureWorkerPeer, func()) {
	t.Helper()
	c, receipt, _ := controllerFixture(t)
	c.interval = time.Second
	var captured workerstore.Claim
	c.launcher.prepare = func(_ context.Context, _ LaunchProfile, claim workerstore.Claim) error { captured = claim; return nil }
	c.checkWorker = func(_ context.Context, _ LaunchProfile, key id.ID, pid uint32) error {
		if key != receipt.Job.ID || pid != 123 {
			return broker.ErrDenied
		}
		return nil
	}
	peer := &fixtureWorkerPeer{key: receipt.Job.ID}
	for _, apply := range configure {
		apply(c, peer)
	}
	cancel, done := runController(t, c)
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); _ = awaitControllerStop(t, done) }) }
	t.Cleanup(stop)
	awaitControllerReady(t, c)
	if err := c.Launch(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	digest, err := c.launcher.profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	claim := WorkerClaim{SchemaVersion: 1, JobID: receipt.Job.ID, Token: captured.Token, Revision: captured.Record.Revision, ProfileDigest: digest}
	return c, claim, peer, stop
}

func TestWorkerAuthorizationBindsPeerProfileAndCurrentClaim(t *testing.T) {
	c, claim, peer, _ := authorizationFixture(t)
	record, err := c.authorizeWorker(t.Context(), peer, claim)
	if err != nil || record.Phase != workerstore.Claimed || record.Revision != claim.Revision || record.Receipt.Job.ID != claim.JobID {
		t.Fatalf("current worker denied: %+v %v", record, err)
	}
	record.Receipt.Job.Argv[0] = "/mutated"
	saved, err := c.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil || saved.Receipt.Job.Argv[0] != "/check" {
		t.Fatalf("authorization returned aliased state: %+v %v", saved, err)
	}
	for _, mutate := range []func(*WorkerClaim){
		func(v *WorkerClaim) { v.SchemaVersion++ },
		func(v *WorkerClaim) { v.JobID = id.New(sandbox.PrefixJob) },
		func(v *WorkerClaim) { v.Token = strings.Repeat("0", 64) },
		func(v *WorkerClaim) { v.Revision++ },
		func(v *WorkerClaim) { v.ProfileDigest = strings.Repeat("a", 64) },
	} {
		changed := claim
		mutate(&changed)
		denied, deniedErr := c.authorizeWorker(t.Context(), peer, changed)
		if deniedErr == nil || !denied.Receipt.Job.ID.IsNil() {
			t.Fatal("changed worker claim retained authority")
		}
		if strings.Contains(deniedErr.Error(), claim.Token) {
			t.Fatal("worker token exposed in error")
		}
	}
}

func TestWorkerAuthorizationRechecksCancellationPeerAndGrants(t *testing.T) {
	for _, mode := range []string{"cancel", "peer exit", "grants", "deployment"} {
		t.Run(mode, func(t *testing.T) {
			c, claim, peer, _ := authorizationFixture(t, func(c *Controller, peer *fixtureWorkerPeer) {
				switch mode {
				case "cancel":
					c.checkWorker = func(ctx context.Context, _ LaunchProfile, key id.ID, _ uint32) error {
						_, err := c.launcher.store.Cancel(ctx, key)
						return err
					}
				case "peer exit":
					peer.dieAfterFirst.Store(true)
				case "grants":
					var calls atomic.Int32
					c.scanner.revalidate = func(_ context.Context, job sandbox.Job) error {
						job.Argv[0] = "/mutated"
						if calls.Add(1) > 1 {
							return broker.ErrDenied
						}
						return nil
					}
				case "deployment":
					c.checkWorker = func(context.Context, LaunchProfile, id.ID, uint32) error {
						return errors.New("changed worker deployment")
					}
				}
			})
			record, err := c.authorizeWorker(t.Context(), peer, claim)
			if err == nil || !record.Receipt.Job.ID.IsNil() {
				t.Fatalf("%s bypassed worker authorization", mode)
			}
		})
	}
}

func TestWorkerAuthorizationRechecksPeerAfterFinalGrantRead(t *testing.T) {
	c, claim, peer, _ := authorizationFixture(t, func(c *Controller, peer *fixtureWorkerPeer) {
		var calls atomic.Int32
		c.scanner.revalidate = func(context.Context, sandbox.Job) error {
			if calls.Add(1) == 2 {
				peer.dieAfterFirst.Store(true)
			}
			return nil
		}
	})
	if record, err := c.authorizeWorker(t.Context(), peer, claim); err == nil || !record.Receipt.Job.ID.IsNil() {
		t.Fatal("peer death during final grant lookup retained authority")
	}
}

func TestWorkerAuthorizationJoinsControllerShutdown(t *testing.T) {
	c, claim, peer, stop := authorizationFixture(t)
	entered := make(chan struct{})
	c.checkWorker = func(ctx context.Context, _ LaunchProfile, _ id.ID, _ uint32) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	finished := make(chan error, 1)
	go func() {
		record, err := c.authorizeWorker(t.Context(), peer, claim)
		if !record.Receipt.Job.ID.IsNil() {
			t.Error("shutdown returned worker authority")
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("authorization did not reach deployment check")
	}
	stop()
	if err := awaitControllerStop(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("authorization outlived daemon: %v", err)
	}
	if record, err := c.authorizeWorker(t.Context(), peer, claim); !errors.Is(err, broker.ErrPending) || !record.Receipt.Job.ID.IsNil() {
		t.Fatal("stopped controller authorized worker")
	}
}
