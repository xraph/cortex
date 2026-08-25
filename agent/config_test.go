package agent_test

import (
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/prompt"
)

// TestPromptSections_SectionsWinOverSystemPrompt pins the precedence the
// two fields need and would otherwise not have. Sections are the truth:
// an agent carrying both must assemble from its sections, not from the
// string a host also left in SystemPrompt.
func TestPromptSections_SectionsWinOverSystemPrompt(t *testing.T) {
	c := &agent.Config{
		SystemPrompt: "stale, from before the sections were written",
		Sections: []prompt.Section{
			{ID: "role", Body: "the current instructions", Order: prompt.OrderRole},
		},
	}

	got := c.PromptSections()
	if len(got) != 1 {
		t.Fatalf("PromptSections returned %d sections, want 1", len(got))
	}
	if got[0].Body != "the current instructions" {
		t.Errorf("PromptSections body = %q, want the section's body; SystemPrompt must not win over Sections", got[0].Body)
	}
}

// TestPromptSections_SystemPromptLowersIntoARoleSection is the other
// direction, and it is what every agent written before sections existed
// depends on.
func TestPromptSections_SystemPromptLowersIntoARoleSection(t *testing.T) {
	c := &agent.Config{SystemPrompt: "you answer questions about the deploy pipeline"}

	got := c.PromptSections()
	if len(got) != 1 {
		t.Fatalf("PromptSections returned %d sections, want 1", len(got))
	}

	want := prompt.Section{
		ID:     agent.RoleSectionID,
		Source: prompt.SourceHost,
		Body:   "you answer questions about the deploy pipeline",
		Order:  prompt.OrderRole,
	}
	if got[0] != want {
		t.Errorf("PromptSections = %+v, want %+v", got[0], want)
	}
}

func TestPromptSections_BothEmptyProducesNothing(t *testing.T) {
	c := &agent.Config{Name: "bare"}

	if got := c.PromptSections(); len(got) != 0 {
		t.Errorf("PromptSections on an agent with neither = %+v, want none", got)
	}
}

// The returned slice is handed to overlay patching, which is free to
// reorder and rewrite it. A shared backing array would let that reach
// back into the stored config.
func TestPromptSections_ReturnsACopy(t *testing.T) {
	c := &agent.Config{Sections: []prompt.Section{{ID: "role", Body: "original"}}}

	got := c.PromptSections()
	got[0].Body = "mutated by the caller"

	if c.Sections[0].Body != "original" {
		t.Errorf("Config.Sections[0].Body = %q after the caller mutated the returned slice, want %q", c.Sections[0].Body, "original")
	}
}

func TestSyncSystemPrompt_DerivesTheStringFromTheSections(t *testing.T) {
	c := &agent.Config{
		SystemPrompt: "stale",
		Sections: []prompt.Section{
			{ID: "role", Body: "first", Order: prompt.OrderRole},
			{ID: "persona:identity", Body: "second", Order: prompt.OrderPersona},
		},
	}

	c.SyncSystemPrompt()

	if want := "first\n\nsecond"; c.SystemPrompt != want {
		t.Errorf("SystemPrompt after sync = %q, want %q", c.SystemPrompt, want)
	}
}

// An agent with no sections has SystemPrompt as its source, not its
// derivative, so syncing must leave it alone rather than blank it.
func TestSyncSystemPrompt_LeavesASectionlessAgentAlone(t *testing.T) {
	c := &agent.Config{SystemPrompt: "the only prompt this agent has"}

	c.SyncSystemPrompt()

	if want := "the only prompt this agent has"; c.SystemPrompt != want {
		t.Errorf("SystemPrompt after sync = %q, want %q", c.SystemPrompt, want)
	}
}
