package sandboxbroker_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type fixtureRuntime struct {
	mu          sync.Mutex
	starts      int
	jobs        map[string]sandbox.Result
	startErr    error
	startAbsent bool
	receipts    []sandbox.Receipt
	onStart     func(*sandbox.Receipt)
}

func (r *fixtureRuntime) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: strings.Repeat("b", 64), Profile: sandbox.ProfileDenyAll, Qualification: "test-fixture-not-live", QualifiedUntil: time.Now().Add(time.Hour), Limits: limits()}, nil
}
func (r *fixtureRuntime) Start(_ context.Context, receipt sandbox.Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
	j := receipt.Job
	if r.onStart != nil {
		r.onStart(&receipt)
	}
	if r.startAbsent {
		return r.startErr
	}
	if _, ok := r.jobs[j.ID.String()]; !ok {
		r.starts++
		r.jobs[j.ID.String()] = sandbox.Result{JobDigest: j.Digest, State: sandbox.StateRunning}
	}
	return r.startErr
}

func TestRuntimeReceivesOriginalDetachedAcceptance(t *testing.T) {
	b, r, _, ctx := setup(t)
	j := job(t)
	r.onStart = func(receipt *sandbox.Receipt) {
		if err := broker.VerifyReceipt(*receipt, b.PublicKey()); err != nil {
			t.Fatal(err)
		}
		receipt.Job.Argv[0] = "mutated"
		receipt.Job.Scope.Levels[0].Value = "mutated"
		receipt.Signature[0] ^= 1
	}
	receipt, err := b.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.VerifyReceipt(receipt, b.PublicKey()); err != nil {
		t.Fatalf("runtime mutated journal receipt: %v", err)
	}
	status, err := b.Inspect(ctx, j.ID)
	if err != nil || !status.Receipt.AcceptedAt.Equal(receipt.AcceptedAt) {
		t.Fatalf("acceptance changed: %+v %v", status, err)
	}
}

func TestRuntimeResultValidationRequiresSealedJob(t *testing.T) {
	if err := broker.ValidateRuntimeResult(sandbox.Result{State: sandbox.StateExited}, sandbox.Job{}); err == nil {
		t.Fatal("unbound runtime result accepted")
	}
}

func TestExpiredAcceptedRetryCannotRestartTheClock(t *testing.T) {
	b, r, cfg, ctx := setup(t)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	accepted := now
	cfg.Now = func() time.Time { return now }
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	r.startAbsent, r.startErr = true, errors.New("lost startup acknowledgement")
	j := job(t)
	if _, err = next.Submit(ctx, j); err == nil {
		t.Fatal("uncertain start hidden")
	}
	now = now.Add(2 * time.Minute)
	r.startAbsent, r.startErr = false, nil
	if _, err = next.Submit(ctx, j); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired retry: %v", err)
	}
	if r.starts != 0 || len(r.receipts) != 1 {
		t.Fatalf("expired runtime dispatched: %d %+v", r.starts, r.receipts)
	}
	result, err := next.Collect(ctx, j.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("expired result: %+v %v", result, err)
	}
	receipt, err := next.Submit(ctx, j)
	if err != nil || !receipt.AcceptedAt.Equal(accepted) || r.starts != 0 {
		t.Fatalf("terminal retry: %+v %v", receipt, err)
	}
}
func (r *fixtureRuntime) Inspect(_ context.Context, key id.ID) (sandbox.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, ok := r.jobs[key.String()]
	if !ok {
		return sandbox.Result{}, broker.ErrNotFound
	}
	return result, nil
}
func (r *fixtureRuntime) Cancel(_ context.Context, key id.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, ok := r.jobs[key.String()]
	if !ok {
		return broker.ErrNotFound
	}
	result.State = sandbox.StateCancelled
	r.jobs[key.String()] = result
	return nil
}
func limits() sandbox.Limits {
	return sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 32, ScratchBytes: 8 << 20, WallTime: time.Minute, OutputBytes: 1 << 20}
}
func identity() broker.Identity {
	return broker.Identity{Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, Principal: "alice"}
}
func job(t *testing.T) sandbox.Job {
	t.Helper()
	who := identity()
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "one", Scope: who.Scope, Principal: who.Principal, Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/task"}, Network: sandbox.NetworkDenied, Limits: limits()})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func setup(t *testing.T) (*broker.Broker, *fixtureRuntime, broker.Config, context.Context) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fixtureRuntime{jobs: map[string]sandbox.Result{}}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := broker.Config{Database: filepath.Join(directory, "jobs.db"), Name: "fixture", SigningKey: key, Runtime: runtime, Authorize: func(_ context.Context, who broker.Identity, j sandbox.Job) error {
		if who.Principal != j.Principal || who.Scope.Canonical() != j.Scope.Canonical() {
			return broker.ErrDenied
		}
		return nil
	}, Images: []string{job(t).Image}}
	b, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, runtime, cfg, broker.WithIdentity(context.Background(), identity())
}
func TestSubmitSameKeySameDigestIsIdempotent(t *testing.T) {
	b, r, _, ctx := setup(t)
	j := job(t)
	first, err := b.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.Digest != second.Job.Digest || r.starts != 1 {
		t.Fatal("duplicate execution")
	}
	if err = broker.VerifyReceipt(first, b.PublicKey()); err != nil {
		t.Fatal(err)
	}
}
func TestSubmitKeyConflictRejected(t *testing.T) {
	b, _, _, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.Argv = []string{"/other"}
	j, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Submit(ctx, j); !errors.Is(err, broker.ErrConflict) {
		t.Fatalf("got %v", err)
	}
}
func TestTenantCannotInspectOtherJob(t *testing.T) {
	b, _, _, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	who := identity()
	who.Scope.Levels[0].Value = "b"
	other := broker.WithIdentity(context.Background(), who)
	if _, err := b.Inspect(other, j.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := b.Cancel(other, j.ID); !errors.Is(err, broker.ErrNotFound) {
		t.Fatal(err)
	}
}
func TestNoHostFallback(t *testing.T) {
	b, r, _, ctx := setup(t)
	r.startErr = errors.New("runtime unavailable")
	j := job(t)
	if _, err := b.Submit(ctx, j); err == nil {
		t.Fatal("runtime error hidden")
	}
	status, err := b.Inspect(ctx, j.ID)
	if err != nil || status.State != sandbox.StateRunning {
		t.Fatalf("ambiguous start not reconciled: %+v %v", status, err)
	}
	if r.starts != 1 {
		t.Fatal("retried execution")
	}
}
func TestRestartPreservesReceiptAndCancellation(t *testing.T) {
	b, r, cfg, ctx := setup(t)
	j := job(t)
	first, err := b.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	second, err := next.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Signature) != string(second.Signature) || r.starts != 1 {
		t.Fatal("restart changed immutable receipt")
	}
	if err = next.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err = next.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	result, err := next.Collect(ctx, j.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestAuthorizerCannotMutateAcceptedJob(t *testing.T) {
	b, _, cfg, ctx := setup(t)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Authorize = func(_ context.Context, _ broker.Identity, j sandbox.Job) error { j.Argv[0] = "/mutated"; return nil }
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	j := job(t)
	receipt, err := next.Submit(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Job.Digest != j.Digest || receipt.Job.Argv[0] != "/bin/task" {
		t.Fatal("authorizer mutated job")
	}
}
func TestRuntimeOutputBoundIsEnforced(t *testing.T) {
	b, r, _, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	r.jobs[j.ID.String()] = sandbox.Result{JobDigest: j.Digest, State: sandbox.StateExited, Stdout: make([]byte, j.Limits.OutputBytes+1)}
	if _, err := b.Collect(ctx, j.ID); err == nil {
		t.Fatal("oversized runtime output accepted")
	}
}
func TestConcurrentSubmitStartsOnce(t *testing.T) {
	b, r, _, ctx := setup(t)
	j := job(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := b.Submit(ctx, j); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if r.starts != 1 {
		t.Fatal(r.starts)
	}
}

func TestRevocationBlocksCollection(t *testing.T) {
	b, _, cfg, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := b.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Authorize = func(context.Context, broker.Identity, sandbox.Job) error { return broker.ErrDenied }
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err = next.Collect(ctx, j.ID); !errors.Is(err, broker.ErrDenied) {
		t.Fatal(err)
	}
}
func TestSignatureBindsTerminalOutput(t *testing.T) {
	b, _, _, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := b.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	result, err := b.Collect(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.VerifyResult(result, j, b.PublicKey()); err != nil {
		t.Fatal(err)
	}
	result.Stdout = []byte("forged success")
	if err = broker.VerifyResult(result, j, b.PublicKey()); err == nil {
		t.Fatal("forged output accepted")
	}
}
func TestJournalRejectsConcurrentBroker(t *testing.T) {
	_, _, cfg, _ := setup(t)
	other, err := broker.Open(cfg)
	if err == nil {
		other.Close()
		t.Fatal("second broker acquired journal")
	}
}
func TestAdmissionCapacityIsDurable(t *testing.T) {
	b, _, cfg, ctx := setup(t)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxActive = 1
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err = next.Submit(ctx, job(t)); err != nil {
		t.Fatal(err)
	}
	j := job(t)
	j.Key = "two"
	j, err = sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.Submit(ctx, j); err == nil {
		t.Fatal("active admission limit ignored")
	}
}

func TestCancelAcceptedButUnstartedJob(t *testing.T) {
	b, r, _, ctx := setup(t)
	r.startAbsent = true
	r.startErr = errors.New("before dispatch")
	j := job(t)
	if _, err := b.Submit(ctx, j); err == nil {
		t.Fatal("expected unavailable runtime")
	}
	if err := b.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	result, err := b.Collect(ctx, j.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("%+v %v", result, err)
	}
	if r.starts != 0 {
		t.Fatal("cancelled job started")
	}
}
