package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/deployment"
)

func workerConfigFixture(t *testing.T) (string, WorkerConfig, ed25519.PrivateKey) {
	t.Helper()
	p := profileFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(p.Config.Path), "runtime")
	if err = os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("test runtime, never executed")
	if err = os.WriteFile(filepath.Join(root, "runsc"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	_, socket := controlListener(t)
	cfg := WorkerConfig{SchemaVersion: 1, Broker: "trusted", BrokerPublicKey: public, ControlSocket: socket,
		Service: WorkerService{Worker: p.Worker, TemplatePath: p.TemplatePath, StateDirectory: p.StateDirectory, CPUMillis: p.CPUMillis, MemoryBytes: p.MemoryBytes, Tasks: p.Tasks, WallTime: p.WallTime},
		Runtime: WorkerRuntime{Directory: root, Manifest: deployment.Manifest{SchemaVersion: 1, Architecture: runtime.GOARCH, Files: []deployment.File{{Path: "runsc", SHA256: evidence.Digest(body), Bytes: int64(len(body)), Mode: 0o755}}}},
	}
	writeWorkerConfig(t, p.Config.Path, cfg)
	return p.Config.Path, cfg, private
}

func writeWorkerConfig(t *testing.T, name string, cfg WorkerConfig) {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerConfigBindsExactBytesWithoutSelfReference(t *testing.T) {
	name, cfg, _ := workerConfigFixture(t)
	loaded, err := readWorkerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	profile := loaded.Profile
	want, err := profile.Digest()
	if err != nil || loaded.ProfileDigest != want || profile.Config.SHA256 != evidence.Digest(body) || profile.Config.Bytes != int64(len(body)) || profile.Config.Path != name || profile.Config.Mode != 0o600 {
		t.Fatal("configuration bytes did not bind the launch profile", err)
	}
	runtimeDigest, _ := cfg.Runtime.Manifest.Digest()
	if loaded.RuntimeDigest != runtimeDigest {
		t.Fatal("runtime inventory was not bound")
	}
	cfg.Broker = "another-broker"
	writeWorkerConfig(t, name, cfg)
	changed, err := readWorkerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil || changed.ProfileDigest == loaded.ProfileDigest {
		t.Fatal("configuration change retained the old profile", err)
	}
}

func TestWorkerConfigRejectsMalformedOrChangedDeployment(t *testing.T) {
	for _, mode := range []string{"unknown", "duplicate", "whitespace", "oversized", "key", "socket", "overlap", "architecture", "worker", "template", "runtime", "permissions", "schema"} {
		t.Run(mode, func(t *testing.T) {
			name, cfg, _ := workerConfigFixture(t)
			var err error
			switch mode {
			case "key":
				cfg.BrokerPublicKey = nil
			case "socket":
				cfg.ControlSocket = "relative.sock"
			case "overlap":
				cfg.Runtime.Directory = cfg.Service.StateDirectory
			case "architecture":
				cfg.Runtime.Manifest.Architecture = "foreign"
			case "schema":
				cfg.SchemaVersion++
			}
			writeWorkerConfig(t, name, cfg)
			body, readErr := os.ReadFile(name)
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch mode {
			case "unknown":
				body = append(body[:len(body)-1], []byte(`,"unknown":true}`)...)
			case "duplicate":
				body = append([]byte(`{"schema_version":1,`), body[1:]...)
			case "whitespace":
				body = append(body, '\n')
			case "oversized":
				body = []byte(strings.Repeat("x", (1<<20)+1))
			case "worker":
				err = os.WriteFile(cfg.Service.Worker.Path, []byte("changed worker"), 0o755)
			case "template":
				err = os.WriteFile(cfg.Service.TemplatePath, []byte("changed template"), 0o644)
			case "runtime":
				err = os.WriteFile(filepath.Join(cfg.Runtime.Directory, "runsc"), []byte("changed runtime"), 0o755)
			case "permissions":
				err = os.Chmod(name, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(name, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if loaded, loadErr := readWorkerConfig(t.Context(), name, uint32(os.Geteuid())); loadErr == nil || loaded.ProfileDigest != "" {
				t.Fatal("unsafe deployment produced a worker configuration")
			}
		})
	}
}
