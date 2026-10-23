//go:build linux && sandbox_peer_kernel

package service

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func TestInstalledInputCommand(t *testing.T) {
	command := os.Getenv("CORTEX_INPUT_COMMAND")
	if !filepath.IsAbs(command) || os.Geteuid() != 0 {
		t.Fatal("root Linux input command fixture is required")
	}
	request, cfg, job, input := provisionInputFixture(t)
	arguments := []string{"--config", request.Config, "--job", request.Job, "--input", request.Input, "--file", request.File}
	binding, _ := input.Ref.BindingDigest()
	for range 2 {
		output, err := exec.CommandContext(t.Context(), command, arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("input command: %v: %s", err, output)
		}
		var receipt InputInstallation
		if err = json.Unmarshal(output, &receipt); err != nil || receipt.JobID != job.ID || receipt.BindingDigest != binding || receipt.SHA256 != input.Ref.SHA256 {
			t.Fatal("command receipt", err)
		}
	}
	pin := hostfs.Pin{Path: filepath.Join(cfg.InputRoot, binding), SHA256: input.Ref.SHA256, Bytes: input.Ref.Bytes, Mode: 0o400}
	if err := hostfs.VerifyPin(t.Context(), pin, 0); err != nil {
		t.Fatal("command did not install input", err)
	}
	secret := []byte("untrusted source bytes must not appear in diagnostics")
	if err := os.WriteFile(request.File, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), command, arguments...).CombinedOutput()
	if err == nil || bytes.Contains(output, secret) {
		t.Fatal("command failure or confidentiality", err)
	}
	if err = hostfs.VerifyPin(t.Context(), pin, 0); err != nil {
		t.Fatal("failed command changed input", err)
	}
}
