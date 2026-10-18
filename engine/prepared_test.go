package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
)

func TestPreparedEmptyToolsDenyAll(t *testing.T) {
	ctx := overlayCtx("prepared")
	e, _, rec := newConfigEngine(t, ctx, &agent.Config{ID: id.NewAgentID(), Name: "worker"})
	var overrides engine.RunOverrides
	if err := json.Unmarshal([]byte(`{"ToolsRestricted":true,"Tools":[]}`), &overrides); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunAgent(ctx, "worker", "work", &overrides); err != nil {
		t.Fatal(err)
	}
	if got := len(rec.Request(t).Tools); got != 0 {
		t.Fatalf("explicit empty allowlist advertised %d tools", got)
	}
}

func TestPreparedSourcesCannotMutateMidRun(t *testing.T) {
	ctx := overlayCtx("prepared")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original"}
	e, s, rec := newConfigEngine(t, ctx, ag)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := prepared.Digest()
	encoded, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	ag.SystemPrompt = "changed"
	if err = s.Update(ctx, ag); err != nil {
		t.Fatal(err)
	}
	var restored engine.PreparedAgent
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	r, err := e.RunPrepared(ctx, &restored, "work", engine.PreparedRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Request(t).System != "original" {
		t.Fatalf("prepared prompt mutated: %q", rec.Request(t).System)
	}
	if restored.Digest() != digest || r.Provenance.PreparedDigest != digest {
		t.Fatal("prepared identity lost")
	}
	saved, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provenance.PreparedDigest != digest {
		t.Fatal("prepared identity was not persisted")
	}
}

func TestPreparedRejectsTamperedAndFutureSchema(t *testing.T) {
	ctx := overlayCtx("prepared")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original"}
	e, _, _ := newConfigEngine(t, ctx, ag)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{
		bytes.Replace(raw, []byte("original"), []byte("tampered"), 1),
		bytes.Replace(raw, []byte(`"schema_version":2`), []byte(`"schema_version":999`), 1),
	} {
		var decoded engine.PreparedAgent
		if err := json.Unmarshal(changed, &decoded); err == nil {
			t.Fatal("unsupported/tampered snapshot accepted")
		}
	}
}

func TestPreparedToolDefinitionsAreFrozen(t *testing.T) {
	ctx := overlayCtx("prepared")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	s := newSQLiteStoreForTest(t)
	rec := &recordingLLM{}
	schema := map[string]any{"type": "object", "description": "original"}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(rec), engine.WithTool(llm.Tool{Name: "probe", Parameters: schema}, noopHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema["description"] = "changed"
	if _, err = e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{}); err != nil {
		t.Fatal(err)
	}
	got := rec.Request(t).Tools[0].Parameters.(map[string]any)["description"]
	if got != "original" {
		t.Fatalf("prepared tool mutated: %v", got)
	}
}

func TestPreparedMissingRequiredToolFails(t *testing.T) {
	ctx := overlayCtx("prepared")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Tools: []string{"missing"}}
	e, _, _ := newConfigEngine(t, ctx, ag)
	if _, err := e.PrepareAgent(ctx, ag.ID, nil); err == nil {
		t.Fatal("missing configured tool silently dropped")
	}
}

func TestPreparedValueCopyCannotMutateOriginal(t *testing.T) {
	ctx := overlayCtx("prepared")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original"}
	e, s, rec := newConfigEngine(t, ctx, ag)
	original, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag.SystemPrompt = "modified"
	if err = s.Update(ctx, ag); err != nil {
		t.Fatal(err)
	}
	replacement, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	copied := *original
	if err = json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RunPrepared(ctx, original, "work", engine.PreparedRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.Request(t).System != "original" {
		t.Fatal("unmarshalling a value copy mutated original snapshot")
	}
}

type missingPreparedKnowledge struct{ staticKnowledge }

func (missingPreparedKnowledge) Retrieve(_ context.Context, source string, _ *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	if source == "failed" {
		return nil, errors.New("backend secret error")
	}
	return nil, nil
}

func TestPreparedRecordsMissingRetrieval(t *testing.T) {
	ctx := overlayCtx("prepared")
	e, s := newPromptEngine(t, engine.WithKnowledge(missingPreparedKnowledge{}), engine.WithLLM(&recordingLLM{}))
	sk := &skill.Skill{ID: id.NewSkillID(), Name: "research", Knowledge: []skill.KnowledgeRef{{Source: "failed"}, {Source: "empty"}}}
	if err := s.CreateSkill(ctx, sk); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", InlineSkills: []string{"research"}}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := strings.Join(r.Provenance.Diagnostics, "\n")
	for _, want := range []string{"knowledge retrieval failed: failed", "knowledge retrieval empty: empty"} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("missing %q in %s", want, diagnostics)
		}
	}
	if strings.Contains(diagnostics, "backend secret") {
		t.Fatal("raw provider error leaked into provenance")
	}
}

type preparedPauseClient struct{ recordingLLM }

func (*preparedPauseClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "pending", Name: "external", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 6}}, nil
}

func TestPreparedResumeAfterStoreReopen(t *testing.T) {
	ctx := overlayCtx("restart")
	path := filepath.Join(t.TempDir(), "restart.db")
	s := openSQLiteStoreForTest(t, path)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(&preparedPauseClient{}), engine.WithExternalTool(llm.Tool{Name: "external"}))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{MaxSteps: 3, Budget: &execution.Budget{MaxTokens: 10, MaxToolCalls: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{Provenance: run.ExecutionProvenance{CandidateID: "candidate-1", EnvironmentReceiptID: "environment-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StatePaused || r.Usage.Tokens != 6 {
		t.Fatalf("did not pause with usage: %+v", r)
	}
	ag.SystemPrompt = "changed while paused"
	if err = s.Update(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openSQLiteStoreForTest(t, path)
	recorder := &preparedProvenanceClient{}
	fresh, err := engine.New(engine.WithStore(reopened), engine.WithLLM(recorder), engine.WithExternalTool(llm.Tool{Name: "external"}))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := fresh.Resume(ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "pending", Content: "result"}}})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Request(t).System != "original" || recorder.Request(t).MaxTokens != 4 {
		t.Fatalf("resume lost frozen prompt or remaining budget: %+v", recorder.Request(t))
	}
	if recorder.provenance.CandidateID != "candidate-1" || recorder.provenance.EnvironmentReceiptID != "environment-1" {
		t.Fatalf("resume context lost correlation: %+v", recorder.provenance)
	}
	if resumed.Provenance.CandidateID != "candidate-1" || resumed.Provenance.EnvironmentReceiptID != "environment-1" || resumed.Provenance.PreparedDigest != r.Provenance.PreparedDigest || resumed.Usage.Steps != 2 || resumed.Usage.ToolCalls != 1 || resumed.Usage.Tokens < 6 {
		t.Fatalf("restart reset identity or accounting: %+v", resumed)
	}
}

func TestPreparedToolSchemaPreservesLargeIntegers(t *testing.T) {
	ctx := overlayCtx("numbers")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original"}
	s := newSQLiteStoreForTest(t)
	rec := &recordingLLM{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(rec), engine.WithTool(llm.Tool{Name: "probe", Parameters: map[string]any{"const": int64(9007199254740993)}}, noopHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := p.Derive(engine.PreparedEdits{SectionBodies: map[string]string{"role": "candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.RunPrepared(ctx, derived, "work", engine.PreparedRunOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rec.Request(t).Tools)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("9007199254740993")) {
		t.Fatalf("snapshot rounded tool schema: %s", raw)
	}
}

type preparedProvenanceClient struct {
	recordingLLM
	provenance run.ExecutionProvenance
}

func (p *preparedProvenanceClient) Complete(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	p.provenance, _ = engine.ExecutionProvenanceFromContext(ctx)
	return p.recordingLLM.Complete(ctx, request)
}
