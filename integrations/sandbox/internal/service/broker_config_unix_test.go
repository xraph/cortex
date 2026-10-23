//go:build linux || darwin

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
)

func brokerConfigFixture(t *testing.T, provided ...daemonCredentialFixture) (string, BrokerConfig) {
	t.Helper()
	workerName, worker, private := workerConfigFixture(t)
	var credentials daemonCredentialFixture
	if len(provided) > 0 {
		credentials = provided[0]
	} else {
		credentials = daemonCredentialsFixture(t)
	}
	credentials.files.SigningKey = credentialPin(t, credentials.files.SigningKey.Path, credentialPEM(t, private))
	root := filepath.Dir(workerName)
	cfg := BrokerConfig{SchemaVersion: 1, WorkerConfig: workerName, Credentials: credentials.files, HostPolicy: filepath.Join(root, "host-policy.json"), EvidencePolicy: filepath.Join(root, "evidence-policy.json"), Listen: "127.0.0.1:8443", Storage: BrokerStorage{BrokerJournal: filepath.Join(worker.Service.StateDirectory, "broker.db"), WorkerJournal: filepath.Join(worker.Service.StateDirectory, "worker.db"), Evidence: filepath.Join(worker.Service.StateDirectory, "evidence.db")}, Images: []string{"sha256:" + strings.Repeat("a", 64)}, ImageRoot: filepath.Join(root, "images"), ImageLimits: image.LocalLimits{MaxBytes: 64 << 20, Readers: 2}, EvidenceBytes: 1 << 20, MaxJobs: 100, MaxActive: 4, CleanupActor: "service:cleanup", PublicLimits: credentialLimits(), Control: ControlLimits{Connections: 4, Reports: 1, Timeout: time.Second}, Interval: time.Second, PageSize: 2, DiscoveryLimit: 100}
	cfg.InputRoot = filepath.Join(root, "inputs")
	cfg.InputReaders = 2
	name := filepath.Join(root, "broker.json")
	writeInstalledPolicy(t, name, cfg)
	return name, cfg
}

func TestBrokerConfigBindsWorkerAndExplicitStartupSettings(t *testing.T) {
	name, cfg := brokerConfigFixture(t)
	loaded, err := readBrokerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SHA256 != evidence.Digest(body) || loaded.Settings.Storage != cfg.Storage || loaded.Worker.Settings.Broker == "" || loaded.Worker.Profile.Config.Path != cfg.WorkerConfig {
		t.Fatal("lost installed settings")
	}
	for _, database := range []string{cfg.Storage.BrokerJournal, cfg.Storage.WorkerJournal, cfg.Storage.Evidence} {
		if _, statErr := os.Stat(database); !os.IsNotExist(statErr) {
			t.Fatal("configuration opened durable storage", statErr)
		}
	}
	cfg.Images[0] = "sha256:" + strings.Repeat("b", 64)
	writeInstalledPolicy(t, name, cfg)
	changed, err := readBrokerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil || changed.SHA256 == loaded.SHA256 || changed.Worker.ProfileDigest != loaded.Worker.ProfileDigest {
		t.Fatal("configuration identities not independent", err)
	}
}

func TestBrokerConfigRejectsUnsafeOrUnboundedSettings(t *testing.T) {
	for _, mode := range []string{"schema", "worker-path", "listener-dns", "listener-port", "listener-zone", "no-images", "duplicate-image", "bad-image", "jobs", "active", "active-jobs", "page", "discovery", "interval", "public-limit", "control-limit", "reports", "image-limit", "image-memory", "evidence-limit", "cleanup", "db-parent", "db-alias", "db-sidecar", "db-lock", "db-workspaces", "db-launch", "secret-overlap", "secret-state", "policy-overlap", "image-state", "image-runtime", "config-image", "pin-mode", "pin-size", "pin-digest", "credential-alias", "input-missing", "input-readers", "input-state", "input-image", "input-runtime", "input-config", "input-ancestor"} {
		t.Run(mode, func(t *testing.T) {
			name, cfg := brokerConfigFixture(t)
			switch mode {
			case "input-missing":
				cfg.InputRoot = ""
			case "input-readers":
				cfg.InputReaders = 17
			case "input-state":
				cfg.InputRoot = filepath.Dir(cfg.Storage.Evidence)
			case "input-image":
				cfg.InputRoot = cfg.ImageRoot
			case "input-runtime":
				cfg.InputRoot = filepath.Join(filepath.Dir(name), "runtime")
			case "input-config":
				cfg.InputRoot = filepath.Dir(name)
			case "input-ancestor":
				cfg.InputRoot = filepath.Dir(filepath.Dir(name))
			case "schema":
				cfg.SchemaVersion++
			case "worker-path":
				cfg.WorkerConfig = "worker.json"
			case "listener-dns":
				cfg.Listen = "localhost:8443"
			case "listener-port":
				cfg.Listen = "127.0.0.1:0"
			case "listener-zone":
				cfg.Listen = "[fe80::1%eth0]:8443"
			case "no-images":
				cfg.Images = nil
			case "duplicate-image":
				cfg.Images = append(cfg.Images, cfg.Images[0])
			case "bad-image":
				cfg.Images[0] = "latest"
			case "jobs":
				cfg.MaxJobs = 100001
			case "active":
				cfg.MaxActive = 0
			case "active-jobs":
				cfg.MaxActive = cfg.MaxJobs + 1
			case "page":
				cfg.PageSize = 257
			case "discovery":
				cfg.DiscoveryLimit = 0
			case "interval":
				cfg.Interval = time.Hour
			case "public-limit":
				cfg.PublicLimits.Connections = 0
			case "control-limit":
				cfg.Control.Timeout = 0
			case "reports":
				cfg.Control.Reports = 0
			case "image-limit":
				cfg.ImageLimits.MaxBytes = 65 << 20
			case "image-memory":
				cfg.ImageLimits.Readers = 32
			case "evidence-limit":
				cfg.EvidenceBytes = 1<<20 + 1
			case "cleanup":
				cfg.CleanupActor = " "
			case "db-parent":
				cfg.Storage.Evidence = filepath.Join(filepath.Dir(name), "evidence.db")
			case "db-alias":
				cfg.Storage.Evidence = cfg.Storage.BrokerJournal
			case "db-sidecar":
				cfg.Storage.WorkerJournal = cfg.Storage.Evidence + "-wal"
			case "db-lock":
				cfg.Storage.BrokerJournal = cfg.Storage.Evidence + ".lock"
			case "db-workspaces":
				cfg.Storage.Evidence = filepath.Join(filepath.Dir(cfg.Storage.Evidence), "workspaces")
			case "db-launch":
				cfg.Storage.Evidence = filepath.Join(filepath.Dir(cfg.Storage.Evidence), "launch")
			case "secret-overlap":
				cfg.Credentials.SigningKey.Path = cfg.Storage.Evidence
			case "secret-state":
				cfg.HostPolicy = filepath.Join(filepath.Dir(cfg.Storage.Evidence), "policy.json")
			case "policy-overlap":
				cfg.EvidencePolicy = cfg.HostPolicy
			case "image-state":
				cfg.ImageRoot = filepath.Dir(cfg.Storage.Evidence)
			case "image-runtime":
				cfg.ImageRoot = filepath.Join(filepath.Dir(cfg.WorkerConfig), "runtime")
			case "config-image":
				cfg.ImageRoot = filepath.Dir(name)
			case "pin-mode":
				cfg.Credentials.SigningKey.Mode = 0o644
			case "pin-size":
				cfg.Credentials.SigningKey.Bytes = 5000
			case "pin-digest":
				cfg.Credentials.SigningKey.SHA256 = "invalid"
			case "credential-alias":
				cfg.Credentials.TLSKey.Path = cfg.Credentials.SigningKey.Path
			}
			writeInstalledPolicy(t, name, cfg)
			if loaded, err := readBrokerConfig(t.Context(), name, uint32(os.Geteuid())); err == nil || loaded.SHA256 != "" {
				t.Fatal("invalid configuration accepted", err)
			}
		})
	}
}

func TestBrokerConfigRejectsChangedInstallationAndInvalidEncoding(t *testing.T) {
	for _, mode := range []string{"unknown", "trailing", "mode", "worker-changed", "owner", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			name, cfg := brokerConfigFixture(t)
			owner := uint32(os.Geteuid())
			ctx := t.Context()
			body, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown":
				body = append([]byte(`{"extra":true,`), body[1:]...)
			case "trailing":
				body = append(body, '\n')
			case "mode":
				err = os.Chmod(name, 0o644)
			case "worker-changed":
				err = os.WriteFile(cfg.WorkerConfig, []byte(`{}`), 0o600)
			case "owner":
				owner++
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unknown" || mode == "trailing" {
				if err = os.WriteFile(name, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if loaded, loadErr := readBrokerConfig(ctx, name, owner); loadErr == nil || loaded.SHA256 != "" {
				t.Fatal("invalid installation returned configuration", loadErr)
			}
		})
	}
}
