// Package behavior defines the Behavior entity — what an agent does.
//
// Behaviors are condition-triggered action patterns — the "habits" and
// "reflexes" that compose skills and traits into contextual responses.
package behavior

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// TriggerType defines when a behavior activates.
type TriggerType string

const (
	TriggerOnInput      TriggerType = "on_input"
	TriggerOnToolResult TriggerType = "on_tool_result"
	TriggerOnError      TriggerType = "on_error"
	TriggerOnStepCount  TriggerType = "on_step_count"
	TriggerOnContext    TriggerType = "on_context"
	TriggerAlways       TriggerType = "always"
)

// Trigger defines when a behavior activates.
type Trigger struct {
	Type    TriggerType `json:"type"`
	Pattern string      `json:"pattern,omitempty"`
}

// ActionType defines what a behavior does when triggered.
type ActionType string

const (
	ActionInjectPrompt    ActionType = "inject_prompt"
	ActionPreferSkill     ActionType = "prefer_skill"
	ActionRequireTool     ActionType = "require_tool"
	ActionModifyParam     ActionType = "modify_param"
	ActionSwitchCognitive ActionType = "switch_cognitive"
	ActionAddGuardrail    ActionType = "add_guardrail"
)

// Action defines what happens when a behavior triggers.
type Action struct {
	Type   ActionType `json:"type"`
	Target string     `json:"target,omitempty"`
	Value  any        `json:"value,omitempty"`
}

// Behavior represents a condition-triggered action pattern.
type Behavior struct {
	cortex.Entity
	ID            id.BehaviorID  `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	AppID         string         `json:"app_id"`
	Triggers      []Trigger      `json:"triggers,omitempty"`
	Actions       []Action       `json:"actions,omitempty"`
	Priority      int            `json:"priority,omitempty"`
	RequiresSkill string         `json:"requires_skill,omitempty"`
	RequiresTrait string         `json:"requires_trait,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Store defines persistence for behaviors.
type Store interface {
	CreateBehavior(ctx context.Context, behavior *Behavior) error
	GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*Behavior, error)
	GetBehaviorByName(ctx context.Context, appID, name string) (*Behavior, error)
	UpdateBehavior(ctx context.Context, behavior *Behavior) error
	DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error
	ListBehaviors(ctx context.Context, filter *ListFilter) ([]*Behavior, error)
}

// ListFilter controls pagination for behavior listing.
type ListFilter struct {
	AppID  string
	Limit  int
	Offset int
}
