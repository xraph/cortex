// Package trait defines the Trait entity — who an agent is.
//
// Traits are personality dimensions that influence HOW an agent works.
// They are declarative, not imperative — you don't tell the agent what to do,
// you tell it who it is.
package trait

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
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
	AppID       string         `json:"app_id"`
	Dimensions  []Dimension    `json:"dimensions,omitempty"`
	Influences  []Influence    `json:"influences,omitempty"`
	Category    Category       `json:"category,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Store defines persistence for traits.
type Store interface {
	CreateTrait(ctx context.Context, trait *Trait) error
	GetTrait(ctx context.Context, traitID id.TraitID) (*Trait, error)
	GetTraitByName(ctx context.Context, appID, name string) (*Trait, error)
	UpdateTrait(ctx context.Context, trait *Trait) error
	DeleteTrait(ctx context.Context, traitID id.TraitID) error
	ListTraits(ctx context.Context, filter *ListFilter) ([]*Trait, error)
}

// ListFilter controls pagination and filtering for trait listing.
type ListFilter struct {
	AppID    string
	Category Category
	Limit    int
	Offset   int
}
