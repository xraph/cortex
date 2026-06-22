package fabriqbrain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/fabriq/core/agent"
)

// fakeRecaller returns a canned ContextPack and records the request it saw.
type fakeRecaller struct {
	pack agent.ContextPack
	got  agent.RecallRequest
	err  error
}

func (f *fakeRecaller) Recall(_ context.Context, req agent.RecallRequest) (agent.ContextPack, error) {
	f.got = req
	return f.pack, f.err
}

func item(entity, id string, score float64, row string, sources ...string) agent.ContextItem {
	return agent.ContextItem{Entity: entity, ID: id, Row: json.RawMessage(row), Score: score, Source: sources}
}

func TestRetrieve_MapsItemsToChunks(t *testing.T) {
	rec := &fakeRecaller{pack: agent.ContextPack{Items: []agent.ContextItem{
		item("doc", "d1", 0.9, `{"t":"hello"}`, "vector", "graph"),
	}}}
	a := NewProvider(rec, WithEntities("doc"), WithBudget(1234))

	chunks, err := a.Retrieve(context.Background(), "hi", &knowledge.RetrieveParams{TopK: 5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Content != `{"t":"hello"}` {
		t.Fatalf("Content = %q", c.Content)
	}
	if c.Score != 0.9 {
		t.Fatalf("Score = %v, want 0.9", c.Score)
	}
	if c.DocumentID != "d1" || c.CollectionID != "doc" {
		t.Fatalf("DocumentID/CollectionID = %q/%q", c.DocumentID, c.CollectionID)
	}
	if c.Source != "vector+graph" {
		t.Fatalf("Source = %q, want vector+graph", c.Source)
	}
	if c.Metadata["entity"] != "doc" {
		t.Fatalf("Metadata[entity] = %q", c.Metadata["entity"])
	}
	// Recall request was built from config + params.
	if rec.got.Query != "hi" || rec.got.Budget != 1234 || rec.got.K != 5 {
		t.Fatalf("recall req = %+v", rec.got)
	}
	if len(rec.got.Entities) != 1 || rec.got.Entities[0] != "doc" {
		t.Fatalf("recall entities = %v", rec.got.Entities)
	}
}

func TestRetrieve_CollectionOverridesEntities(t *testing.T) {
	rec := &fakeRecaller{}
	a := NewProvider(rec, WithEntities("doc", "note"))
	_, err := a.Retrieve(context.Background(), "q", &knowledge.RetrieveParams{Collection: "note"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(rec.got.Entities) != 1 || rec.got.Entities[0] != "note" {
		t.Fatalf("entities = %v, want [note]", rec.got.Entities)
	}
}

func TestRetrieve_FiltersByMinScoreAndCapsTopK(t *testing.T) {
	rec := &fakeRecaller{pack: agent.ContextPack{Items: []agent.ContextItem{
		item("doc", "a", 0.95, `{}`),
		item("doc", "b", 0.40, `{}`),
		item("doc", "c", 0.80, `{}`),
	}}}
	a := NewProvider(rec, WithEntities("doc"))
	chunks, err := a.Retrieve(context.Background(), "q", &knowledge.RetrieveParams{TopK: 1, MinScore: 0.5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// b is dropped (0.40 < 0.5); then capped to TopK=1 → only the first survivor.
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if chunks[0].DocumentID != "a" {
		t.Fatalf("kept %q, want a", chunks[0].DocumentID)
	}
}

func TestRetrieve_NilParamsUsesDefaults(t *testing.T) {
	rec := &fakeRecaller{}
	a := NewProvider(rec, WithEntities("doc"))
	if _, err := a.Retrieve(context.Background(), "q", nil); err != nil {
		t.Fatalf("Retrieve nil params: %v", err)
	}
	if rec.got.Budget != 4096 {
		t.Fatalf("budget = %d, want default 4096", rec.got.Budget)
	}
}
