// Package agent defines the agent configuration entity and its store interface.
package agent

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
)

// Config defines an agent's configuration. Supports both flat mode (tools list + system prompt)
// and persona mode (PersonaRef or inline skill/trait/behavior assignments).
type Config struct {
	cortex.Entity
	ID          id.AgentID   `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Scope       cortex.Scope `json:"scope"`
	// SystemPrompt is the agent's own instructions. It is a derived
	// field once Sections is in play: see Sections for which of the two
	// wins.
	SystemPrompt  string         `json:"system_prompt"`
	Model         string         `json:"model,omitempty"`
	Tools         []string       `json:"tools,omitempty"`
	MaxSteps      int            `json:"max_steps,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   float64        `json:"temperature,omitempty"`
	ReasoningLoop string         `json:"reasoning_loop,omitempty"`
	Guardrails    map[string]any `json:"guardrails,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Enabled       bool           `json:"enabled"`

	// Persona fields.
	PersonaRef      string   `json:"persona_ref,omitempty"`
	InlineSkills    []string `json:"inline_skills,omitempty"`
	InlineTraits    []string `json:"inline_traits,omitempty"`
	InlineBehaviors []string `json:"inline_behaviors,omitempty"`

	// Sections is the system prompt as an ordered set of addressable
	// pieces, which is what a scope overlay patches. It is empty for
	// every agent that only ever set SystemPrompt, and an empty Sections
	// means "use SystemPrompt as-is" rather than "this agent has no
	// prompt": an agent written before sections existed has to assemble
	// to the exact string it always did.
	//
	// Sections and SystemPrompt can both be set, so they need a stated
	// precedence, and it is this: sections are the truth.
	//
	//   - Sections non-empty: the prompt comes from the sections, and
	//     SystemPrompt is derived from them. Whatever a host put in
	//     SystemPrompt is overwritten rather than merged, because two
	//     sources of truth for one prompt is how a prompt starts lying
	//     about itself.
	//   - Sections empty and SystemPrompt non-empty: SystemPrompt lowers
	//     into a single section with id "role", which is what keeps
	//     every agent written before sections existed assembling to the
	//     byte-identical string it always did.
	//   - Both empty: the agent contributes nothing of its own, and the
	//     prompt is whatever its persona, skills and traits produce.
	//
	// PromptSections and SyncSystemPrompt are the two directions of that
	// rule.
	Sections []prompt.Section `json:"sections,omitempty"`
}

// RoleSectionID is the section an agent's plain SystemPrompt lowers into
// when the agent has no sections of its own. An overlay that wants to
// reach a legacy agent's instructions patches this id.
const RoleSectionID = "role"

// PromptSections returns the agent's own contribution to its assembled
// prompt, applying the precedence documented on Config.Sections: its
// sections when it has any, otherwise its SystemPrompt lowered into a
// single role section.
//
// The returned slice is a copy, so a caller reordering or patching it
// does not reach back into the stored config.
func (c *Config) PromptSections() []prompt.Section {
	if len(c.Sections) > 0 {
		out := make([]prompt.Section, len(c.Sections))
		copy(out, c.Sections)

		return out
	}

	if c.SystemPrompt == "" {
		return nil
	}

	return []prompt.Section{{
		ID:     RoleSectionID,
		Source: prompt.SourceHost,
		Body:   c.SystemPrompt,
		Order:  prompt.OrderRole,
	}}
}

// SyncSystemPrompt rewrites SystemPrompt from Sections so the stored
// string matches the sections it is derived from. It is a no-op for an
// agent with no sections, whose SystemPrompt is the source rather than
// the derivative.
//
// Callers run this before a write. Without it a host that edits sections
// leaves a stale SystemPrompt behind, and every reader that has not been
// taught about sections yet keeps serving the old prompt.
func (c *Config) SyncSystemPrompt() {
	if len(c.Sections) == 0 {
		return
	}

	c.SystemPrompt = prompt.Assemble(c.Sections)
}

// HasPersona returns true if this agent uses the persona system.
func (c *Config) HasPersona() bool {
	return c.PersonaRef != "" ||
		len(c.InlineSkills) > 0 ||
		len(c.InlineTraits) > 0 ||
		len(c.InlineBehaviors) > 0
}

// Store defines persistence for agent configs.
type Store interface {
	Create(ctx context.Context, config *Config) error
	Get(ctx context.Context, agentID id.AgentID) (*Config, error)
	GetByName(ctx context.Context, name string) (*Config, error)
	Update(ctx context.Context, config *Config) error
	Delete(ctx context.Context, agentID id.AgentID) error
	List(ctx context.Context, filter *ListFilter) ([]*Config, error)
	CountAgents(ctx context.Context, filter *ListFilter) (int64, error)
}

// ListFilter controls pagination and matching for agent listing. Scope
// arrives on the context; Exact narrows to rows stored at precisely that
// depth instead of everything beneath it.
type ListFilter struct {
	Exact  bool
	Search string
	Limit  int
	Offset int
}
