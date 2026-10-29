package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/shield"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	shieldsafety "github.com/xraph/cortex/safety/shield"
	"github.com/xraph/cortex/store/scopespy"
)

// allowScanner allows everything, so a run it alone screened completes.
type allowScanner struct{}

func (allowScanner) ScanInput(context.Context, *safety.ScanRequest) (*safety.ScanResult, error) {
	return &safety.ScanResult{Decision: safety.DecisionAllow, ProfileUsed: "allow"}, nil
}

func (allowScanner) ScanOutput(context.Context, *safety.ScanRequest) (*safety.ScanResult, error) {
	return &safety.ScanResult{Decision: safety.DecisionAllow, ProfileUsed: "allow"}, nil
}

func TestShieldOptionsReachTheAutoAdapter(t *testing.T) {
	eng, engErr := shieldengine.New()
	if engErr != nil {
		t.Fatalf("shield engine: %v", engErr)
	}
	provide := func(app interface{ Container() vessel.Vessel }) {
		if err := vessel.Provide(app.Container(), func() (*shieldengine.Engine, error) { return eng, nil }); err != nil {
			t.Fatalf("provide: %v", err)
		}
	}
	// A tenant func that names no tenant makes the adapter's refusal
	// visible without any shield store behind it.
	refuse := shieldsafety.WithTenant(func(context.Context, cortex.Scope) (string, error) { return "", nil })

	app := newTestApp(t, "cortex-shield-options-test")
	provide(app)
	e := New(WithStore(scopespy.New()), WithShieldOptions(refuse))
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.Engine().Safety() == nil {
		t.Fatal("Safety = nil, want the shield adapter")
	}
	_, err := e.Engine().Safety().ScanInput(scopeCtx(), &safety.ScanRequest{Content: "hello"})
	if !errors.Is(err, safety.ErrUnavailable) || !errors.Is(err, shield.ErrTenantRequired) {
		t.Fatalf("ScanInput err = %v, want the adapter's tenant refusal", err)
	}

	// The adapter is the publication gate too: a host that only replaces
	// the scanner still has its runs screened by shield, which refuses.
	app = newTestApp(t, "cortex-shield-options-gate-test")
	provide(app)
	gated := New(WithStore(scopespy.New()), WithShieldOptions(refuse), WithEngineOption(engine.WithLLM(fakeLLM{})), WithEngineOption(engine.WithSafety(allowScanner{})))
	if err := gated.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r, runErr := gated.Engine().RunAgent(scopeCtx(), "assistant", "hello", nil); runErr == nil && r.State == run.StateCompleted {
		t.Fatalf("run completed with output %q, want shield's gate to refuse it", r.Output)
	}

	app = newTestApp(t, "cortex-shield-options-off-test")
	provide(app)
	off := New(WithStore(scopespy.New()), WithShieldOptions(refuse), WithoutAutoAdapters())
	if err := off.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if off.Engine().Safety() != nil {
		t.Fatalf("Safety = %T, want nil without automatic adapters", off.Engine().Safety())
	}

	app = newTestApp(t, "cortex-shield-options-host-test")
	provide(app)
	host := New(WithStore(scopespy.New()), WithShieldOptions(refuse), WithEngineOption(engine.WithSafety(allowScanner{})))
	if err := host.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if host.Engine().Safety() != safety.Scanner(allowScanner{}) {
		t.Fatalf("Safety = %T, want the host's scanner", host.Engine().Safety())
	}
}
