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

func runOnce(t *testing.T, e *Extension) {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	if _, err := e.Engine().RunAgent(ctx, "assistant", "hello", nil); err != nil {
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
	e := New(WithStore(scopespy.New()), WithEngineOption(engine.WithLLM(&llm.MockClient{})))
	if err := e.Register(app); err != nil {
		t.Fatal(err)
	}
	runOnce(t, e)

	body := scrape(t, app)
	for _, want := range []string{
		"# TYPE forge_cortex_runs_finished_total counter",
		`forge_cortex_runs_finished_total{purpose="normal",state="completed",stop_reason="final_answer"} 1`,
		"# TYPE forge_cortex_run_duration_seconds histogram",
		`forge_cortex_run_duration_seconds_count{purpose="normal",state="completed"} 1`,
		`forge_cortex_runs_in_progress{mode="sync"} 0`,
		`forge_cortex_steps_executed_total{outcome="final"} 1`,
		`forge_cortex_publication_checks_completed_total{direction="input",result="unscanned",source="user_input"} 1`,
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
	if runs != 1 {
		t.Errorf("run agent spans on forge's provider = %d, want 1", runs)
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
