package extension

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xraph/nexus"
	"github.com/xraph/nexus/pipeline"
	"github.com/xraph/nexus/provider"
	"github.com/xraph/nexus/tenant"
	"github.com/xraph/shield"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/vessel"
	"github.com/xraph/weave"
	weaveengine "github.com/xraph/weave/engine"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/knowledge"
	weaveknowledge "github.com/xraph/cortex/knowledge/weave"
	"github.com/xraph/cortex/llm"
	nexusllm "github.com/xraph/cortex/llm/nexus"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	shieldsafety "github.com/xraph/cortex/safety/shield"
	"github.com/xraph/cortex/store/scopespy"
)

// tenantProvider is a nexus provider that records the tenant each request
// arrived under.
type tenantProvider struct {
	mu      sync.Mutex
	tenants []string
}

func (*tenantProvider) Name() string { return "fake" }
func (*tenantProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Chat: true, Streaming: true}
}

func (*tenantProvider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "m", Provider: "fake"}}, nil
}

func (p *tenantProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.mu.Lock()
	p.tenants = append(p.tenants, pipeline.TenantID(ctx))
	p.mu.Unlock()
	return &provider.CompletionResponse{Model: req.Model, Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}, nil
}

func (*tenantProvider) CompleteStream(context.Context, *provider.CompletionRequest) (provider.Stream, error) {
	return nil, provider.ErrNotSupported
}

func (*tenantProvider) Embed(context.Context, *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return nil, provider.ErrNotSupported
}
func (*tenantProvider) Healthy(context.Context) bool { return true }

func TestNexusOptionsReachTheAutoAdapter(t *testing.T) {
	fake := &tenantProvider{}
	gw := nexus.New(nexus.WithProvider(fake))
	if err := gw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	tn, err := gw.Tenants().Create(context.Background(), &tenant.CreateInput{Name: "ws", Slug: "ws"})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	provide := func(app interface{ Container() vessel.Vessel }) {
		if err := vessel.Provide(app.Container(), func() (*nexus.Gateway, error) { return gw, nil }); err != nil {
			t.Fatalf("provide: %v", err)
		}
	}
	toTenant := nexusllm.WithTenant(func(context.Context, cortex.Scope) (string, error) { return tn.ID.String(), nil })

	app := newTestApp(t, "cortex-nexus-options-test")
	provide(app)
	e := New(WithStore(scopespy.New()), WithNexusOptions(toTenant))
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The scope's own value (ws_x) is not a nexus tenant, so the call only
	// succeeds when the option reached the adapter.
	if _, err := e.Engine().LLM().Complete(scopeCtx(), &llm.Request{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	fake.mu.Lock()
	got := append([]string(nil), fake.tenants...)
	fake.mu.Unlock()
	if len(got) != 1 || got[0] != tn.ID.String() {
		t.Fatalf("tenants = %v, want [%s]", got, tn.ID)
	}

	// The options do not change the order rule: WithoutAutoAdapters still
	// leaves nexus out, and a host's engine.WithLLM still wins.
	app = newTestApp(t, "cortex-nexus-options-off-test")
	provide(app)
	off := New(WithStore(scopespy.New()), WithNexusOptions(toTenant), WithoutAutoAdapters())
	if err := off.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if off.Engine().LLM() != nil {
		t.Fatalf("LLM = %T, want nil without automatic adapters", off.Engine().LLM())
	}
	app = newTestApp(t, "cortex-nexus-options-host-test")
	provide(app)
	host := New(WithStore(scopespy.New()), WithNexusOptions(toTenant), WithEngineOption(engine.WithLLM(fakeLLM{})))
	if err := host.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if host.Engine().LLM() != llm.Client(fakeLLM{}) {
		t.Fatalf("LLM = %T, want the host's fake", host.Engine().LLM())
	}
}

func TestWeaveOptionsReachTheAutoAdapter(t *testing.T) {
	eng, err := weaveengine.New()
	if err != nil {
		t.Fatalf("weave engine: %v", err)
	}
	provide := func(app interface{ Container() vessel.Vessel }) {
		if err := vessel.Provide(app.Container(), func() (*weaveengine.Engine, error) { return eng, nil }); err != nil {
			t.Fatalf("provide: %v", err)
		}
	}
	// A tenant func that names no tenant makes the adapter's refusal
	// visible without any weave store behind it.
	refuse := weaveknowledge.WithTenant(func(context.Context, cortex.Scope) (string, error) { return "", nil })

	app := newTestApp(t, "cortex-weave-options-test")
	provide(app)
	e := New(WithStore(scopespy.New()), WithWeaveOptions(refuse))
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.Engine().Knowledge() == nil {
		t.Fatal("Knowledge = nil, want the weave adapter")
	}
	if _, err := e.Engine().Knowledge().ListCollections(scopeCtx()); !errors.Is(err, weave.ErrTenantRequired) {
		t.Fatalf("ListCollections err = %v, want the adapter's tenant refusal", err)
	}

	app = newTestApp(t, "cortex-weave-options-off-test")
	provide(app)
	off := New(WithStore(scopespy.New()), WithWeaveOptions(refuse), WithoutAutoAdapters())
	if err := off.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if off.Engine().Knowledge() != nil {
		t.Fatalf("Knowledge = %T, want nil without automatic adapters", off.Engine().Knowledge())
	}

	app = newTestApp(t, "cortex-weave-options-host-test")
	provide(app)
	host := New(WithStore(scopespy.New()), WithWeaveOptions(refuse), WithEngineOption(engine.WithKnowledge(fakeKnowledge{})))
	if err := host.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if host.Engine().Knowledge() != knowledge.Provider(fakeKnowledge{}) {
		t.Fatalf("Knowledge = %T, want the host's fake", host.Engine().Knowledge())
	}
}

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
