package agent_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

func TestCloneConfigResetsIdentityAndDeepCopies(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &agent.Config{
		Entity:       cortex.Entity{CreatedAt: old, UpdatedAt: old},
		ID:           id.NewAgentID(),
		Name:         "original",
		AppID:        "app1",
		SystemPrompt: "sp",
		Model:        "smart",
		Tools:        []string{"t1", "t2"},
		Guardrails:   map[string]any{"budget": "10"},
		Metadata:     map[string]any{"m": "n"},
		InlineSkills: []string{"s1"},
		Enabled:      true,
		PersonaRef:   "p1",
	}

	newID := id.NewAgentID()
	clone, err := agent.CloneConfig(src, newID, "copy")
	if err != nil {
		t.Fatalf("CloneConfig: %v", err)
	}

	// Identity reset.
	if clone.ID.String() != newID.String() {
		t.Errorf("ID = %q, want %q", clone.ID, newID)
	}
	if clone.Name != "copy" {
		t.Errorf("Name = %q, want copy", clone.Name)
	}
	if clone.CreatedAt.Equal(old) || clone.UpdatedAt.Equal(old) {
		t.Errorf("timestamps not reset: created=%v updated=%v", clone.CreatedAt, clone.UpdatedAt)
	}

	// Config preserved.
	if clone.AppID != "app1" || clone.SystemPrompt != "sp" || clone.Model != "smart" ||
		!clone.Enabled || clone.PersonaRef != "p1" {
		t.Errorf("preserved fields wrong: %+v", clone)
	}

	// Deep-copy independence: mutate clone, source must not change.
	clone.Tools[0] = "MUT"
	clone.Guardrails["budget"] = "MUT"
	clone.Metadata["m"] = "MUT"
	clone.InlineSkills[0] = "MUT"
	if src.Tools[0] != "t1" {
		t.Error("Tools slice aliased to source")
	}
	if src.Guardrails["budget"] != "10" {
		t.Error("Guardrails map aliased to source")
	}
	if src.Metadata["m"] != "n" {
		t.Error("Metadata map aliased to source")
	}
	if src.InlineSkills[0] != "s1" {
		t.Error("InlineSkills slice aliased to source")
	}
}
