package engine_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	shieldsafety "github.com/xraph/cortex/safety/shield"
	"github.com/xraph/cortex/store/sqlite"

	"github.com/xraph/shield/awareness"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/presets"
	"github.com/xraph/shield/profile"
	shieldsqlite "github.com/xraph/shield/store/sqlite"
)

// scriptedStream yields its chunks in order, then io.EOF.
type scriptedStream struct {
	chunks []llm.Chunk
	next   int
}

func (s *scriptedStream) Next(context.Context) (*llm.Chunk, error) {
	if s.next == len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.next]
	s.next++
	return &c, nil
}

func (*scriptedStream) Close() error      { return nil }
func (*scriptedStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 3} }

// scriptedStreamClient streams the same chunks for every call.
type scriptedStreamClient struct{ chunks []llm.Chunk }

func (c scriptedStreamClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	content := ""
	for _, ch := range c.chunks {
		content += ch.Content
	}
	return &llm.Response{Content: content, FinishReason: "stop"}, nil
}

func (c scriptedStreamClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return &scriptedStream{chunks: append([]llm.Chunk(nil), c.chunks...)}, nil
}

// blockSecrets names a profile seeded next to the InfraAgent preset whose
// secrets awareness blocks rather than redacts.
const blockSecrets = "block-secrets"

// seededShield returns a shield engine on an in-memory SQLite store seeded
// with the InfraAgent preset and the blockSecrets profile for tenant and
// app.
func seededShield(t *testing.T, tenant, app string) *shieldengine.Engine {
	t.Helper()
	ctx := context.Background()
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, ":memory:"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := shieldsqlite.New(db)
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = presets.InfraAgent().Seed(ctx, st, tenant, app); err != nil {
		t.Fatal(err)
	}
	strict := presets.Bundle{
		Name: blockSecrets,
		Awareness: []*awareness.Awareness{{
			Name: "secrets-block", Focus: awareness.FocusSecrets, Action: string(awareness.PIIActionBlock), Enabled: true,
		}},
		Profiles: []*profile.SafetyProfile{{Name: blockSecrets, Awareness: []profile.AwarenessAssignment{{AwarenessName: "secrets-block"}}, Enabled: true}},
	}
	if _, err = strict.Seed(ctx, st, tenant, app); err != nil {
		t.Fatal(err)
	}
	eng, err := shieldengine.New(shieldengine.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// segmentEngine returns an engine on a SQLite store that streams chunks
// under checked segments, with the shield adapter as its required gate,
// and the agent "assistant" on shield profile shieldProfile.
func segmentEngine(ctx context.Context, t *testing.T, shieldProfile string, chunks []llm.Chunk) (*engine.Engine, *sqlite.Store, *agent.Config) {
	t.Helper()
	const tenant, app = "o1", "loomplane"
	adapter := shieldsafety.New(seededShield(t, tenant, app), shieldsafety.WithApp(app))
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{
		ID: id.NewAgentID(), Name: "assistant", MaxSteps: 2,
		Guardrails: map[string]any{"shield_profile": shieldProfile},
	}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(
		engine.WithStore(s),
		engine.WithPublicationGate(adapter),
		engine.WithPublicationPolicy(safety.Policy{Mode: safety.ModeCheckedSegments, Required: true}),
		engine.WithLLM(scriptedStreamClient{chunks: chunks}),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e, s, ag
}

// segmentCtx is a context scoped to the seeded tenant.
func segmentCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "o1"}}})
}

// segmentRun returns the agent's single run.
func segmentRun(ctx context.Context, t *testing.T, s *sqlite.Store, ag *agent.Config) *run.Run {
	t.Helper()
	runs, err := s.ListRuns(ctx, &run.ListFilter{AgentID: ag.ID.String()})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %d runs, %v", len(runs), err)
	}
	return runs[0]
}

func TestEngineStreamsCheckedSegments(t *testing.T) {
	ctx := segmentCtx()
	e, s, ag := segmentEngine(ctx, t, presets.InfraAgentOutput, []llm.Chunk{
		{Content: "One. "}, {Content: "Two. "}, {Content: "Three.", FinishReason: "stop"},
	})

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "status?", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	var tokens []string
	output, done := "", false
	for ev := range events {
		switch ev.Type {
		case engine.EventToken:
			if done {
				t.Fatal("token after done")
			}
			text, _ := ev.Data["content"].(string)
			tokens = append(tokens, text)
			output += text
		case engine.EventDone:
			done = true
		case engine.EventError, engine.EventSafetyBlock:
			t.Fatalf("stream event %s: %v", ev.Type, ev.Data)
		}
	}
	if !done {
		t.Fatal("stream ended without done")
	}
	if len(tokens) < 3 || output != "One. Two. Three." {
		t.Fatalf("tokens %q, output %q; want at least 3 tokens making %q", tokens, output, "One. Two. Three.")
	}
	r := segmentRun(ctx, t, s, ag)
	if r.State != run.StateCompleted || r.Provenance.PublicationStatus != safety.StatusApproved {
		t.Fatalf("run state %q, publication %q; want completed and approved", r.State, r.Provenance.PublicationStatus)
	}
}

func TestEngineNeverPublishesABlockedTail(t *testing.T) {
	ctx := segmentCtx()
	token := "ghp_" + strings.Repeat("a1B2c3", 6)
	e, s, ag := segmentEngine(ctx, t, blockSecrets, []llm.Chunk{
		{Content: "Status ok. "}, {Content: "Pushed with " + token, FinishReason: "stop"},
	})

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "status?", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	output, blocked := "", false
	for ev := range events {
		switch ev.Type {
		case engine.EventToken:
			text, _ := ev.Data["content"].(string)
			output += text
		case engine.EventSafetyBlock:
			blocked = true
		case engine.EventDone:
			t.Fatal("a blocked stream finished")
		}
	}
	// The approved sentence went out before the block and stays out; the
	// held tail never does.
	if output != "Status ok. " || !blocked {
		t.Fatalf("output %q, blocked %v; want only the approved sentence, then a block", output, blocked)
	}
	r := segmentRun(ctx, t, s, ag)
	if r.StopReason != run.StopSafetyBlock {
		t.Fatalf("stop reason %q, want %q", r.StopReason, run.StopSafetyBlock)
	}
	steps, err := s.ListSteps(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if strings.Contains(step.Output, token) || strings.Contains(step.Output, "Pushed with") {
			t.Fatal("the blocked tail was stored")
		}
	}
}
