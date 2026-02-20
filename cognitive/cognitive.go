// Package cognitive defines the CognitiveStyle value object — how an agent thinks.
//
// Replaces rigid reasoning loop selection with a phase-based thinking strategy
// chain. The cognitive engine manages transitions between strategies based on
// conditions.
package cognitive

// Strategy represents a thinking approach, each maps to a reasoning loop implementation.
type Strategy string

const (
	StrategyAnalytical    Strategy = "analytical"
	StrategyCreative      Strategy = "creative"
	StrategyMethodical    Strategy = "methodical"
	StrategyReactive      Strategy = "reactive"
	StrategyReflective    Strategy = "reflective"
	StrategyCollaborative Strategy = "collaborative"
)

// TransitionCondition defines when the cognitive engine switches phases.
type TransitionCondition string

const (
	TransitionAfterSteps     TransitionCondition = "after_steps"
	TransitionOnStuck        TransitionCondition = "on_stuck"
	TransitionOnPlanComplete TransitionCondition = "on_plan_complete"
	TransitionOnError        TransitionCondition = "on_error"
)

// Phase represents one stage in a cognitive strategy chain.
type Phase struct {
	Strategy   Strategy            `json:"strategy"`
	MaxSteps   int                 `json:"max_steps,omitempty"`
	Transition TransitionCondition `json:"transition,omitempty"`
}

// Style defines how an agent thinks through problems.
type Style struct {
	Phases              []Phase `json:"phases,omitempty"`
	DepthPreference     float64 `json:"depth_preference,omitempty"`
	FocusPreference     float64 `json:"focus_preference,omitempty"`
	ReflectionFrequency float64 `json:"reflection_frequency,omitempty"`
}
