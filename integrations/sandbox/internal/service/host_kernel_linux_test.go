//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

type hostManager struct{ fakeManager }

func (m *hostManager) Start(context.Context, string) error { return m.step("start") }

func (*hostManager) Workers(ctx context.Context, _ int) ([]id.ID, error) { return nil, ctx.Err() }

func hostFixture(t *testing.T) (HostConfig, WorkerConfig, *hostManager) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("host assembly fixture requires root")
	}
	name, settings, private := workerConfigFixture(t)
	settings.ControlSocket = filepath.Join(filepath.Dir(settings.ControlSocket), "host.sock")
	writeWorkerConfig(t, name, settings)
	if err := os.Mkdir(filepath.Join(settings.Service.StateDirectory, "workspaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(settings.Service.StateDirectory, "launch"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := workerstore.Open(t.Context(), workerstore.Config{Database: filepath.Join(settings.Service.StateDirectory, "worker.db"), Broker: settings.Broker, PublicKey: settings.BrokerPublicKey, MaxJobs: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := &hostManager{}
	reaper := &Reaper{manager: manager, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}}
	repository := &workerEvidenceRepository{}
	policy := func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }
	cfg := HostConfig{WorkerConfig: name, Store: store, Reaper: reaper,
		Broker: broker.Config{Database: filepath.Join(settings.Service.StateDirectory, "broker.db"), Name: settings.Broker, SigningKey: private, Images: []string{"sha256:" + strings.Repeat("a", 64)}, Authorize: func(context.Context, broker.Identity, sandbox.Job) error { return nil }},
		Qualification: func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
			return qualificationFixture(identity), nil
		},
		AuthorizeLaunch: func(context.Context, sandbox.Job) error { return nil }, Revalidate: func(context.Context, sandbox.Job) error { return nil },
		Inputs: repository, InputPolicy: policy, Images: &workerImageSource{}, ImagePolicy: func(context.Context, sandbox.Job, string) error { return nil },
		Captures: CaptureRecoveryConfig{Repository: repository, Audit: repository, Authorize: func(context.Context, sandbox.Job, evidence.Ref) error { return nil }, Actor: "service:cleanup", PageSize: 2},
		Control:  ControlLimits{Connections: 3, Reports: 1, Timeout: time.Second}, Interval: time.Second, PageSize: 2, DiscoveryLimit: 100,
	}
	return cfg, settings, manager
}

func TestHostAssemblyBindsInstalledServicesWithoutStarting(t *testing.T) {
	cfg, settings, manager := hostFixture(t)
	qualified := false
	cfg.Qualification = func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
		qualified = true
		return qualificationFixture(identity), nil
	}
	daemon, err := AssembleDaemon(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if qualified || len(manager.calls) != 0 || daemon.cfg.Controller.Status().Running {
		t.Fatal("construction started or qualified worker")
	}
	c := daemon.cfg.Controller
	if daemon.cfg.Socket != settings.ControlSocket || c.launcher.store != cfg.Store || c.scanner.store != cfg.Store || c.scanner.workspaces == nil || c.scanner.workspaces.store != cfg.Store || c.scanner.captures.store != cfg.Store || c.scanner.captures.repository != cfg.Captures.Repository || c.capturePolicy != nil {
		t.Fatal("assembly lost installed configuration or shared recovery services")
	}
	loaded, err := ReadWorkerConfig(t.Context(), cfg.WorkerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if digest, digestErr := c.launcher.profile.Digest(); digestErr != nil || digest != loaded.ProfileDigest {
		t.Fatal("launcher profile not bound", digestErr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	err = daemon.Run(ctx, func(lifetime context.Context, instance *broker.Broker) error {
		if _, denied := instance.Capabilities(lifetime); !errors.Is(denied, broker.ErrDenied) {
			return errors.New("anonymous capability access permitted")
		}
		caller := broker.WithIdentity(lifetime, broker.Identity{Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "alice"})
		caps, capErr := instance.Capabilities(caller)
		if capErr != nil || caps.RuntimeDigest != loaded.RuntimeDigest {
			return errors.New("assembled broker lost attestation")
		}
		cancel()
		return lifetime.Err()
	})
	cancel()
	if !errors.Is(err, context.Canceled) || !qualified {
		t.Fatal("assembled daemon lifecycle failed", err)
	}
	if _, err = os.Lstat(settings.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned endpoint leaked", err)
	}
	if err = cfg.Store.CheckBroker(settings.Broker, settings.BrokerPublicKey); err != nil {
		t.Fatal("daemon closed caller-owned journal", err)
	}
}

func TestHostAssemblyRequiresCompleteAndConsistentServices(t *testing.T) {
	for _, mode := range []string{"store", "foreign-store", "reaper", "broker", "key", "runtime", "qualifier", "launch", "revalidate", "inputs", "input-policy", "images", "image-policy", "capture-store", "capture-repository", "capture-audit", "capture-cleanup", "capture-policy", "capture-retention", "reports", "page", "interval", "discovery"} {
		t.Run(mode, func(t *testing.T) {
			cfg, settings, manager := hostFixture(t)
			switch mode {
			case "store":
				cfg.Store = nil
			case "foreign-store":
				cfg.Store, _, _ = recoveryStore(t)
			case "reaper":
				cfg.Reaper = nil
			case "broker":
				cfg.Broker.Name = "other"
			case "key":
				cfg.Broker.SigningKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			case "runtime":
				cfg.Broker.Runtime = &Runtime{}
			case "qualifier":
				cfg.Qualification = nil
			case "launch":
				cfg.AuthorizeLaunch = nil
			case "revalidate":
				cfg.Revalidate = nil
			case "inputs":
				cfg.Inputs = nil
			case "input-policy":
				cfg.InputPolicy = nil
			case "images":
				cfg.Images = nil
			case "image-policy":
				cfg.ImagePolicy = nil
			case "capture-repository":
				cfg.Captures.Repository = nil
			case "capture-store":
				cfg.Captures.Store, _, _ = recoveryStore(t)
			case "capture-audit":
				cfg.Captures.Audit = nil
			case "capture-cleanup":
				cfg.Captures.Authorize = nil
			case "capture-policy":
				settings.CaptureRetention = time.Hour
				writeWorkerConfig(t, cfg.WorkerConfig, settings)
			case "capture-retention":
				cfg.CapturePolicy = cfg.InputPolicy
			case "reports":
				cfg.Control.Reports = 0
			case "page":
				cfg.PageSize = 0
			case "interval":
				cfg.Interval = 0
			case "discovery":
				cfg.DiscoveryLimit = 0
			}
			if daemon, err := AssembleDaemon(t.Context(), cfg); err == nil || daemon != nil {
				t.Fatal("incomplete host accepted")
			}
			if len(manager.calls) != 0 {
				t.Fatal("failed assembly reached service manager")
			}
		})
	}
}

func TestHostAssemblyEnablesCaptureOnlyWithInstalledRetention(t *testing.T) {
	cfg, settings, _ := hostFixture(t)
	settings.CaptureRetention = time.Hour
	writeWorkerConfig(t, cfg.WorkerConfig, settings)
	cfg.CapturePolicy = cfg.InputPolicy
	daemon, err := AssembleDaemon(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	controller := daemon.cfg.Controller
	if controller.capturePolicy == nil || controller.scanner.captures.repository != cfg.Captures.Repository {
		t.Fatal("capture authority did not share recovery repository")
	}
}

func TestHostAssemblyRecoveryRequiresTheStoredJob(t *testing.T) {
	cfg, _, _ := hostFixture(t)
	called := false
	cfg.Revalidate = func(context.Context, sandbox.Job) error { called = true; return nil }
	daemon, err := AssembleDaemon(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	receipt := hostReceipt(t, cfg, daemon)
	for _, missing := range []bool{false, true} {
		job := receipt.Job
		if missing {
			job.ID = id.New(sandbox.PrefixJob)
		} else {
			job.Principal = "mallory"
		}
		job, err = sandbox.Seal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err = daemon.cfg.Controller.scanner.revalidate(t.Context(), job); err == nil || called {
			t.Fatal("unbound job reached recovery authority", err)
		}
	}
}

func hostReceipt(t *testing.T, cfg HostConfig, daemon *Daemon) sandbox.Receipt {
	t.Helper()
	caps, err := daemon.cfg.Attest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "assembly", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "alice", Image: cfg.Broker.Images[0], Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: caps.Limits})
	if err != nil {
		t.Fatal(err)
	}
	receipt := sandbox.Receipt{SchemaVersion: 1, Job: job, Broker: cfg.Broker.Name, Capabilities: caps, AcceptedAt: time.Now().UTC()}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = ed25519.Sign(cfg.Broker.SigningKey, body)
	if _, err = cfg.Store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestHostAssemblyRevalidatesAcceptedQualification(t *testing.T) {
	for _, mode := range []string{"grant", "qualification", "expiry", "limits", "installation", "mutating-grant"} {
		t.Run(mode, func(t *testing.T) {
			cfg, settings, _ := hostFixture(t)
			changed := false
			cfg.Revalidate = func(ctx context.Context, job sandbox.Job) error {
				if cortex.ScopeFromContext(ctx).Canonical() != job.Scope.Canonical() {
					t.Fatal("signed scope lost")
				}
				if changed && mode == "grant" {
					return broker.ErrDenied
				}
				if mode == "mutating-grant" {
					job.Scope.Levels[0].Value = "other"
					job.Argv[0] = "other"
				}
				return nil
			}
			cfg.Qualification = func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
				q := qualificationFixture(identity)
				if changed {
					switch mode {
					case "qualification":
						q.Capabilities.Qualification = "different"
					case "expiry":
						q.Capabilities.QualifiedUntil = time.Now().Add(time.Second)
					case "limits":
						q.Capabilities.Limits.Processes--
					}
				}
				return q, nil
			}
			daemon, err := AssembleDaemon(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			receipt := hostReceipt(t, cfg, daemon)
			if err = daemon.cfg.Controller.scanner.eligible(t.Context(), receipt); err != nil {
				t.Fatal("initial eligibility", err)
			}
			changed = true
			if mode == "installation" {
				settings.Broker = "changed"
				writeWorkerConfig(t, cfg.WorkerConfig, settings)
			}
			err = daemon.cfg.Controller.scanner.eligible(t.Context(), receipt)
			if mode == "mutating-grant" {
				if err != nil {
					t.Fatal("policy mutated retained job", err)
				}
			} else if err == nil {
				t.Fatal("stale acceptance survived current authority change")
			}
		})
	}
}

func TestHostAssemblyReclaimsRetainedWorkspacesBeforeAdmission(t *testing.T) {
	cfg, settings, manager := hostFixture(t)
	root := filepath.Join(settings.Service.StateDirectory, "workspaces")
	key := id.New(sandbox.PrefixJob)
	directory := filepath.Join(root, key.String())
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "stage"), 0o700); err != nil {
		t.Fatal(err)
	}
	daemon, err := AssembleDaemon(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := daemon.cfg.Controller.scanner.Recover(t.Context())
	if err != nil || report.Workspaces.Cleaned != 1 || len(manager.calls) == 0 {
		t.Fatal(report, err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("retained workspace survived recovery", err)
	}
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err = AssembleDaemon(t.Context(), cfg); err == nil {
		t.Fatal("missing workspace root accepted")
	}
}

func TestHostAssemblyReclaimsLaunchCredentials(t *testing.T) {
	cfg, settings, _ := hostFixture(t)
	root := filepath.Join(settings.Service.StateDirectory, "launch")
	key := id.New(sandbox.PrefixJob)
	directory := filepath.Join(root, key.String())
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "claim.tmp"), []byte("interrupted credential"), 0o400); err != nil {
		t.Fatal(err)
	}
	daemon, err := AssembleDaemon(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := daemon.cfg.Controller.scanner.Recover(t.Context())
	if err != nil || report.Credentials.Cleaned != 1 {
		t.Fatal(report, err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("credential survived recovery", err)
	}
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err = AssembleDaemon(t.Context(), cfg); err == nil {
		t.Fatal("missing launch directory accepted")
	}
}

func TestHostAssemblyCredentialPublicationCannotReappear(t *testing.T) {
	launcher, receipt, _ := launcherFixture(t)
	root := filepath.Join(launcher.profile.StateDirectory, "launch")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	scanner, err := NewCredentialScanner(OrphanConfig{Store: launcher.store, Reaper: launcher.reaper, MaxUnits: 10, PageSize: 2}, root)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := launcher.store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	prepare := scanner.gate.prepare(launcher.store, func(ctx context.Context, profile LaunchProfile, owned workerstore.Claim) error {
		return writeCredential(ctx, profile, owned, 0)
	})
	if err = prepare(t.Context(), launcher.profile, claim); err != nil {
		t.Fatal(err)
	}
	if _, err = launcher.reaper.Reap(t.Context(), launcher.store, receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	report, err := scanner.Recover(t.Context())
	if err != nil || report.Cleaned != 1 {
		t.Fatal(report, err)
	}
	if err = prepare(t.Context(), launcher.profile, claim); err == nil {
		t.Fatal("credential recreated after cleanup")
	}
	if _, err = os.Stat(filepath.Join(root, receipt.Job.ID.String())); !os.IsNotExist(err) {
		t.Fatal("credential directory returned", err)
	}
}
