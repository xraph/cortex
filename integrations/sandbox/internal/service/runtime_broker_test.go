package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func runtimeBrokerFixture(t *testing.T) (*broker.Broker, broker.Config, *Controller, sandbox.Job, context.Context) {
	t.Helper()
	runtime, controller, receipt := runtimeFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := workerstore.Open(t.Context(), workerstore.Config{Database: filepath.Join(directory, "worker.db"), Broker: receipt.Broker, PublicKey: public, MaxJobs: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller.launcher.store = store
	controller.scanner.store = store
	config := broker.Config{Database: filepath.Join(directory, "broker.db"), Name: receipt.Broker, SigningKey: private, Runtime: runtime, Images: []string{receipt.Job.Image}, Authorize: func(context.Context, broker.Identity, sandbox.Job) error { return nil }}
	instance, err := broker.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	ctx := broker.WithIdentity(t.Context(), broker.Identity{Scope: receipt.Job.Scope, Principal: receipt.Job.Principal})
	return instance, config, controller, receipt.Job, ctx
}

func TestRuntimeBrokerPublishesOnlyFinalizedSignedResult(t *testing.T) {
	instance, config, controller, job, ctx := runtimeBrokerFixture(t)
	var claim workerstore.Claim
	controller.launcher.prepare = func(_ context.Context, _ LaunchProfile, value workerstore.Claim) error { claim = value; return nil }
	receipt, err := instance.Submit(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.VerifyReceipt(receipt, instance.PublicKey()); err != nil {
		t.Fatal(err)
	}
	proposal := sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Stdout: []byte("result")}
	if _, err = controller.launcher.store.Propose(ctx, job.ID, claim.Token, claim.Record.Revision, proposal); err != nil {
		t.Fatal(err)
	}
	if _, err = instance.Collect(ctx, job.ID); !errors.Is(err, broker.ErrPending) {
		t.Fatal("proposal published", err)
	}
	if _, err = controller.scanner.Sweep(ctx, ""); err != nil {
		t.Fatal(err)
	}
	result, err := instance.Collect(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.VerifyResult(result, job, instance.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "result" {
		t.Fatal("lost finalized output")
	}
	denied := broker.WithIdentity(t.Context(), broker.Identity{Scope: job.Scope, Principal: "other"})
	if _, err = instance.Collect(denied, job.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal("foreign principal read result", err)
	}
	if err = instance.Cancel(denied, job.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal("foreign principal cancelled", err)
	}
	if err = instance.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := broker.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err := restarted.Collect(ctx, job.ID)
	if err != nil || string(recovered.Stdout) != "result" {
		t.Fatal("broker restart lost result", err)
	}
	if err = broker.VerifyResult(recovered, job, restarted.PublicKey()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeBrokerRecoversUncertainStartAfterRestart(t *testing.T) {
	instance, config, controller, job, ctx := runtimeBrokerFixture(t)
	lost := errors.New("lost manager response")
	calls := 0
	controller.launcher.start = func(context.Context, id.ID) error { calls++; return lost }
	if _, err := instance.Submit(ctx, job); !errors.Is(err, lost) {
		t.Fatal("lost response hidden", err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := broker.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.Submit(ctx, job); err != nil || calls != 1 {
		t.Fatal("restart repeated execution", calls, err)
	}
	result, err := restarted.Collect(ctx, job.ID)
	if err != nil || result.State != sandbox.StateFailed {
		t.Fatalf("uncertain result: %+v %v", result, err)
	}
	if err = broker.VerifyResult(result, job, restarted.PublicKey()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeBrokerCancellationBeforeEnrollmentRequiresFence(t *testing.T) {
	instance, config, controller, job, ctx := runtimeBrokerFixture(t)
	runtime := config.Runtime.(*Runtime)
	attest := runtime.attest
	calls := 0
	runtime.attest = func(check context.Context) (sandbox.Capabilities, error) {
		calls++
		if calls == 3 {
			return sandbox.Capabilities{}, broker.ErrDenied
		}
		return attest(check)
	}
	if _, err := instance.Submit(ctx, job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("runtime denial lost", err)
	}
	if _, err := controller.launcher.store.Load(ctx, job.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal("rejected job enrolled", err)
	}
	manager := controller.launcher.reaper.manager.(*fakeManager)
	manager.fail = "stop"
	if err := instance.Cancel(ctx, job.ID); err == nil || errors.Is(err, broker.ErrNotFound) {
		t.Fatal("unconfirmed fence became cancellation", err)
	}
	receipt, err := instance.Submit(ctx, job)
	if err != nil || receipt.Job.ID != job.ID || calls != 3 {
		t.Fatal("cancelled acceptance dispatched again", calls, err)
	}
	manager.fail = ""
	if err = instance.Cancel(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	result, err := instance.Collect(ctx, job.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("missing cancellation result: %+v %v", result, err)
	}
	if err = broker.VerifyResult(result, job, instance.PublicKey()); err != nil {
		t.Fatal(err)
	}
}
