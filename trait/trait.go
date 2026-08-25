// Package trait defines the Trait entity — who an agent is.
//
// Traits are personality dimensions that influence HOW an agent works.
// They are declarative, not imperative — you don't tell the agent what to do,
// you tell it who it is.
package trait

import (
	"context"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
)

// Dimension represents a bipolar personality axis.
// Value ranges from 0.0 to 1.0 where each extreme has a label.
type Dimension struct {
	Name      string  `json:"name"`
	LowLabel  string  `json:"low_label"`
	HighLabel string  `json:"high_label"`
	Value     float64 `json:"value"`
}

// InfluenceTarget is what a trait influence modifies at runtime.
type InfluenceTarget string

const (
	TargetPromptInjection InfluenceTarget = "prompt_injection"
	TargetTemperature     InfluenceTarget = "temperature"
	TargetMaxSteps        InfluenceTarget = "max_steps"
	TargetToolSelection   InfluenceTarget = "tool_selection"
	TargetResponseStyle   InfluenceTarget = "response_style"
)

// Influence describes a runtime modification applied by a trait.
type Influence struct {
	Target    InfluenceTarget `json:"target"`
	Value     any             `json:"value"`
	Condition string          `json:"condition,omitempty"`
	Weight    float64         `json:"weight,omitempty"`
}

// Category groups related traits.
type Category string

const (
	CategoryPersonality   Category = "personality"
	CategoryWorkstyle     Category = "workstyle"
	CategoryCommunication Category = "communication"
	CategoryRisk          Category = "risk"
)

// Trait represents a personality dimension that influences agent behavior.
type Trait struct {
	cortex.Entity
	ID          id.TraitID     `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Scope       cortex.Scope   `json:"scope"`
	Dimensions  []Dimension    `json:"dimensions,omitempty"`
	Influences  []Influence    `json:"influences,omitempty"`
	Category    Category       `json:"category,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Store defines persistence for traits.
type Store interface {
	CreateTrait(ctx context.Context, trait *Trait) error
	GetTrait(ctx context.Context, traitID id.TraitID) (*Trait, error)
	GetTraitByName(ctx context.Context, name string) (*Trait, error)
	UpdateTrait(ctx context.Context, trait *Trait) error
	DeleteTrait(ctx context.Context, traitID id.TraitID) error
	ListTraits(ctx context.Context, filter *ListFilter) ([]*Trait, error)
	CountTraits(ctx context.Context, filter *ListFilter) (int64, error)
}

// ListFilter controls pagination and matching for trait listing. Scope
// arrives on the context; Exact narrows to rows stored at precisely that
// depth instead of everything beneath it.
type ListFilter struct {
	Exact    bool
	Category Category
	Search   string
	Limit    int
	Offset   int
}

// Sections returns the trait's contribution to an agent's system prompt:
// one section per prompt-injection influence, in the order the
// influences are declared. A trait with no prompt injections returns
// nothing, which is the common case for traits that only move
// temperature or tool selection.
//
// order is where the trait's first section sits in the assembled prompt,
// and further sections take consecutive orders from there. A caller
// walking an agent's trait list starts at prompt.OrderTrait and advances
// by the number of sections each trait returned, since assembly falls
// back to sorting by ID when two sections share an order and that would
// reorder the traits alphabetically.
//
// The heading stays inside Body rather than moving to Title so the
// assembled text is byte-identical to the prompt this injection has
// always produced. Title is for sections a host authors.
//
// The first injection is addressed as "trait:<name>". A trait with more
// than one gets "trait:<name>:2" onwards, so the ordinary single
// injection keeps the ID an overlay author would guess.
func (t *Trait) Sections(order int) []prompt.Section {
	var out []prompt.Section

	for _, inf := range t.Influences {
		if inf.Target != TargetPromptInjection {
			continue
		}

		v, ok := inf.Value.(string)
		if !ok || v == "" {
			continue
		}

		sectionID := "trait:" + t.Name
		if len(out) > 0 {
			sectionID = fmt.Sprintf("trait:%s:%d", t.Name, len(out)+1)
		}

		out = append(out, prompt.Section{
			ID:     sectionID,
			Source: prompt.SourceTrait,
			Body:   "## Trait: " + t.Name + "\n" + v,
			Order:  order + len(out),
		})
	}

	return out
}
