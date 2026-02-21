// Package agent defines the agent configuration entity and its store interface.
package agent

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Config defines an agent's configuration. Supports both flat mode (tools list + system prompt)
// and persona mode (PersonaRef or inline skill/trait/behavior assignments).
type Config struct {
	cortex.Entity
	ID            id.AgentID     `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	AppID         string         `json:"app_id"`
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
	GetByName(ctx context.Context, appID, name string) (*Config, error)
	Update(ctx context.Context, config *Config) error
	Delete(ctx context.Context, agentID id.AgentID) error
	List(ctx context.Context, filter *ListFilter) ([]*Config, error)
}

// ListFilter controls pagination for agent listing.
type ListFilter struct {
	AppID  string
	Limit  int
	Offset int
}
