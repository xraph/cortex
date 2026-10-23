//go:build linux || darwin

package hostfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xraph/cortex/evidence"
)

func publicationFixture(t *testing.T, data []byte) (Pin, uint32) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return Pin{Path: filepath.Join(root, "input"), SHA256: evidence.Digest(data), Bytes: int64(len(data)), Mode: 0o400}, uint32(os.Geteuid())
}

func TestPublishPinDurableIdempotentAndEmpty(t *testing.T) {
	for _, data := range [][]byte{[]byte("approved bytes"), {}} {
		pin, owner := publicationFixture(t, data)
		for range 2 {
			checks := 0
			if err := PublishPin(t.Context(), pin, owner, data, func() error { checks++; return nil }); err != nil {
				t.Fatal(err)
			}
			if checks != 1 {
				t.Fatal("publication policy bypass", checks)
			}
			if err := VerifyPin(t.Context(), pin, owner); err != nil {
				t.Fatal("published bytes", err)
			}
			entries, err := os.ReadDir(filepath.Dir(pin.Path))
			if err != nil || len(entries) != 1 {
				t.Fatal("temporary publication leaked", err)
			}
		}
	}
}

func TestPublishPinConcurrentAndNonReplacing(t *testing.T) {
	data := []byte("immutable input")
	pin, owner := publicationFixture(t, data)
	var writers sync.WaitGroup
	for range 12 {
		writers.Go(func() {
			if err := PublishPin(t.Context(), pin, owner, data, func() error { return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	if err := VerifyPin(t.Context(), pin, owner); err != nil {
		t.Fatal(err)
	}
	changed := []byte("different content")
	other := pin
	other.SHA256 = evidence.Digest(changed)
	other.Bytes = int64(len(changed))
	if err := PublishPin(t.Context(), other, owner, changed, func() error { return nil }); err == nil {
		t.Fatal("replaced existing file")
	}
	if err := VerifyPin(t.Context(), pin, owner); err != nil {
		t.Fatal("conflict changed original", err)
	}
	entries, err := os.ReadDir(filepath.Dir(pin.Path))
	if err != nil || len(entries) != 1 {
		t.Fatal("concurrent publication leaked", err)
	}
}

func TestPublishPinRejectsUnsafeInputsBeforePublication(t *testing.T) {
	for _, mode := range []string{"bytes", "size", "mode", "owner", "parent", "symlink", "hardlink", "existing-mode", "policy", "missing-policy", "cancelled", "cancel-at-commit"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("approved")
			pin, owner := publicationFixture(t, data)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			policy := func() error { return nil }
			var err error
			existing := false
			switch mode {
			case "bytes":
				data = []byte("tampered")
			case "size":
				pin.Bytes++
			case "mode":
				pin.Mode = 0o600
			case "owner":
				owner++
			case "parent":
				err = os.Chmod(filepath.Dir(pin.Path), 0o755)
			case "symlink":
				existing = true
				err = os.Symlink("/etc/passwd", pin.Path)
			case "hardlink", "existing-mode":
				existing = true
				err = os.WriteFile(pin.Path, data, 0o400)
				if err == nil {
					if mode == "hardlink" {
						err = os.Link(pin.Path, pin.Path+"-alias")
					} else {
						err = os.Chmod(pin.Path, 0o600)
					}
				}
			case "policy":
				policy = func() error { return evidence.ErrDenied }
			case "missing-policy":
				policy = nil
			case "cancelled":
				cancel()
			case "cancel-at-commit":
				policy = func() error { cancel(); return nil }
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = PublishPin(ctx, pin, owner, data, policy); err == nil {
				t.Fatal("unsafe publication accepted")
			}
			if mode == "cancel-at-commit" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if !existing {
				if _, err = os.Lstat(pin.Path); !os.IsNotExist(err) {
					t.Fatal("failed publication created target", err)
				}
			}
			entries, err := os.ReadDir(filepath.Dir(pin.Path))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "input" && entry.Name() != "input-alias" {
					t.Fatal("staging file retained", entry.Name())
				}
			}
		})
	}
}

func TestPublishPinRechecksExistingFileAfterPolicy(t *testing.T) {
	data := []byte("approved")
	pin, owner := publicationFixture(t, data)
	if err := PublishPin(t.Context(), pin, owner, data, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	err := PublishPin(t.Context(), pin, owner, data, func() error {
		if removeErr := os.Remove(pin.Path); removeErr != nil {
			return removeErr
		}
		return os.WriteFile(pin.Path, []byte("changed"), 0o400)
	})
	if err == nil {
		t.Fatal("retry accepted changed existing file")
	}
}

func TestPublishPinRecoversAcrossProcessExit(t *testing.T) {
	data := []byte("durable input")
	if target := os.Getenv("CORTEX_TEST_PUBLISH_PATH"); target != "" {
		pin := Pin{Path: target, SHA256: evidence.Digest(data), Bytes: int64(len(data)), Mode: 0o400}
		policy := func() error {
			if os.Getenv("CORTEX_TEST_PUBLISH_PHASE") == "prepared" {
				os.Exit(23)
			}
			return nil
		}
		if err := PublishPin(t.Context(), pin, uint32(os.Geteuid()), data, policy); err != nil {
			t.Fatal(err)
		}
		os.Exit(23)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "committed"} {
		t.Run(phase, func(t *testing.T) {
			pin, owner := publicationFixture(t, data)
			child := exec.CommandContext(t.Context(), executable, "-test.run=^TestPublishPinRecoversAcrossProcessExit$")
			child.Env = append(os.Environ(), "CORTEX_TEST_PUBLISH_PATH="+pin.Path, "CORTEX_TEST_PUBLISH_PHASE="+phase)
			output, runErr := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child exit: %v: %s", runErr, output)
			}
			if phase == "prepared" {
				if _, statErr := os.Stat(pin.Path); !os.IsNotExist(statErr) {
					t.Fatal("uncommitted input visible", statErr)
				}
				entries, readErr := os.ReadDir(filepath.Dir(pin.Path))
				if readErr != nil || len(entries) != 1 {
					t.Fatal("missing interrupted staging file", readErr)
				}
				temporary := pin
				temporary.Path = filepath.Join(filepath.Dir(pin.Path), entries[0].Name())
				if verifyErr := VerifyPin(t.Context(), temporary, owner); verifyErr != nil {
					t.Fatal("staging file unsafe", verifyErr)
				}
			} else if verifyErr := VerifyPin(t.Context(), pin, owner); verifyErr != nil {
				t.Fatal("committed input lost", verifyErr)
			}
			if retryErr := PublishPin(t.Context(), pin, owner, data, func() error { return nil }); retryErr != nil {
				t.Fatal("retry after exit", retryErr)
			}
			if verifyErr := VerifyPin(t.Context(), pin, owner); verifyErr != nil {
				t.Fatal(verifyErr)
			}
		})
	}
}
