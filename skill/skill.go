// Package skill defines the Skill entity — what an agent can do.
//
// A Skill bundles related tools, knowledge sources, and behavioral guidance
// into a coherent capability. Think of it as a human's professional training.
package skill

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Proficiency represents mastery level of a skill or tool binding.
type Proficiency string

const (
	ProficiencyNovice     Proficiency = "novice"
	ProficiencyApprentice Proficiency = "apprentice"
	ProficiencyCompetent  Proficiency = "competent"
	ProficiencyProficient Proficiency = "proficient"
	ProficiencyExpert     Proficiency = "expert"
)

// Weight returns the numeric weight for a proficiency level.
func (p Proficiency) Weight() float64 {
	switch p {
	case ProficiencyNovice:
		return 0.2
	case ProficiencyApprentice:
		return 0.4
	case ProficiencyCompetent:
		return 0.6
	case ProficiencyProficient:
		return 0.8
	case ProficiencyExpert:
		return 1.0
	default:
		return 0.6
	}
}

// ToolBinding binds a tool to a skill with mastery and contextual guidance.
type ToolBinding struct {
	ToolName   string      `json:"tool_name"`
	Mastery    Proficiency `json:"mastery,omitempty"`
	Guidance   string      `json:"guidance,omitempty"`
	PreferWhen string      `json:"prefer_when,omitempty"`
}

// KnowledgeRef references a knowledge source to inject when a skill is active.
type KnowledgeRef struct {
	Source     string `json:"source"`
	InjectMode string `json:"inject_mode,omitempty"`
	Priority   int    `json:"priority,omitempty"`
}

// Skill represents a coherent capability an agent can have.
type Skill struct {
	cortex.Entity
	ID                   id.SkillID     `json:"id"`
	Name                 string         `json:"name"`
	Description          string         `json:"description,omitempty"`
	AppID                string         `json:"app_id"`
	Tools                []ToolBinding  `json:"tools,omitempty"`
	Knowledge            []KnowledgeRef `json:"knowledge,omitempty"`
	SystemPromptFragment string         `json:"system_prompt_fragment,omitempty"`
	Dependencies         []string       `json:"dependencies,omitempty"`
	DefaultProficiency   Proficiency    `json:"default_proficiency,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
}

// Store defines persistence for skills.
type Store interface {
	CreateSkill(ctx context.Context, skill *Skill) error
	GetSkill(ctx context.Context, skillID id.SkillID) (*Skill, error)
	GetSkillByName(ctx context.Context, appID, name string) (*Skill, error)
	UpdateSkill(ctx context.Context, skill *Skill) error
	DeleteSkill(ctx context.Context, skillID id.SkillID) error
	ListSkills(ctx context.Context, filter *ListFilter) ([]*Skill, error)
}

// ListFilter controls pagination for skill listing.
type ListFilter struct {
	AppID  string
	Limit  int
	Offset int
}
