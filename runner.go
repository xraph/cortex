package cortex

import (
	"context"
	"errors"

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

// AgentResult is the caller-facing view of an agent run. A waiting result retains
// its identity without a terminal answer; callers must inspect both error values.
type AgentResult struct {
	AgentName string        `json:"agent_name"`
	RunID     id.AgentRunID `json:"run_id,omitempty"`
	Output    string        `json:"output"`
	Err       error         `json:"-"`
}

// IsOrchestrationWaiting reports whether every nonnil leaf of an error tree is
// ErrOrchestrationWaiting. A mixed failure must never be treated as suspension.
func IsOrchestrationWaiting(err error) bool {
	if err == nil {
		return false
	}
	if children, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range children.Unwrap() {
			if child != nil {
				found = true
				if !IsOrchestrationWaiting(child) {
					return false
				}
			}
		}
		return found
	}
	if child, ok := err.(interface{ Unwrap() error }); ok {
		return IsOrchestrationWaiting(child.Unwrap())
	}
	return errors.Is(err, ErrOrchestrationWaiting)
}

// AgentRunner is the one host capability the coordination packages depend
// on: run a named agent and hand back its result. The engine satisfies it
// through a thin adapter, which is what keeps orchestration and a2a from
// importing the engine.
type AgentRunner interface {
	RunAgent(ctx context.Context, agentName, input string, opts *RunOpts) (*AgentResult, error)
}

// ReservedInvocation identifies a child before execution. Execute must start
// that exact run at most once. Unused allocations remain reserved until the
// host can prove that releasing them is safe.
type ReservedInvocation struct {
	RunID   id.AgentRunID
	Execute func(context.Context) (*AgentResult, error)
}

// ReservedAgentRunner reserves capacity and identity without executing tools
// or calling a provider. Return the invocation with an error when its identity
// needs reconciliation. Callers must persist the identity before execution and
// must never execute an invocation returned with an error.
type ReservedAgentRunner interface {
	ReserveAgent(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error)
}
