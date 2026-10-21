package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
)

func profileFixture(t *testing.T) LaunchProfile {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	p := LaunchProfile{SchemaVersion: 1, TemplatePath: filepath.Join(root, "cortex-sandbox-worker@.service"), StateDirectory: filepath.Join(root, "state"), CPUMillis: 2000, MemoryBytes: 512 << 20, Tasks: 256, WallTime: 5 * time.Minute}
	for _, entry := range []struct {
		target     *hostfs.Pin
		name, body string
		mode       uint32
	}{{&p.Worker, "worker", "trusted worker", 0o755}, {&p.Config, "worker.json", "{}", 0o600}} {
		*entry.target = hostfs.Pin{Path: filepath.Join(root, entry.name), SHA256: evidence.Digest([]byte(entry.body)), Bytes: int64(len(entry.body)), Mode: entry.mode}
		if err = os.WriteFile(entry.target.Path, []byte(entry.body), os.FileMode(entry.mode)); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Mkdir(p.StateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := p.Template()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.TemplatePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLaunchProfilePinsBytesAndRejectsWritableDeployment(t *testing.T) {
	p := profileFixture(t)
	if err := verifyProfile(t.Context(), p, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Worker.Path, []byte("changed worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyProfile(t.Context(), p, uint32(os.Geteuid())); err == nil {
		t.Fatal("changed worker admitted")
	}
}

func TestProfileRejectsInjectedOrOverlappingPathsAndUnboundedLimits(t *testing.T) {
	for _, change := range []func(*LaunchProfile){func(p *LaunchProfile) { p.Worker.Path += "\nExecStart=/evil" }, func(p *LaunchProfile) { p.Config.Path = "/etc/%i.json" }, func(p *LaunchProfile) { p.StateDirectory = "/" }, func(p *LaunchProfile) { p.Config.Path = p.StateDirectory + "/config" }, func(p *LaunchProfile) { p.Worker.SHA256 = "" }, func(p *LaunchProfile) { p.Worker.Mode = 0o777 }, func(p *LaunchProfile) { p.WallTime = 0 }, func(p *LaunchProfile) { p.Tasks = 0 }, func(p *LaunchProfile) { p.MemoryBytes = 0 }, func(p *LaunchProfile) { p.CPUMillis = 0 }} {
		p := profileFixture(t)
		change(&p)
		if _, err := p.Template(); err == nil {
			t.Fatal("unsafe profile rendered")
		}
	}
}

func TestTemplateContainsOnlyFixedWorkerCommandAndWholeServiceLimits(t *testing.T) {
	p := profileFixture(t)
	body, err := p.Template()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Slice=system.slice\n", "Delegate=cpu memory pids\n", "DelegateSubgroup=supervisor\n", "Restart=no\n", "KillMode=control-group\n", "MemorySwapMax=0\n", "CPUQuota=200.0%\n", "LoadCredential=cortex-claim:" + p.StateDirectory + "/launch/%i/claim.json\n"} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("missing control %q", expected)
		}
	}
	if strings.Count(string(body), "ExecStart=") != 1 {
		t.Fatal("multiple worker commands")
	}
}

func TestAdmissionRejectsChangedFilesBeforeLoadingService(t *testing.T) {
	p := profileFixture(t)
	if err := os.WriteFile(p.TemplatePath, []byte("[Service]\nExecStart=/evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	check := func(context.Context, string) (map[string]any, error) { calls++; return nil, nil }
	if err := checkDeployment(t.Context(), p, id.New(sandbox.PrefixJob), uint32(os.Geteuid()), 0, check); err == nil || calls != 0 {
		t.Fatal("changed template reached service admission")
	}
}

func TestAdmissionLoadsEffectiveSettingsWithoutStartingWorker(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	values := launchProperties(p, key)
	properties := make(map[string]dbus.Variant, len(values))
	for name, value := range values {
		properties[name] = dbus.MakeVariant(value)
	}
	b := &fakeBus{reply: func(c busCall) ([]any, error) {
		switch c.method {
		case managerInterface + "LoadUnit":
			return []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/test")}, nil
		case "org.freedesktop.DBus.Properties.GetAll":
			return []any{properties}, nil
		default:
			t.Fatalf("unexpected manager operation: %s", c.method)
			return nil, nil
		}
	}}
	manager := &systemManager{bus: b}
	if err := checkDeployment(t.Context(), p, key, uint32(os.Geteuid()), 0, manager.Loaded); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 2 {
		t.Fatalf("unexpected calls: %v", b.calls)
	}
}
