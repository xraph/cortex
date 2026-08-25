// Package skill defines the Skill entity — what an agent can do.
//
// A Skill bundles related tools, knowledge sources, and behavioral guidance
// into a coherent capability. Think of it as a human's professional training.
package skill

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
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
	Scope                cortex.Scope   `json:"scope"`
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
	GetSkillByName(ctx context.Context, name string) (*Skill, error)
	UpdateSkill(ctx context.Context, skill *Skill) error
	DeleteSkill(ctx context.Context, skillID id.SkillID) error
	ListSkills(ctx context.Context, filter *ListFilter) ([]*Skill, error)
	CountSkills(ctx context.Context, filter *ListFilter) (int64, error)
}

// ListFilter controls pagination and matching for skill listing. Scope
// arrives on the context; Exact narrows to rows stored at precisely that
// depth instead of everything beneath it.
type ListFilter struct {
	Exact  bool
	Search string
	Limit  int
	Offset int
}

// Sections returns the skill's contribution to an agent's system prompt:
// one section carrying the prompt fragment, or nothing when the skill
// has no fragment.
//
// order is where this skill's section sits in the assembled prompt. A
// caller walking an agent's skill list starts at prompt.OrderSkill and
// advances by one section each time, because assembly falls back to
// sorting by ID when two sections share an order and that would put the
// skills in alphabetical order instead of the order the agent lists
// them.
//
// The heading stays inside Body rather than moving to Title, so the
// assembled text is byte-identical to the prompt this fragment has
// always produced. Title is for sections a host authors.
func (s *Skill) Sections(order int) []prompt.Section {
	if s.SystemPromptFragment == "" {
		return nil
	}

	return []prompt.Section{{
		ID:     "skill:" + s.Name,
		Source: prompt.SourceSkill,
		Body:   "## Skill: " + s.Name + "\n" + s.SystemPromptFragment,
		Order:  order,
	}}
}
