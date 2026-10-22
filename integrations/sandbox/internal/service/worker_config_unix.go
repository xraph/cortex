//go:build linux || darwin

package service

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/deployment"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
)

// WorkerService configures the installed service without a self-referential
// configuration hash. The loader supplies that pin from the exact file bytes.
type WorkerService struct {
	Worker         hostfs.Pin    `json:"worker"`
	TemplatePath   string        `json:"template_path"`
	StateDirectory string        `json:"state_directory"`
	CPUMillis      uint64        `json:"cpu_millis"`
	MemoryBytes    uint64        `json:"memory_bytes"`
	Tasks          uint64        `json:"tasks"`
	WallTime       time.Duration `json:"wall_time_ns"`
}

// WorkerRuntime inventories the complete host-approved runtime installation.
type WorkerRuntime struct {
	Directory string              `json:"directory"`
	Manifest  deployment.Manifest `json:"manifest"`
}

// WorkerConfig is installed by the host as canonical JSON with mode 0600.
// It contains no broker signing key, guest commands or source credentials.
type WorkerConfig struct {
	SchemaVersion    int               `json:"schema_version"`
	Broker           string            `json:"broker"`
	BrokerPublicKey  ed25519.PublicKey `json:"broker_public_key"`
	ControlSocket    string            `json:"control_socket"`
	Service          WorkerService     `json:"service"`
	Runtime          WorkerRuntime     `json:"runtime"`
	Workspace        workspace.Policy  `json:"workspace"`
	ImageStaging     image.Limits      `json:"image_staging"`
	InputStaging     staging.Limits    `json:"input_staging"`
	CaptureRetention time.Duration     `json:"capture_retention_ns,omitempty"`
}

// WorkerConfiguration binds a parsed configuration to its installed profile.
// This snapshot proves file identity only, not current execution authority.
type WorkerConfiguration struct {
	Settings      WorkerConfig
	Profile       LaunchProfile
	ProfileDigest string
	RuntimeDigest string
}

// ReadWorkerConfig loads protected configuration and verifies the installed
// worker, template, private state directory and complete runtime inventory.
func ReadWorkerConfig(ctx context.Context, name string) (WorkerConfiguration, error) {
	return readWorkerConfig(ctx, name, 0)
}

func readWorkerConfig(ctx context.Context, name string, owner uint32) (WorkerConfiguration, error) {
	body, err := hostfs.ReadOwned(ctx, name, owner, 0o600, 1<<20)
	if err != nil {
		return WorkerConfiguration{}, err
	}
	var cfg WorkerConfig
	if decodeCanonical(body, &cfg) != nil || cfg.SchemaVersion != 1 || !utf8.ValidString(cfg.Broker) || strings.TrimSpace(cfg.Broker) == "" || len(cfg.Broker) > 512 || len(cfg.BrokerPublicKey) != ed25519.PublicKeySize || !profilePath(cfg.ControlSocket) || len(cfg.ControlSocket) > 100 {
		return WorkerConfiguration{}, fmt.Errorf("cortex: invalid worker configuration")
	}
	settings := cfg.Service
	if cfg.CaptureRetention != 0 {
		if captureErr := capture.ValidateRetention(cfg.CaptureRetention); captureErr != nil {
			return WorkerConfiguration{}, captureErr
		}
	}
	if policyErr := cfg.Workspace.Validate(); policyErr != nil {
		return WorkerConfiguration{}, policyErr
	}
	if limitErr := cfg.ImageStaging.Validate(); limitErr != nil {
		return WorkerConfiguration{}, limitErr
	}
	if limitErr := cfg.InputStaging.Validate(); limitErr != nil {
		return WorkerConfiguration{}, limitErr
	}
	profile := LaunchProfile{SchemaVersion: 1, Worker: settings.Worker, Config: hostfs.Pin{Path: name, SHA256: evidence.Digest(body), Bytes: int64(len(body)), Mode: 0o600}, TemplatePath: settings.TemplatePath, StateDirectory: settings.StateDirectory, CPUMillis: settings.CPUMillis, MemoryBytes: settings.MemoryBytes, Tasks: settings.Tasks, WallTime: settings.WallTime}
	profileDigest, err := profile.Digest()
	if err != nil {
		return WorkerConfiguration{}, err
	}
	root := cfg.Runtime.Directory
	if !profilePath(root) || under(root, profile.StateDirectory) || under(profile.StateDirectory, root) || under(cfg.ControlSocket, root) || cfg.Runtime.Manifest.Architecture != runtime.GOARCH {
		return WorkerConfiguration{}, fmt.Errorf("cortex: unsafe worker runtime layout or architecture")
	}
	runtimeDigest, err := cfg.Runtime.Manifest.Digest()
	if err != nil {
		return WorkerConfiguration{}, err
	}
	loaded := WorkerConfiguration{Settings: cfg, Profile: profile, ProfileDigest: profileDigest, RuntimeDigest: runtimeDigest}
	if verificationErr := loaded.verify(ctx, owner); verificationErr != nil {
		return WorkerConfiguration{}, verificationErr
	}
	return loaded, nil
}

func (c WorkerConfiguration) verify(ctx context.Context, owner uint32) error {
	if err := verifyProfile(ctx, c.Profile, owner); err != nil {
		return err
	}
	digest, err := deployment.Verify(ctx, c.Settings.Runtime.Directory, c.Settings.Runtime.Manifest, owner)
	if err != nil {
		return err
	}
	if digest != c.RuntimeDigest {
		return fmt.Errorf("cortex: worker runtime deployment changed")
	}
	return ctx.Err()
}
