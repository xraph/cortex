// Package persona defines the Persona entity — the whole person.
//
// A Persona is the composition entity that brings everything together.
// It defines an agent's complete identity by composing skills, traits,
// behaviors, cognitive style, communication style, and perception.
package persona

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/communication"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/perception"
	"github.com/xraph/cortex/skill"
)

// SkillAssignment assigns a skill to a persona with an optional proficiency override.
type SkillAssignment struct {
	SkillName   string           `json:"skill_name"`
	Proficiency skill.Proficiency `json:"proficiency,omitempty"`
}

// TraitAssignment assigns a trait to a persona with dimension value overrides.
type TraitAssignment struct {
	TraitName       string             `json:"trait_name"`
	DimensionValues map[string]float64 `json:"dimension_values,omitempty"`
}

// Persona represents a complete agent identity composed from skills, traits, and behaviors.
type Persona struct {
	cortex.Entity
	ID                 id.PersonaID        `json:"id"`
	Name               string              `json:"name"`
	Description        string              `json:"description,omitempty"`
	AppID              string              `json:"app_id"`
	Identity           string              `json:"identity"`
	Skills             []SkillAssignment   `json:"skills,omitempty"`
	Traits             []TraitAssignment   `json:"traits,omitempty"`
	Behaviors          []string            `json:"behaviors,omitempty"`
	CognitiveStyle     cognitive.Style     `json:"cognitive_style,omitempty"`
	CommunicationStyle communication.Style `json:"communication_style,omitempty"`
	Perception         perception.Model    `json:"perception,omitempty"`
	Metadata           map[string]any      `json:"metadata,omitempty"`
}

// Store defines persistence for personas.
type Store interface {
	CreatePersona(ctx context.Context, persona *Persona) error
	GetPersona(ctx context.Context, personaID id.PersonaID) (*Persona, error)
	GetPersonaByName(ctx context.Context, appID, name string) (*Persona, error)
	UpdatePersona(ctx context.Context, persona *Persona) error
	DeletePersona(ctx context.Context, personaID id.PersonaID) error
	ListPersonas(ctx context.Context, filter *ListFilter) ([]*Persona, error)
}

// ListFilter controls pagination for persona listing.
type ListFilter struct {
	AppID  string
	Limit  int
	Offset int
}
