package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

type changingSourceStore struct {
	store.Store
	afterTrait func()
}

func (s *changingSourceStore) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	value, err := s.Store.GetTraitByName(ctx, name)
	if s.afterTrait != nil {
		change := s.afterTrait
		s.afterTrait = nil
		change()
	}
	return value, err
}

func TestPreparationRejectsSourcesChangedDuringCollection(t *testing.T) {
	for _, source := range []string{"stable", "agent", "persona", "skill", "trait", "overlay_added", "overlay_changed", "overlay_removed"} {
		t.Run(source, func(t *testing.T) {
			ctx := overlayCtx("collection")
			base := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", PersonaRef: "identity", InlineSkills: []string{"research"}, InlineTraits: []string{"careful"}}
			p := &persona.Persona{ID: id.NewPersonaID(), Name: "identity", Identity: "original"}
			sk := &skill.Skill{ID: id.NewSkillID(), Name: "research", SystemPromptFragment: "original"}
			tr := &trait.Trait{ID: id.NewTraitID(), Name: "careful", Description: "original"}
			o := &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID}
			for _, err := range []error{base.Create(ctx, ag), base.CreatePersona(ctx, p), base.CreateSkill(ctx, sk), base.CreateTrait(ctx, tr)} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if source == "overlay_changed" || source == "overlay_removed" {
				if err := base.CreateOverlay(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			s := &changingSourceStore{Store: base, afterTrait: func() {
				var err error
				switch source {
				case "agent":
					ag.SystemPrompt = "changed"
					err = base.Update(ctx, ag)
				case "persona":
					p.Identity = "changed"
					err = base.UpdatePersona(ctx, p)
				case "skill":
					sk.SystemPromptFragment = "changed"
					err = base.UpdateSkill(ctx, sk)
				case "trait":
					tr.Description = "changed"
					err = base.UpdateTrait(ctx, tr)
				case "overlay_added":
					err = base.CreateOverlay(ctx, o)
				case "overlay_changed":
					o.ToolsRemoved = []string{"probe"}
					err = base.UpdateOverlay(ctx, o)
				case "overlay_removed":
					err = base.DeleteOverlay(ctx, o.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			e, err := engine.New(engine.WithStore(s))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
			if source == "stable" {
				if err != nil {
					t.Fatal(err)
				}
				view, inspectErr := prepared.Inspect()
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				if view.Provenance.SourceValidation != "read_set_checked" || len(view.Provenance.SourceReads) != 5 {
					t.Fatalf("missing source validation receipt: %+v", view.Provenance)
				}
				return
			}
			if !errors.Is(err, cortex.ErrPreparationConflict) || prepared != nil {
				t.Fatalf("changed %s accepted: snapshotReturned=%v err=%v", source, prepared != nil, err)
			}
		})
	}
}

func TestExecutionRequiresExactAgentScope(t *testing.T) {
	child := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w"}, {Key: "project", Value: "p"}}})
	parent := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w"}}})
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "child"}
	if err := s.Create(child, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.PrepareAgent(parent, ag.ID, nil); err == nil {
		t.Error("prepared child under ancestor authority")
	}
	if _, err = e.RunAgent(parent, ag.Name, "work", nil); err == nil {
		t.Error("ran child under ancestor authority")
	}
	events := make(chan engine.StreamEvent, 32)
	if err = e.StreamAgent(parent, ag.Name, "work", nil, events); err == nil {
		t.Error("streamed child under ancestor authority")
	}
	for event := range events {
		t.Errorf("unexpected event: %s", event.Type)
	}
}
