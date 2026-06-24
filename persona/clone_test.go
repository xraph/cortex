package persona_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

func TestClonePersonaResetsIdentityAndDeepCopies(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &persona.Persona{
		Entity:   cortex.Entity{CreatedAt: old, UpdatedAt: old},
		ID:       id.NewPersonaID(),
		Name:     "original",
		AppID:    "app1",
		Identity: "I am a helper",
		Skills:   []persona.SkillAssignment{{SkillName: "research"}},
		Traits: []persona.TraitAssignment{
			{TraitName: "openness", DimensionValues: map[string]float64{"curiosity": 0.8}},
		},
		Behaviors: []string{"greet"},
		Metadata:  map[string]any{"m": "n"},
	}

	newID := id.NewPersonaID()
	clone, err := persona.ClonePersona(src, newID, "copy")
	if err != nil {
		t.Fatalf("ClonePersona: %v", err)
	}

	// Identity reset.
	if clone.ID.String() != newID.String() {
		t.Errorf("ID = %q, want %q", clone.ID, newID)
	}
	if clone.Name != "copy" {
		t.Errorf("Name = %q, want copy", clone.Name)
	}
	if clone.CreatedAt.Equal(old) || clone.UpdatedAt.Equal(old) {
		t.Errorf("timestamps not reset")
	}

	// Preserved.
	if clone.AppID != "app1" || clone.Identity != "I am a helper" {
		t.Errorf("preserved fields wrong: %+v", clone)
	}

	// Deep-copy independence.
	clone.Skills[0].SkillName = "MUT"
	clone.Traits[0].DimensionValues["curiosity"] = 0.1
	clone.Behaviors[0] = "MUT"
	clone.Metadata["m"] = "MUT"
	if src.Skills[0].SkillName != "research" {
		t.Error("Skills slice aliased to source")
	}
	if src.Traits[0].DimensionValues["curiosity"] != 0.8 {
		t.Error("Traits DimensionValues map aliased to source")
	}
	if src.Behaviors[0] != "greet" {
		t.Error("Behaviors slice aliased to source")
	}
	if src.Metadata["m"] != "n" {
		t.Error("Metadata map aliased to source")
	}
}
