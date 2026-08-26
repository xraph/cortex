package extension

import (
	"net/http"
	"testing"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/engine"
)

// fakeRoutes stands in for whatever a host mounts. It exists to prove the
// extension never needs the api package to have an HTTP surface.
type fakeRoutes struct{ registered int }

func (f *fakeRoutes) Handler() http.Handler { return http.NotFoundHandler() }

func (f *fakeRoutes) RegisterRoutes(forge.Router) error {
	f.registered++
	return nil
}

// An extension built without WithRoutes has to be usable, not merely
// constructible. Routes reports nothing mounted and Handler answers rather
// than panicking, which is what a host running only the engine relies on.
func TestExtension_WithoutRoutesMountsNothing(t *testing.T) {
	e := New()

	if e.Routes() != nil {
		t.Errorf("Routes() = %v on an extension built without WithRoutes, want nil", e.Routes())
	}
	if h := e.Handler(); h == nil {
		t.Error("Handler() returned nil rather than a 404 handler")
	}
	if err := e.RegisterRoutes(nil); err != nil {
		t.Errorf("RegisterRoutes on an extension with no route set: %v", err)
	}
}

// WithRoutes stores the builder rather than calling it, because the engine
// and the router it needs do not exist until Register runs.
func TestWithRoutes_DefersUntilRegister(t *testing.T) {
	calls := 0
	e := New(WithRoutes(func(*engine.Engine, forge.Router) RouteSet {
		calls++
		return &fakeRoutes{}
	}))

	if calls != 0 {
		t.Errorf("the route builder ran %d times during New, want 0", calls)
	}
	if e.buildRoutes == nil {
		t.Fatal("WithRoutes did not record the builder")
	}
	if got := e.buildRoutes(nil, nil); got == nil {
		t.Error("the recorded builder returned nil")
	}
	if calls != 1 {
		t.Errorf("the builder ran %d times when invoked once, want 1", calls)
	}
}

// A host that mounts a route set gets its RegisterRoutes called.
func TestExtension_RegisterRoutesDelegates(t *testing.T) {
	f := &fakeRoutes{}
	e := New()
	e.routes = f

	if err := e.RegisterRoutes(nil); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	if f.registered != 1 {
		t.Errorf("the route set was registered %d times, want 1", f.registered)
	}
}

// The nil check in mountRoutes is what stops a host that never asked for
// REST from panicking during Register. That is the default path now, so it
// matters more than it reads.
func TestMountRoutes_NilRouteSetDoesNotDereference(t *testing.T) {
	e := New()

	if err := e.mountRoutes(nil); err != nil {
		t.Fatalf("mountRoutes with no route set: %v", err)
	}
}

// DisableRoutes has to keep winning over a route set the host did mount.
func TestMountRoutes_DisableRoutesSkipsAMountedSet(t *testing.T) {
	f := &fakeRoutes{}
	e := New()
	e.routes = f
	e.config.DisableRoutes = true

	if err := e.mountRoutes(nil); err != nil {
		t.Fatalf("mountRoutes: %v", err)
	}
	if f.registered != 0 {
		t.Errorf("routes were registered %d times with DisableRoutes set, want 0", f.registered)
	}
}
