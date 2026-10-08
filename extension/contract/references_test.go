package contract

import (
	"context"
	"errors"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

func TestReferenceChoicesUseStoredOwnerScope(t *testing.T) {
	d, s, _ := testService(t)
	p := principal("a")
	ctx, err := s.context(context.Background(), p, "manage")
	if err != nil {
		t.Fatal(err)
	}
	childScope := cortex.ScopeFromContext(ctx)
	childScope.Levels = append(childScope.Levels, cortex.Level{Key: "project", Value: "child"})
	child := cortex.WithScope(ctx, childScope)
	for _, scoped := range []context.Context{ctx, child} {
		if createErr := s.deps.Engine.CreatePersona(scoped, &persona.Persona{ID: id.NewPersonaID(), Name: "operator"}); createErr != nil {
			t.Fatal(createErr)
		}
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "runner"}
	if createErr := s.deps.Engine.CreateAgent(child, ag); createErr != nil {
		t.Fatal(createErr)
	}
	raw, err := dispatch(t, d, p, "references.list", ReferenceInput{Kind: "personas", OwnerKind: "agents", OwnerID: ag.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[ListOutput[Reference]](t, raw)
	if out.Total != 1 || len(out.Items) != 1 || out.Items[0].Scope.Canonical() != childScope.Canonical() {
		t.Fatalf("choices: %+v", out)
	}
	if _, foreignErr := dispatch(t, d, principal("b"), "references.list", ReferenceInput{Kind: "personas", OwnerKind: "agents", OwnerID: ag.ID.String()}); !errors.Is(foreignErr, dc.ErrNotFound) {
		t.Fatalf("foreign owner: %v", foreignErr)
	}
	raw, err = dispatch(t, d, p, "agents.stats", IDInput{ag.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	stats := decode[map[string]any](t, raw)
	if stats["success_rate"] != nil || stats["total"] != float64(0) {
		t.Fatalf("empty stats: %s", raw)
	}
}
