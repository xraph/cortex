//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/evidencedb"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

type installedPorts struct {
	openReaper func(context.Context) (*Reaper, error)
	listen     func(context.Context, string, string) (net.Listener, error)
}

// RunInstalledDaemon owns one configured Linux broker lifetime. It loads pinned
// credentials and current policies, opens storage and systemd, recovers worker
// state, checks live qualification, then opens the bounded public mTLS listener.
// It joins the daemon before closing owned services. A verifier is mandatory;
// configuration has no way to manufacture qualification or bypass its checks.
// Directory/policy provisioning and the trusted live verifier remain host duties.
func RunInstalledDaemon(ctx context.Context, name string, verify QualificationVerifier) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errUnsupported
	}
	return runInstalledDaemon(ctx, name, verify, installedPorts{openReaper: Open, listen: new(net.ListenConfig).Listen})
}

func runInstalledDaemon(ctx context.Context, name string, verify QualificationVerifier, ports installedPorts) (err error) {
	if verify == nil || ports.openReaper == nil || ports.listen == nil {
		return fmt.Errorf("cortex: missing installed broker authority or services")
	}
	preparation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	configuration, err := ReadBrokerConfig(preparation, name)
	if err != nil {
		return err
	}
	cfg, worker := configuration.Settings, configuration.Worker
	hostPolicy, err := NewHostPolicy(preparation, cfg.HostPolicy)
	if err != nil {
		return err
	}
	evidencePolicy, err := NewEvidencePolicy(preparation, cfg.EvidencePolicy)
	if err != nil {
		return err
	}
	credentials, err := ReadDaemonCredentials(preparation, cfg.Credentials, worker.Settings.BrokerPublicKey, hostPolicy.Authenticate, cfg.PublicLimits)
	if err != nil {
		return err
	}
	defer clear(credentials.SigningKey)
	images, err := image.NewLocalSource(cfg.ImageRoot, cfg.ImageLimits.MaxBytes, cfg.ImageLimits.Readers)
	if err != nil {
		return err
	}
	storage, err := evidencedb.Open(preparation, cfg.Storage.Evidence)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, storage.Close()) }()
	var capturePolicy CapturePolicy
	if worker.Settings.CaptureRetention != 0 {
		capturePolicy = evidencePolicy.AuthorizeCapture
	}
	repository, err := NewEvidenceRepository(EvidenceRepositoryConfig{Storage: storage, Audit: storage, Inputs: evidencePolicy.AuthorizeInput, Captures: capturePolicy, Cleanup: evidencePolicy.AuthorizeCleanup, MaxBytes: cfg.EvidenceBytes})
	if err != nil {
		return err
	}
	workers, err := workerstore.Open(preparation, workerstore.Config{Database: cfg.Storage.WorkerJournal, Broker: worker.Settings.Broker, PublicKey: worker.Settings.BrokerPublicKey, MaxJobs: cfg.MaxJobs})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, workers.Close()) }()
	reaper, err := ports.openReaper(preparation)
	if reaper != nil {
		defer func() { err = errors.Join(err, reaper.Close()) }()
	}
	if err != nil {
		return err
	}
	if reaper == nil {
		return fmt.Errorf("cortex: service manager returned no reaper")
	}
	daemon, err := AssembleDaemon(preparation, HostConfig{
		WorkerConfig: cfg.WorkerConfig, Store: workers, Reaper: reaper,
		Broker:        broker.Config{Database: cfg.Storage.BrokerJournal, Name: worker.Settings.Broker, SigningKey: credentials.SigningKey, Authorize: hostPolicy.Authorize, MaxJobs: cfg.MaxJobs, MaxActive: cfg.MaxActive, Images: cfg.Images},
		Qualification: verify, AuthorizeLaunch: hostPolicy.Revalidate, Revalidate: hostPolicy.Revalidate,
		Inputs: repository, InputPolicy: evidencePolicy.AuthorizeInput, Images: images, ImagePolicy: hostPolicy.AuthorizeImage,
		Captures: CaptureRecoveryConfig{Repository: repository, Audit: repository, Authorize: evidencePolicy.AuthorizeCleanup, Actor: cfg.CleanupActor, PageSize: cfg.PageSize}, CapturePolicy: capturePolicy,
		Control: cfg.Control, Interval: cfg.Interval, PageSize: cfg.PageSize, DiscoveryLimit: cfg.DiscoveryLimit,
	})
	if err != nil {
		return err
	}
	actual, err := daemon.cfg.Controller.launcher.profile.Digest()
	if err != nil {
		return err
	}
	body, err := hostfs.ReadOwned(preparation, name, 0, 0o600, 1<<20)
	if err != nil {
		return err
	}
	if actual != worker.ProfileDigest || evidence.Digest(body) != configuration.SHA256 {
		return broker.ErrConflict
	}
	if preparationErr := preparation.Err(); preparationErr != nil {
		return preparationErr
	}
	cancel()
	return daemon.Run(ctx, func(lifetime context.Context, instance *broker.Broker) error {
		// Recovery must run even when qualification is unavailable. Public admission
		// follows recovery and a fresh deployment observation, never configuration alone.
		if _, qualificationErr := daemon.cfg.Attest(lifetime); qualificationErr != nil {
			return qualificationErr
		}
		listener, listenErr := ports.listen(lifetime, "tcp", cfg.Listen)
		if listenErr != nil {
			if listener != nil {
				return errors.Join(listenErr, listener.Close())
			}
			return listenErr
		}
		if listener == nil {
			return fmt.Errorf("cortex: public server returned no listener")
		}
		return credentials.Server.Serve(lifetime, listener, instance)
	})
}
