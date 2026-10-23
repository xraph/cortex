package grading_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/grading"
	"github.com/xraph/cortex/sandbox"
)

func TestAuthenticityRejectsTamperedOrForeignJob(t *testing.T) {
	key, private, err := ed25519.GenerateKey(rand.Reader)
	noError(t, err)
	verifier, err := grading.NewAuthenticity("broker-one", key)
	noError(t, err)
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "captured-test", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "host", Image: "sha256:" + evidence.Digest([]byte("image")), Argv: []string{"/bin/grader"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 1000, MemoryBytes: 1 << 20, Processes: 8, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024}})
	noError(t, err)
	receipt := sandbox.Receipt{SchemaVersion: 1, Job: j, Broker: "broker-one", AcceptedAt: time.Now().UTC()}
	body, err := json.Marshal(receipt)
	noError(t, err)
	receipt.Signature = ed25519.Sign(private, body)
	result := sandbox.Result{JobDigest: j.Digest, State: sandbox.StateExited}
	body, err = json.Marshal(result)
	noError(t, err)
	result.Signature = ed25519.Sign(private, body)
	noError(t, verifier.VerifyReceipt(t.Context(), receipt, j))
	noError(t, verifier.VerifyResult(t.Context(), result, j))
	other := j
	other.Key = "another"
	other, err = sandbox.Seal(other)
	noError(t, err)
	wantError(t, verifier.VerifyReceipt(t.Context(), receipt, other))
	wantError(t, verifier.VerifyResult(t.Context(), result, other))
	receipt.Broker = "foreign"
	receipt.Signature = nil
	body, err = json.Marshal(receipt)
	noError(t, err)
	receipt.Signature = ed25519.Sign(private, body)
	wantError(t, verifier.VerifyReceipt(t.Context(), receipt, j))
	result.Stdout = []byte("forged pass")
	wantError(t, verifier.VerifyResult(t.Context(), result, j))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = verifier.VerifyResult(cancelled, result, j); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestAuthenticityRequiresPinnedIdentity(t *testing.T) {
	key, _, err := ed25519.GenerateKey(rand.Reader)
	noError(t, err)
	_, err = grading.NewAuthenticity("", key)
	wantError(t, err)
	_, err = grading.NewAuthenticity("broker", nil)
	wantError(t, err)
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected rejection")
	}
}
