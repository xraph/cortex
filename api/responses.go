package api

import (
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// ── List response wrappers ──────────────────────────────
// Forge router requires handler responses to be pointer types.

// ListAgentsResponse wraps a list of agents.
type ListAgentsResponse struct {
	Items []*agent.Config `json:"items"`
}

// ListRunsResponse wraps a list of runs.
type ListRunsResponse struct {
	Items []*run.Run `json:"items"`
}

// ListSkillsResponse wraps a list of skills.
type ListSkillsResponse struct {
	Items []*skill.Skill `json:"items"`
}

// ListTraitsResponse wraps a list of traits.
type ListTraitsResponse struct {
	Items []*trait.Trait `json:"items"`
}

// ListBehaviorsResponse wraps a list of behaviors.
type ListBehaviorsResponse struct {
	Items []*behavior.Behavior `json:"items"`
}

// ListPersonasResponse wraps a list of personas.
type ListPersonasResponse struct {
	Items []*persona.Persona `json:"items"`
}

// ListCheckpointsResponse wraps a list of checkpoints.
type ListCheckpointsResponse struct {
	Items []*checkpoint.Checkpoint `json:"items"`
}

// ListToolsResponse wraps a list of tools.
type ListToolsResponse struct {
	Items []map[string]any `json:"items"`
}

// GetConversationResponse wraps conversation messages.
type GetConversationResponse struct {
	Messages []memory.Message `json:"messages"`
}

// RunAgentResponse wraps the result of a synchronous agent run.
type RunAgentResponse struct {
	RunID      string `json:"run_id"`
	Output     string `json:"output"`
	State      string `json:"state"`
	StepCount  int    `json:"step_count"`
	TokensUsed int    `json:"tokens_used"`
	DurationMs int64  `json:"duration_ms"`
}

// PreviewPromptResponse wraps the computed system prompt preview.
type PreviewPromptResponse struct {
	Prompt string `json:"prompt"`
}

// StreamEvent represents a single SSE event during agent streaming.
type StreamEvent struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}
