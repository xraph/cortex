package api

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
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
	Items []*CheckpointResponse `json:"items"`
}

// CheckpointResponse is a checkpoint as the API serves it. It has no
// field for the decider's principal: the store keeps those codec bytes
// as the record of who decided, and they are the host's own.
type CheckpointResponse struct {
	cortex.Entity
	ID        id.CheckpointID   `json:"id"`
	RunID     id.AgentRunID     `json:"run_id"`
	AgentID   id.AgentID        `json:"agent_id"`
	Scope     cortex.Scope      `json:"scope"`
	Reason    string            `json:"reason"`
	StepIndex int               `json:"step_index"`
	State     string            `json:"state"`
	Decision  *DecisionResponse `json:"decision,omitempty"`
	Metadata  map[string]any    `json:"metadata,omitempty"`
}

// DecisionResponse is a checkpoint decision as the API serves it.
type DecisionResponse struct {
	Approved  bool      `json:"approved"`
	DecidedBy string    `json:"decided_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// newCheckpointResponse copies cp into its response shape, leaving the
// decider's principal behind.
func newCheckpointResponse(cp *checkpoint.Checkpoint) *CheckpointResponse {
	if cp == nil {
		return nil
	}
	out := &CheckpointResponse{
		Entity:    cp.Entity,
		ID:        cp.ID,
		RunID:     cp.RunID,
		AgentID:   cp.AgentID,
		Scope:     cp.Scope,
		Reason:    cp.Reason,
		StepIndex: cp.StepIndex,
		State:     cp.State,
		Metadata:  cp.Metadata,
	}
	if d := cp.Decision; d != nil {
		out.Decision = &DecisionResponse{Approved: d.Approved, DecidedBy: d.DecidedBy, Reason: d.Reason, DecidedAt: d.DecidedAt}
	}
	return out
}

// ListSessionsResponse wraps a list of sessions.
type ListSessionsResponse struct {
	Items []*session.Session `json:"items"`
}

// CountSessionsResponse wraps a session count.
type CountSessionsResponse struct {
	Count int64 `json:"count"`
}

// ListToolsResponse wraps the engine's tool catalog.
type ListToolsResponse struct {
	Items []ToolResponse `json:"items"`
}

// ToolResponse describes one tool in the catalog.
type ToolResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kind is where the tool comes from: builtin, registered or external.
	Kind string `json:"kind" description:"builtin, registered or external"`
	// RetrySafe is whether the tool may run again after a crash.
	RetrySafe bool `json:"retry_safe"`
}

// GetConversationResponse wraps conversation messages.
type GetConversationResponse struct {
	Messages []memory.Message `json:"messages"`
}

// RunAgentResponse wraps the result of a synchronous agent run.
type RunAgentResponse struct {
	StopReason         string `json:"stop_reason,omitempty"`
	VerificationStatus string `json:"verification_status"`
	PublicationStatus  string `json:"publication_status"`
	ResultRef          string `json:"result_ref,omitempty"`
	RunID              string `json:"run_id"`
	Output             string `json:"output"`
	State              string `json:"state"`
	StepCount          int    `json:"step_count"`
	TokensUsed         int    `json:"tokens_used"`
	DurationMs         int64  `json:"duration_ms"`
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

// ── Orchestration response wrappers ─────────────────────

// ListOrchestrationsResponse wraps a list of orchestration configs.
type ListOrchestrationsResponse struct {
	Items []*orchestration.Config `json:"items"`
}

// ListOrchestrationRunsResponse wraps a list of orchestration runs.
type ListOrchestrationRunsResponse struct {
	Items []*orchestration.Run `json:"items"`
}

// RunOrchestrationResponse summarizes a completed orchestration run.
type RunOrchestrationResponse struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Strategy   string `json:"strategy"`
	Output     string `json:"output"`
	DurationMs int64  `json:"duration_ms"`
}

// ListA2AConversationsResponse is a page of messaging conversations.
type ListA2AConversationsResponse struct {
	Items []*a2a.Conversation `json:"items"`
}

// A2AConversationResponse is one messaging conversation with its
// transcript, oldest message first.
type A2AConversationResponse struct {
	Conversation *a2a.Conversation `json:"conversation"`
	Messages     []*a2a.Envelope   `json:"messages"`
}

// AgentInboxResponse is what an agent has waiting for it.
type AgentInboxResponse struct {
	Items []a2a.InboxItem `json:"items"`
}

// VerificationFailureResponse carries safe recovery metadata for a failed run.
// Details is optional because other conflicts can share the HTTP status code.
type VerificationFailureResponse struct {
	Error   string                   `json:"error"`
	Code    int                      `json:"code"`
	Details *run.VerificationFailure `json:"details,omitempty"`
}
