package cortex

import (
	"context"

	"github.com/xraph/cortex/id"
)

// RunOpts is the subset of run overrides a caller needs when invoking an
// agent through AgentRunner. The engine maps it to its own RunOverrides.
type RunOpts struct {
	Model        string
	Temperature  *float64
	MaxSteps     int
	SystemPrompt string
}

// AgentResult is the caller-facing view of one completed agent run.
type AgentResult struct {
	AgentName string        `json:"agent_name"`
	RunID     id.AgentRunID `json:"run_id,omitempty"`
	Output    string        `json:"output"`
	Err       error         `json:"-"`
}

// AgentRunner is the one host capability the coordination packages depend
// on: run a named agent and hand back its result. The engine satisfies it
// through a thin adapter, which is what keeps orchestration and a2a from
// importing the engine.
type AgentRunner interface {
	RunAgent(ctx context.Context, agentName, input string, opts *RunOpts) (*AgentResult, error)
}
