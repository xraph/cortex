//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

func provisionInputFixture(t *testing.T) (InputProvisionRequest, BrokerConfig, sandbox.Job, evidence.Artifact) {
	t.Helper()
	config, cfg := brokerConfigFixture(t)
	if err := os.Mkdir(cfg.InputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, controller, claim, _, input, _, _ := workerEvidenceFixture(t)
	record, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	job := record.Receipt.Job
	_, policy := evidencePolicyFixture(t, job)
	writeInstalledPolicy(t, cfg.EvidencePolicy, policy)
	request := InputProvisionRequest{Config: config, Job: filepath.Join(filepath.Dir(config), "input-job.json"), Input: job.Inputs[0].Path, File: filepath.Join(filepath.Dir(config), "source.bin")}
	writeInstalledPolicy(t, request.Job, job)
	if err = os.WriteFile(request.File, input.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	return request, cfg, job, input
}

func TestProvisionInputInstalledReadAndIdempotentReceipt(t *testing.T) {
	request, cfg, job, input := provisionInputFixture(t)
	owner := uint32(os.Geteuid())
	var first InputInstallation
	for attempt := range 2 {
		receipt, err := provisionInput(t.Context(), request, owner)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := input.Ref.BindingDigest()
		if receipt.SchemaVersion != 1 || receipt.BindingDigest != want || receipt.JobDigest != job.Digest || receipt.JobID != job.ID || receipt.SHA256 != input.Ref.SHA256 || receipt.Bytes != input.Ref.Bytes {
			t.Fatal("incomplete receipt", receipt)
		}
		if attempt == 0 {
			first = receipt
		} else if receipt != first {
			t.Fatal("retry changed receipt")
		}
	}
	policy, err := newEvidencePolicy(t.Context(), cfg.EvidencePolicy, owner, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newLocalInputs(cfg.InputRoot, cfg.EvidenceBytes, cfg.InputReaders, policy.AuthorizeInput, owner)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := source.Open(evidenceContext(t.Context(), job), input.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, input.Data) {
		t.Fatal("installed input", err)
	}
	for _, database := range []string{cfg.Storage.Evidence, cfg.Storage.WorkerJournal, cfg.Storage.BrokerJournal} {
		if _, err = os.Stat(database); !os.IsNotExist(err) {
			t.Fatal("provisioning opened database", err)
		}
	}
}

func TestProvisionInputDeniesInvalidAndRevokedRequests(t *testing.T) {
	for _, mode := range []string{"unknown-input", "changed-job", "job-mode", "raw-mode", "raw-symlink", "raw-bytes", "revoked", "bad-config", "cancelled", "conflict", "unsafe-existing"} {
		t.Run(mode, func(t *testing.T) {
			request, cfg, job, input := provisionInputFixture(t)
			binding, _ := input.Ref.BindingDigest()
			target := filepath.Join(cfg.InputRoot, binding)
			ctx := t.Context()
			var err error
			existing := false
			switch mode {
			case "unknown-input":
				request.Input = "/inputs/other"
			case "changed-job":
				job.Principal = "changed"
				writeInstalledPolicy(t, request.Job, job)
			case "job-mode":
				err = os.Chmod(request.Job, 0o644)
			case "raw-mode":
				err = os.Chmod(request.File, 0o644)
			case "raw-symlink":
				if err = os.Remove(request.File); err == nil {
					err = os.Symlink("/etc/passwd", request.File)
				}
			case "raw-bytes":
				err = os.WriteFile(request.File, []byte("different"), 0o600)
			case "revoked":
				_, policy := evidencePolicyFixture(t, job)
				policy.Subjects = nil
				writeInstalledPolicy(t, cfg.EvidencePolicy, policy)
			case "bad-config":
				err = os.Chmod(request.Config, 0o644)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "conflict":
				existing = true
				err = os.WriteFile(target, []byte("original"), 0o400)
			case "unsafe-existing":
				existing = true
				err = os.WriteFile(target, input.Data, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if receipt, installErr := provisionInput(ctx, request, uint32(os.Geteuid())); installErr == nil || receipt.BindingDigest != "" {
				t.Fatal("unsafe installation succeeded", installErr)
			}
			if existing {
				body, readErr := os.ReadFile(target)
				if readErr != nil {
					t.Fatal(readErr)
				}
				expected := input.Data
				if mode == "conflict" {
					expected = []byte("original")
				}
				if !bytes.Equal(body, expected) {
					t.Fatal("rejected existing file modified")
				}
			} else if _, err = os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("rejected input installed", err)
			}
		})
	}
}

func TestProvisionInputRechecksPreparedInstallation(t *testing.T) {
	for _, mode := range []string{"config", "job", "policy", "directory", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			request, cfg, job, input := provisionInputFixture(t)
			publication, err := prepareInput(t.Context(), request, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch mode {
			case "config":
				cfg.MaxActive++
				writeInstalledPolicy(t, request.Config, cfg)
			case "job":
				job.Principal = "changed"
				writeInstalledPolicy(t, request.Job, job)
			case "policy":
				_, policy := evidencePolicyFixture(t, job)
				policy.Subjects = nil
				writeInstalledPolicy(t, cfg.EvidencePolicy, policy)
			case "directory":
				if err = os.Rename(cfg.InputRoot, cfg.InputRoot+"-old"); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(cfg.InputRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if receipt, publishErr := publication.publish(ctx); publishErr == nil || receipt.BindingDigest != "" {
				t.Fatal("stale preparation published", publishErr)
			}
			binding, _ := input.Ref.BindingDigest()
			if _, err = os.Stat(filepath.Join(cfg.InputRoot, binding)); !os.IsNotExist(err) {
				t.Fatal("stale preparation left input", err)
			}
		})
	}
}
