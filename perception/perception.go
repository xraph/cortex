// Package perception defines the Perception value object — how an agent sees.
package perception

// AttentionFilter directs the agent's focus to specific aspects of input.
type AttentionFilter struct {
	Name     string   `json:"name"`
	Keywords []string `json:"keywords,omitempty"`
	Patterns []string `json:"patterns,omitempty"`
	Prompt   string   `json:"prompt,omitempty"`
}

// Model defines how an agent perceives and filters information.
type Model struct {
	AttentionFilters  []AttentionFilter `json:"attention_filters,omitempty"`
	ContextWindow     float64           `json:"context_window,omitempty"`
	DetailOrientation float64           `json:"detail_orientation,omitempty"`
}
