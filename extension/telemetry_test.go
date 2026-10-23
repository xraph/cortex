package extension

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xraph/forge"
	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

func scrape(t *testing.T, app forge.App) string {
	t.Helper()
	p, ok := app.Metrics().(interface{ PrometheusHandler() http.Handler })
	if !ok {
		t.Fatalf("app metrics %T have no Prometheus handler", app.Metrics())
	}
	rr := httptest.NewRecorder()
	p.PrometheusHandler().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_/metrics", nil))
	body, err := io.ReadAll(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func newTestApp(t *testing.T, name string) forge.App {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return forge.New(forge.WithAppName(name), forge.WithAppLogger(forge.NewNoopLogger()),
		forge.WithEnableConfigAutoDiscovery(false))
}

// lengthLLM answers in full, unless the input asks it to run out of
// tokens, so one app sees runs end two different ways.
type lengthLLM struct{}

func (lengthLLM) answer(req *llm.Request) (string, string) {
	if last := req.Messages[len(req.Messages)-1].Content; last == "long" {
		return "cut off", "length"
	}
	return "hi", "stop"
}

func (m lengthLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	content, finish := m.answer(req)
	return &llm.Response{Content: content, FinishReason: finish}, nil
}

func (m lengthLLM) CompleteStream(_ context.Context, req *llm.Request) (llm.Stream, error) {
	content, finish := m.answer(req)
	return &oneChunk{chunk: &llm.Chunk{Content: content, FinishReason: finish}}, nil
}

type oneChunk struct{ chunk *llm.Chunk }

func (s *oneChunk) Next(context.Context) (*llm.Chunk, error) {
	if s.chunk == nil {
		return nil, io.EOF
	}
	c := s.chunk
	s.chunk = nil
	return c, nil
}

func (s *oneChunk) Close() error      { return nil }
func (s *oneChunk) Usage() *llm.Usage { return nil }

func scopeCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
}

func runOnce(t *testing.T, e *Extension) {
	t.Helper()
	if _, err := e.Engine().RunAgent(scopeCtx(), "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
}

// Register hands the engine the app's metrics, so a run shows up on the
// app's own scrape with forge's prefix, and the engine's spans go to the
// tracer provider forge installed.
func TestRegisterWiresAppMetricsAndTracing(t *testing.T) {
	app := newTestApp(t, "cortex-telemetry-test")
	rec := tracetest.NewSpanRecorder()
	if err := forge.RegisterSpanProcessor(app, rec); err != nil {
		t.Fatal(err)
	}
	e := New(WithStore(scopespy.New()), WithEngineOption(engine.WithLLM(lengthLLM{})))
	if err := e.Register(app); err != nil {
		t.Fatal(err)
	}
	// Three runs that end differently, so every family has more than one
	// series and the scrape shows how the labels split them.
	runOnce(t, e)
	if _, err := e.Engine().RunAgent(scopeCtx(), "assistant", "long", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	events := make(chan engine.StreamEvent, 16)
	if err := e.Engine().StreamAgent(scopeCtx(), "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	for ev := range events {
		_ = ev
	}

	body := scrape(t, app)
	for _, want := range []string{
		"# TYPE forge_cortex_runs_finished_total counter",
		`forge_cortex_runs_finished_total{purpose="normal",state="completed",stop_reason="final_answer"} 2`,
		`forge_cortex_runs_finished_total{purpose="normal",state="incomplete",stop_reason="token_limit"} 1`,
		"# TYPE forge_cortex_run_duration_seconds histogram",
		`forge_cortex_run_duration_seconds_count{purpose="normal",state="completed"} 2`,
		`forge_cortex_run_duration_seconds_count{purpose="normal",state="incomplete"} 1`,
		`forge_cortex_runs_in_progress{mode="sync"} 0`,
		`forge_cortex_runs_in_progress{mode="stream"} 0`,
		`forge_cortex_steps_executed_total{outcome="final"} 2`,
		`forge_cortex_steps_executed_total{outcome="stopped"} 1`,
		`forge_cortex_publication_checks_completed_total{direction="input",result="unscanned",source="user_input"} 3`,
		`forge_cortex_publication_checks_completed_total{direction="output",result="unscanned",source="model_output"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
	if strings.Contains(body, "cortex.agent") || strings.Contains(body, "forge_cortex_agent_run") {
		t.Error("an old dotted observability counter is on the scrape")
	}

	var runs int
	for _, s := range rec.Ended() {
		if s.Name() == "run agent" && s.InstrumentationScope().Name == "github.com/xraph/cortex" {
			runs++
		}
	}
	if runs != 3 {
		t.Errorf("run agent spans on forge's provider = %d, want 3", runs)
	}
}

// A host that wants no cortex metrics passes engine.WithMetrics(nil); its
// option runs after the extension's and wins.
func TestEngineOptionCanTurnMetricsOff(t *testing.T) {
	app := newTestApp(t, "cortex-metrics-off")
	e := New(WithStore(scopespy.New()),
		WithEngineOption(engine.WithLLM(&llm.MockClient{})),
		WithEngineOption(engine.WithMetrics(nil)))
	if err := e.Register(app); err != nil {
		t.Fatal(err)
	}
	runOnce(t, e)
	if body := scrape(t, app); strings.Contains(body, "forge_cortex_") {
		t.Error("cortex metrics are on the scrape though the host turned them off")
	}
}

func startedLines(l log.Logger) int {
	tl, ok := l.(*log.TestLogger)
	if !ok {
		return -1
	}
	n := 0
	for _, e := range tl.GetLogs() {
		if e.Message == "cortex engine started" {
			n++
		}
	}
	return n
}

// Register hands the engine the app's logger, so the engine's lines reach
// wherever the app logs, and a logger you pass with WithEngineOption wins.
func TestRegisterPassesTheAppLogger(t *testing.T) {
	ctx := context.Background()

	appLogger := log.NewTestLogger()
	app := forge.New(forge.WithAppName("cortex-logger-test"), forge.WithAppLogger(appLogger),
		forge.WithEnableConfigAutoDiscovery(false))
	e := New(WithStore(scopespy.New()))
	if err := e.Register(app); err != nil {
		t.Fatal(err)
	}
	if err := e.Engine().Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = e.Engine().Stop(ctx)
	if got := startedLines(appLogger); got != 1 {
		t.Errorf("the app's logger got %d engine start lines, want 1", got)
	}

	appLogger2, mine := log.NewTestLogger(), log.NewTestLogger()
	app2 := forge.New(forge.WithAppName("cortex-logger-override"), forge.WithAppLogger(appLogger2),
		forge.WithEnableConfigAutoDiscovery(false))
	e2 := New(WithStore(scopespy.New()), WithEngineOption(engine.WithLogger(mine)))
	if err := e2.Register(app2); err != nil {
		t.Fatal(err)
	}
	if err := e2.Engine().Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = e2.Engine().Stop(ctx)
	if startedLines(mine) != 1 || startedLines(appLogger2) != 0 {
		t.Errorf("start lines: yours %d, the app's %d; want your logger to win", startedLines(mine), startedLines(appLogger2))
	}
}
