package extension

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	"github.com/xraph/nexus"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/vessel"
	weaveengine "github.com/xraph/weave/engine"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store/scopespy"
)

func registerExt(t *testing.T, e *Extension) *engine.Engine {
	t.Helper()
	if err := e.Register(newTestApp(t, "cortex-config-test")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return e.Engine()
}

func TestExtensionAppliesEngineDefaults(t *testing.T) {
	eng := registerExt(t, New(WithStore(scopespy.New()), WithConfig(Config{
		DefaultModel:         "m1",
		DefaultMaxSteps:      7,
		DefaultMaxTokens:     99,
		DefaultTemperature:   0.2,
		DefaultReasoningLoop: "react",
	})))
	want := cortex.Config{
		DefaultModel:         "m1",
		DefaultMaxSteps:      7,
		DefaultMaxTokens:     99,
		DefaultTemperature:   0.2,
		DefaultReasoningLoop: "react",
		ShutdownTimeout:      DefaultConfig().ShutdownTimeout,
		RunConcurrency:       0,
	}
	if got := eng.Config(); got != want {
		t.Fatalf("engine config = %+v, want %+v", got, want)
	}
}

func TestExtensionHostConfigOptionWins(t *testing.T) {
	eng := registerExt(t, New(
		WithStore(scopespy.New()),
		WithConfig(Config{DefaultModel: "m1"}),
		WithEngineOption(engine.WithConfig(cortex.Config{DefaultModel: "host"})),
	))
	if got := eng.Config().DefaultModel; got != "host" {
		t.Fatalf("DefaultModel = %q, want the host's %q", got, "host")
	}
}

func TestExtensionAppliesRunConcurrency(t *testing.T) {
	eng := registerExt(t, New(WithStore(scopespy.New()), WithConfig(Config{RunConcurrency: 3})))
	if got := eng.RunConcurrency(); got != 3 {
		t.Fatalf("RunConcurrency = %d, want 3", got)
	}
}

func TestExtensionDefaultRunConcurrencyIsUnlimited(t *testing.T) {
	eng := registerExt(t, New(WithStore(scopespy.New())))
	if got := eng.RunConcurrency(); got != 0 {
		t.Fatalf("RunConcurrency = %d, want 0 (unlimited)", got)
	}
	if got := DefaultConfig().RunConcurrency; got != 0 {
		t.Fatalf("extension DefaultConfig().RunConcurrency = %d, want 0", got)
	}
	if got := cortex.DefaultConfig().RunConcurrency; got != 0 {
		t.Fatalf("cortex DefaultConfig().RunConcurrency = %d, want 0", got)
	}
}

// blockingLLM holds every call until release is closed.
type blockingLLM struct {
	entered chan struct{}
	release chan struct{}
}

func (m *blockingLLM) Complete(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	m.entered <- struct{}{}
	select {
	case <-m.release:
		return &llm.Response{Content: "done", FinishReason: "stop"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *blockingLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("blockingLLM: streaming not supported")
}

func TestExtensionStopUsesShutdownTimeout(t *testing.T) {
	model := &blockingLLM{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e := New(
		WithStore(scopespy.New()),
		WithConfig(Config{ShutdownTimeout: 50 * time.Millisecond}),
		WithEngineOption(engine.WithLLM(model)),
	)
	eng := registerExt(t, e)

	var runs sync.WaitGroup
	t.Cleanup(func() {
		close(model.release)
		runs.Wait()
	})
	runs.Add(1)
	go func() {
		defer runs.Done()
		_, _ = eng.RunAgent(scopeCtx(), "assistant", "blocks", nil)
	}()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the model")
	}

	// Stop runs on its own goroutine so an unbounded one fails the test
	// instead of hanging it. The cleanup releases the run, which lets a
	// stuck Stop return before the store goes away.
	stopped := make(chan error, 1)
	runs.Add(1)
	go func() {
		defer runs.Done()
		stopped <- e.Stop(context.Background())
	}()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Stop = %v, want context.DeadlineExceeded from the 50ms shutdown timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within 1s; shutdown_timeout does not bound it")
	}
}

type fakeLLM struct{}

func (fakeLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: "from the host model", FinishReason: "stop"}, nil
}

func (fakeLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("fakeLLM: streaming not supported")
}

type fakeKnowledge struct{}

func (fakeKnowledge) Retrieve(context.Context, string, *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return nil, nil
}

func (fakeKnowledge) ListCollections(context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}

// countingGate approves everything and counts what it was asked.
type countingGate struct{ calls atomic.Int32 }

func (g *countingGate) Check(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	g.calls.Add(1)
	return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
}

// provideAdapters puts a nexus gateway, a shield engine and a weave
// engine in the app's container, which is what the automatic adapters
// look for.
func provideAdapters(t *testing.T, c vessel.Vessel) {
	t.Helper()
	gw := nexus.New()
	shield, err := shieldengine.New()
	if err != nil {
		t.Fatalf("shield engine: %v", err)
	}
	weave, err := weaveengine.New()
	if err != nil {
		t.Fatalf("weave engine: %v", err)
	}
	for _, provide := range []any{
		func() (*nexus.Gateway, error) { return gw, nil },
		func() (*shieldengine.Engine, error) { return shield, nil },
		func() (*weaveengine.Engine, error) { return weave, nil },
	} {
		if err := vessel.Provide(c, provide); err != nil {
			t.Fatalf("provide: %v", err)
		}
	}
}

func TestHostEngineOptionsOverrideAutoWiring(t *testing.T) {
	app := newTestApp(t, "cortex-override-test")
	provideAdapters(t, app.Container())
	host, kn, gate := fakeLLM{}, fakeKnowledge{}, &countingGate{}
	e := New(
		WithStore(scopespy.New()),
		WithEngineOption(engine.WithLLM(host)),
		WithEngineOption(engine.WithKnowledge(kn)),
		WithEngineOption(engine.WithPublicationGate(gate)),
	)
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eng := e.Engine()
	if eng.LLM() != llm.Client(host) {
		t.Fatalf("LLM = %T, want the host's fake", eng.LLM())
	}
	if eng.Knowledge() != knowledge.Provider(kn) {
		t.Fatalf("Knowledge = %T, want the host's fake", eng.Knowledge())
	}
	// The shield engine here has no store, so it refuses every scan and a
	// run that reached it would fail. A completed run screened by the host's
	// gate proves the gate replaced shield's.
	r, err := eng.RunAgent(scopeCtx(), "assistant", "hello", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.Output != "from the host model" {
		t.Fatalf("output = %q, want the host model's answer", r.Output)
	}
	if gate.calls.Load() == 0 {
		t.Fatal("the host's publication gate was never asked")
	}
}

func TestHostStoreOverridesDiscoveredStore(t *testing.T) {
	app := newTestApp(t, "cortex-store-test")
	drv := sqlitedriver.New()
	if err := drv.Open(context.Background(), filepath.Join(t.TempDir(), "discovered.db")); err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := vessel.Provide(app.Container(), func() (*grove.DB, error) { return db, nil }); err != nil {
		t.Fatalf("provide grove: %v", err)
	}
	hostStore := scopespy.New()
	e := New(WithStore(hostStore))
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.Engine().Store() != hostStore {
		t.Fatalf("Store = %T, want the host's store over the discovered grove database", e.Engine().Store())
	}
}

func TestWithoutAutoAdapters(t *testing.T) {
	app := newTestApp(t, "cortex-no-adapters-test")
	provideAdapters(t, app.Container())
	e := New(WithStore(scopespy.New()), WithoutAutoAdapters())
	if err := e.Register(app); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eng := e.Engine()
	if eng.LLM() != nil {
		t.Fatalf("LLM = %T, want nil without automatic adapters", eng.LLM())
	}
	if eng.Safety() != nil {
		t.Fatalf("Safety = %T, want nil without automatic adapters", eng.Safety())
	}
	if eng.Knowledge() != nil {
		t.Fatalf("Knowledge = %T, want nil without automatic adapters", eng.Knowledge())
	}
}
