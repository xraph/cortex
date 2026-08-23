package fabriqbrain

import (
	"testing"

	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"
	log "github.com/xraph/go-utils/log"
	"github.com/xraph/vessel"
)

// containerWithFabric registers the query port, and the registry only when
// withRegistry is set, so a test can reproduce the half-wired container.
func containerWithFabric(t *testing.T, withRegistry bool) vessel.Vessel {
	t.Helper()
	c := vessel.New()
	if err := vessel.Provide(c, func() (query.Fabric, error) { return stubFabric{}, nil }); err != nil {
		t.Fatalf("Provide fabric: %v", err)
	}
	if withRegistry {
		if err := vessel.Provide(c, func() (*registry.Registry, error) { return registry.New(), nil },
			vessel.WithName(registry.ServiceName)); err != nil {
			t.Fatalf("Provide registry: %v", err)
		}
	}
	return c
}

// A container with no fabriq at all is not a misconfiguration, it is an app
// that does not use fabriq. That path stays quiet.
func TestInjectFabric_AbsentFabricDoesNotLog(t *testing.T) {
	cl := &captureLogger{Logger: log.NewNoopLogger()}
	if _, _, err := injectFabric(vessel.New(), cl); err == nil {
		t.Fatal("injectFabric on an empty container = nil error, want one")
	}
	if len(cl.errors) != 0 {
		t.Fatalf("absent fabriq logged %v, want silence", cl.errors)
	}
}

// A facade WITHOUT its registry is a misconfiguration: fabriq is present but
// wired wrong. It must not degrade to a silent no-op brain.
func TestInjectFabric_MissingRegistryIsLogged(t *testing.T) {
	cl := &captureLogger{Logger: log.NewNoopLogger()}
	c := containerWithFabric(t, false)

	if _, _, err := injectFabric(c, cl); err == nil {
		t.Fatal("injectFabric without a registry = nil error, want one")
	}
	if len(cl.errors) == 0 {
		t.Fatal("missing registry was silently dropped; expected an error log")
	}
}

// The fully wired container resolves both and logs nothing.
func TestInjectFabric_Wired(t *testing.T) {
	cl := &captureLogger{Logger: log.NewNoopLogger()}
	c := containerWithFabric(t, true)

	f, reg, err := injectFabric(c, cl)
	if err != nil {
		t.Fatalf("injectFabric on a wired container: %v", err)
	}
	if f == nil || reg == nil {
		t.Fatalf("injectFabric = (%v, %v), want both non-nil", f, reg)
	}
	if len(cl.errors) != 0 {
		t.Fatalf("wired container logged %v, want silence", cl.errors)
	}
}
