package engine_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/store"
)

// recordingLLM captures the request the engine built, which is where the
// resolved run config actually lands. Asserting on it rather than on an
// exported copy of the config means the layering is tested through the
// path a real run takes, tool filtering included.
type recordingLLM struct {
	mu   sync.Mutex
	last *llm.Request
}

func (r *recordingLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := *req
	r.last = &copied

	return &llm.Response{Content: "done"}, nil
}

func (r *recordingLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errNoStream
}

func (r *recordingLLM) Request(t *testing.T) *llm.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		t.Fatal("the LLM was never called, so this test proves nothing about the resolved config")
	}

	return r.last
}

// errNoStream keeps the recording double honest: it backs the
// synchronous path only, so a half-working stream can never mask a
// failure on the streaming one.
var errNoStream = errors.New("recordingLLM: streaming not supported")

func namedTool(name string) llm.Tool {
	return llm.Tool{Name: name, Description: name}
}

func noopHandler(_ context.Context, _ cortex.Invocation) (string, error) { return "", nil }

// newConfigEngine builds an engine over a real sqlite store with three
// registered tools, and stores one agent so RunAgent can resolve it.
//
// The agent is created at runCtx, the scope the test's run happens in,
// because an agent stored at a broader scope is not visible to a
// narrower caller. Overlays are looked up by agent id and scope alone,
// so an ancestor's overlay still reaches a run whose agent lives in the
// descendant scope, which is exactly the inheritance under test.
//
//nolint:revive // ctx placement matches the other test helpers here; test-only, not a public API
func newConfigEngine(t *testing.T, runCtx context.Context, ag *agent.Config) (*engine.Engine, store.Store, *recordingLLM) {
	t.Helper()

	s := newSQLiteStoreForTest(t)
	rec := &recordingLLM{}
	e, err := engine.New(
		engine.WithStore(s),
		engine.WithLLM(rec),
		engine.WithTool(namedTool("alpha"), noopHandler),
		engine.WithTool(namedTool("beta"), noopHandler),
		engine.WithTool(namedTool("gamma"), noopHandler),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ag.Entity = cortex.NewEntity()
	if createErr := s.Create(runCtx, ag); createErr != nil {
		t.Fatalf("create agent: %v", createErr)
	}

	return e, s, rec
}

// toolNames returns the request's registered tool names, dropping the
// builtins, which cfg.Tools never filters.
func toolNames(req *llm.Request) []string {
	builtin := map[string]bool{}
	for _, n := range []string{"knowledge_search", "memory_search", "memory_write"} {
		builtin[n] = true
	}
	out := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		if builtin[tl.Name] {
			continue
		}
		out = append(out, tl.Name)
	}
	sort.Strings(out)

	return out
}

// TestEffectiveConfig_OverlayModelBeatsTheAgentAndLosesToARunOverride
// pins the middle two rungs of the four-deep layering. An overlay
// outranks the agent because it is the host customizing that agent for
// its scope. A per-run override outranks the overlay because a caller
// naming a model for this one call means it.
func TestEffectiveConfig_OverlayModelBeatsTheAgentAndLosesToARunOverride(t *testing.T) {
	agentID := id.NewAgentID()
	ctx := overlayCtx(overlayWorkspace)
	e, s, rec := newConfigEngine(t, ctx, &agent.Config{
		ID:           agentID,
		Name:         "configured",
		Model:        "agent-model",
		SystemPrompt: "You answer questions.",
	})

	mustCreateOverlayConfigAt(t, s, ctx, agentID, func(o *prompt.Overlay) { o.Model = "overlay-model" })

	if _, err := e.RunAgent(ctx, "configured", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if got := rec.Request(t).Model; got != "overlay-model" {
		t.Errorf("model = %q, want the overlay's %q to beat the agent's own", got, "overlay-model")
	}

	if _, err := e.RunAgent(ctx, "configured", "hello", &engine.RunOverrides{Model: "run-model"}); err != nil {
		t.Fatalf("RunAgent with override: %v", err)
	}
	if got := rec.Request(t).Model; got != "run-model" {
		t.Errorf("model = %q, want the per-run override %q to beat the overlay", got, "run-model")
	}
}

// TestEffectiveConfig_NarrowerOverlayModelBeatsTheBroaderOne is the same
// narrowest-wins rule the prompt patches follow, applied to config.
func TestEffectiveConfig_NarrowerOverlayModelBeatsTheBroaderOne(t *testing.T) {
	agentID := id.NewAgentID()
	e, s, rec := newConfigEngine(t, overlayCtx(overlayWorkspace, overlayProject), &agent.Config{
		ID:           agentID,
		Name:         "configured",
		Model:        "agent-model",
		SystemPrompt: "You answer questions.",
	})

	temp := 0.9
	tokens := 4096
	mustCreateOverlayConfigAt(t, s, overlayCtx(overlayWorkspace), agentID, func(o *prompt.Overlay) {
		o.Model = "workspace-model"
		o.Temperature = &temp
		o.MaxTokens = &tokens
	})
	narrowTemp := 0.1
	mustCreateOverlayConfigAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID, func(o *prompt.Overlay) {
		o.Model = "project-model"
		o.Temperature = &narrowTemp
	})

	if _, err := e.RunAgent(overlayCtx(overlayWorkspace, overlayProject), "configured", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	req := rec.Request(t)
	if req.Model != "project-model" {
		t.Errorf("model = %q, want the project overlay's %q", req.Model, "project-model")
	}
	if req.Temperature == nil || *req.Temperature != narrowTemp {
		t.Errorf("temperature = %v, want the project overlay's %v", req.Temperature, narrowTemp)
	}
	// The workspace overlay set a field the project overlay left alone,
	// so it must survive: narrowest wins per field, not per overlay.
	if req.MaxTokens != tokens {
		t.Errorf("max tokens = %d, want the workspace overlay's %d to survive a project overlay that never mentioned it", req.MaxTokens, tokens)
	}
}

// TestEffectiveConfig_ToolDeltasResolveAsDocumented walks the tool
// resolution rules on the Overlay fields: removals beat additions inside
// one overlay, and across overlays the narrower scope applies last so it
// can withdraw what a broader one granted.
func TestEffectiveConfig_ToolDeltasResolveAsDocumented(t *testing.T) {
	tests := []struct {
		name      string
		agent     []string
		workspace func(*prompt.Overlay)
		project   func(*prompt.Overlay)
		want      []string
	}{
		{
			name:      "an overlay adds to the agent's list",
			agent:     []string{"alpha"},
			workspace: func(o *prompt.Overlay) { o.ToolsAdded = []string{"beta"} },
			want:      []string{"alpha", "beta"},
		},
		{
			name:      "an overlay withdraws one of the agent's tools",
			agent:     []string{"alpha", "beta"},
			workspace: func(o *prompt.Overlay) { o.ToolsRemoved = []string{"beta"} },
			want:      []string{"alpha"},
		},
		{
			name:      "one overlay naming a tool in both lists withdraws it",
			agent:     []string{"alpha"},
			workspace: func(o *prompt.Overlay) { o.ToolsAdded = []string{"beta"}; o.ToolsRemoved = []string{"beta"} },
			want:      []string{"alpha"},
		},
		{
			name:      "the broader overlay adds and the narrower removes",
			agent:     []string{"alpha"},
			workspace: func(o *prompt.Overlay) { o.ToolsAdded = []string{"beta"} },
			project:   func(o *prompt.Overlay) { o.ToolsRemoved = []string{"beta"} },
			want:      []string{"alpha"},
		},
		{
			name:      "the broader overlay removes and the narrower adds it back",
			agent:     []string{"alpha", "beta"},
			workspace: func(o *prompt.Overlay) { o.ToolsRemoved = []string{"beta"} },
			project:   func(o *prompt.Overlay) { o.ToolsAdded = []string{"beta"} },
			want:      []string{"alpha", "beta"},
		},
		{
			// An agent that names no tools is allowed all of them, so a
			// removal has to subtract from the real registered list
			// rather than from an empty one.
			name:      "a removal reaches an agent that never named its tools",
			agent:     nil,
			workspace: func(o *prompt.Overlay) { o.ToolsRemoved = []string{"beta"} },
			want:      []string{"alpha", "gamma"},
		},
		{
			// Withdrawing the last tool must mean no tools. Falling back
			// to "every registered tool" would turn a removal into a
			// grant of everything.
			name:      "withdrawing every tool leaves none",
			agent:     []string{"alpha"},
			workspace: func(o *prompt.Overlay) { o.ToolsRemoved = []string{"alpha"} },
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentID := id.NewAgentID()
			e, s, rec := newConfigEngine(t, overlayCtx(overlayWorkspace, overlayProject), &agent.Config{
				ID:           agentID,
				Name:         "configured",
				SystemPrompt: "You answer questions.",
				Tools:        tt.agent,
			})

			if tt.workspace != nil {
				mustCreateOverlayConfigAt(t, s, overlayCtx(overlayWorkspace), agentID, tt.workspace)
			}
			if tt.project != nil {
				mustCreateOverlayConfigAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID, tt.project)
			}

			if _, err := e.RunAgent(overlayCtx(overlayWorkspace, overlayProject), "configured", "hello", nil); err != nil {
				t.Fatalf("RunAgent: %v", err)
			}

			got := toolNames(rec.Request(t))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("tools = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEffectiveConfig_ASiblingProjectsOverlayNeverReachesTheConfig is
// the cross-tenant guard for the config half of the seam, matching the
// one the prompt half already has. One agent, two projects under one
// workspace: p1's model and its tool grant must not reach a run in p2.
func TestEffectiveConfig_ASiblingProjectsOverlayNeverReachesTheConfig(t *testing.T) {
	agentID := id.NewAgentID()
	e, s, rec := newConfigEngine(t, overlayCtx(overlayWorkspace, siblingProject), &agent.Config{
		ID:           agentID,
		Name:         "configured",
		Model:        "agent-model",
		SystemPrompt: "You answer questions.",
		Tools:        []string{"alpha"},
	})

	mustCreateOverlayConfigAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID, func(o *prompt.Overlay) {
		o.Model = "p1-only-model"
		o.ToolsAdded = []string{"gamma"}
	})

	if _, err := e.RunAgent(overlayCtx(overlayWorkspace, siblingProject), "configured", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	req := rec.Request(t)
	if req.Model == "p1-only-model" {
		t.Fatalf("a sibling project's overlay set the model for an unrelated run")
	}
	if req.Model != "agent-model" {
		t.Errorf("model = %q, want the agent's own %q", req.Model, "agent-model")
	}
	if got := toolNames(req); strings.Join(got, ",") != "alpha" {
		t.Errorf("tools = %v, want just the agent's own [alpha]; a sibling project's grant leaked", got)
	}
}

//nolint:revive // ctx placement matches the other test helpers here; test-only, not a public API
func mustCreateOverlayConfigAt(t *testing.T, s store.Store, ctx context.Context, agentID id.AgentID, set func(*prompt.Overlay)) {
	t.Helper()

	o := &prompt.Overlay{
		Entity:  cortex.NewEntity(),
		ID:      id.NewOverlayID(),
		AgentID: agentID,
	}
	set(o)
	if err := s.CreateOverlay(ctx, o); err != nil {
		t.Fatalf("create overlay: %v", err)
	}
}
