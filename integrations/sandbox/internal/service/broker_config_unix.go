//go:build linux || darwin

package service

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
)

// BrokerStorage assigns distinct files directly inside the worker state directory.
// SQLite sidecar names and the evidence ownership file are reserved too.
type BrokerStorage struct {
	BrokerJournal string `json:"broker_journal"`
	WorkerJournal string `json:"worker_journal"`
	Evidence      string `json:"evidence"`
}

// BrokerConfig is protected canonical JSON for one installed daemon lifetime.
// Limits are explicit. This document contains credential pins, never private key
// bytes or a qualification override. Host/evidence policy files remain current
// on each request; changing other settings requires a new daemon lifetime.
type BrokerConfig struct {
	SchemaVersion  int                   `json:"schema_version"`
	WorkerConfig   string                `json:"worker_config"`
	Credentials    DaemonCredentialFiles `json:"credentials"`
	HostPolicy     string                `json:"host_policy"`
	EvidencePolicy string                `json:"evidence_policy"`
	Listen         string                `json:"listen"`
	Storage        BrokerStorage         `json:"storage"`
	Images         []string              `json:"images"`
	ImageRoot      string                `json:"image_root"`
	ImageLimits    image.LocalLimits     `json:"image_limits"`
	EvidenceBytes  int64                 `json:"evidence_bytes"`
	MaxJobs        int                   `json:"max_jobs"`
	MaxActive      int                   `json:"max_active"`
	CleanupActor   string                `json:"cleanup_actor"`
	PublicLimits   broker.ServerLimits   `json:"public_limits"`
	Control        ControlLimits         `json:"control"`
	Interval       time.Duration         `json:"interval_ns"`
	PageSize       int                   `json:"page_size"`
	DiscoveryLimit int                   `json:"discovery_limit"`
}

// BrokerConfiguration binds startup settings to exact bytes and a verified
// worker installation. It proves no runtime qualification or source permission.
type BrokerConfiguration struct {
	Settings BrokerConfig
	Worker   WorkerConfiguration
	SHA256   string
}

// ReadBrokerConfig loads a root-owned mode-0600 file of at most one MiB and
// verifies its worker installation. It opens no journals, service connections or
// listeners and does not read credential material. Startup must still load the
// pinned credentials, current policies and providers and require qualification.
func ReadBrokerConfig(ctx context.Context, name string) (BrokerConfiguration, error) {
	return readBrokerConfig(ctx, name, 0)
}
func readBrokerConfig(ctx context.Context, name string, owner uint32) (BrokerConfiguration, error) {
	body, err := hostfs.ReadOwned(ctx, name, owner, 0o600, 1<<20)
	if err != nil {
		return BrokerConfiguration{}, err
	}
	var cfg BrokerConfig
	if decodeCanonical(body, &cfg) != nil || !validBrokerSettings(cfg) {
		return BrokerConfiguration{}, fmt.Errorf("cortex: invalid installed broker configuration")
	}
	worker, err := readWorkerConfig(ctx, cfg.WorkerConfig, owner)
	if err != nil {
		return BrokerConfiguration{}, err
	}
	if !validBrokerLayout(name, cfg, worker) || cfg.ImageLimits.MaxBytes > worker.Settings.ImageStaging.CompressedBytes || cfg.EvidenceBytes > worker.Settings.InputStaging.PerInputBytes {
		return BrokerConfiguration{}, fmt.Errorf("cortex: unsafe installed broker layout or provider limits")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return BrokerConfiguration{}, contextErr
	}
	return BrokerConfiguration{Settings: cfg, Worker: worker, SHA256: evidence.Digest(body)}, nil
}

func validBrokerSettings(cfg BrokerConfig) bool {
	address, err := netip.ParseAddrPort(cfg.Listen)
	if err != nil || address.String() != cfg.Listen || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsMulticast() {
		return false
	}
	if cfg.SchemaVersion != 1 || !profilePath(cfg.WorkerConfig) || !validPolicyPrincipal(cfg.CleanupActor) {
		return false
	}
	if cfg.MaxJobs < 1 || cfg.MaxJobs > 100000 || cfg.MaxActive < 1 || cfg.MaxActive > 256 || cfg.MaxActive > cfg.MaxJobs || cfg.PageSize < 1 || cfg.PageSize > 256 || cfg.DiscoveryLimit < 1 || cfg.DiscoveryLimit > 10000 {
		return false
	}
	if cfg.Interval < 10*time.Millisecond || cfg.Interval > time.Second || cfg.PublicLimits.Validate() != nil || cfg.Control.validate() != nil || cfg.Control.Reports == 0 {
		return false
	}
	if cfg.ImageLimits.Validate() != nil || cfg.EvidenceBytes <= 0 || cfg.EvidenceBytes > 1<<20 || len(cfg.Images) < 1 || len(cfg.Images) > 256 {
		return false
	}
	images := make(map[string]bool)
	for _, pin := range cfg.Images {
		if !image.ValidDigest(pin) || images[pin] {
			return false
		}
		images[pin] = true
	}
	files := cfg.Credentials
	return validDaemonPin(files.SigningKey, daemonSigningPEMBytes) && validDaemonPin(files.Certificate, daemonCertificatePEMBytes) && validDaemonPin(files.TLSKey, daemonTLSKeyPEMBytes) && validDaemonPin(files.ClientCAs, daemonClientCAPEMBytes)
}

func validBrokerLayout(name string, cfg BrokerConfig, worker WorkerConfiguration) bool {
	state, runtimeRoot := worker.Settings.Service.StateDirectory, worker.Settings.Runtime.Directory
	if !profilePath(cfg.ImageRoot) || under(cfg.ImageRoot, state) || under(state, cfg.ImageRoot) || under(cfg.ImageRoot, runtimeRoot) || under(runtimeRoot, cfg.ImageRoot) {
		return false
	}
	reserved := map[string]bool{filepath.Join(state, "workspaces"): true, filepath.Join(state, "launch"): true}
	add := func(path string) bool {
		if !profilePath(path) || reserved[path] {
			return false
		}
		reserved[path] = true
		return true
	}
	files := cfg.Credentials
	for _, path := range []string{name, cfg.WorkerConfig, cfg.HostPolicy, cfg.EvidencePolicy, files.SigningKey.Path, files.Certificate.Path, files.TLSKey.Path, files.ClientCAs.Path, worker.Profile.Worker.Path, worker.Profile.TemplatePath} {
		if !add(path) || under(path, state) || under(path, cfg.ImageRoot) {
			return false
		}
	}
	socket := worker.Settings.ControlSocket
	if !add(socket) || under(socket, cfg.ImageRoot) || under(socket, filepath.Join(state, "workspaces")) || under(socket, filepath.Join(state, "launch")) {
		return false
	}
	for _, database := range []string{cfg.Storage.BrokerJournal, cfg.Storage.WorkerJournal, cfg.Storage.Evidence} {
		if filepath.Dir(database) != state {
			return false
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
			if !add(database + suffix) {
				return false
			}
		}
	}
	return true
}
