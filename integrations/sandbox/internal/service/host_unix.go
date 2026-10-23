//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// HostConfig supplies installed daemon services. Store, Reaper and content
// providers remain caller-owned and must stay open until Daemon.Run returns.
// AuthorizeLaunch checks the authenticated caller's current execution grants;
// Revalidate checks the signed job subject independently of that caller's
// connection. Broker.Authorize separately governs every public operation.
// All callbacks must honor cancellation and support concurrent calls.
type HostConfig struct {
	WorkerConfig    string
	Store           *workerstore.Store
	Reaper          *Reaper
	Broker          broker.Config
	Qualification   QualificationVerifier
	AuthorizeLaunch func(context.Context, sandbox.Job) error
	Revalidate      func(context.Context, sandbox.Job) error
	Inputs          staging.Source
	InputPolicy     CapturePolicy
	Images          image.Source
	ImagePolicy     ImagePolicy
	// Captures is required even when new capture is disabled, so old evidence
	// remains recoverable. Its Store is filled by assembly or must match Store.
	Captures CaptureRecoveryConfig
	// CapturePolicy must be present exactly when installed retention is nonzero.
	CapturePolicy  CapturePolicy
	Control        ControlLimits
	Interval       time.Duration
	PageSize       int
	DiscoveryLimit int
}

// AssembleDaemon binds one verified installation to launch, recovery, content
// and private transport. It neither starts services nor invokes qualification.
// Public TLS serving is supplied to Run. Production requires root on Linux.
func AssembleDaemon(ctx context.Context, cfg HostConfig) (*Daemon, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	if cfg.Store == nil || cfg.Reaper == nil || cfg.Broker.Runtime != nil || cfg.Broker.Authorize == nil || cfg.Qualification == nil || cfg.AuthorizeLaunch == nil || cfg.Revalidate == nil || cfg.Control.Reports == 0 || (cfg.Captures.Store != nil && cfg.Captures.Store != cfg.Store) {
		return nil, fmt.Errorf("cortex: incomplete installed sandbox host")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	configuration, err := ReadWorkerConfig(operation, cfg.WorkerConfig)
	if err != nil {
		return nil, err
	}
	settings := configuration.Settings
	if cfg.Broker.Name != settings.Broker || len(cfg.Broker.SigningKey) != ed25519.PrivateKeySize || !bytes.Equal(cfg.Broker.SigningKey[ed25519.SeedSize:], settings.BrokerPublicKey) || (settings.CaptureRetention != 0) != (cfg.CapturePolicy != nil) {
		return nil, broker.ErrConflict
	}
	if identityErr := cfg.Store.CheckBroker(settings.Broker, settings.BrokerPublicKey); identityErr != nil {
		return nil, identityErr
	}
	// Use the same verified snapshot for launch and attestation. Reloading here
	// could bind those components to different installations during an update.
	attest := bindDeploymentAttestation(cfg.WorkerConfig, configuration, cfg.Qualification, 0)
	launcher, err := NewLauncher(cfg.Store, cfg.Reaper, configuration.Profile, func(ctx context.Context, job sandbox.Job) error {
		if admissionErr := settings.Workspace.Admit(job, configuration.Profile.MemoryBytes); admissionErr != nil {
			return admissionErr
		}
		if len(job.Outputs) != 0 && cfg.CapturePolicy == nil {
			return broker.ErrDenied
		}
		return cfg.AuthorizeLaunch(ctx, job)
	})
	if err != nil {
		return nil, err
	}
	cfg.Captures.Store = cfg.Store
	captures, err := NewCaptureRecovery(cfg.Captures)
	if err != nil {
		return nil, err
	}
	orphans, err := NewOrphanScanner(OrphanConfig{Store: cfg.Store, Reaper: cfg.Reaper, MaxUnits: cfg.DiscoveryLimit, PageSize: cfg.PageSize})
	if err != nil {
		return nil, err
	}
	scanner, err := NewRecoveryScanner(RecoveryConfig{Orphans: orphans, Store: cfg.Store, Reaper: cfg.Reaper, Captures: captures, PageSize: cfg.PageSize, Now: cfg.Broker.Now, Revalidate: installedRevalidation(cfg.Store, cfg.Revalidate, attest)})
	if err != nil {
		return nil, err
	}
	var policies []CapturePolicy
	if cfg.CapturePolicy != nil {
		policies = []CapturePolicy{cfg.CapturePolicy}
	}
	controller, err := NewController(launcher, scanner, cfg.Interval, policies...)
	if err != nil {
		return nil, err
	}
	inputs, err := NewWorkerEvidence(controller, cfg.Inputs, cfg.InputPolicy, settings.InputStaging.PerInputBytes)
	if err != nil {
		return nil, err
	}
	images, err := NewWorkerImages(controller, cfg.Images, cfg.ImagePolicy, settings.ImageStaging.CompressedBytes)
	if err != nil {
		return nil, err
	}
	control, err := NewWorkerControl(controller, cfg.Control, WorkerContent{Evidence: inputs, Images: images})
	if err != nil {
		return nil, err
	}
	if operationErr := operation.Err(); operationErr != nil {
		return nil, operationErr
	}
	return NewDaemon(DaemonConfig{Controller: controller, Control: control, Broker: cfg.Broker, Attest: attest, Socket: settings.ControlSocket, Interval: cfg.Interval})
}

func installedRevalidation(store *workerstore.Store, grants func(context.Context, sandbox.Job) error, attest DeploymentAttestation) func(context.Context, sandbox.Job) error {
	return func(ctx context.Context, job sandbox.Job) error {
		if err := job.Validate(); err != nil {
			return err
		}
		record, err := store.Load(ctx, job.ID)
		if err != nil {
			return err
		}
		if record.Receipt.Job.Digest != job.Digest {
			return broker.ErrConflict
		}
		detached, err := sandbox.Seal(job)
		if err != nil {
			return err
		}
		if grantErr := errors.Join(grants(ctx, detached), ctx.Err()); grantErr != nil {
			return grantErr
		}
		caps, err := attest(ctx)
		if err != nil {
			return err
		}
		return errors.Join(checkCurrentCapabilities(caps, record.Receipt), ctx.Err())
	}
}
