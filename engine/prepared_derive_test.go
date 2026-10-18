package engine_test

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
)

func TestPreparedDerivationUsesFrozenBaseline(t *testing.T) {
	ctx := overlayCtx("derived")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Sections: []prompt.Section{{ID: "role", Body: "original", Order: 1}, {ID: "instructions", Body: "old instructions", Order: 2}}}
	e, s, rec := newConfigEngine(t, ctx, ag)
	baseline, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag.Sections[1].Body = "live replacement"
	if err = s.Update(ctx, ag); err != nil {
		t.Fatal(err)
	}
	view, err := baseline.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	view.Sections[1].Body = "detached mutation"
	derived, err := baseline.Derive(engine.PreparedEdits{SectionBodies: map[string]string{"role": "candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.RunPrepared(ctx, derived, "work", engine.PreparedRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Request(t).System; got != "candidate\n\nold instructions" {
		t.Fatalf("derived from live or mutable data: %q", got)
	}
	if r.Provenance.DerivedFrom != baseline.Digest() || derived.Digest() == baseline.Digest() {
		t.Fatal("derivation lost baseline identity")
	}
	if _, err = e.RunPrepared(ctx, baseline, "work", engine.PreparedRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Request(t).System, "candidate") {
		t.Fatal("derivation mutated baseline")
	}
}

func TestPreparedDerivationRejectsAuthorityExpansion(t *testing.T) {
	ctx := overlayCtx("derived")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Sections: []prompt.Section{{ID: "policy", Body: "pinned", Locked: true}}}
	e, _, _ := newConfigEngine(t, ctx, ag)
	baseline, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	expanded := []string{"invented"}
	invalidTemp := 3.0
	for _, edits := range []engine.PreparedEdits{
		{SectionBodies: map[string]string{"policy": "replacement"}},
		{SectionBodies: map[string]string{"absent": "new"}},
		{Tools: &expanded},
		{ToolDescriptions: map[string]string{"invented": "new"}},
		{Temperature: &invalidTemp},
	} {
		if _, deriveErr := baseline.Derive(edits); deriveErr == nil {
			t.Fatalf("invalid edit accepted: %+v", edits)
		}
	}
	empty := []string{}
	derived, err := baseline.Derive(engine.PreparedEdits{Tools: &empty})
	if err != nil {
		t.Fatal(err)
	}
	view, err := derived.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tools) != 0 || !view.Config.AllToolsRestricted {
		t.Fatal("empty subset regained implicit tools")
	}
}
