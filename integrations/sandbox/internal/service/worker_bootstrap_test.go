package service

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func workerBootstrapFixture(t *testing.T, changes ...func(*sandbox.Job)) (string, string, Credential, workerBoundary) {
	t.Helper()
	name, cfg, private := workerConfigFixture(t)
	loaded, err := readWorkerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	_, receipt, _ := recoveryStore(t, changes...)
	receipt.Capabilities.RuntimeDigest = loaded.RuntimeDigest
	receipt.Signature = nil
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(private, body)
	token := strings.Repeat("a", 64)
	credential := Credential{SchemaVersion: 1, ProfileDigest: loaded.ProfileDigest, Token: token, Record: workerstore.Record{SchemaVersion: workerstore.RecordVersion, Receipt: receipt, Phase: workerstore.Claimed, Revision: 2, TokenHash: evidence.Digest([]byte(token))}}
	directory := filepath.Join(cfg.Service.StateDirectory, "credentials")
	if err = os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBootstrapCredential(t, directory, credential)
	boundary := workerBoundary{owner: uint32(os.Geteuid()), verifyCurrent: func(ctx context.Context, key id.ID) error {
		if key != receipt.Job.ID {
			return broker.ErrDenied
		}
		return ctx.Err()
	}, authorize: func(ctx context.Context, socket string, claim WorkerClaim) error {
		if socket != cfg.ControlSocket || claim.JobID != receipt.Job.ID || claim.Token != token || claim.Revision != 2 || claim.ProfileDigest != loaded.ProfileDigest {
			return broker.ErrDenied
		}
		return ctx.Err()
	}, report: func(ctx context.Context, _ string, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
		body, _ := json.Marshal(result)
		return WorkerReportReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: claim.Revision, ProposalDigest: evidence.Digest(body)}, ctx.Err()
	}}
	return name, directory, credential, boundary
}

func writeBootstrapCredential(t *testing.T, directory string, credential Credential) {
	t.Helper()
	name := filepath.Join(directory, "cortex-claim")
	if err := os.Chmod(name, 0o600); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	body, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, body, 0o400); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(name, 0o400); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerBootstrapBindsCredentialDeploymentAndCurrentAuthority(t *testing.T) {
	name, directory, credential, boundary := workerBootstrapFixture(t)
	var calls atomic.Int32
	authorize := boundary.authorize
	boundary.authorize = func(ctx context.Context, socket string, claim WorkerClaim) error {
		calls.Add(1)
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(acceptanceDeadline(credential.Record.Receipt)) {
			t.Fatal("startup extended the signed deadline")
		}
		return authorize(ctx, socket, claim)
	}
	worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil || worker == nil || calls.Load() != 1 {
		t.Fatal("verified worker did not bootstrap", err)
	}
	if worker.Deadline() != acceptanceDeadline(credential.Record.Receipt) {
		t.Fatal("worker deadline differs from signed acceptance")
	}
	job, err := worker.Job()
	if err != nil {
		t.Fatal(err)
	}
	job.Argv[0] = "/mutated"
	configuration := worker.Configuration()
	configuration.Settings.BrokerPublicKey[0] ^= 1
	configuration.Settings.Runtime.Manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	if err = worker.Authorize(t.Context()); err != nil || calls.Load() != 2 {
		t.Fatal("detached output changed worker authority", err)
	}
	job, err = worker.Job()
	if err != nil || job.Argv[0] != credential.Record.Receipt.Job.Argv[0] {
		t.Fatal("job copy mutated signed worker state")
	}
}

func TestWorkerBootstrapWaitsOnlyForExplicitPendingAndHonorsDeadline(t *testing.T) {
	for _, mode := range []string{"ready", "denied", "outage", "deadline", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			name, directory, credential, boundary := workerBootstrapFixture(t)
			var calls atomic.Int32
			maximum := 3 * time.Second
			if mode == "deadline" {
				maximum = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), maximum)
			defer cancel()
			boundary.authorize = func(context.Context, string, WorkerClaim) error {
				attempt := calls.Add(1)
				switch mode {
				case "ready":
					if attempt > 1 {
						return nil
					}
				case "denied":
					return broker.ErrDenied
				case "outage":
					return errors.New("provider unavailable")
				case "cancelled":
					cancel()
				}
				return broker.ErrPending
			}
			worker, err := bootstrapWorker(ctx, name, directory, credential.Record.Receipt.Job.ID, boundary)
			if mode == "ready" {
				if err != nil || worker == nil || calls.Load() != 2 {
					t.Fatal("pending worker never reached ready", err)
				}
			} else if err == nil || worker != nil {
				t.Fatal("failed activation granted a worker session")
			} else if (mode == "denied" || mode == "outage" || mode == "cancelled") && calls.Load() != 1 {
				t.Fatal("non-pending authorization was retried")
			}
		})
	}
}

func TestWorkerBootstrapRejectsChangedCredentialBeforeControl(t *testing.T) {
	for _, mode := range []string{"profile", "signature", "runtime", "membership", "configuration"} {
		t.Run(mode, func(t *testing.T) {
			name, directory, credential, boundary := workerBootstrapFixture(t)
			var calls atomic.Int32
			boundary.authorize = func(context.Context, string, WorkerClaim) error { calls.Add(1); return nil }
			switch mode {
			case "profile":
				credential.ProfileDigest = strings.Repeat("0", 64)
			case "signature":
				credential.Record.Receipt.Signature[0] ^= 1
			case "runtime":
				// A valid configuration still must match the signed runtime pin.
				loaded, err := readWorkerConfig(t.Context(), name, boundary.owner)
				if err != nil {
					t.Fatal(err)
				}
				body := []byte("another approved runtime")
				cfg := loaded.Settings
				if err = os.WriteFile(filepath.Join(cfg.Runtime.Directory, "runsc"), body, 0o755); err != nil {
					t.Fatal(err)
				}
				cfg.Runtime.Manifest.Files[0].SHA256, cfg.Runtime.Manifest.Files[0].Bytes = evidence.Digest(body), int64(len(body))
				writeWorkerConfig(t, name, cfg)
				loaded, err = readWorkerConfig(t.Context(), name, boundary.owner)
				if err != nil {
					t.Fatal(err)
				}
				credential.ProfileDigest = loaded.ProfileDigest
			case "membership":
				boundary.verifyCurrent = func(context.Context, id.ID) error { return broker.ErrDenied }
			case "configuration":
				if err := os.WriteFile(name, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeBootstrapCredential(t, directory, credential)
			if worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary); err == nil || worker != nil || calls.Load() != 0 {
				t.Fatal("changed startup identity reached current authority")
			}
		})
	}
}

func TestWorkerBootstrapRefusesStorageOutsideServiceBudget(t *testing.T) {
	name, directory, credential, boundary := workerBootstrapFixture(t)
	loaded, err := readWorkerConfig(t.Context(), name, boundary.owner)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Settings
	cfg.Workspace.StagingBytes = int64(cfg.Service.MemoryBytes)
	writeWorkerConfig(t, name, cfg)
	loaded, err = readWorkerConfig(t.Context(), name, boundary.owner)
	if err != nil {
		t.Fatal(err)
	}
	credential.ProfileDigest = loaded.ProfileDigest
	writeBootstrapCredential(t, directory, credential)
	var calls atomic.Int32
	boundary.authorize = func(context.Context, string, WorkerClaim) error { calls.Add(1); return nil }
	if worker, bootstrapErr := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary); bootstrapErr == nil || worker != nil || calls.Load() != 0 {
		t.Fatal("storage budget reached execution authority")
	}
}
