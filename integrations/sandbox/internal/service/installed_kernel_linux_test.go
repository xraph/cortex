//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/evidencedb"
	"github.com/xraph/cortex/integrations/sandbox/internal/journal"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

func installedFixture(t *testing.T) (string, BrokerConfig, daemonCredentialFixture, *Reaper, *atomic.Bool) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("installed broker fixture requires root")
	}
	credentials := daemonCredentialsFixture(t)
	name, cfg := brokerConfigFixture(t, credentials)
	worker, err := ReadWorkerConfig(t.Context(), cfg.WorkerConfig)
	if err != nil {
		t.Fatal(err)
	}
	settings := worker.Settings
	settings.ControlSocket = filepath.Join(filepath.Dir(settings.ControlSocket), "installed.sock")
	writeWorkerConfig(t, cfg.WorkerConfig, settings)
	for _, directory := range []string{filepath.Join(settings.Service.StateDirectory, "workspaces"), filepath.Join(settings.Service.StateDirectory, "launch"), filepath.Join(cfg.ImageRoot, "blobs", "sha256"), cfg.InputRoot} {
		if err = os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, policy, _, _ := policyFixture(t, credentials)
	writeInstalledPolicy(t, cfg.HostPolicy, policy)
	writeInstalledPolicy(t, cfg.EvidencePolicy, EvidencePolicyDocument{SchemaVersion: 1, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)})
	closed := &atomic.Bool{}
	reaper := &Reaper{manager: &hostManager{}, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}, close: func() error { closed.Store(true); return nil }}
	return name, cfg, credentials, reaper, closed
}

func assertInstalledOwnershipReleased(t *testing.T, cfg BrokerConfig) {
	t.Helper()
	worker, err := ReadWorkerConfig(t.Context(), cfg.WorkerConfig)
	if err != nil {
		t.Fatal(err)
	}
	evidenceStore, err := evidencedb.Open(t.Context(), cfg.Storage.Evidence)
	if err != nil {
		t.Fatal("evidence ownership retained", err)
	}
	if err = evidenceStore.Close(); err != nil {
		t.Fatal(err)
	}
	workers, err := workerstore.Open(t.Context(), workerstore.Config{Database: cfg.Storage.WorkerJournal, Broker: worker.Settings.Broker, PublicKey: worker.Settings.BrokerPublicKey, MaxJobs: cfg.MaxJobs})
	if err != nil {
		t.Fatal("worker ownership retained", err)
	}
	if err = workers.Close(); err != nil {
		t.Fatal(err)
	}
	receipts, err := journal.Open(t.Context(), cfg.Storage.BrokerJournal, journal.Broker)
	if err != nil {
		t.Fatal("broker ownership retained", err)
	}
	if err = receipts.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledBrokerServesMTLSAfterRecoveryAndJoinsResources(t *testing.T) {
	name, cfg, credentials, reaper, closed := installedFixture(t)
	address := make(chan string, 1)
	qualified := &atomic.Bool{}
	ports := installedPorts{openReaper: func(context.Context) (*Reaper, error) { return reaper, nil }, listen: func(ctx context.Context, network, requested string) (net.Listener, error) {
		if requested != cfg.Listen || network != "tcp" || !qualified.Load() {
			t.Error("listener opened before qualified startup")
		}
		listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
		if err == nil {
			address <- listener.Addr().String()
		}
		return listener, err
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runInstalledDaemon(ctx, name, func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
			qualified.Store(true)
			return qualificationFixture(identity), nil
		}, ports)
	}()
	var endpoint string
	select {
	case endpoint = <-address:
	case err := <-done:
		t.Fatal("installed startup", err)
	case <-time.After(10 * time.Second):
		t.Fatal("installed startup stalled")
	}
	transport := credentialTransport(t, credentials)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	getCapabilities := func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+endpoint+"/v1/capabilities", nil)
		if err != nil {
			return nil, err
		}
		return client.Do(request)
	}
	response, err := getCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("authenticated capabilities", response.StatusCode)
	}
	now := time.Now()
	writeInstalledPolicy(t, cfg.HostPolicy, HostPolicyDocument{SchemaVersion: 1, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)})
	response, err = getCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("reused connection retained revoked authority")
	}
	transport.CloseIdleConnections()
	cancel()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !closed.Load() {
		t.Fatal("service connection retained")
	}
	assertInstalledOwnershipReleased(t, cfg)
}

func TestInstalledBrokerRefusesUnavailableQualificationAndCleansFailures(t *testing.T) {
	for _, mode := range []string{"qualifier", "input-missing", "input-mode", "service", "service-empty", "service-partial", "qualification", "listener", "listener-empty", "listener-partial", "worker-changed", "config-changed"} {
		t.Run(mode, func(t *testing.T) {
			name, cfg, _, reaper, closed := installedFixture(t)
			listens := 0
			var partial net.Listener
			ports := installedPorts{openReaper: func(context.Context) (*Reaper, error) { return reaper, nil }, listen: func(context.Context, string, string) (net.Listener, error) {
				listens++
				return nil, errors.New("listener unavailable")
			}}
			verify := QualificationVerifier(func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
				return qualificationFixture(identity), nil
			})
			switch mode {
			case "qualifier":
				verify = nil
			case "input-missing":
				if err := os.Remove(cfg.InputRoot); err != nil {
					t.Fatal(err)
				}
			case "input-mode":
				if err := os.Chmod(cfg.InputRoot, 0o755); err != nil {
					t.Fatal(err)
				}
			case "service":
				ports.openReaper = func(context.Context) (*Reaper, error) { return nil, errors.New("manager unavailable") }
			case "service-empty":
				ports.openReaper = func(context.Context) (*Reaper, error) { return nil, nil }
			case "service-partial":
				ports.openReaper = func(context.Context) (*Reaper, error) { return reaper, errors.New("partial manager") }
			case "listener-empty":
				ports.listen = func(context.Context, string, string) (net.Listener, error) { listens++; return nil, nil }
			case "listener-partial":
				ports.listen = func(ctx context.Context, _, _ string) (net.Listener, error) {
					listens++
					var listenErr error
					partial, listenErr = new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
					if listenErr != nil {
						return nil, listenErr
					}
					return partial, errors.New("partial listener")
				}
			case "qualification":
				verify = func(context.Context, DeploymentIdentity) (DeploymentQualification, error) {
					return DeploymentQualification{}, broker.ErrDenied
				}
			case "worker-changed":
				ports.openReaper = func(ctx context.Context) (*Reaper, error) {
					current, err := ReadWorkerConfig(ctx, cfg.WorkerConfig)
					if err != nil {
						return nil, err
					}
					current.Settings.ImageStaging.CompressedBytes /= 2
					writeWorkerConfig(t, cfg.WorkerConfig, current.Settings)
					return reaper, nil
				}
			case "config-changed":
				ports.openReaper = func(context.Context) (*Reaper, error) {
					cfg.MaxActive++
					writeInstalledPolicy(t, name, cfg)
					return reaper, nil
				}
			}
			if err := runInstalledDaemon(t.Context(), name, verify, ports); err == nil {
				t.Fatal("failed startup succeeded")
			}
			expected := 0
			if mode == "listener" || mode == "listener-empty" || mode == "listener-partial" {
				expected = 1
			}
			if listens != expected {
				t.Fatal("opened listener before startup completed", listens)
			}
			early := mode == "qualifier" || mode == "input-missing" || mode == "input-mode"
			if mode != "service" && mode != "service-empty" && !early && !closed.Load() {
				t.Fatal("manager leaked")
			}
			if mode == "listener-partial" {
				if partial == nil {
					t.Fatal("partial listener was not created")
				}
				if closeErr := partial.Close(); !errors.Is(closeErr, net.ErrClosed) {
					t.Fatal("partial listener retained", closeErr)
				}
			}
			if early {
				if _, err := os.Stat(cfg.Storage.Evidence); !os.IsNotExist(err) {
					t.Fatal("invalid installation opened storage", err)
				}
			} else {
				assertInstalledOwnershipReleased(t, cfg)
			}
		})
	}
}
