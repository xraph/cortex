package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func qualificationFixture(identity DeploymentIdentity) DeploymentQualification {
	return DeploymentQualification{Deployment: identity, Capabilities: sandbox.Capabilities{
		SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: identity.RuntimeDigest,
		Profile: sandbox.ProfileDenyAll, Qualification: "fixture-only",
		QualifiedUntil: time.Now().Add(time.Hour),
		Limits:         sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024},
	}}
}

func TestInstalledAttestationRechecksQualification(t *testing.T) {
	name, _, _ := workerConfigFixture(t)
	loaded, err := readWorkerConfig(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	attest, err := newDeploymentAttestation(t.Context(), name, func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
		calls++
		if identity.Broker != loaded.Settings.Broker || identity.ProfileDigest != loaded.ProfileDigest || identity.RuntimeDigest != loaded.RuntimeDigest {
			t.Fatal("verifier received an unbound deployment", identity)
		}
		if calls == 2 {
			return DeploymentQualification{}, broker.ErrDenied
		}
		return qualificationFixture(identity), nil
	}, uint32(os.Geteuid()))
	if err != nil || calls != 0 {
		t.Fatal("construction granted qualification", err)
	}
	if caps, checkErr := attest(t.Context()); checkErr != nil || caps.Qualification != "fixture-only" {
		t.Fatal("bound qualification rejected", checkErr)
	}
	if caps, checkErr := attest(t.Context()); !errors.Is(checkErr, broker.ErrDenied) || caps != (sandbox.Capabilities{}) {
		t.Fatal("revoked qualification reused", checkErr)
	}
}

func TestInstalledAttestationRejectsChangedInstallation(t *testing.T) {
	for _, during := range []bool{false, true} {
		for _, target := range []string{"configuration", "worker", "template", "runtime", "state"} {
			t.Run(target+"/"+map[bool]string{false: "before", true: "during"}[during], func(t *testing.T) {
				name, cfg, _ := workerConfigFixture(t)
				change := func() {
					var err error
					switch target {
					case "configuration":
						cfg.Broker = "changed"
						writeWorkerConfig(t, name, cfg)
					case "worker":
						err = os.WriteFile(cfg.Service.Worker.Path, []byte("changed worker"), 0o755)
					case "template":
						err = os.WriteFile(cfg.Service.TemplatePath, []byte("changed template"), 0o644)
					case "runtime":
						err = os.WriteFile(filepath.Join(cfg.Runtime.Directory, "runsc"), []byte("changed runtime"), 0o755)
					case "state":
						err = os.Chmod(cfg.Service.StateDirectory, 0o755)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				attest, err := newDeploymentAttestation(t.Context(), name, func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
					calls++
					if during {
						change()
					}
					return qualificationFixture(identity), nil
				}, uint32(os.Geteuid()))
				if err != nil {
					t.Fatal(err)
				}
				if !during {
					change()
				}
				if caps, checkErr := attest(t.Context()); checkErr == nil || caps != (sandbox.Capabilities{}) {
					t.Fatal("changed installation retained qualification", checkErr)
				}
				if !during && calls != 0 {
					t.Fatal("unverified installation reached qualification provider")
				}
			})
		}
	}
}

func TestInstalledAttestationRejectsUnboundOrInvalidQualification(t *testing.T) {
	for _, mode := range []string{"broker", "profile-digest", "runtime-digest", "capability-digest", "schema", "runtime", "profile", "empty", "oversized", "nul", "utf8", "expired", "limits", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			name, _, _ := workerConfigFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attest, err := newDeploymentAttestation(ctx, name, func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
				q := qualificationFixture(identity)
				switch mode {
				case "broker":
					q.Deployment.Broker = "other"
				case "profile-digest":
					q.Deployment.ProfileDigest = strings.Repeat("0", 64)
				case "runtime-digest":
					q.Deployment.RuntimeDigest = strings.Repeat("0", 64)
				case "capability-digest":
					q.Capabilities.RuntimeDigest = strings.Repeat("0", 64)
				case "schema":
					q.Capabilities.SchemaVersion++
				case "runtime":
					q.Capabilities.Runtime = "host"
				case "profile":
					q.Capabilities.Profile = "other"
				case "empty":
					q.Capabilities.Qualification = " "
				case "oversized":
					q.Capabilities.Qualification = strings.Repeat("q", 513)
				case "nul":
					q.Capabilities.Qualification = "q\x00"
				case "utf8":
					q.Capabilities.Qualification = "\xff"
				case "expired":
					q.Capabilities.QualifiedUntil = time.Now().Add(-time.Second)
				case "limits":
					q.Capabilities.Limits.MemoryBytes = -1
				case "error":
					return q, broker.ErrDenied
				case "cancel":
					cancel()
				}
				return q, nil
			}, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			if caps, checkErr := attest(ctx); checkErr == nil || caps != (sandbox.Capabilities{}) {
				t.Fatal("unsafe qualification escaped", checkErr)
			}
		})
	}
}

func TestInstalledAttestationRejectsIncompleteConstruction(t *testing.T) {
	name, _, _ := workerConfigFixture(t)
	verify := func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
		return qualificationFixture(identity), nil
	}
	if attest, err := newDeploymentAttestation(t.Context(), name, nil, uint32(os.Geteuid())); err == nil || attest != nil {
		t.Fatal("missing qualification verifier accepted")
	}
	if attest, err := newDeploymentAttestation(t.Context(), name+".absent", verify, uint32(os.Geteuid())); err == nil || attest != nil {
		t.Fatal("missing installed configuration accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if attest, err := newDeploymentAttestation(ctx, name, verify, uint32(os.Geteuid())); !errors.Is(err, context.Canceled) || attest != nil {
		t.Fatal("cancelled construction accepted", err)
	}
}

func TestInstalledAttestationConcurrentObservationsAndCancellation(t *testing.T) {
	name, _, _ := workerConfigFixture(t)
	var calls atomic.Int32
	attest, err := newDeploymentAttestation(t.Context(), name, func(ctx context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
		calls.Add(1)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Error("qualification observation is unbounded")
		}
		return qualificationFixture(identity), nil
	}, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, checkErr := attest(t.Context()); checkErr != nil {
				t.Error(checkErr)
			}
		})
	}
	group.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if caps, checkErr := attest(ctx); !errors.Is(checkErr, context.Canceled) || caps != (sandbox.Capabilities{}) {
		t.Fatal("cancelled observation succeeded", checkErr)
	}
	if calls.Load() != 8 {
		t.Fatal("qualification reused or cancelled request reached verifier", calls.Load())
	}
}
