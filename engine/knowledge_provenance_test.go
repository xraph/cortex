package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

type recalledKnowledge struct{ chunks []knowledge.ScoredChunk }

func (p recalledKnowledge) Retrieve(context.Context, string, *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return p.chunks, nil
}
func (recalledKnowledge) ListCollections(context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}
func recalledChunk() knowledge.ScoredChunk {
	key := id.New("lesson").String()
	return knowledge.ScoredChunk{Content: "private recalled lesson", Source: "lesson:" + key, DocumentID: key, Metadata: map[string]string{"revision": "2", "digest": evidence.Digest([]byte("lesson")), "evidence_class": "validated"}}
}
func recallRefs(t *testing.T, r *run.Run) []run.Revision {
	t.Helper()
	raw, err := json.Marshal(r.Provenance)
	require.NoError(t, err)
	var wire struct {
		RecalledLessons []run.Revision `json:"recalled_lessons"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	return wire.RecalledLessons
}
func TestKnowledgeProvenancePrepared(t *testing.T) {
	ctx := overlayCtx("recall")
	storage := newSQLiteStoreForTest(t)
	chunk := recalledChunk()
	ag := &agent.Config{ID: id.NewAgentID(), Name: "recall", InlineSkills: []string{"docs"}}
	require.NoError(t, storage.Create(ctx, ag))
	require.NoError(t, storage.CreateSkill(ctx, &skill.Skill{Entity: cortex.NewEntity(), ID: id.NewSkillID(), Name: "docs", Knowledge: []skill.KnowledgeRef{{Source: "runbook"}}}))
	e, err := engine.New(engine.WithStore(storage), engine.WithLLM(&recordingLLM{}), engine.WithKnowledge(recalledKnowledge{[]knowledge.ScoredChunk{chunk, chunk}}))
	require.NoError(t, err)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	r, err := e.RunPrepared(ctx, prepared, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	saved, err := storage.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, []run.Revision{{Name: chunk.Source, Revision: "2", Digest: chunk.Metadata["digest"]}}, recallRefs(t, saved))
}

type recallClient struct {
	recordingLLM
	calls    int
	observed []llm.Message
}

func (c *recallClient) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls++
	c.observed = append([]llm.Message(nil), req.Messages...)
	if c.calls == 1 {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "recall-1", Name: "knowledge_search", Arguments: `{"query":"runbook"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 5}}, nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 5}}, nil
}

type failedRecallStore struct {
	store.Store
	fail bool
}

func (s *failedRecallStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if s.fail {
		raw, _ := json.Marshal(r.Provenance)
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if _, ok := v["recalled_lessons"]; ok {
			return errors.New("recording unavailable")
		}
	}
	return s.Store.UpdateRun(ctx, r)
}
func TestKnowledgeProvenanceRuntime(t *testing.T) {
	for _, mode := range []string{"retained", "missing revision", "missing digest", "write failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("recall")
			storage := newSQLiteStoreForTest(t)
			chunk := recalledChunk()
			switch mode {
			case "missing revision":
				delete(chunk.Metadata, "revision")
			case "missing digest":
				delete(chunk.Metadata, "digest")
			}
			wrapped := &failedRecallStore{Store: storage, fail: mode == "write failure"}
			client := &recallClient{}
			ag := &agent.Config{ID: id.NewAgentID(), Name: "recall"}
			require.NoError(t, storage.Create(ctx, ag))
			e, err := engine.New(engine.WithStore(wrapped), engine.WithLLM(client), engine.WithKnowledge(recalledKnowledge{[]knowledge.ScoredChunk{chunk}}))
			require.NoError(t, err)
			r, err := e.RunAgent(ctx, "recall", "work", nil)
			if mode == "write failure" {
				if err != nil {
					return
				}
			} else {
				require.NoError(t, err)
			}
			require.NotNil(t, r)
			raw, err := json.Marshal(client.observed)
			require.NoError(t, err)
			if mode == "retained" {
				require.Contains(t, string(raw), chunk.Content)
				saved, getErr := storage.GetRun(ctx, r.ID)
				require.NoError(t, getErr)
				require.Equal(t, []run.Revision{{Name: chunk.Source, Revision: "2", Digest: chunk.Metadata["digest"]}}, recallRefs(t, saved))
			} else {
				require.NotContains(t, string(raw), chunk.Content)
			}
		})
	}
}

func (c *recallClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	response, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &requiredStream{chunks: []llm.Chunk{{Content: response.Content, ToolCalls: response.ToolCalls, FinishReason: response.FinishReason}}}, nil
}

func TestKnowledgeProvenanceVerifiedRuntime(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval resume"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			chunk := recalledChunk()
			client := &recallClient{}
			auth := &requiredToolAuthority{approval: mode == "approval resume"}
			e := requiredToolEngine(t, f, client, auth, engine.WithKnowledge(recalledKnowledge{[]knowledge.ScoredChunk{chunk}}), engine.WithToolRevision("knowledge_search", "1"))
			ag, overrides := requiredToolAgent(t, f)
			ag.Tools = []string{"knowledge_search"}
			require.NoError(t, f.storage.Update(f.ctx, ag))
			var r *run.Run
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
				done := false
				for event := range events {
					done = done || event.Type == engine.EventDone
				}
				require.True(t, done)
				rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				r = rows[0]
			} else {
				var err error
				r, err = e.RunAgent(f.ctx, ag.Name, "work", overrides)
				require.NoError(t, err)
			}
			if mode == "approval resume" {
				require.Equal(t, run.StatePaused, r.State)
				checkpoints, err := f.storage.ListPending(f.ctx, &checkpoint.ListFilter{RunID: r.ID.String()})
				require.NoError(t, err)
				require.Len(t, checkpoints, 1)
				auth.approval = false
				require.NoError(t, e.ResolveCheckpoint(f.ctx, checkpoints[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
				r, err = f.storage.GetRun(f.ctx, r.ID)
				require.NoError(t, err)
			}
			require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			require.Equal(t, []run.Revision{{Name: chunk.Source, Revision: "2", Digest: chunk.Metadata["digest"]}}, recallRefs(t, r))
			resultID, err := id.Parse(r.ResultRef)
			require.NoError(t, err)
			receipt, err := f.storage.GetCommittedResult(f.ctx, resultID)
			require.NoError(t, err)
			_, err = f.storage.AcceptLesson(f.ctx, learning.AcceptRequest{ID: id.New(learning.PrefixLesson), Domain: "reasoning", Result: verification.ResultReadRequest{SchemaVersion: 1, RunID: r.ID, ResultID: resultID, ReceiptDigest: receipt.Digest}, ExpiresAt: time.Now().Add(30 * time.Second), Actor: "operator"})
			require.ErrorIs(t, err, learning.ErrIneligible)
		})
	}
}

func TestKnowledgeProvenanceInheritedHistory(t *testing.T) {
	for _, mode := range []string{"recalled", "clean", "stripped", "forged digest", "wrong origin", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			chunk := recalledChunk()
			ag := &agent.Config{ID: id.NewAgentID(), Name: "history"}
			require.NoError(t, f.storage.Create(f.ctx, ag))
			options := []engine.Option{engine.WithStore(f.storage), engine.WithLLM(&recordingLLM{})}
			if mode == "recalled" || mode == "stripped" || mode == "forged digest" || mode == "wrong origin" {
				ag.InlineSkills = []string{"docs"}
				require.NoError(t, f.storage.Update(f.ctx, ag))
				require.NoError(t, f.storage.CreateSkill(f.ctx, &skill.Skill{Entity: cortex.NewEntity(), ID: id.NewSkillID(), Name: "docs", Knowledge: []skill.KnowledgeRef{{Source: "runbook"}}}))
				options = append(options, engine.WithKnowledge(recalledKnowledge{[]knowledge.ScoredChunk{chunk}}))
			}
			initial, err := engine.New(options...)
			require.NoError(t, err)
			first, err := initial.RunAgent(f.ctx, ag.Name, "first", nil)
			require.NoError(t, err)
			history, err := f.storage.LoadConversation(f.ctx, ag.ID, first.SessionID, 100)
			require.NoError(t, err)
			require.NotEmpty(t, history)
			if mode == "stripped" || mode == "legacy" || mode == "forged digest" || mode == "wrong origin" {
				require.NoError(t, f.storage.ClearConversation(f.ctx, ag.ID, first.SessionID))
				for index := range history {
					switch mode {
					case "stripped", "legacy":
						history[index].Metadata = nil
					case "forged digest":
						history[index].Content = "replacement content"
					case "wrong origin":
						history[index].Metadata["_cortex_source_run"] = id.NewAgentRunID().String()
					}
				}
				require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, first.SessionID, history))
			}
			ag.InlineSkills = nil
			require.NoError(t, f.storage.Update(f.ctx, ag))
			verified, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(&recordingLLM{}), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)))
			require.NoError(t, err)
			overrides := &engine.RunOverrides{SessionID: first.SessionID, ToolsRestricted: true, Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(time.Minute)}}
			r, err := verified.RunAgent(f.ctx, ag.Name, "second", overrides)
			require.NoError(t, err)
			require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			if mode == "recalled" {
				require.Equal(t, recallRefs(t, first), recallRefs(t, r))
			}
			if mode == "clean" || mode == "recalled" {
				require.False(t, r.Provenance.UnattributedHistory)
			} else {
				require.True(t, r.Provenance.UnattributedHistory)
			}
			resultID, err := id.Parse(r.ResultRef)
			require.NoError(t, err)
			receipt, err := f.storage.GetCommittedResult(f.ctx, resultID)
			require.NoError(t, err)
			_, err = f.storage.AcceptLesson(f.ctx, learning.AcceptRequest{ID: id.New(learning.PrefixLesson), Domain: "reasoning", Result: verification.ResultReadRequest{SchemaVersion: 1, RunID: r.ID, ResultID: resultID, ReceiptDigest: receipt.Digest}, ExpiresAt: time.Now().Add(30 * time.Second), Actor: "operator"})
			if mode == "clean" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, learning.ErrIneligible)
			}
		})
	}
}

type failedConversationReceiptStore struct{ store.Store }

func (s failedConversationReceiptStore) UpdateRun(ctx context.Context, current *run.Run) error {
	if len(current.Provenance.ConversationWrites) > 0 {
		return errors.New("receipt unavailable")
	}
	return s.Store.UpdateRun(ctx, current)
}
func TestKnowledgeProvenanceConversationReceiptFailure(t *testing.T) {
	ctx := overlayCtx("conversation-receipt")
	storage := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "receipt"}
	require.NoError(t, storage.Create(ctx, ag))
	e, err := engine.New(engine.WithStore(failedConversationReceiptStore{Store: storage}), engine.WithLLM(&recordingLLM{}))
	require.NoError(t, err)
	current, err := e.RunAgent(ctx, ag.Name, "work", nil)
	require.NoError(t, err)
	history, err := storage.LoadConversation(ctx, ag.ID, current.SessionID, 100)
	require.NoError(t, err)
	require.Empty(t, history, "failed source receipt must withhold memory content")
}
